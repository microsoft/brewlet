package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenewedRotationTargetKeepsCompatibleCurrentWithoutBarrier(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testAuthorization()
	a.Generation = 2
	a.Digest = "sha256:" + strings.Repeat("c", 64)
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}

	old := fixtureGeneration(t, s, 1)
	if old.SourceDigest == a.Digest {
		t.Fatal("fixture must represent different approved target bytes")
	}
	if err := os.Symlink(filepath.Join(old.InventoryUID, "1"), filepath.Join(s.Root, old.ID, "current")); err != nil {
		t.Fatal(err)
	}
	lease, _, err := s.Resolve(ctx, old.ID, "k8s.io", "ordinary-create", nil, time.Second)
	if err != nil || lease.Metadata.Generation != 1 || lease.Metadata.SourceDigest != old.SourceDigest {
		t.Fatal("rotation invalidated compatible current", lease, err)
	}
	if err := s.ValidateLease(ctx, lease); err != nil {
		t.Fatal("final Create gate invalidated compatible old generation", err)
	}
	barrier := &Barrier{ID: old.ID, InventoryUID: old.InventoryUID, Generation: 2}
	if _, _, err := s.Resolve(ctx, old.ID, "k8s.io", "barrier-create", barrier, time.Millisecond); reason(err) != "RuntimeImageUpdating" {
		t.Fatal(err)
	}
	markers, err := s.Requests()
	if err != nil || len(markers) != 0 {
		t.Fatal("installed rotation emitted first-use marker", markers, err)
	}
	a.Revoked = true
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, minimum := range []*Barrier{nil, barrier} {
		if _, _, err := s.Resolve(ctx, old.ID, "k8s.io", "revoked-create", minimum, time.Millisecond); reason(err) != "RuntimeImageRevoked" {
			t.Fatal("revocation bypassed", err)
		}
	}
	a.Revoked = false
	a.InventoryUID = "replacement-epoch"
	a.Generation = 1
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, minimum := range []*Barrier{nil, {ID: old.ID, InventoryUID: a.InventoryUID, Generation: 1}, barrier} {
		if _, _, err := s.Resolve(ctx, old.ID, "k8s.io", "replacement-create", minimum, time.Millisecond); reason(err) != "RuntimeImageEpochMismatch" {
			t.Fatal("epoch replacement adopted old root", err)
		}
	}
	leases, err := s.Leases()
	if err != nil || len(leases) != 1 || leases[0].Metadata.InventoryUID != old.InventoryUID {
		t.Fatal("revocation/epoch replacement discarded running lease", leases, err)
	}
}
