package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"fmt"
	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func runtimeFixture(t *testing.T) (*RuntimeReconciler, *api.RuntimeImage) {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = api.AddToScheme(s)
	im := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12", UID: "epoch-one", Generation: 1, Finalizers: []string{api.CleanupFinalizer}}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: "registry.example.com/python@sha256:" + strings.Repeat("a", 64)}, User: api.User{UID: 1000, GID: 1000}}}
	_ = encodeJournal(im, journalFor(im, 1))
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.RuntimeImage{}, &api.NodeProfile{}).WithObjects(im).Build()
	return &RuntimeReconciler{Client: c, Reader: c}, im
}
func TestMonotonicGenerationsAndRestartRecovery(t *testing.T) {
	r, im := runtimeFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: im.Name}}
	for i, d := range []string{"a", "b", "a"} {
		if i > 0 {
			_ = r.Get(ctx, req.NamespacedName, im)
			im.Generation++
			im.Spec.Source.Image = "registry.example.com/python@sha256:" + strings.Repeat(d, 64)
			if err := r.Update(ctx, im); err != nil {
				t.Fatal(err)
			}
		}
		restarted := &RuntimeReconciler{Client: r.Client, Reader: r.Reader}
		if _, err := restarted.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		if _, err := restarted.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		_ = r.Get(ctx, req.NamespacedName, im)
		if im.Status.Generation != int64(i+1) || im.Status.Digest != "sha256:"+strings.Repeat(d, 64) {
			t.Fatalf("generation reused: %+v", im.Status)
		}
	}
	im.Spec.User.UID = 1234
	im.Generation++
	_ = r.Update(ctx, im)
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, req.NamespacedName, im)
	if meta.IsStatusConditionTrue(im.Status.Conditions, "Approved") {
		t.Fatal("bypassed immutable user update approved")
	}
}
func TestExplicitRevocationNotOverwritten(t *testing.T) {
	r, im := runtimeFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: im.Name}}
	_, _ = r.Reconcile(ctx, req)
	_ = r.Get(ctx, req.NamespacedName, im)
	meta.SetStatusCondition(&im.Status.Conditions, metav1.Condition{Type: "Approved", Status: metav1.ConditionFalse, Reason: "AdministratorRevocation", Message: "revoked", LastTransitionTime: metav1.Now()})
	_ = r.Status().Update(ctx, im)
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, req.NamespacedName, im)
	if meta.IsStatusConditionTrue(im.Status.Conditions, "Approved") {
		t.Fatal("explicit revocation overwritten")
	}
}

func TestRotationInstalledRetryParticipantAndConfirmedExclusion(t *testing.T) {
	r, im := runtimeFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: im.Name}}
	_, _ = r.Reconcile(ctx, req)
	_ = r.Get(ctx, req.NamespacedName, im)
	spec := api.NodeProfileSpec{NodePool: api.NodePool{Names: []string{"web"}}, RuntimeImages: []api.RuntimeEntry{{ID: im.Name, Install: "OnDemand"}}, Rollout: api.Rollout{ContainerdRestart: "none"}}
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile", Generation: 1}, Spec: spec, Status: api.NodeProfileStatus{OwnershipInitialized: true, ProvisioningGeneration: 1, ProvisioningSpec: &spec}}
	for _, name := range []string{"warm", "retry", "cold", "cordoned"} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: typesUID(name), Labels: map[string]string{api.OwnerUID: "profile", api.OwnerNodeUID: name, api.RuntimeLabel: "ready", "brewlet.sh/runtime." + im.Name: "ready"}, Annotations: map[string]string{api.OwnerName: "web"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
		local := api.NodeRuntime{Install: "OnDemand", State: "ready", Installed: true, InventoryUID: im.UID, Generation: im.Status.Generation, Digest: im.Status.Digest}
		if name == "retry" {
			local.State = "failed"
		}
		if name == "cold" {
			local.State = "available"
			local.Installed = false
		}
		if name == "cordoned" {
			n.Spec.Unschedulable = true
			local.Generation = 0
			local.Installed = false
		}
		b, _ := json.Marshal(map[string]api.NodeRuntime{im.Name: local})
		n.Annotations[api.InventoryAnnotation] = string(b)
		p.Status.Targets = append(p.Status.Targets, api.Target{Name: name, UID: n.UID, Claimed: true, ContainerdRestart: "none"})
		if err := r.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := r.rotation(ctx, im); err != nil {
		t.Fatal(err)
	}
	if meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") || len(im.Status.Rotation.Participants) != 2 {
		t.Fatal("installed retry disappeared from participants")
	}
	n := new(corev1.Node)
	_ = r.Get(ctx, client.ObjectKey{Name: "retry"}, n)
	b, _ := json.Marshal(map[string]api.NodeRuntime{im.Name: {Install: "OnDemand", State: "ready", Installed: true, InventoryUID: im.UID, Generation: im.Status.Generation, Digest: im.Status.Digest}})
	n.Annotations[api.InventoryAnnotation] = string(b)
	_ = r.Update(ctx, n)
	// A cordoned installed entry must be fenced and retained as excluded.
	_ = r.Get(ctx, client.ObjectKey{Name: "cordoned"}, n)
	n.Annotations[api.InventoryAnnotation] = string(b)
	_ = r.Update(ctx, n)
	if err := r.rotation(ctx, im); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") || len(im.Status.Rotation.ExcludedNodes) != 1 {
		t.Fatalf("completion/exclusion incorrect: %+v", im.Status)
	}
	once := *im.Status.RotationCompletedAt
	if err := r.rotation(ctx, im); err != nil {
		t.Fatal(err)
	}
	if !once.Equal(im.Status.RotationCompletedAt) {
		t.Fatal("completion timestamp rewritten")
	}
	_ = r.Get(ctx, client.ObjectKey{Name: "cordoned"}, n)
	if _, present := n.Labels["brewlet.sh/runtime."+im.Name]; present {
		t.Fatal("excluded node's capability still advertised")
	}
}

func TestRotationBothFencesFailBlocksCompletion(t *testing.T) {
	r, im := runtimeFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: im.Name}}
	_, _ = r.Reconcile(ctx, req)
	_ = r.Get(ctx, req.NamespacedName, im)
	spec := api.NodeProfileSpec{RuntimeImages: []api.RuntimeEntry{{ID: im.Name}}, Rollout: api.Rollout{ContainerdRestart: "none"}}
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "p", Generation: 1}, Spec: spec, Status: api.NodeProfileStatus{OwnershipInitialized: true, ProvisioningGeneration: 1, ProvisioningSpec: &spec, Targets: []api.Target{{Name: "n", UID: "n", Claimed: true, ContainerdRestart: "none"}}}}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", UID: "n", Labels: map[string]string{api.OwnerUID: "p", api.OwnerNodeUID: "n", "brewlet.sh/runtime." + im.Name: "ready"}, Annotations: map[string]string{api.OwnerName: "web"}}, Spec: corev1.NodeSpec{Unschedulable: true}}
	_ = r.Create(ctx, p)
	_ = r.Create(ctx, n)
	c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, obj.GetName(), fmt.Errorf("provider refused"))
	}})
	r.Client = c
	r.Reader = c
	if err := r.rotation(ctx, im); err != nil {
		t.Fatal(err)
	}
	if meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") || im.Status.RotationCompletedAt != nil {
		t.Fatal("unconfirmed exclusion completed rotation")
	}
	if !strings.Contains(im.Status.Rotation.ExcludedNodes[0].Reason, "UnconfirmedFence") {
		t.Fatal("blocked fence not reported")
	}
}
func TestRotationEligibilityUsesLiveOwnershipAndExplicitInstallation(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		mutate                 func(*api.NodeProfile, *corev1.Node, *api.NodeRuntime)
		participants, excluded int
		reason                 string
		complete               bool
	}{
		{"ready eager", func(*api.NodeProfile, *corev1.Node, *api.NodeRuntime) {}, 1, 0, "", true},
		{"not ready", func(_ *api.NodeProfile, n *corev1.Node, _ *api.NodeRuntime) {
			n.Status.Conditions[0].Status = corev1.ConditionFalse
		}, 0, 1, "NodeNotReady", true},
		{"startup fenced", func(_ *api.NodeProfile, n *corev1.Node, _ *api.NodeRuntime) {
			n.Spec.Taints = []corev1.Taint{{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}}
		}, 0, 1, "NodeWideStartupFence", true},
		{"stale node claim", func(_ *api.NodeProfile, n *corev1.Node, _ *api.NodeRuntime) {
			n.Labels[api.OwnerNodeUID] = "different-node"
		}, 0, 1, "OwnershipUnverified", false},
		{"stale profile generation", func(p *api.NodeProfile, _ *corev1.Node, _ *api.NodeRuntime) {
			p.Generation++
		}, 0, 1, "OwnershipUnverified", false},
		{"retiring profile", func(p *api.NodeProfile, _ *corev1.Node, _ *api.NodeRuntime) {
			p.Status.Retirement = &api.Retirement{Targets: p.Status.Targets, Generation: 1, Spec: p.Spec, Phase: "Cleaning"}
		}, 0, 1, "ProfileRetiring", true},
		{"cold positive-generation on-demand", func(p *api.NodeProfile, n *corev1.Node, local *api.NodeRuntime) {
			p.Spec.RuntimeImages[0].Install = "OnDemand"
			local.Install, local.State, local.Installed = "OnDemand", "available", false
			n.Labels["brewlet.sh/runtime."+p.Spec.RuntimeImages[0].ID] = "available"
		}, 0, 0, "", true},
		{"installed failed on-demand", func(p *api.NodeProfile, n *corev1.Node, local *api.NodeRuntime) {
			p.Spec.RuntimeImages[0].Install = "OnDemand"
			local.Install, local.State = "OnDemand", "failed"
			delete(n.Labels, "brewlet.sh/runtime."+p.Spec.RuntimeImages[0].ID)
		}, 1, 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, im, p, n := revocationSetup(t)
			ctx := context.Background()
			n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			local := api.NodeRuntime{Install: "Eager", State: "ready", Installed: true, InventoryUID: im.UID, Generation: im.Status.Generation, Digest: im.Status.Digest}
			tc.mutate(p, n, &local)
			frozen := p.Spec
			p.Status.ProvisioningSpec = &frozen
			profileStatus := p.DeepCopy().Status
			if err := r.Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			p.Status = profileStatus
			if err := r.Status().Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(map[string]api.NodeRuntime{im.Name: local})
			if err != nil {
				t.Fatal(err)
			}
			n.Annotations[api.InventoryAnnotation] = string(data)
			nodeStatus := n.DeepCopy().Status
			if err := r.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			n.Status = nodeStatus
			if err := r.Status().Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			if err := r.rotation(ctx, im); err != nil {
				t.Fatal(err)
			}
			if len(im.Status.Rotation.Participants) != tc.participants || len(im.Status.Rotation.ExcludedNodes) != tc.excluded ||
				meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete") != tc.complete {
				t.Fatalf("eligibility or confirmed-exclusion result changed: %+v", im.Status)
			}
			if tc.excluded != 0 {
				observed := im.Status.Rotation.ExcludedNodes[0]
				if observed.UID != n.UID || observed.ObservedGeneration != local.Generation || !strings.HasPrefix(observed.Reason, tc.reason) {
					t.Fatalf("exclusion lost original identity/generation/reason: %+v", observed)
				}
			}
		})
	}
}

func typesUID(s string) types.UID { return types.UID(s) }
