//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/microsoft/brewlet/pkg/node"
)

func TestCompletionGateIsProtectedFreshAndReadOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned isolated Linux working directory")
	}
	dir, err := os.MkdirTemp(".", ".health-test-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := node.NewStore(dir).Check(); err != nil {
		t.Skipf("requires root-owned isolated Linux working directory: %v", err)
	}
	marker := filepath.Join(dir, "ready")
	if err := healthCheckAt(marker); err == nil {
		t.Fatal("missing completion accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("health mode mutated missing marker", err)
	}
	if err := markReadyAt(marker, "profile/1/node/provisioning"); err != nil {
		t.Fatal(err)
	}
	if err := healthCheckAt(marker); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-3 * time.Minute)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	if err := healthCheckAt(marker); err == nil {
		t.Fatal("stale completion accepted")
	}
	if err := markReadyAt(marker, "profile/1/node/cleanup"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(marker, 0666); err != nil {
		t.Fatal(err)
	}
	if err := healthCheckAt(marker); err == nil {
		t.Fatal("writable completion accepted")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", marker); err != nil {
		t.Fatal(err)
	}
	if err := healthCheckAt(marker); err == nil {
		t.Fatal("symlink completion accepted")
	}
	if info, err := os.Lstat(marker); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("health mode mutated unsafe marker", err)
	}
}
