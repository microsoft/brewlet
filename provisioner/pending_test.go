package provisioner

import (
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPendingValidRotationIsNotExplicitRevocation(t *testing.T) {
	old := "registry.example.com/runtime@sha256:" + strings.Repeat("a", 64)
	user := api.User{UID: 1000, GID: 1000}
	compatibility := api.Compatibility{}
	r := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "static", Generation: 2}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: "registry.example.com/runtime@sha256:" + strings.Repeat("c", 64)}, User: user}, Status: api.RuntimeImageStatus{Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64), SourceImage: old, ApprovedUser: &user, ApprovedCompatibility: &compatibility, Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	if !pendingApproval(r) {
		t.Fatal("valid pending rotation classified as revocation")
	}
	r.Status.Conditions[0].Status = metav1.ConditionFalse
	if pendingApproval(r) {
		t.Fatal("explicit revocation treated as pending")
	}
	r.Status.Conditions[0].Status = metav1.ConditionTrue
	r.Spec.Source.Image = "registry.example.com/runtime:mutable"
	if pendingApproval(r) {
		t.Fatal("invalid source treated as pending")
	}
	r.Spec.Source.Image = old
	r.Spec.User.UID = 2000
	if pendingApproval(r) {
		t.Fatal("invalid identity change treated as pending")
	}
}
