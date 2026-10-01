// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package runnablestage accounts for and conservatively reclaims immutable
// runnable-image stages. The root must be administrator-owned and must not be
// renamed or replaced while launchers or the reaper are running.
package runnablestage

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const DefaultMinAge = 24 * time.Hour

type Options struct {
	Root   string
	MinAge time.Duration
	DryRun bool
	// AllowNestedPIDNamespace accepts a non-initial PID namespace when the
	// reaper shares it with that namespace's PID 1. Only for nodes that are
	// themselves containers (kind) whose stage root is private to the node.
	AllowNestedPIDNamespace bool
}

type Result struct {
	Removed        int
	ReclaimedBytes int64
	RemainingBytes int64
}

// ErrBusy means a launcher or another reaper holds the staging root lock.
var ErrBusy = errors.New("runnable staging root is busy")

// LinuxDefaultRoot is fixed rather than derived from TMPDIR so the shim, which
// inherits containerd's environment, and the provisioner's reaper agree.
const LinuxDefaultRoot = "/tmp/brewlet-runnable"

func Root() string {
	if root := os.Getenv("BREWLET_RUNNABLE_STAGE"); root != "" {
		return root
	}
	if runtime.GOOS == "linux" {
		return LinuxDefaultRoot
	}
	return filepath.Join(os.TempDir(), "brewlet-runnable")
}

// Bytes sums logical regular-file sizes, including legacy and pending stages,
// without following symlinks. An absent root has size zero.
func Bytes(root string) (int64, error) {
	if root == "" {
		root = Root()
	}
	r, err := openRoot(root)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer r.Close()
	return treeBytes(r, ".")
}

func openRoot(path string) (*os.Root, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("staging root %q is not a real directory", path)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	after, err := r.Stat(".")
	if err != nil || !os.SameFile(before, after) {
		r.Close()
		return nil, fmt.Errorf("staging root %q changed while opening", path)
	}
	return r, nil
}

func treeBytes(root *os.Root, name string) (int64, error) {
	var size int64
	err := fs.WalkDir(root.FS(), name, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func canonical(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, c := range name {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
