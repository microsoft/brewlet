// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package runnablestage

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Acquire holds a shared interprocess directory lock until Close. Callers must
// hold it from stage resolution through establishment of the workload's mounts
// (or for the lifetime of direct host execution).
func Acquire(root string) (io.Closer, error) {
	if root == "" {
		root = Root()
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return lock(root, false)
}

func lock(root string, exclusive bool) (*os.File, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), root)
	operation := unix.LOCK_SH
	if exclusive {
		operation = unix.LOCK_EX | unix.LOCK_NB
	}
	for {
		err = unix.Flock(fd, operation)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	current, err := os.Lstat(root)
	opened, statErr := file.Stat()
	if err != nil || statErr != nil || !current.IsDir() || !os.SameFile(current, opened) {
		file.Close()
		return nil, fmt.Errorf("staging root changed while locking")
	}
	return file, nil
}
