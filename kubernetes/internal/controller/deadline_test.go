package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestStalledRotationQuarantineOnlyAfterConfirmedWithdrawal(t *testing.T) {
	r, im := runtimeFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: im.Name}}
	_, _ = r.Reconcile(ctx, req)
	_ = r.Get(ctx, req.NamespacedName, im)
	spec := api.NodeProfileSpec{RuntimeImages: []api.RuntimeEntry{{ID: im.Name}}, Rollout: api.Rollout{ContainerdRestart: "none"}}
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile", Generation: 1}, Spec: spec, Status: api.NodeProfileStatus{OwnershipInitialized: true, ProvisioningGeneration: 1, ProvisioningSpec: &spec, Targets: []api.Target{{Name: "node", UID: "node", Claimed: true, ContainerdRestart: "none"}}}}
	b, _ := json.Marshal(map[string]api.NodeRuntime{im.Name: {Install: "Eager", State: "failed", InventoryUID: im.UID, Generation: 0}})
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node", Labels: map[string]string{api.OwnerUID: "profile", api.OwnerNodeUID: "node", api.RuntimeLabel: "ready", "brewlet.sh/runtime." + im.Name: "ready"}, Annotations: map[string]string{api.OwnerName: "web", api.InventoryAnnotation: string(b)}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	_ = r.Create(ctx, p)
	_ = r.Create(ctx, n)
	now := time.Date(2026, 10, 16, 13, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return now }
	r.NodeProgressDeadline = 10 * time.Minute
	if err := r.rotation(ctx, im); err != nil {
		t.Fatal(err)
	}
	if meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") || len(im.Status.Rotation.Participants) != 1 {
		t.Fatal("initial stalled target did not participate")
	}
	// A new reconciler uses persisted status timestamps, not process uptime.
	persisted := im.DeepCopy()
	r = &RuntimeReconciler{Client: r.Client, Reader: r.Reader, Now: func() time.Time { return now.Add(11 * time.Minute) }, NodeProgressDeadline: 10 * time.Minute}
	if err := r.rotation(ctx, persisted); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(persisted.Status.Conditions, "RotationComplete") || len(persisted.Status.Rotation.ExcludedNodes) != 1 || persisted.Status.Rotation.ExcludedNodes[0].Reason != "RotationStalled" {
		t.Fatalf("confirmed stalled quarantine incorrect: %+v", persisted.Status)
	}
	_ = r.Get(ctx, client.ObjectKey{Name: n.Name}, n)
	if _, ok := n.Labels["brewlet.sh/runtime."+im.Name]; ok {
		t.Fatal("stalled node remained schedulable")
	}
}
