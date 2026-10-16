package controller

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExplicitRotationConditionsTrackEpochTargetCompletionAndRevocation(t *testing.T) {
	r, im := runtimeFixture(t)
	ctx := context.Background()
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if !meta.IsStatusConditionTrue(im.Status.Conditions, "RotationProgressing") || meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") {
		t.Fatal("allocated target lacks explicit progressing condition")
	}
	for _, kind := range []string{"RotationProgressing", "RotationComplete"} {
		c := meta.FindStatusCondition(im.Status.Conditions, kind)
		if c == nil || c.ObservedGeneration != im.Generation || !strings.Contains(c.Message, string(im.UID)) || !strings.Contains(c.Message, im.Status.Digest) || !strings.Contains(c.Message, "generation=1") {
			t.Fatalf("condition loses target identity: %+v", c)
		}
	}
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if meta.IsStatusConditionTrue(im.Status.Conditions, "RotationProgressing") || !meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") {
		t.Fatal("completion did not clear explicit progressing condition")
	}
	im.Spec.Source.Image = "registry.example.com/python@sha256:" + strings.Repeat("b", 64)
	im.Generation++
	_ = r.Update(ctx, im)
	revokeStep(t, r, im)
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if im.Status.Generation != 2 || !meta.IsStatusConditionTrue(im.Status.Conditions, "RotationProgressing") || meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") {
		t.Fatal("new target reused prior target completion")
	}
	meta.SetStatusCondition(&im.Status.Conditions, metav1.Condition{Type: "Approved", Status: metav1.ConditionFalse, Reason: "AdministratorRevocation", Message: "revoked", ObservedGeneration: im.Generation})
	_ = r.Status().Update(ctx, im)
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if meta.IsStatusConditionTrue(im.Status.Conditions, "RotationProgressing") || meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") {
		t.Fatal("revoked target still reported progressing/complete")
	}
}
