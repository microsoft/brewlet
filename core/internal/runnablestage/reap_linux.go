// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package runnablestage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type dependencies struct {
	references func(context.Context, string) (map[string]bool, error)
	mounts     func(context.Context) (mountSnapshot, error)
	now        func() time.Time
}

// Reap removes only old, unreferenced immutable-v2 digest directories. MinAge
// must be strictly positive. Metadata and every visible process's mounts
// must be readable before any deletion. Run in the host namespaces with a
// complete proc mount; concurrent legacy launchers must be quiesced.
func Reap(ctx context.Context, address string, opts Options) (Result, error) {
	return reap(ctx, address, opts, dependencies{containerReferences, hostMounts, time.Now})
}

func reap(ctx context.Context, address string, opts Options, deps dependencies) (Result, error) {
	var result Result
	if opts.MinAge <= 0 {
		return result, fmt.Errorf("minimum stage age must be positive")
	}
	if opts.Root == "" {
		opts.Root = Root()
	}
	absolute, err := filepath.Abs(opts.Root)
	if err != nil {
		return result, err
	}
	// Resolve ancestor aliases for mount matching, but never accept a symlink
	// as the staging root itself.
	before, err := os.Lstat(absolute)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !before.IsDir() {
		return result, fmt.Errorf("staging root is not a real directory")
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return result, err
	}
	guard, err := lock(absolute, true)
	if err != nil {
		return result, err
	}
	defer guard.Close()
	root, err := openRoot(absolute)
	if err != nil {
		return result, err
	}
	defer root.Close()
	lockedInfo, err := guard.Stat()
	if err != nil {
		return result, err
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !os.SameFile(lockedInfo, rootInfo) || !os.SameFile(before, rootInfo) {
		return result, fmt.Errorf("staging root changed while opening")
	}
	versionInfo, err := root.Lstat("immutable-v2")
	if os.IsNotExist(err) {
		result.RemainingBytes, err = treeBytes(root, ".")
		return result, err
	}
	if err != nil {
		return result, err
	}
	if !versionInfo.IsDir() {
		return result, fmt.Errorf("immutable-v2 is not a real directory")
	}
	version, err := root.OpenRoot("immutable-v2")
	if err != nil {
		return result, err
	}
	defer version.Close()
	openedInfo, err := version.Stat(".")
	if err != nil || !os.SameFile(versionInfo, openedInfo) {
		return result, fmt.Errorf("immutable-v2 changed while opening")
	}
	entries, err := fs.ReadDir(version.FS(), ".")
	if err != nil {
		return result, err
	}
	refs, err := deps.references(ctx, address)
	if err != nil {
		return result, err
	}
	mounts, err := deps.mounts(ctx)
	if err != nil {
		return result, err
	}
	type candidate struct {
		name string
		info fs.FileInfo
		size int64
	}
	var candidates []candidate
	cutoff := deps.now().Add(-opts.MinAge)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if !canonical(entry.Name()) || !entry.IsDir() || refs[entry.Name()] {
			continue
		}
		info, err := version.Lstat(entry.Name())
		if err != nil {
			return result, err
		}
		if !info.IsDir() || !info.ModTime().Before(cutoff) {
			continue
		}
		var stat unix.Stat_t
		path := filepath.Join(absolute, "immutable-v2", entry.Name())
		if err := unix.Lstat(path, &stat); err != nil {
			return result, err
		}
		device := fmt.Sprintf("%d:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev)))
		protected, err := mounts.protects(path, device)
		if err != nil {
			return result, err
		}
		if protected {
			continue
		}
		size, err := treeBytes(version, entry.Name())
		if err != nil {
			return result, err
		}
		candidates = append(candidates, candidate{entry.Name(), info, size})
	}
	// Finish all accounting and reference validation before the first removal.
	result.RemainingBytes, err = treeBytes(root, ".")
	if err != nil {
		return result, err
	}
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		current, err := version.Lstat(candidate.name)
		if err != nil || !current.IsDir() || !os.SameFile(current, candidate.info) ||
			!current.ModTime().Equal(candidate.info.ModTime()) {
			return result, fmt.Errorf("stage %s changed during reaping", candidate.name)
		}
		if !opts.DryRun {
			if err := version.RemoveAll(candidate.name); err != nil {
				// A filesystem failure can leave a partially removed tree.
				result.RemainingBytes, _ = treeBytes(root, ".")
				return result, err
			}
			result.RemainingBytes -= candidate.size
		}
		result.Removed++
		result.ReclaimedBytes += candidate.size
	}
	return result, nil
}
