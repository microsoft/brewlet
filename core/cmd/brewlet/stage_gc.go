// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/microsoft/brewlet/internal/artifact"
	"github.com/microsoft/brewlet/internal/runnablestage"
)

func cmdStageGC(args []string) error {
	fs := flag.NewFlagSet("stage-gc", flag.ContinueOnError)
	root := fs.String("stage-root", runnablestage.Root(), "runnable staging root (BREWLET_RUNNABLE_STAGE)")
	address := fs.String("address", "/run/containerd/containerd.sock", "containerd socket; all namespaces are checked")
	age := fs.Duration("min-age", runnablestage.DefaultMinAge, "minimum stage age (must be positive)")
	dryRun := fs.Bool("dry-run", false, "report eligible trees without deleting them")
	nested := fs.Bool("allow-nested-pid-namespace", false, "accept the node init's non-initial PID namespace (test nodes such as kind only)")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || *age <= 0 {
		return fmt.Errorf("usage: stage-gc [--stage-root DIR] [--address SOCKET] [--min-age positive-duration] [--dry-run] [--allow-nested-pid-namespace]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	result, err := runnablestage.Reap(ctx, *address, runnablestage.Options{
		Root: *root, MinAge: *age, DryRun: *dryRun, AllowNestedPIDNamespace: *nested,
	})
	if err != nil {
		return err
	}
	action := "removed"
	if *dryRun {
		action = "would remove"
	}
	fmt.Printf("%s %d stages (%d bytes); remaining stage bytes: %d\n", action, result.Removed, result.ReclaimedBytes, result.RemainingBytes)
	return nil
}

// An exported bundle may be started long after this command exits. Give it
// independent payloads rather than dangling references into the evictable cache.
func retainBundlePayloads(blobs *artifact.ResolvedBlobs, out string) (func(), error) {
	if blobs.Format != "image" {
		return func() {}, nil
	}
	out, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(out, "brewlet-image-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := os.CopyFS(dir, os.DirFS(filepath.Dir(blobs.JarHostPath))); err != nil {
		cleanup()
		return nil, fmt.Errorf("retain bundle payloads: %w", err)
	}
	blobs.JarHostPath = filepath.Join(dir, filepath.Base(blobs.JarHostPath))
	if blobs.CDSHostPath != "" {
		blobs.CDSHostPath = filepath.Join(dir, filepath.Base(blobs.CDSHostPath))
	}
	return cleanup, nil
}
