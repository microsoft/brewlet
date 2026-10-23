package controller

import (
	"context"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func evidenceSetup(t *testing.T) (*EvidenceReconciler, *api.NodeProfile, *api.NodeRetirementEvidence) {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	spec := api.NodeProfileSpec{NodePool: api.NodePool{Key: "agentpool", Names: []string{"web"}}, RuntimeImages: []api.RuntimeEntry{{ID: "python-3.12"}}, Rollout: api.Rollout{ContainerdRestart: "none"}}
	target := api.Target{Name: "old", UID: "old-node", Claimed: true, ProviderID: "cloud://old-instance", SystemUUID: "old-system", ContainerdRestart: "none"}
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile", Generation: 1, Finalizers: []string{api.CleanupFinalizer}}, Spec: spec, Status: api.NodeProfileStatus{OwnershipInitialized: true, ProvisioningGeneration: 1, ProvisioningSpec: &spec, DetachedRetirements: []api.Retirement{{Targets: []api.Target{target}, Generation: 1, Spec: spec, Phase: "Missing"}}}}
	e := &api.NodeRetirementEvidence{ObjectMeta: metav1.ObjectMeta{Name: "verified-retirement", UID: "evidence", Generation: 1}, Spec: api.NodeRetirementEvidenceSpec{ProfileName: p.Name, ProfileUID: p.UID, NodeName: target.Name, NodeUID: target.UID, ProviderID: target.ProviderID, SystemUUID: target.SystemUUID, ExternalInstanceID: "cloud://old-instance", PlatformRecord: "https://audit.example.com/retirements/123", PermanentlyDecommissioned: true}}
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.NodeProfile{}, &api.NodeRetirementEvidence{}).WithObjects(p, e).Build()
	return &EvidenceReconciler{Client: c, Reader: c}, p, e
}
func evidenceStep(t *testing.T, r *EvidenceReconciler, e *api.NodeRetirementEvidence) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: e.Name}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(e), e); err != nil {
		t.Fatal(err)
	}
}
func TestEvidenceAcceptanceReceiptResolutionAndIndependentRetention(t *testing.T) {
	r, p, e := evidenceSetup(t)
	ctx := context.Background()
	evidenceStep(t, r, e)
	if !contains(e.Finalizers, api.EvidenceFinalizer) || e.Status.AcceptedAt != nil {
		t.Fatal("evidence not retained before acceptance")
	}
	evidenceStep(t, r, e)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if e.Status.AcceptedAt == nil || e.Status.CleanupSpec == nil || e.Status.OriginalTarget == nil || p.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceUID != "" {
		t.Fatal("acceptance checkpoint/receipt ordering violated")
	}
	evidenceStep(t, r, e)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if p.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceUID != e.UID || e.Status.ResolvedAt != nil {
		t.Fatal("UID-bound receipt was not checkpointed before resolution")
	}
	evidenceStep(t, r, e)
	if e.Status.ResolvedAt == nil {
		t.Fatal("durably receipted obligation did not resolve")
	}
	pr := &ProfileReconciler{Client: r.Client, Reader: r.Reader, Namespace: "brewlet-system", WorkerImage: "worker"}
	advance(t, pr, p, 1)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if len(p.Status.DetachedRetirements) != 0 || !p.Status.ExternalRetirementResolved {
		t.Fatal("resolved detached receipt not consumed")
	}
	if err := r.Delete(ctx, p); err != nil {
		t.Fatal(err)
	}
	advance(t, pr, p, 1)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	c := meta.FindStatusCondition(p.Status.Conditions, "CleanupComplete")
	if c == nil || c.Reason != "CleanupResolved" {
		t.Fatal("external disposition falsely called executed host cleanup")
	}
	advance(t, pr, p, 1)
	if err := r.Get(ctx, client.ObjectKeyFromObject(e), e); err != nil || len(e.OwnerReferences) != 0 || e.Spec.ProfileUID != "profile" || e.Status.ResolvedAt == nil {
		t.Fatal("independent resolved evidence was garbage collected or lost original identity")
	}
}
func TestEvidenceOriginalNodeAndPhysicalIdentityMismatchRejected(t *testing.T) {
	for _, present := range []bool{true, false} {
		t.Run(map[bool]string{true: "original-node-present", false: "provider-mismatch"}[present], func(t *testing.T) {
			r, _, e := evidenceSetup(t)
			ctx := context.Background()
			if present {
				_ = r.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "renamed-original", UID: "old-node"}})
			} else {
				e.Spec.ProviderID = "cloud://foreign"
				_ = r.Update(ctx, e)
			}
			evidenceStep(t, r, e)
			evidenceStep(t, r, e)
			if e.Status.AcceptedAt != nil || e.Status.ResolvedAt != nil {
				t.Fatal("unverified external identity accepted")
			}
		})
	}
}
func TestEvidenceLegacyMappingAndRecreatedReceiptFailClosed(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(map[bool]string{true: "legacy-mapping", false: "recreated-evidence"}[legacy], func(t *testing.T) {
			r, p, e := evidenceSetup(t)
			ctx := context.Background()
			if legacy {
				p.Status.DetachedRetirements[0].Targets[0].ProviderID = ""
				p.Status.DetachedRetirements[0].Targets[0].SystemUUID = ""
			} else {
				p.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceName = e.Name
				p.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceUID = "old-evidence"
			}
			_ = r.Status().Update(ctx, p)
			evidenceStep(t, r, e)
			evidenceStep(t, r, e)
			if e.Status.AcceptedAt != nil {
				t.Fatal("legacy/unverified/recreated receipt accepted")
			}
		})
	}
}
func TestEvidenceOldWorkerBlocksButBoundedSuccessorMayCoexist(t *testing.T) {
	r, p, e := evidenceSetup(t)
	ctx := context.Background()
	d := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "arbitrary", Namespace: "foreign"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "worker", Command: []string{"/brewlet-node"}, Env: []corev1.EnvVar{{Name: "BREWLET_PROFILE_UID", Value: string(p.UID)}, {Name: "BREWLET_TARGET_UIDS", Value: `["old-node"]`}}}}}}}}
	_ = r.Create(ctx, d)
	evidenceStep(t, r, e)
	evidenceStep(t, r, e)
	if e.Status.AcceptedAt != nil {
		t.Fatal("writer teardown bypassed")
	}
	d.Spec.Template.Spec.Containers[0].Env[1].Value = `["distinct-new-node"]`
	_ = r.Update(ctx, d)
	evidenceStep(t, r, e)
	if e.Status.AcceptedAt == nil {
		t.Fatal("bounded successor unable to coexist with external evidence acceptance")
	}
}
func TestEvidencePrunedCheckpointCannotIssueReceipt(t *testing.T) {
	r, p, e := evidenceSetup(t)
	ctx := context.Background()
	evidenceStep(t, r, e)
	r.Reader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, o, opts...); err != nil {
			return err
		}
		if e, ok := o.(*api.NodeRetirementEvidence); ok {
			e.Status.CleanupSpec = nil
		}
		return nil
	}})
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: e.Name}})
	if err == nil || !strings.Contains(err.Error(), "checkpoint") {
		t.Fatalf("pruned accepted snapshot authorized receipt: %v", err)
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if p.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceUID != "" {
		t.Fatal("receipt issued without durable acceptance")
	}
}
func TestResolvedEvidenceFencesForeignReturningHostWithoutAdoption(t *testing.T) {
	r, p, e := evidenceSetup(t)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		evidenceStep(t, r, e)
	}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "different-registration", UID: "new-node", Labels: map[string]string{api.RuntimeLabel: "ready", "brewlet.sh/runtime.python-3.12": "ready", api.OwnerUID: "foreign-profile", api.OwnerNodeUID: "new-node"}, Annotations: map[string]string{api.OwnerName: "foreign"}}, Spec: corev1.NodeSpec{ProviderID: "cloud://old-instance"}}
	_ = r.Create(ctx, n)
	evidenceStep(t, r, e)
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	if n.Labels[api.RuntimeLabel] != "" || n.Labels["brewlet.sh/runtime.python-3.12"] != "" || n.Labels[api.OwnerUID] != "foreign-profile" || n.Annotations[api.OwnerName] != "foreign" {
		t.Fatal("returning host was not quarantined without ownership adoption")
	}
	if e.Status.OriginalTarget.UID != "old-node" || e.Status.ResolvedAt == nil {
		t.Fatal("returning registration rewrote original evidence")
	}
	_ = p
}
