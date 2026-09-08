// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"strings"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func invalidateAndDeleteProfile(t *testing.T, f *cleanupFixture) {
	t.Helper()
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Spec.Registry = &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{"docker.io": "unapproved.example/cache"}}
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(p.Name))
}

func assertInvalidCleanupBlocked(t *testing.T, f *cleanupFixture) nodev1alpha1.NodeProfile {
	t.Helper()
	result := reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if result.RequeueAfter == 0 || !containsString(p.Finalizers, brewlet.FinalizerCleanup) ||
		conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
		t.Fatalf("invalid owned profile must remain blocked, not disappear: %+v", p.Status)
	}
	if !strings.Contains(p.Status.Conditions[0].Message, "repair") {
		t.Fatalf("cleanup status must explain the repair requirement: %+v", p.Status.Conditions)
	}
	f.assertNoCleanup(t)
	return p
}

func TestNodeProfileInvalidDeletingOwnershipRequiresRepairAndCleanup(t *testing.T) {
	f := newCleanupFixture(t, 1)
	markNodeReady(t, f.ctx, f.client, f.nodes[0], f.profile.Name, f.profile.Generation)
	updateTargetNode(t, f, f.nodes[0], func(n *corev1.Node) { delete(n.Labels, "agentpool") })
	invalidateAndDeleteProfile(t, f)
	p := assertInvalidCleanupBlocked(t, f)
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != string(p.UID) || node.Labels[brewlet.LabelRuntimeReady] != "" {
		t.Fatal("invalid deletion must retain ownership but withdraw readiness")
	}
	// Repairing a deleting profile is allowed. Its recorded old target remains
	// authoritative even though that node no longer matches today's pool labels.
	p.Spec.Registry = nil
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	ds := f.daemonSet(t, brewlet.CleanupDaemonSetName(p.Name))
	pod := createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	f.assertTeardownHeld(t)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.assertFinalized(t)
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != "" {
		t.Fatal("claim should release only after repaired profile completes host cleanup and teardown")
	}
}

func TestNodeProfileInvalidDeletingLiveClaimCannotHideBehindMissingLedger(t *testing.T) {
	f := newCleanupFixture(t, 1)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Status.Targets = nil
	if err := f.client.Status().Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	invalidateAndDeleteProfile(t, f)
	p = assertInvalidCleanupBlocked(t, f)
	p.Spec.Registry = nil
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) ||
		!strings.Contains(p.Status.Conditions[0].Message, "restore its original target") {
		t.Fatal("source repair must not hide an orphaned live node claim")
	}
	f.assertNoCleanup(t)
}

func TestNodeProfileInvalidDeletingUnclaimedIntentCanFinalize(t *testing.T) {
	f := newCleanupFixture(t, 0)
	name := createNode(t, f.ctx, f.client, nil)
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if hasProvisioningHistory(&p) {
		t.Fatal("a fresh zero-target profile must not record provisioning history")
	}
	p.Status.Targets = []nodev1alpha1.NodeTarget{{Name: name, UID: node.UID}}
	if err := f.client.Status().Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	invalidateAndDeleteProfile(t, f)
	f.assertFinalized(t)
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != "" {
		t.Fatal("unprovisioned invalid deletion must not touch or claim the intended target")
	}
}

func TestNodeProfileInvalidDeletingOrphanedProvisioningHistoryCannotFinalize(t *testing.T) {
	for _, evidence := range []string{"saved-policy", "saved-generation"} {
		t.Run(evidence, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			p.Status.Targets = nil
			p.Status.Conditions = nil
			if evidence == "saved-policy" {
				p.Status.ProvisioningGeneration = 0
			} else {
				p.Status.ProvisioningSpec = nil
			}
			if err := f.client.Status().Update(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			var old corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &old); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Delete(f.ctx, &old); err != nil {
				t.Fatal(err)
			}
			invalidateAndDeleteProfile(t, f)
			p = assertInvalidCleanupBlocked(t, f)
			// A source repair and a reconciler restart cannot manufacture the
			// missing host-cleanup ledger from the current selector.
			p.Spec.Registry = nil
			if err := f.client.Update(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			f.r = newProfileReconciler(f.client, f.r.Config.Namespace)
			f.r.APIReader = f.client
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if !containsString(p.Finalizers, brewlet.FinalizerCleanup) ||
				conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked ||
				!strings.Contains(p.Status.Conditions[0].Message, "restore its original target") {
				t.Fatal("orphaned provisioning history must retain the finalizer until its cleanup records are restored")
			}
			f.assertNoCleanup(t)
		})
	}
}
