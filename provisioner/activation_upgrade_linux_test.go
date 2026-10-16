//go:build linux

package provisioner

import (
	"context"
	"errors"
	"os"
	"testing"
)

func TestOwnedShimUpgradePreservesOriginalCleanupObligationUnit(t *testing.T) {
	a, original, calls := activationFixture(t)
	ctx := context.Background()
	fence := func(context.Context) error { return nil }
	if err := a.Ensure(ctx, "validated", fence); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.SourceBinary, []byte("new owned shim"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := a.Ensure(ctx, "none", fence); err != nil {
		t.Fatal(err)
	}
	record, err := a.load()
	if err != nil || record.Phase != "Committed" || !record.ConfigChanged {
		t.Fatal(record, err)
	}
	sourceHash, err := fileHash(a.SourceBinary)
	if err != nil {
		t.Fatal(err)
	}
	if installed, err := fileHash(a.binary()); err != nil || installed != sourceHash {
		t.Fatal("upgrade not installed", err)
	}
	if err := a.Restore(ctx, fence); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(a.config())
	if err != nil || string(config) != string(original) {
		t.Fatal("upgrade lost original config backup", err)
	}
	restarts := 0
	for _, call := range *calls {
		if call == "systemctl restart containerd" {
			restarts++
		}
	}
	if restarts != 2 {
		t.Fatal("none upgrade restarted or cleanup forgot validated history", *calls)
	}
}

func TestFailedOwnedShimUpgradeRollsBackPreviousCommittedBinaryUnit(t *testing.T) {
	a, _, _ := activationFixture(t)
	ctx := context.Background()
	fence := func(context.Context) error { return nil }
	if err := a.Ensure(ctx, "validated", fence); err != nil {
		t.Fatal(err)
	}
	oldHash, err := fileHash(a.binary())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.SourceBinary, []byte("bad new shim"), 0755); err != nil {
		t.Fatal(err)
	}
	a.validateBaseline = func(context.Context) error {
		hash, err := fileHash(a.binary())
		if err != nil {
			return err
		}
		if hash != oldHash {
			return errors.New("new shim validation failed")
		}
		return nil
	}
	if err := a.Ensure(ctx, "validated", fence); err == nil {
		t.Fatal("bad upgrade accepted")
	}
	installed, err := fileHash(a.binary())
	if err != nil || installed != oldHash {
		t.Fatal("previous committed binary not restored", err)
	}
	record, err := a.load()
	if err != nil || record.Phase != "Committed" || record.DesiredBinary != oldHash {
		t.Fatal("previous transaction not recovered", record, err)
	}
}
