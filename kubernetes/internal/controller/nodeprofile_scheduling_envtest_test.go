// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Placement and membership are the same security boundary seen from two sides:
// if the reconciler counted a control-plane node it can never provision, the
// profile would never reach Ready and an operator would be tempted to widen the
// affinity back out. These tests pin both halves against a real API server.

func TestNodeProfileExcludesControlPlaneNode(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "agentpool"
	worker := createNode(t, ctx, c, map[string]string{poolKey: "general"})
	// Same pool label, but it is the control plane — kind and Docker Desktop
	// label it without tainting it, so the label is the only signal.
	createNode(t, ctx, c, map[string]string{
		poolKey:                           "general",
		brewlet.ControlPlaneRoleLabels[0]: "",
	})

	name := uniqueName("cp-excluded")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"general"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	reconcileProfile(t, ctx, r, name)

	p := getProfile(t, ctx, c, name)
	if p.Status.AssignedNodes != 1 {
		t.Fatalf("assignedNodes = %d, want 1 (the control-plane node must not count)", p.Status.AssignedNodes)
	}

	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.ProfileDaemonSetName(name)}, &ds); err != nil {
		t.Fatalf("profile DaemonSet not created: %v", err)
	}
	for _, tol := range ds.Spec.Template.Spec.Tolerations {
		if tol.Key == "" && tol.Operator == corev1.TolerationOpExists {
			t.Fatalf("DaemonSet carries a blanket toleration: %+v", tol)
		}
	}
	assertExcludesControlPlane(t, "persisted DaemonSet", ds.Spec.Template.Spec.Affinity)

	// The excluded node must not hold the profile short of Ready either.
	markNodeReady(t, ctx, c, worker, name, p.Generation)
	reconcileProfile(t, ctx, r, name)
	p = getProfile(t, ctx, c, name)
	if p.Status.ReadyNodes != 1 {
		t.Fatalf("readyNodes = %d, want 1", p.Status.ReadyNodes)
	}
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonAllNodesProvisioned {
		t.Fatalf("condition reason = %q, want AllNodesProvisioned", reason)
	}
}

func TestNodeProfileIncludeControlPlaneOptIn(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "agentpool"
	createNode(t, ctx, c, map[string]string{
		poolKey:                           "single",
		brewlet.ControlPlaneRoleLabels[0]: "",
	})

	name := uniqueName("cp-optin")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{
			Names:               []string{"single"},
			Key:                 poolKey,
			IncludeControlPlane: true,
		},
		JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 21)},
		Tolerations: []corev1.Toleration{{
			Key:      brewlet.ControlPlaneRoleLabels[0],
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}},
	})
	reconcileProfile(t, ctx, r, name)

	// The CRD must round-trip both opt-ins, and they must reach the pod spec.
	p := getProfile(t, ctx, c, name)
	if !p.Spec.NodePool.IncludeControlPlane {
		t.Fatal("includeControlPlane did not survive the CRD schema")
	}
	if p.Status.AssignedNodes != 1 {
		t.Fatalf("assignedNodes = %d, want 1", p.Status.AssignedNodes)
	}

	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.ProfileDaemonSetName(name)}, &ds); err != nil {
		t.Fatalf("profile DaemonSet not created: %v", err)
	}
	if keys := exclusionKeys(ds.Spec.Template.Spec.Affinity); len(keys) != 0 {
		t.Fatalf("opted-in profile still excludes %v", keys)
	}
	tols := ds.Spec.Template.Spec.Tolerations
	if len(tols) != 1 || tols[0].Key != brewlet.ControlPlaneRoleLabels[0] {
		t.Fatalf("tolerations = %+v, want exactly the declared control-plane toleration", tols)
	}
}
