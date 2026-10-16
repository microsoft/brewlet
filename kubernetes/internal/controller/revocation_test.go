package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	noderuntime "github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func revocationSetup(t *testing.T) (*RuntimeReconciler, *api.RuntimeImage, *api.NodeProfile, *corev1.Node) {
	t.Helper()
	r, im := runtimeFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKey{Name: im.Name}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, req.NamespacedName, im)
	spec := api.NodeProfileSpec{RuntimeImages: []api.RuntimeEntry{{ID: im.Name}}, Rollout: api.Rollout{ContainerdRestart: "none"}}
	target := api.Target{Name: "node", UID: "node", Claimed: true, ContainerdRestart: "none", ProviderID: "cloud://node", SystemUUID: "system"}
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile", Generation: 1}, Spec: spec, Status: api.NodeProfileStatus{OwnershipInitialized: true, ProvisioningGeneration: 1, ProvisioningSpec: &spec, Targets: []api.Target{target}}}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node", Labels: map[string]string{api.OwnerUID: "profile", api.OwnerNodeUID: "node", "brewlet.sh/runtime." + im.Name: "ready"}, Annotations: map[string]string{api.OwnerName: "web"}}, Spec: corev1.NodeSpec{ProviderID: "cloud://node"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: "system"}}}
	if err := r.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := r.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	return r, im, p, n
}
func setReceipt(t *testing.T, r *RuntimeReconciler, im *api.RuntimeImage, p *api.NodeProfile, n *corev1.Node, mutate func(*noderuntime.RevocationReceipt)) {
	t.Helper()
	ctx := context.Background()
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	receipt := noderuntime.RevocationReceipt{InventoryUID: string(im.UID), Generation: im.Status.Generation, Digest: im.Status.Digest, NodeUID: string(n.UID), ProfileUID: string(p.UID), ProfileGeneration: p.Generation, RevokedAt: time.Now(), AuthorizationExpiresAt: time.Now().Add(5 * time.Minute), RootsCleaned: true}
	if mutate != nil {
		mutate(&receipt)
	}
	b, _ := json.Marshal(map[string]noderuntime.RevocationReceipt{im.Name: receipt})
	n.Annotations[noderuntime.RevocationsAnnotation] = string(b)
	if err := r.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
}
func revokeStep(t *testing.T, r *RuntimeReconciler, im *api.RuntimeImage) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(im)}); err != nil {
		t.Fatal(err)
	}
}
func TestRuntimeDeletionRequiresDurableExactEpochRootReceipt(t *testing.T) {
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	setReceipt(t, r, im, p, n, nil)
	_ = r.Delete(ctx, im)
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if len(im.Status.RevocationTargets) != 1 || im.Status.RevocationTargets[0].Resolved {
		t.Fatal("revocation targets were not checkpointed before consuming receipts")
	}
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if !im.Status.RevocationTargets[0].Resolved || !meta.IsStatusConditionTrue(im.Status.Conditions, "CleanupComplete") {
		t.Fatal("exact cleaned-root receipt did not checkpoint completion")
	}
	revokeStep(t, r, im)
	if err := r.Get(ctx, client.ObjectKeyFromObject(im), new(api.RuntimeImage)); !apierrors.IsNotFound(err) {
		t.Fatalf("proven inventory deletion did not finalize: %v", err)
	}
}
func TestRuntimeDeletionRejectsStaleOrIncompleteReceipts(t *testing.T) {
	cases := map[string]func(*noderuntime.RevocationReceipt){
		"epoch":                func(r *noderuntime.RevocationReceipt) { r.InventoryUID = "retired-epoch" },
		"node":                 func(r *noderuntime.RevocationReceipt) { r.NodeUID = "replacement" },
		"profile":              func(r *noderuntime.RevocationReceipt) { r.ProfileUID = "foreign" },
		"profile generation":   func(r *noderuntime.RevocationReceipt) { r.ProfileGeneration++ },
		"runtime generation":   func(r *noderuntime.RevocationReceipt) { r.Generation++ },
		"leased roots":         func(r *noderuntime.RevocationReceipt) { r.RootsCleaned = false },
		"no atomic revocation": func(r *noderuntime.RevocationReceipt) { r.RevokedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, im, p, n := revocationSetup(t)
			ctx := context.Background()
			setReceipt(t, r, im, p, n, mutate)
			_ = r.Delete(ctx, im)
			revokeStep(t, r, im)
			revokeStep(t, r, im)
			_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
			if !contains(im.Finalizers, api.CleanupFinalizer) || meta.IsStatusConditionTrue(im.Status.Conditions, "CleanupComplete") {
				t.Fatal("stale/incomplete receipt finalized inventory")
			}
		})
	}
}
func TestRuntimeDeletionPinnedWithdrawalConfirmedFallbackAndBothFailure(t *testing.T) {
	for _, failTaint := range []bool{false, true} {
		t.Run(fmt.Sprint(failTaint), func(t *testing.T) {
			r, im, p, n := revocationSetup(t)
			ctx := context.Background()
			setReceipt(t, r, im, p, n, nil)
			c := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if n, ok := obj.(*corev1.Node); ok {
					if _, exists := n.Labels["brewlet.sh/runtime."+im.Name]; !exists || failTaint {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, n.Name, fmt.Errorf("provider refused"))
					}
				}
				return c.Update(ctx, obj, opts...)
			}})
			r.Client = c
			r.Reader = c
			_ = r.Delete(ctx, im)
			for i := 0; i < 3; i++ {
				revokeStep(t, r, im)
			}
			if failTaint {
				if err := r.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil || meta.IsStatusConditionTrue(im.Status.Conditions, "CleanupComplete") {
					t.Fatal("unconfirmed fence finalized deletion")
				}
			} else {
				if err := r.Get(ctx, client.ObjectKeyFromObject(im), im); !apierrors.IsNotFound(err) {
					t.Fatal("confirmed fallback did not permit cleaned epoch deletion")
				}
				_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
				if !control.StartupFenced(n) || n.Labels["brewlet.sh/runtime."+im.Name] != "ready" {
					t.Fatal("pinned label/fence state was not retained")
				}
			}
		})
	}
}
func TestRuntimeMissingNodeNeedsRetainedResolvedEvidence(t *testing.T) {
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	_ = r.Delete(ctx, n)
	target := p.Status.Targets[0]
	p.Status.Targets = nil
	p.Status.DetachedRetirements = []api.Retirement{{Targets: []api.Target{target}, Generation: 1, Spec: p.Spec, Phase: "Missing"}}
	_ = r.Status().Update(ctx, p)
	_ = r.Delete(ctx, im)
	revokeStep(t, r, im)
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if meta.IsStatusConditionTrue(im.Status.Conditions, "CleanupComplete") || !contains(im.Finalizers, api.CleanupFinalizer) {
		t.Fatal("Node absence claimed root cleanup")
	}
	now := metav1.Now()
	e := &api.NodeRetirementEvidence{ObjectMeta: metav1.ObjectMeta{Name: "verified", UID: "verified-uid", Finalizers: []string{api.EvidenceFinalizer}}, Spec: api.NodeRetirementEvidenceSpec{ProfileName: p.Name, ProfileUID: p.UID, NodeName: target.Name, NodeUID: target.UID, ProviderID: target.ProviderID, SystemUUID: target.SystemUUID, ExternalInstanceID: target.ProviderID, PlatformRecord: "audit-record", PermanentlyDecommissioned: true}, Status: api.NodeRetirementEvidenceStatus{AcceptedAt: &now, ResolvedAt: &now, OriginalTarget: &target, CleanupGeneration: 1, CleanupSpec: &p.Spec}}
	_ = r.Create(ctx, e)
	revokeStep(t, r, im)
	revokeStep(t, r, im)
	if err := r.Get(ctx, client.ObjectKeyFromObject(im), new(api.RuntimeImage)); !apierrors.IsNotFound(err) {
		t.Fatalf("retained external disposition did not resolve missing root obligation: %v", err)
	}
}
