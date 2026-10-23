package controller

import (
	"context"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	noderuntime "github.com/microsoft/brewlet/pkg/node"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestEpochObligationSurvivesProfileEntryRemovalAndReauthorization(t *testing.T) {
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if len(im.Status.RevocationTargets) != 1 || im.Status.RevocationTargets[0].Resolved {
		t.Fatal("normal approval did not retain a pre-authorization epoch obligation")
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	p.Spec.RuntimeImages = []api.RuntimeEntry{{ID: "other-runtime"}}
	p.Generation++
	if err := r.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	p.Status.ProvisioningGeneration = p.Generation
	snapshot := p.Spec
	p.Status.ProvisioningSpec = &snapshot
	if err := r.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	delete(n.Labels, "brewlet.sh/runtime."+im.Name)
	_ = r.Update(ctx, n)
	setReceipt(t, r, im, p, n, func(receipt *noderuntime.RevocationReceipt) { receipt.RootsCleaned = false })
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if im.Status.RevocationTargets[0].Resolved {
		t.Fatal("profile entry removal forgot outstanding leased roots")
	}
	setReceipt(t, r, im, p, n, nil)
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if !im.Status.RevocationTargets[0].Resolved {
		t.Fatal("retired entry's genuine complete receipt not consumed")
	}
	// Readding the same ID is a new grant episode; the old positive receipt
	// cannot make that new authority disappear from the inventory ledger.
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	p.Spec.RuntimeImages = append(p.Spec.RuntimeImages, api.RuntimeEntry{ID: im.Name})
	p.Generation++
	_ = r.Update(ctx, p)
	p.Status.ProvisioningGeneration = p.Generation
	snapshot = p.Spec
	p.Status.ProvisioningSpec = &snapshot
	_ = r.Status().Update(ctx, p)
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if im.Status.RevocationTargets[0].Resolved || im.Status.RevocationTargets[0].ProfileGeneration != p.Generation {
		t.Fatal("reauthorization inherited a resolved historical grant")
	}
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if im.Status.RevocationTargets[0].Resolved {
		t.Fatal("active offered approval was resolved from a stale positive receipt")
	}
}
func TestProfileTeardownWaitsForIndependentInventoryReceiptCheckpoint(t *testing.T) {
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	p.Status.Retirement = &api.Retirement{Targets: p.Status.Targets, Generation: 1, Spec: p.Spec, Phase: "Teardown"}
	_ = r.Status().Update(ctx, p)
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	delete(n.Labels, "brewlet.sh/runtime."+im.Name)
	n.Annotations["brewlet.sh/cleanup-complete"] = "profile/1"
	_ = r.Update(ctx, n)
	setReceipt(t, r, im, p, n, nil)
	pr := &ProfileReconciler{Client: r.Client, Reader: r.Reader, Namespace: "brewlet-system", WorkerImage: "worker"}
	if _, err := pr.retire(ctx, p); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	if n.Labels[api.OwnerUID] != "profile" {
		t.Fatal("claim released before inventory consumed the genuine per-ID receipt")
	}
	// A global rotation after this target's proven cleanup must not demand a
	// cleanup-worker replay beyond the frozen Teardown authority.
	im.Spec.Source.Image = "registry.example.com/python@sha256:" + strings.Repeat("b", 64)
	im.Generation++
	if err := r.Update(ctx, im); err != nil {
		t.Fatal(err)
	}
	revokeStep(t, r, im)
	revokeStep(t, r, im)
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if !im.Status.RevocationTargets[0].Resolved {
		t.Fatal("durable proven-clean Teardown receipt was not retained independently")
	}
	if im.Status.Generation != 2 || im.Status.RevocationTargets[0].Generation != 1 {
		t.Fatal("retired authorization ceiling was silently advanced by a later global target")
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if _, err := pr.retire(ctx, p); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	if n.Labels[api.OwnerUID] != "" {
		t.Fatal("resolved independent inventory obligation did not permit completed claim teardown")
	}
}
func TestMissingEpochTargetRemainsVisibleInRotationExclusions(t *testing.T) {
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	_ = r.Delete(ctx, n)
	if err := r.rotation(ctx, im); err != nil {
		t.Fatal(err)
	}
	if len(im.Status.Rotation.ExcludedNodes) != 1 || im.Status.Rotation.ExcludedNodes[0].UID != "node" {
		t.Fatal("missing original UID disappeared from rotation diagnostics")
	}
	_ = p
}
