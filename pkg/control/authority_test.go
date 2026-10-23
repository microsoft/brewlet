package control

import (
	"context"
	"fmt"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func fixture(t *testing.T, intercept interceptor.Funcs) (client.Client, Authority) {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = api.AddToScheme(s)
	spec := api.NodeProfileSpec{NodePool: api.NodePool{Names: []string{"web"}}, RuntimeImages: []api.RuntimeEntry{{ID: "python-3.12"}}, Rollout: api.Rollout{ContainerdRestart: "none"}}
	target := api.Target{Name: "node", UID: "node-uid", Claimed: true, ContainerdRestart: "none"}
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile-uid", Generation: 1}, Spec: spec, Status: api.NodeProfileStatus{OwnershipInitialized: true, ProvisioningGeneration: 1, ProvisioningSpec: &spec, Targets: []api.Target{target}}}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid", Labels: map[string]string{api.OwnerUID: string(p.UID), api.OwnerNodeUID: "node-uid", api.RuntimeLabel: "ready", "brewlet.sh/runtime.python-3.12": "ready"}, Annotations: map[string]string{api.OwnerName: "web"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.NodeProfile{}).WithObjects(p, n).WithInterceptorFuncs(intercept).Build()
	return c, Authority{NodeName: n.Name, NodeUID: n.UID, ProfileName: p.Name, ProfileUID: p.UID, Generation: 1, TargetUIDs: []types.UID{n.UID}, Mode: "provisioning"}
}
func TestCheckRejectsMissingAndStaleAuthority(t *testing.T) {
	c, a := fixture(t, interceptor.Funcs{})
	ctx := context.Background()
	if _, err := Check(ctx, c, a); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Authority){
		"uid":                    func(a *Authority) { a.NodeUID = "replacement" },
		"literal profile":        func(a *Authority) { a.ProfileUID = "" },
		"generation":             func(a *Authority) { a.Generation = 2 },
		"target allowlist":       func(a *Authority) { a.TargetUIDs = nil },
		"cleanup without ledger": func(a *Authority) { a.Mode = "cleanup" },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			b := a
			fn(&b)
			if _, err := Check(ctx, c, b); err == nil {
				t.Fatal("invalid authority passed")
			}
		})
	}
	p := new(api.NodeProfile)
	_ = c.Get(ctx, client.ObjectKey{Name: a.ProfileName}, p)
	p.Status.Retirement = &api.Retirement{Targets: p.Status.Targets, Generation: 1, Spec: p.Spec, Phase: "Cleaning"}
	_ = c.Status().Update(ctx, p)
	if _, err := Check(ctx, c, a); err == nil {
		t.Fatal("retiring target still authorized provisioning")
	}
	a.Mode = "cleanup"
	if _, err := Check(ctx, c, a); err != nil {
		t.Fatal(err)
	}
}
func TestPinnedLabelRefusalUsesConfirmedNodeWideFence(t *testing.T) {
	refusals := 0
	c, a := fixture(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if n, ok := obj.(*corev1.Node); ok && n.Labels["brewlet.sh/runtime.python-3.12"] == "" {
			refusals++
			return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, n.Name, fmt.Errorf("provider pins pool label"))
		}
		return c.Update(ctx, obj, opts...)
	}})
	broader, err := Withdraw(context.Background(), c, c, a, []string{"brewlet.sh/runtime.python-3.12"})
	if err != nil || !broader || refusals != 1 {
		t.Fatalf("fallback failed: %t %v (%d refusals)", broader, err, refusals)
	}
	n := new(corev1.Node)
	_ = c.Get(context.Background(), client.ObjectKey{Name: a.NodeName}, n)
	if !StartupFenced(n) || n.Labels["brewlet.sh/runtime.python-3.12"] != "ready" {
		t.Fatal("fallback must retain pinned labels behind confirmed startup fence")
	}
}
func TestBothWritesFailAndDroppedTaintNeverConfirm(t *testing.T) {
	for _, drop := range []bool{false, true} {
		t.Run(fmt.Sprint(drop), func(t *testing.T) {
			c, a := fixture(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if n, ok := obj.(*corev1.Node); ok {
					if n.Labels["brewlet.sh/runtime.python-3.12"] == "" {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, n.Name, fmt.Errorf("provider refused mutation"))
					}
					if !drop {
						return fmt.Errorf("provider refused taint mutation")
					}
					n.Spec.Taints = nil
				}
				return c.Update(ctx, obj, opts...)
			}})
			confirmed, err := Withdraw(context.Background(), c, c, a, []string{"brewlet.sh/runtime.python-3.12"})
			if err == nil || confirmed {
				t.Fatal("unconfirmed fence treated as success")
			}
		})
	}
}
func TestConcurrentNodeReplacementCannotReceiveFallback(t *testing.T) {
	calls := 0
	c, a := fixture(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if n, ok := obj.(*corev1.Node); ok {
			calls++
			old := new(corev1.Node)
			_ = c.Get(ctx, client.ObjectKey{Name: n.Name}, old)
			_ = c.Delete(ctx, old)
			newNode := old.DeepCopy()
			newNode.UID = "new-node"
			newNode.ResourceVersion = ""
			_ = c.Create(ctx, newNode)
			return apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, n.Name, fmt.Errorf("replaced"))
		}
		return c.Update(ctx, obj, opts...)
	}})
	_, err := Withdraw(context.Background(), c, c, a, []string{api.RuntimeLabel})
	if err == nil || !strings.Contains(err.Error(), "ownership-fence-failed") || calls != 1 {
		t.Fatalf("replacement was mutated: %v calls=%d", err, calls)
	}
}
func TestClaimReleaseWaitsForWorkerAndRevalidatesCAS(t *testing.T) {
	c, a := fixture(t, interceptor.Funcs{})
	ctx := context.Background()
	p := new(api.NodeProfile)
	_ = c.Get(ctx, client.ObjectKey{Name: a.ProfileName}, p)
	p.Status.Retirement = &api.Retirement{Targets: p.Status.Targets, Generation: 1, Spec: p.Spec, Phase: "Teardown"}
	_ = c.Status().Update(ctx, p)
	n := new(corev1.Node)
	_ = c.Get(ctx, client.ObjectKey{Name: a.NodeName}, n)
	delete(n.Labels, api.RuntimeLabel)
	delete(n.Labels, "brewlet.sh/runtime.python-3.12")
	_ = c.Update(ctx, n)
	d := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: "ns"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Env: []corev1.EnvVar{{Name: "BREWLET_PROFILE_UID", Value: string(a.ProfileUID)}}}}}}}}
	_ = c.Create(ctx, d)
	if ReleaseClaim(ctx, c, c, a) == nil {
		t.Fatal("claim released while controller could recreate cleanup pods")
	}
	_ = c.Delete(ctx, d)
	_ = c.Get(ctx, client.ObjectKey{Name: a.NodeName}, n)
	n.Labels[api.OwnerUID] = "foreign"
	_ = c.Update(ctx, n)
	if ReleaseClaim(ctx, c, c, a) == nil {
		t.Fatal("foreign claim released")
	}
}
