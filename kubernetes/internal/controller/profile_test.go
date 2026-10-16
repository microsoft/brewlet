package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func profileFixture(t *testing.T) (*ProfileReconciler, *api.NodeProfile, *corev1.Node) {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile", Generation: 1}, Spec: api.NodeProfileSpec{NodePool: api.NodePool{Names: []string{"web"}, Key: "agentpool"}, RuntimeImages: []api.RuntimeEntry{{ID: "python-3.12", Install: "OnDemand"}}, Rollout: api.Rollout{ContainerdRestart: "none"}}}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-original", Labels: map[string]string{"agentpool": "web", api.RuntimeLabel: "ready", "brewlet.sh/runtime.python-3.12": "available"}}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.NodeProfile{}, &api.RuntimeImage{}).WithObjects(p, n).Build()
	return &ProfileReconciler{Client: c, Reader: c, Namespace: "brewlet-system", WorkerImage: "registry.example.com/brewlet-node:2.0"}, p, n
}
func advance(t *testing.T, r *ProfileReconciler, p *api.NodeProfile, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: p.Name}}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}
func TestFreshTemplatedNodeClaimsCheckpointBeforeWorker(t *testing.T) {
	r, p, n := profileFixture(t)
	ctx := context.Background()
	advance(t, r, p, 3)
	_ = r.Get(ctx, client.ObjectKey{Name: n.Name}, n)
	if n.Labels[api.OwnerUID] != "" {
		t.Fatal("claim granted before durable target ledger")
	}
	_ = r.Get(ctx, client.ObjectKey{Name: p.Name}, p)
	if !p.Status.OwnershipInitialized || len(p.Status.Targets) != 1 || p.Status.Targets[0].Claimed {
		t.Fatal("missing unclaimed target checkpoint")
	}
	advance(t, r, p, 3)
	_ = r.Get(ctx, client.ObjectKey{Name: n.Name}, n)
	if n.Labels[api.OwnerUID] != string(p.UID) || n.Labels[api.OwnerNodeUID] != string(n.UID) {
		t.Fatal("UID-bound claim not persisted")
	}
	ds := new(appsv1.DaemonSet)
	if err := r.Get(ctx, client.ObjectKey{Name: "brewlet-node-provisioner-web", Namespace: r.Namespace}, ds); err != nil {
		t.Fatal(err)
	}
	if workerEnv(ds, "BREWLET_PROFILE_UID") != string(p.UID) || workerEnv(ds, "BREWLET_TARGET_UIDS") != `["node-original"]` {
		t.Fatal("worker missing literal immutable authority")
	}
	if len(ds.Spec.Template.Spec.Tolerations) != 0 {
		t.Fatal("blanket toleration introduced")
	}
	terms := ds.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchFields) != 1 || len(terms[0].MatchExpressions) != 5 {
		t.Fatal("worker lost identity/pool/control-plane constraints")
	}
}
func TestUnfencedWriterBlocksBeforeDeletionOrAdoption(t *testing.T) {
	r, p, _ := profileFixture(t)
	ctx := context.Background()
	d := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "brewlet-node-provisioner-legacy", Namespace: "foreign"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "legacy"}}}}}}
	_ = r.Create(ctx, d)
	advance(t, r, p, 1)
	_ = r.Get(ctx, client.ObjectKey{Name: p.Name}, p)
	if len(p.Status.Targets) != 0 || len(p.Finalizers) != 0 {
		t.Fatal("adopted unfenced worker")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(d), d); err != nil {
		t.Fatal("unfenced worker mutated")
	}
}
func TestMissingTargetRetainsFrozenRetirement(t *testing.T) {
	r, p, n := profileFixture(t)
	ctx := context.Background()
	advance(t, r, p, 5)
	_ = r.Get(ctx, client.ObjectKey{Name: p.Name}, p)
	if len(p.Status.Targets) != 1 || !p.Status.Targets[0].Claimed {
		t.Fatal("fixture not claimed")
	}
	_ = r.Delete(ctx, n)
	advance(t, r, p, 3)
	_ = r.Get(ctx, client.ObjectKey{Name: p.Name}, p)
	if len(p.Status.DetachedRetirements) != 1 || p.Status.DetachedRetirements[0].Targets[0].UID != "node-original" || p.Status.DetachedRetirements[0].Phase != "Missing" || len(p.Status.Targets) != 0 {
		t.Fatal("missing host was not retained as independent frozen obligation")
	}
	if !contains(p.Finalizers, api.CleanupFinalizer) {
		t.Fatal("finalizer erased")
	}
}
func TestPrunedAuthorityCheckpointCannotAuthorizeClaim(t *testing.T) {
	r, p, n := profileFixture(t)
	ctx := context.Background()
	r.Reader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if p, ok := obj.(*api.NodeProfile); ok {
			p.Status.ProvisioningSpec = nil
		}
		return nil
	}})
	advance(t, r, p, 1)
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: p.Name}})
	if err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("pruned ledger accepted: %v", err)
	}
	_ = r.Get(ctx, client.ObjectKey{Name: n.Name}, n)
	if n.Labels[api.OwnerUID] != "" {
		t.Fatal(fmt.Sprint("claim granted with pruned CRD"))
	}
}
