// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// trainingContext also cancels on Ctrl-C/SIGTERM: the training JVM runs in its
// own process group (see isolateTrainingGroup), so the terminal no longer
// signals it directly.
func trainingContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}

// isolateTrainingGroup puts the training JVM in a fresh process group and makes
// cancellation kill the whole group, so a JEP 514 cache-assembly child spawned by
// the launcher dies with it instead of being orphaned.
func isolateTrainingGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
}

// reapTrainingGroup kills whatever is left of the training process group and
// waits (bounded) until it is gone, so nothing writes the output after we clean it.
func reapTrainingGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
	}
}
