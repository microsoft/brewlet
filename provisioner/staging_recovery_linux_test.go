//go:build linux

package provisioner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUnpublishedStagingRequiresUIDBoundWorkspaceAndMountProofUnit(t *testing.T) {
	a, _, _ := activationFixture(t)
	stage := filepath.Join(a.InventoryRoot, ".staging", "runtime-123")
	if err := os.MkdirAll(filepath.Join(stage, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "rootfs", "partial"), []byte("private uncommitted bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	i := &ContainerdInstaller{ProfileUID: a.ProfileUID, NodeUID: a.NodeUID, ProfileGeneration: 1, Fence: func(context.Context) error { return nil }, MountEvidence: cacheEvidence{gone: true}}
	if err := i.RecoverStaging(context.Background(), a.InventoryRoot); err == nil {
		t.Fatal("unclaimed staging namespace adopted")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("unclaimed stage removed", err)
	}
	if err := os.RemoveAll(filepath.Dir(stage)); err != nil {
		t.Fatal(err)
	}
	if err := a.Ensure(context.Background(), "validated", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stage, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	i.MountEvidence = cacheEvidence{gone: false}
	if err := i.RecoverStaging(context.Background(), a.InventoryRoot); err == nil {
		t.Fatal("mounted stage removed")
	}
	i.MountEvidence = cacheEvidence{gone: true}
	if err := i.RecoverStaging(context.Background(), a.InventoryRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("owned unmounted unpublished stage retained", err)
	}
}
