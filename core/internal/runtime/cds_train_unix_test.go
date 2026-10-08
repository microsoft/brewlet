// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/microsoft/brewlet/internal/artifact"
)

// TestGenerateAOTCacheTimeoutReapsAssemblyChild pauses the JEP 514 assembly
// child (JDK_AOT_VM_OPTIONS reaches only that child) so the training timeout
// deterministically expires mid-assembly. The child must not outlive the call,
// and neither the cache nor its temporary AOTConfiguration may be left behind.
func TestGenerateAOTCacheTimeoutReapsAssemblyChild(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AOT cache JDK integration test in -short mode")
	}
	javaBin, javacBin, jarBin := locateJDK(t)
	if feature := jdkFeature(t, javaBin); feature < 25 {
		t.Skipf("AOT cache training needs JDK >= 25, found feature %d; skipping", feature)
	}
	jar := buildTinyFatJar(t, javacBin, jarBin)

	wrapper := filepath.Join(t.TempDir(), "java")
	script := "#!/bin/sh\nJDK_AOT_VM_OPTIONS='-XX:+UnlockDiagnosticVMOptions -XX:+PauseAtStartup' exec '" + javaBin + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// The paused child writes vm.paused.<pid> into its working directory, the
	// scratch dir, which is removed on return; pin TMPDIR to find it meanwhile.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	var children []int
	defer func() {
		for _, pid := range children {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}()
	done := make(chan struct{})
	found := make(chan []int, 1)
	go func() {
		var pids []int
		defer func() { found <- pids }()
		for {
			matches, _ := filepath.Glob(filepath.Join(tmp, "brewlet-aot-*", "vm.paused.*"))
			for _, m := range matches {
				if pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), "vm.paused.")); err == nil {
					pids = append(pids, pid)
				}
			}
			if len(pids) > 0 {
				return
			}
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()

	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	out := filepath.Join(t.TempDir(), "app.aot")
	err := GenerateAOTCache(cfg, jar, wrapper, out, 10*time.Second, nil)
	close(done)
	children = <-found

	if err == nil || !strings.Contains(err.Error(), "did not exit within") {
		t.Fatalf("expected a training timeout error, got %v", err)
	}
	if len(children) == 0 {
		t.Fatal("the AOT assembly child never paused; the test did not exercise it")
	}
	for _, pid := range children {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("leaked AOT assembly child %d (kill -0: %v)", pid, err)
		}
	}
	for _, p := range []string{out, out + ".config"} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a timed-out run must not leave %s (stat: %v)", p, err)
		}
	}
}
