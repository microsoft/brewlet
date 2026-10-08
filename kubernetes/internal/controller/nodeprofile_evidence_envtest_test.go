// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func retirementEvidence(f *cleanupFixture, target nodev1alpha1.NodeTarget) *nodev1alpha1.NodeRetirementEvidence {
	provider := target.ProviderID
	if provider == "" {
		provider = "platform://original-host"
	}
	return &nodev1alpha1.NodeRetirementEvidence{
		ObjectMeta: metav1.ObjectMeta{Name: uniqueName("retired")},
		Spec: nodev1alpha1.RetirementEvidenceSpec{
			ProfileName: f.profile.Name, ProfileUID: f.profile.UID,
			NodeName: target.Name, NodeUID: target.UID,
			ProviderID: provider, SystemUUID: target.SystemUUID, InstanceID: "original-instance-uuid",
			EvidenceRef:               "https://records.example.test/decommission/123",
			IdentityBinding:           "Archived Node JSON and platform record bind this Node UID to the permanently destroyed VM instance.",
			RetiredAt:                 metav1.NewTime(time.Now().Add(-time.Minute)),
			PermanentlyDecommissioned: true,
		},
	}
}

func submitEvidence(t *testing.T, f *cleanupFixture, e *nodev1alpha1.NodeRetirementEvidence) {
	t.Helper()
	if err := f.client.Create(f.ctx, e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.client.Delete(context.Background(), e) })
}

func removeOriginalNode(t *testing.T, f *cleanupFixture, name string) {
	t.Helper()
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(f.ctx, &node); err != nil {
		t.Fatal(err)
	}
}

func beginLostNodeRetirement(t *testing.T, f *cleanupFixture) {
	t.Helper()
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
}

func requireEvidenceResolved(t *testing.T, f *cleanupFixture, e *nodev1alpha1.NodeRetirementEvidence, target nodev1alpha1.NodeTarget) {
	t.Helper()
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(e), e); err != nil {
		t.Fatal(err)
	}
	if e.Status.Phase != nodev1alpha1.EvidenceResolved || e.Status.AcceptedAt == nil || e.Status.ResolvedAt == nil ||
		e.Status.Obligation == nil || !reflect.DeepEqual(e.Status.Obligation.Targets, []nodev1alpha1.NodeTarget{target}) ||
		len(e.OwnerReferences) != 0 {
		t.Fatalf("lost external retirement history: %+v", e)
	}
}

func TestExternalRetirementResumesReplacementProvisioning(t *testing.T) {
	for _, sameName := range []bool{false, true} {
		t.Run(fmt.Sprintf("same-name-%t", sameName), func(t *testing.T) {
			f := newCleanupFixture(t, 2)
			target := f.profile.Status.Targets[0]
			kept := updateTargetNode(t, f, f.profile.Status.Targets[1].Name, func(n *corev1.Node) {
				n.Labels[brewlet.LabelRuntimeReady] = brewlet.ValueReady
			})
			removeOriginalNode(t, f, target.Name)
			replacement := corev1.Node{ObjectMeta: metav1.ObjectMeta{
				Name: uniqueName("replacement"), Labels: map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]},
			}}
			if sameName {
				replacement.Name = target.Name
			}
			if err := f.client.Create(f.ctx, &replacement); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.client.Delete(context.Background(), &replacement) })
			beginLostNodeRetirement(t, f)
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if p.Status.Retirement != nil || includesTarget(p.Status.Targets, target) ||
				len(p.Status.DetachedRetirements) != 1 ||
				!reflect.DeepEqual(p.Status.DetachedRetirements[0].Targets, []nodev1alpha1.NodeTarget{target}) {
				t.Fatal("missing evidence must retain independent history without blocking provisioning")
			}
			var current corev1.Node
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&replacement), &current); err != nil {
				t.Fatal(err)
			}
			if current.Labels[brewlet.LabelNodeIdentity] != string(replacement.UID) {
				t.Fatal("replacement must provision before any evidence")
			}
			e := retirementEvidence(f, target)
			submitEvidence(t, f, e)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			requireEvidenceResolved(t, f, e, target)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if p.Status.Retirement != nil || includesTarget(p.Status.Targets, target) || len(p.Status.DetachedRetirements) != 0 {
				t.Fatal("resolved target was not retired")
			}
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&replacement), &current); err != nil {
				t.Fatal(err)
			}
			if current.UID != replacement.UID || current.Labels[brewlet.LabelNodeIdentity] != string(replacement.UID) {
				t.Fatal("evidence changed the replacement identity")
			}
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&kept), &current); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(current.Labels, kept.Labels) {
				t.Fatal("retained node advertisements changed")
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if len(p.Status.Targets) != 2 || p.Status.Retirement != nil {
				t.Fatalf("replacement provisioning did not resume: %+v", p.Status)
			}
			ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(p.Name))
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&replacement), &current); err != nil {
				t.Fatal(err)
			}
			if !templateMatchesNode(&ds.Spec.Template.Spec, &current) || current.Labels[brewlet.LabelNodeIdentity] != string(replacement.UID) {
				t.Fatal("replacement did not receive its own identity-bound provisioning")
			}
		})
	}
}

func TestExternalRetirementMixedCleanupAndDeletion(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleting-%t", deleting), func(t *testing.T) {
			f := newCleanupFixture(t, 2)
			target := f.profile.Status.Targets[0]
			removeOriginalNode(t, f, target.Name)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if deleting {
				if err := f.client.Delete(f.ctx, &p); err != nil {
					t.Fatal(err)
				}
			} else {
				p.Spec.NodePool.Names = []string{"elsewhere"}
				if err := f.client.Update(f.ctx, &p); err != nil {
					t.Fatal(err)
				}
			}
			beginLostNodeRetirement(t, f)
			e := retirementEvidence(f, target)
			submitEvidence(t, f, e)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if deleting && (len(p.Status.Targets) != 2 || !externallyRetired(p.Status.Targets[0])) {
				t.Fatal("original obligations discarded before surviving host cleanup")
			}
			if !deleting && (len(p.Status.Targets) != 1 || len(p.Status.DetachedRetirements) != 1) {
				t.Fatal("missing host was not separated from ordinary cleanup")
			}
			cleanup := f.daemonSet(t, brewlet.CleanupDaemonSetName(p.Name))
			var survivor corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.profile.Status.Targets[1].Name}, &survivor); err != nil {
				t.Fatal(err)
			}
			if !templateMatchesNode(&cleanup.Spec.Template.Spec, &survivor) {
				t.Fatal("surviving target excluded from real cleanup")
			}
			pod := createDaemonSetPod(t, f.ctx, f.client, cleanup, survivor.Name, true)
			completeCleanupStatus(t, f.ctx, f.client, cleanup, 1)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if deleting && !cleanupCompleted(&p) {
				t.Fatal("mixed cleanup did not checkpoint resolution")
			}
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, cleanup.Namespace, cleanup.Name)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(e), e); err != nil {
				t.Fatal(err)
			}
			if deleting && e.Status.Phase != nodev1alpha1.EvidenceAccepted {
				t.Fatal("remaining cleanup Pod did not hold final resolution")
			}
			if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			if !deleting {
				reconcileProfile(t, f.ctx, f.r, p.Name)
			}
			requireEvidenceResolved(t, f, e, target)
			if deleting {
				err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&p), &p)
				if !apierrors.IsNotFound(err) {
					t.Fatalf("deleting profile did not finalize: %v", err)
				}
			} else {
				p = getProfile(t, f.ctx, f.client, p.Name)
				if p.Status.Retirement != nil || len(p.Status.Targets) != 0 {
					t.Fatal("mixed retirement did not finish")
				}
			}
		})
	}
}

func TestExternalRetirementAllTargetsNeedEvidence(t *testing.T) {
	f := newCleanupFixture(t, 2)
	for _, target := range f.profile.Status.Targets {
		removeOriginalNode(t, f, target.Name)
	}
	beginLostNodeRetirement(t, f)
	first := retirementEvidence(f, f.profile.Status.Targets[0])
	submitEvidence(t, f, first)
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if len(p.Status.DetachedRetirements) != 1 || p.Status.DetachedRetirements[0].Targets[0].UID != f.profile.Status.Targets[1].UID || len(p.Status.Targets) != 0 {
		t.Fatal("partial evidence discarded another unresolved target")
	}
	second := retirementEvidence(f, f.profile.Status.Targets[1])
	submitEvidence(t, f, second)
	f.r = newProfileReconciler(f.client, f.r.Config.Namespace)
	f.r.APIReader = f.client
	reconcileProfile(t, f.ctx, f.r, p.Name)
	requireEvidenceResolved(t, f, first, f.profile.Status.Targets[0])
	requireEvidenceResolved(t, f, second, f.profile.Status.Targets[1])
}

func TestExternalRetirementRejectsUntrustedEvidence(t *testing.T) {
	for _, scenario := range []string{"profile-uid", "node-uid", "host-identity", "original-present", "owner-reference"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			target := f.profile.Status.Targets[0]
			if scenario == "host-identity" {
				p := getProfile(t, f.ctx, f.client, f.profile.Name)
				p.Status.Targets[0].ProviderID = "platform://original"
				if err := f.client.Status().Update(f.ctx, &p); err != nil {
					t.Fatal(err)
				}
				target = p.Status.Targets[0]
			}
			if scenario == "original-present" {
				updateTargetNode(t, f, target.Name, func(n *corev1.Node) { delete(n.Labels, "agentpool") })
				var node corev1.Node
				if err := f.client.Get(f.ctx, types.NamespacedName{Name: target.Name}, &node); err != nil {
					t.Fatal(err)
				}
				node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
				if err := f.client.Status().Update(f.ctx, &node); err != nil {
					t.Fatal(err)
				}
			} else {
				removeOriginalNode(t, f, target.Name)
			}
			beginLostNodeRetirement(t, f)
			e := retirementEvidence(f, target)
			switch scenario {
			case "profile-uid":
				e.Spec.ProfileUID = "another-profile"
			case "node-uid":
				e.Spec.NodeUID = "another-node"
			case "host-identity":
				e.Spec.ProviderID = "platform://replacement"
			case "owner-reference":
				e.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&f.profile, nodev1alpha1.GroupVersion.WithKind("NodeProfile"))}
			}
			submitEvidence(t, f, e)
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if scenario == "original-present" {
				if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked || len(p.Status.DetachedRetirements) != 0 {
					t.Fatal("present NotReady original was detached")
				}
			} else if len(p.Status.DetachedRetirements) != 1 || externallyRetired(p.Status.DetachedRetirements[0].Targets[0]) {
				t.Fatal("invalid evidence resolved original cleanup obligation")
			}
		})
	}
}

func TestExternalRetirementCheckpointFailures(t *testing.T) {
	for _, checkpoint := range []string{"evidence", "receipt", "resolved", "release", "pruned-receipt", "pruned-evidence"} {
		t.Run(checkpoint, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			target := f.profile.Status.Targets[0]
			removeOriginalNode(t, f, target.Name)
			beginLostNodeRetirement(t, f)
			e := retirementEvidence(f, target)
			submitEvidence(t, f, e)
			fired := false
			f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				fail := false
				switch o := obj.(type) {
				case *nodev1alpha1.NodeRetirementEvidence:
					fail = (checkpoint == "evidence" && o.Status.Phase == nodev1alpha1.EvidenceAccepted) ||
						(checkpoint == "resolved" && o.Status.Phase == nodev1alpha1.EvidenceResolved)
					if checkpoint == "pruned-evidence" && !fired {
						stored := o.DeepCopy()
						stored.Status.Obligation = nil
						fired = true
						if err := f.client.Status().Update(ctx, stored, opts...); err != nil {
							return err
						}
						o.ResourceVersion = stored.ResourceVersion
						return nil
					}
				case *nodev1alpha1.NodeProfile:
					fail = (checkpoint == "receipt" && len(o.Status.DetachedRetirements) > 0 && externallyRetired(o.Status.DetachedRetirements[0].Targets[0])) ||
						(checkpoint == "release" && len(o.Status.DetachedRetirements) == 0)
					if checkpoint == "pruned-receipt" && !fired && len(o.Status.DetachedRetirements) > 0 && externallyRetired(o.Status.DetachedRetirements[0].Targets[0]) {
						stored := o.DeepCopy()
						stored.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceName, stored.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceUID = "", ""
						fired = true
						if err := f.client.Status().Update(ctx, stored, opts...); err != nil {
							return err
						}
						o.ResourceVersion = stored.ResourceVersion
						return nil
					}
				}
				if fail && !fired {
					fired = true
					return apierrors.NewConflict(nodev1alpha1.GroupVersion.WithResource("nodeprofiles").GroupResource(), obj.GetName(), fmt.Errorf("injected concurrent writer"))
				}
				return f.client.Status().Update(ctx, obj, opts...)
			}}
			_, _ = f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)})
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if !fired || len(p.Status.DetachedRetirements) != 1 || !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
				t.Fatalf("checkpoint failure discarded obligation: %+v", p.Status)
			}
			f.r = newProfileReconciler(f.client, f.r.Config.Namespace)
			f.r.APIReader = f.client
			if checkpoint == "pruned-evidence" {
				reconcileProfile(t, f.ctx, f.r, p.Name)
				p = getProfile(t, f.ctx, f.client, p.Name)
				if !meta.IsStatusConditionTrue(p.Status.Conditions, nodev1alpha1.ConditionRetirementPending) {
					t.Fatal("damaged accepted record must not be silently reconstructed")
				}
				return
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			requireEvidenceResolved(t, f, e, target)
		})
	}
}

func TestExternalRetirementWaitsForUnlabelledWriter(t *testing.T) {
	f := newCleanupFixture(t, 1)
	target := f.profile.Status.Targets[0]
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	// Simulate an intact pre-change worker: no immutable target-UID fence.
	for i := range ds.Spec.Template.Spec.Containers {
		var env []corev1.EnvVar
		for _, value := range ds.Spec.Template.Spec.Containers[i].Env {
			if value.Name != "BREWLET_TARGET_UIDS" {
				env = append(env, value)
			}
		}
		ds.Spec.Template.Spec.Containers[i].Env = env
	}
	pod := createDaemonSetPod(t, f.ctx, f.client, ds, target.Name, false)
	pod.Labels = nil
	if err := f.client.Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	removeOriginalNode(t, f, target.Name)
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
	e := retirementEvidence(f, target)
	submitEvidence(t, f, e)
	_, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)})
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if externallyRetired(p.Status.Targets[0]) || err == nil || !strings.Contains(err.Error(), "writer pod") || len(p.Status.DetachedRetirements) != 0 {
		t.Fatal("unlabelled writer bypassed recovery teardown barrier")
	}
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	requireEvidenceResolved(t, f, e, target)
}

func TestExternalRetirementConcurrentLifecycleChanges(t *testing.T) {
	for _, scenario := range []string{"spec-edit", "delete", "writer-reappears", "evidence-replaced"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			target := f.profile.Status.Targets[0]
			oldWriter := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			removeOriginalNode(t, f, target.Name)
			beginLostNodeRetirement(t, f)
			e := retirementEvidence(f, target)
			submitEvidence(t, f, e)
			fired := false
			f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if err := f.client.Status().Update(ctx, obj, opts...); err != nil {
					return err
				}
				record, ok := obj.(*nodev1alpha1.NodeRetirementEvidence)
				if !ok || fired {
					return nil
				}
				phase := nodev1alpha1.EvidenceAccepted
				if scenario == "writer-reappears" {
					phase = nodev1alpha1.EvidenceResolved
				}
				if record.Status.Phase != phase {
					return nil
				}
				fired = true
				p := getProfile(t, ctx, f.client, f.profile.Name)
				switch scenario {
				case "spec-edit":
					p.Spec.JDKs[0].Feature = 25
					return f.client.Update(ctx, &p)
				case "delete":
					return f.client.Delete(ctx, &p)
				case "writer-reappears":
					reappeared := buildProfileDaemonSet(f.r.Config, &p, "agentpool", nil)
					reappeared.OwnerReferences = oldWriter.OwnerReferences
					reappeared.Spec = oldWriter.Spec
					var current appsv1.DaemonSet
					if err := f.client.Get(ctx, client.ObjectKeyFromObject(reappeared), &current); err != nil {
						return err
					}
					current.Spec = reappeared.Spec
					return f.client.Update(ctx, &current)
				case "evidence-replaced":
					replacement := record.DeepCopy()
					if err := f.client.Delete(ctx, record); err != nil {
						return err
					}
					replacement.ObjectMeta = metav1.ObjectMeta{Name: record.Name}
					replacement.Status = nodev1alpha1.RetirementEvidenceStatus{}
					return f.client.Create(ctx, replacement)
				}
				return nil
			}}
			_, _ = f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)})
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if !fired || len(p.Status.DetachedRetirements) != 1 {
				t.Fatal("concurrent change released the cleanup obligation")
			}
			if scenario == "evidence-replaced" {
				if externallyRetired(p.Status.DetachedRetirements[0].Targets[0]) {
					t.Fatal("replacement evidence inherited the original record's acceptance")
				}
				return
			}
			f.r = newProfileReconciler(f.client, f.r.Config.Namespace)
			f.r.APIReader = f.client
			if scenario == "writer-reappears" {
				reconcileProfile(t, f.ctx, f.r, p.Name)
				p = getProfile(t, f.ctx, f.client, p.Name)
				if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
					t.Fatal("reappeared old authorization must block new claims")
				}
				var current appsv1.DaemonSet
				if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(oldWriter), &current); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Delete(f.ctx, &current); err != nil {
					t.Fatal(err)
				}
				completeForegroundDaemonSetDeletion(t, f.ctx, f.client, oldWriter.Namespace, oldWriter.Name)
			}
			if scenario == "delete" {
				reconcileProfile(t, f.ctx, f.r, p.Name)
				completeForegroundDaemonSetDeletion(t, f.ctx, f.client, oldWriter.Namespace, oldWriter.Name)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			requireEvidenceResolved(t, f, e, target)
			if scenario == "delete" {
				reconcileProfile(t, f.ctx, f.r, p.Name)
				if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&p), &p); !apierrors.IsNotFound(err) {
					t.Fatalf("concurrent deletion failed to resume safely: %v", err)
				}
			}
		})
	}
}

func TestExternalRetirementDuplicateAndRecreatedEvidence(t *testing.T) {
	f := newCleanupFixture(t, 2)
	for _, target := range f.profile.Status.Targets {
		removeOriginalNode(t, f, target.Name)
	}
	beginLostNodeRetirement(t, f)
	target := f.profile.Status.Targets[0]
	first, duplicate := retirementEvidence(f, target), retirementEvidence(f, target)
	first.Name, duplicate.Name = "a-"+first.Name, "z-"+duplicate.Name
	submitEvidence(t, f, first)
	submitEvidence(t, f, duplicate)
	f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if e, ok := obj.(*nodev1alpha1.NodeRetirementEvidence); ok && e.Status.Phase == nodev1alpha1.EvidenceResolved {
			return fmt.Errorf("hold resolution to inspect committed receipt")
		}
		return f.client.Status().Update(ctx, obj, opts...)
	}}
	_, _ = f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)})
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if p.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceUID != first.UID {
		t.Fatal("duplicate selection was not deterministic")
	}
	if err := f.client.Delete(f.ctx, first); err != nil {
		t.Fatal(err)
	}
	replacement := retirementEvidence(f, target)
	replacement.Name = first.Name
	submitEvidence(t, f, replacement)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if !meta.IsStatusConditionTrue(p.Status.Conditions, nodev1alpha1.ConditionRetirementPending) ||
		p.Status.DetachedRetirements[0].Targets[0].RetirementEvidenceUID != first.UID ||
		!strings.Contains(meta.FindStatusCondition(p.Status.Conditions, nodev1alpha1.ConditionRetirementPending).Message, "evidence UID changed") {
		t.Fatal("recreated evidence or duplicate replaced a committed receipt")
	}
}

func TestExternalRetirementCapturesOriginalHostIdentity(t *testing.T) {
	f := newCleanupFixture(t, 0)
	name := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
	node := updateTargetNode(t, f, name, func(n *corev1.Node) { n.Spec.ProviderID = "platform://original-vm" })
	node.Status.NodeInfo.SystemUUID = "original-system-uuid"
	if err := f.client.Status().Update(f.ctx, &node); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	target := p.Status.Targets[0]
	if target.ProviderID != node.Spec.ProviderID || target.SystemUUID != node.Status.NodeInfo.SystemUUID {
		t.Fatal("original provider identity was not durably captured before authorization")
	}
	removeOriginalNode(t, f, name)
	beginLostNodeRetirement(t, f)
	e := retirementEvidence(f, target)
	submitEvidence(t, f, e)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	requireEvidenceResolved(t, f, e, target)
}

func TestExternalRetirementRefusesReregisteredHost(t *testing.T) {
	f := newCleanupFixture(t, 1)
	target := f.profile.Status.Targets[0]
	removeOriginalNode(t, f, target.Name)
	beginLostNodeRetirement(t, f)
	e := retirementEvidence(f, target)
	e.Spec.SystemUUID = "original-system-uuid"
	submitEvidence(t, f, e)
	name := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	node.Status.NodeInfo.SystemUUID = e.Spec.SystemUUID
	if err := f.client.Status().Update(f.ctx, &node); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if len(p.Status.DetachedRetirements) != 1 || externallyRetired(p.Status.DetachedRetirements[0].Targets[0]) ||
		!strings.Contains(meta.FindStatusCondition(p.Status.Conditions, nodev1alpha1.ConditionRetirementPending).Message, "still registered") {
		t.Fatal("new Node UID was mistaken for proof that the original host was destroyed")
	}
}

func TestExternalRetirementReceiptExcludesScheduling(t *testing.T) {
	p := profileNamed("owner", []string{"pool"}, jdk("temurin", 21))
	p.UID = "profile-uid"
	target := nodev1alpha1.NodeTarget{Name: "old", UID: "node-uid", Claimed: true, RetirementEvidenceUID: "evidence-uid"}
	p.Status.Targets = []nodev1alpha1.NodeTarget{target}
	p.Status.OwnershipInitialized = true
	node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: target.Name, UID: target.UID, Labels: map[string]string{
		"agentpool": "pool", brewlet.LabelNodeOwner: string(p.UID), brewlet.LabelNodeIdentity: string(target.UID),
	}}}
	for _, ds := range []*appsv1.DaemonSet{
		buildProfileDaemonSet(Config{}, &p, "agentpool", nil),
		buildCleanupDaemonSet(Config{}, &p, "agentpool", nil),
	} {
		if templateMatchesNode(&ds.Spec.Template.Spec, &node) {
			t.Fatal("external receipt authorized a host writer")
		}
	}
}

func TestExternalRetirementEvidenceSchema(t *testing.T) {
	f := newCleanupFixture(t, 1)
	target := f.profile.Status.Targets[0]
	for _, scenario := range []string{"empty", "deallocated", "long", "immutable"} {
		t.Run(scenario, func(t *testing.T) {
			e := retirementEvidence(f, target)
			switch scenario {
			case "empty":
				e.Spec.IdentityBinding = " "
			case "deallocated":
				e.Spec.PermanentlyDecommissioned = false
			case "long":
				e.Spec.EvidenceRef = strings.Repeat("x", 2049)
			case "immutable":
				submitEvidence(t, f, e)
				e.Spec.NodeUID = "replacement"
				if err := f.client.Update(f.ctx, e); !apierrors.IsInvalid(err) {
					t.Fatalf("immutable spec update was not rejected: %v", err)
				}
				return
			}
			if err := f.client.Create(f.ctx, e); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid evidence was not rejected: %v", err)
			}
		})
	}
}
