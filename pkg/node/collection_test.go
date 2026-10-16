package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollectionGraceStartsAtRetirementNotOldInstallation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testAuthorization()
	a.Generation = 2
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	old := fixtureGeneration(t, s, 1)
	_ = fixtureGeneration(t, s, 2)
	if err := os.Symlink(filepath.Join(old.InventoryUID, "2"), filepath.Join(s.Root, old.ID, "current")); err != nil {
		t.Fatal(err)
	}
	fence := func(context.Context) error { return nil }
	oldPath := filepath.Join(s.Root, old.ID, old.InventoryUID, "1")
	if err := s.Collect(ctx, old.ID, time.Hour, fixedEvidence{gone: true}, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatal("old install age bypassed retirement grace", err)
	}
	if err := atomicJSON(s.retirementName(old.ID, old.InventoryUID, 1), retirement{InventoryUID: old.InventoryUID, Generation: 1, RetiredAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Collect(ctx, old.ID, time.Hour, fixedEvidence{gone: true}, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("unleased retired root not reclaimed", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, old.ID, old.InventoryUID, "2")); err != nil {
		t.Fatal("current root reclaimed", err)
	}
}
