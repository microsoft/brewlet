package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestUnknownIDDoesNotWithholdValidInventoryWorkerOrBaselineObservation(t *testing.T) {
	r, p, n := profileFixture(t)
	ctx := context.Background()
	if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	p.Spec.RuntimeImages = []api.RuntimeEntry{{ID: "python-3.12", Install: "Eager"}, {ID: "not-approved-yet", Install: "Eager"}}
	if err := r.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	source := "registry.example.com/runtime@sha256:" + strings.Repeat("a", 64)
	im := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12", UID: "runtime", Generation: 1}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: source}, User: api.User{UID: 1000, GID: 1000}}, Status: api.RuntimeImageStatus{Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64), SourceImage: source, Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	if err := r.Create(ctx, im); err != nil {
		t.Fatal(err)
	}
	advance(t, r, p, 6)
	ds := new(appsv1.DaemonSet)
	if err := r.Get(ctx, client.ObjectKey{Name: "brewlet-node-provisioner-web", Namespace: r.Namespace}, ds); err != nil {
		t.Fatal("unknown ID withheld worker for approved IDs:", err)
	}
	var rendered api.NodeProfileSpec
	if err := json.Unmarshal([]byte(workerEnv(ds, "BREWLET_PROFILE_SPEC")), &rendered); err != nil || len(rendered.RuntimeImages) != 2 {
		t.Fatal("per-entry worker was not given the full fenced profile")
	}
	// Simulate genuine baseline/approved-Eager success. Unknown Eager is not
	// an installable requirement; it must not suppress readiness for valid IDs.
	if err := r.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
		t.Fatal(err)
	}
	n.Spec.Taints = nil
	n.Labels["brewlet.sh/runtime.python-3.12"] = "ready"
	if err := r.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	advance(t, r, p, 1)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	c := meta.FindStatusCondition(p.Status.Conditions, "Ready")
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != "UnknownRuntimeImage" || !strings.Contains(c.Message, "not-approved-yet") {
		t.Fatalf("unknown inventory was not explicitly reported: %+v", c)
	}
	if p.Status.ReadyNodes != 1 || n.Annotations["brewlet.sh/provision-state"] != "Ready" {
		t.Fatal("unknown Eager entry suppressed valid node baseline observation")
	}
	if _, present := n.Labels["brewlet.sh/runtime.not-approved-yet"]; present {
		t.Fatal("controller advertised an unknown ID")
	}
	// Approved Eager is still mandatory: missing its capability must not be
	// hidden by the exceptional treatment of an unknown inventory entry.
	delete(n.Labels, "brewlet.sh/runtime.python-3.12")
	if err := r.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	advance(t, r, p, 1)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if p.Status.ReadyNodes != 0 {
		t.Fatal("approved Eager readiness gate was weakened")
	}
}
