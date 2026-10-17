//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/microsoft/brewlet/pkg/node"
)

func healthTestDirectory(t *testing.T) string {
	t.Helper()
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
	return dir
}

func TestProvisioningReadinessTracksSuccessfulReconcileAndFreshAuthority(t *testing.T) {
	marker := filepath.Join(healthTestDirectory(t), "ready")
	identity := "profile/1/node/provisioning"
	checks := 0
	fence := func() error { checks++; return nil }
	if err := syncProvisioningReadinessAt(marker, identity, false, fence); err != nil {
		t.Fatal(err)
	}
	if checks != 0 {
		t.Fatal("failed reconciliation tried to certify completion")
	}
	if err := healthCheckAt(marker); err == nil {
		t.Fatal("failed reconciliation minted readiness")
	}
	if err := syncProvisioningReadinessAt(marker, identity, true, fence); err != nil {
		t.Fatal(err)
	}
	if checks != 1 {
		t.Fatal("successful reconciliation lacked a fresh ownership check")
	}
	if err := healthCheckAt(marker); err != nil {
		t.Fatal("successful reconciliation did not publish readiness", err)
	}
	if err := syncProvisioningReadinessAt(marker, identity, false, fence); err != nil {
		t.Fatal(err)
	}
	if err := healthCheckAt(marker); err == nil || checks != 1 {
		t.Fatal("failure retained a prior success marker")
	}
	if err := syncProvisioningReadinessAt(marker, identity, true, fence); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("fresh ownership lost")
	err := syncProvisioningReadinessAt(marker, identity, true, func() error { return lost })
	if !errors.Is(err, lost) {
		t.Fatal("fresh ownership failure was hidden", err)
	}
	if err := healthCheckAt(marker); err == nil {
		t.Fatal("lost ownership retained readiness")
	}
}

func TestReadinessRemovalFailureIsNotIgnored(t *testing.T) {
	marker := filepath.Join(healthTestDirectory(t), "ready")
	if err := os.Mkdir(marker, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(marker, "unexpected"), []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncProvisioningReadinessAt(marker, "profile/1/node/provisioning", false,
		func() error { t.Fatal("failed reconciliation checked a success fence"); return nil }); err == nil {
		t.Fatal("failed marker removal was ignored")
	}
}

func TestCompletionGateIsProtectedFreshAndReadOnly(t *testing.T) {
	dir := healthTestDirectory(t)
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
