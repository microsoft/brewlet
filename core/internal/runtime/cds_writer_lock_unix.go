// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runtime

import (
	"os"
	"syscall"
)

func acquireCDSWriterStateLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
