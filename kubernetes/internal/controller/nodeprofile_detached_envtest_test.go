// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDetachedRetirementCheckpointsPreserveLegacyLedger(t *testing.T) {
	for _, failure := range []string{"copy-conflict", "pruned-copy", "detach-conflict"} {
		t.Run(failure, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			target := f.profile.Status.Targets[0]
			removeOriginalNode(t, f, target.Name)
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
			fired := false
			f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				p, ok := obj.(*nodev1alpha1.NodeProfile)
				if !ok || fired || len(p.Status.DetachedRetirements) == 0 {
					return f.client.Status().Update(ctx, obj, opts...)
				}
				if failure == "detach-conflict" && len(p.Status.Targets) != 0 {
					return f.client.Status().Update(ctx, obj, opts...)
				}
				fired = true
				if failure == "pruned-copy" {
					pruned := p.DeepCopy()
					pruned.Status.DetachedRetirements = nil
					if err := f.client.Status().Update(ctx, pruned, opts...); err != nil {
						return err
					}
					p.ResourceVersion = pruned.ResourceVersion
					return nil
				}
				return apierrors.NewConflict(nodev1alpha1.GroupVersion.WithResource("nodeprofiles").GroupResource(), p.Name, fmt.Errorf("concurrent checkpoint"))
			}}
			_, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)})
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if !fired || err == nil || !includesTarget(p.Status.Targets, target) || p.Status.Retirement == nil {
				t.Fatalf("failed copy/detach erased legacy obligations: err=%v status=%+v", err, p.Status)
			}
			f.r = newProfileReconciler(f.client, f.r.Config.Namespace)
			f.r.APIReader = f.client
			reconcileProfile(t, f.ctx, f.r, p.Name)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if len(p.Status.Targets) != 0 || len(p.Status.DetachedRetirements) != 1 ||
				!reflect.DeepEqual(p.Status.DetachedRetirements[0].Targets, []nodev1alpha1.NodeTarget{target}) {
				t.Fatal("restart did not migrate exactly one intact obligation")
			}
		})
	}
}

func TestDetachedRetirementMigratesAcceptedLegacyReceipt(t *testing.T) {
	f := newCleanupFixture(t, 1)
	target := f.profile.Status.Targets[0]
	removeOriginalNode(t, f, target.Name)
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	e := retirementEvidence(f, target)
	submitEvidence(t, f, e)
	// Seed the intact #240 checkpoint: accepted evidence and receipt in the
	// original active retirement ledger, before any independent detachment.
	if err := f.r.acceptTargetEvidence(f.ctx, &p, target); err != nil {
		t.Fatal(err)
	}
	frozen := evidenceObligation(&p, target)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if len(p.Status.DetachedRetirements) != 1 ||
		p.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceUID != e.UID {
		t.Fatal("migration lost an accepted legacy receipt")
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	requireEvidenceResolved(t, f, e, target)
	if !reflect.DeepEqual(e.Status.Obligation, &frozen) {
		t.Fatal("migration changed the frozen legacy evidence obligation")
	}
}

func TestDetachedRetirementsKeepSeparatePoliciesAndBlockDeletion(t *testing.T) {
	f := newCleanupFixture(t, 2)
	first, second := f.profile.Status.Targets[0], f.profile.Status.Targets[1]
	removeOriginalNode(t, f, first.Name)
	beginLostNodeRetirement(t, f)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	frozen := *p.Status.DetachedRetirements[0].DeepCopy()
	p.Spec.JDKs[0].Feature = 25
	p.Spec.Rollout.ContainerdRestart = nodev1alpha1.ContainerdRestartNone
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	secondPolicy := *p.Status.ProvisioningSpec.DeepCopy()
	secondGeneration := p.Status.ProvisioningGeneration
	removeOriginalNode(t, f, second.Name)
	beginLostNodeRetirement(t, f)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if len(p.Status.DetachedRetirements) != 2 || !reflect.DeepEqual(frozen, p.Status.DetachedRetirements[0]) ||
		!reflect.DeepEqual(secondPolicy, p.Status.DetachedRetirements[1].Spec) ||
		p.Status.DetachedRetirements[1].Generation != secondGeneration {
		t.Fatal("repeated loss overwrote an earlier authorized policy")
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	if err := f.client.Delete(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(p.Name))
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if len(p.Status.DetachedRetirements) != 2 || conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked ||
		!containsString(p.Finalizers, brewlet.FinalizerCleanup) || !HasNodeProfileCleanupObligations(&p) {
		t.Fatal("deletion treated independent history as successful cleanup")
	}
	// Resolve the later loss first: an earlier missing attestation must not
	// block processing of another independent obligation.
	e2 := retirementEvidence(f, second)
	submitEvidence(t, f, e2)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if len(p.Status.DetachedRetirements) != 1 || !reflect.DeepEqual(frozen, p.Status.DetachedRetirements[0]) {
		t.Fatal("partial evidence discarded another host's history")
	}
	e1 := retirementEvidence(f, first)
	submitEvidence(t, f, e1)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&p), &p); !apierrors.IsNotFound(err) {
		t.Fatalf("resolved deletion did not finish: %v", err)
	}
	requireEvidenceResolved(t, f, e1, first)
	requireEvidenceResolved(t, f, e2, second)
	if !reflect.DeepEqual(e1.Status.Obligation.Spec, frozen.Spec) ||
		!reflect.DeepEqual(e2.Status.Obligation.Spec, secondPolicy) {
		t.Fatal("independent evidence lost per-host policies after profile deletion")
	}
}

func TestDetachedHostIdentityConflictsDoNotStopDistinctProvisioning(t *testing.T) {
	for _, identity := range []string{"providerID", "systemUUID", "same-name-providerID", "returning-advertisements", "unowned-advertisements", "unselected-foreign-advertisements"} {
		t.Run(identity, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			p.Status.Targets[0].ProviderID = "platform://host-" + p.Name
			p.Status.Targets[0].SystemUUID = "system-" + p.Name
			if err := f.client.Status().Update(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			target := p.Status.Targets[0]
			removeOriginalNode(t, f, target.Name)
			beginLostNodeRetirement(t, f)
			name := uniqueName("reregistered")
			if identity == "same-name-providerID" {
				name = target.Name
			}
			node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"agentpool": p.Spec.NodePool.Names[0]}}}
			if identity == "unselected-foreign-advertisements" {
				node.Labels = map[string]string{"agentpool": "unselected-" + p.Name}
			}
			if identity != "systemUUID" {
				node.Spec.ProviderID = target.ProviderID
			}
			if err := f.client.Create(f.ctx, &node); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.client.Delete(context.Background(), &node) })
			switch identity {
			case "returning-advertisements":
				node.Labels[brewlet.LabelNodeOwner] = string(p.UID)
				node.Labels[brewlet.LabelNodeIdentity] = string(target.UID)
				node.Labels[brewlet.LabelRuntimeReady] = brewlet.ValueReady
				node.Annotations = map[string]string{
					brewlet.AnnotationNodeOwner: p.Name, brewlet.AnnotationProfile: p.Name,
				}
			case "unowned-advertisements":
				node.Labels[brewlet.LabelRuntimeReady] = brewlet.ValueReady
				node.Labels[brewlet.LabelLauncherPrefix+"jaz"] = "true"
			case "unselected-foreign-advertisements":
				node.Labels[brewlet.LabelNodeOwner] = "foreign-profile-uid"
				node.Labels[brewlet.LabelRuntimeReady] = brewlet.ValueReady
				node.Annotations = map[string]string{brewlet.AnnotationNodeOwner: "foreign", brewlet.AnnotationProfile: "foreign"}
			}
			if strings.HasSuffix(identity, "advertisements") {
				if err := f.client.Update(f.ctx, &node); err != nil {
					t.Fatal(err)
				}
			}
			if identity == "systemUUID" {
				node.Status.NodeInfo.SystemUUID = strings.ToUpper(target.SystemUUID)
				if err := f.client.Status().Update(f.ctx, &node); err != nil {
					t.Fatal(err)
				}
			}
			healthy := createNode(t, f.ctx, f.client, map[string]string{"agentpool": p.Spec.NodePool.Names[0]})
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if len(p.Status.Targets) != 1 || p.Status.Targets[0].Name != healthy || len(p.Status.DetachedRetirements) != 1 {
				t.Fatal("identity conflict authorized returning host or blocked distinct host")
			}
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&node), &node); err != nil {
				t.Fatal(err)
			}
			switch identity {
			case "returning-advertisements":
				if node.Labels[brewlet.LabelRuntimeReady] != "" || node.Labels[brewlet.LabelNodeIdentity] != string(target.UID) {
					t.Fatal("returning host kept stale readiness or lost original ownership")
				}
			case "unowned-advertisements":
				if node.Labels[brewlet.LabelRuntimeReady] != "" || node.Labels[brewlet.LabelLauncherPrefix+"jaz"] != "" || node.Labels[brewlet.LabelNodeOwner] != "" {
					t.Fatal("unowned returning host kept stale advertisements or was claimed")
				}
			case "unselected-foreign-advertisements":
				if node.Labels[brewlet.LabelRuntimeReady] != "" || node.Labels[brewlet.LabelNodeOwner] != "foreign-profile-uid" ||
					node.Annotations[brewlet.AnnotationNodeOwner] != "foreign" {
					t.Fatal("unselected returning host kept stale readiness or lost foreign ownership metadata")
				}
			default:
				if node.Labels[brewlet.LabelNodeOwner] != "" {
					t.Fatal("same physical host was silently claimed as fresh")
				}
			}
			e := retirementEvidence(f, target)
			submitEvidence(t, f, e)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if len(p.Status.DetachedRetirements) != 1 || !meta.IsStatusConditionTrue(p.Status.Conditions, nodev1alpha1.ConditionRetirementPending) {
				t.Fatal("evidence incorrectly resolved a registered original host")
			}
		})
	}
}

func TestRetainedEvidenceFencesNewProfileAfterDeletion(t *testing.T) {
	f := newCleanupFixture(t, 1)
	target := f.profile.Status.Targets[0]
	target.ProviderID = "platform://retired-" + f.profile.Name
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Status.Targets[0] = target
	if err := f.client.Status().Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	removeOriginalNode(t, f, target.Name)
	beginLostNodeRetirement(t, f)
	e := retirementEvidence(f, target)
	submitEvidence(t, f, e)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	if err := f.client.Delete(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(p.Name))
	reconcileProfile(t, f.ctx, f.r, p.Name)
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&p), &p); !apierrors.IsNotFound(err) {
		t.Fatalf("profile was not deleted: %v", err)
	}
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: uniqueName("returning"), UID: types.UID("new-uid")}, Spec: corev1.NodeSpec{ProviderID: target.ProviderID}}
	if err := f.r.retiredHostConflict(f.ctx, &node); err == nil {
		t.Fatal("profile deletion erased retained host identity fence")
	}
}
