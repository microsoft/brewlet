package controller

import (
	"context"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAllocationJournalSurvivesPrunedStatusAndInterruptedABA(t *testing.T) {
	r, im := runtimeFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: im.Name}}
	_, _ = r.Reconcile(ctx, req)
	_ = r.Get(ctx, req.NamespacedName, im)
	original := im.Spec.Source.Image
	im.Spec.Source.Image = "registry.example.com/python@sha256:" + strings.Repeat("b", 64)
	im.Generation++
	_ = r.Update(ctx, im)
	// Allocate B, but simulate a crash before its status is published.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, req.NamespacedName, im)
	im.Spec.Source.Image = original
	im.Generation++
	_ = r.Update(ctx, im)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, req.NamespacedName, im)
	im.Status = api.RuntimeImageStatus{}
	_ = r.Status().Update(ctx, im)
	restarted := &RuntimeReconciler{Client: r.Client, Reader: r.Reader}
	if _, err := restarted.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, req.NamespacedName, im)
	if im.Status.Generation != 3 || !meta.IsStatusConditionTrue(im.Status.Conditions, "Approved") {
		t.Fatalf("interrupted/lost status reused a generation: %+v", im.Status)
	}
}
func TestMissingAllocationJournalFailsClosed(t *testing.T) {
	r, im := runtimeFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: im.Name}}
	delete(im.Annotations, inventoryJournalKey)
	_ = r.Update(ctx, im)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, req.NamespacedName, im)
	if im.Status.Generation != 0 || meta.IsStatusConditionTrue(im.Status.Conditions, "Approved") {
		t.Fatal("missing history synthesized a new generation")
	}
}
