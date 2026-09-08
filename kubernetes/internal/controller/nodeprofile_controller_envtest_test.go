// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strconv"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newProfileReconciler(c client.Client, ns string) *NodeProfileReconciler {
	return &NodeProfileReconciler{
		Client:   c,
		Recorder: record.NewFakeRecorder(100),
		Config: Config{
			Namespace:        ns,
			ProvisionerImage: "ghcr.io/microsoft/brewlet-node-provisioner:test",
		},
	}
}

// createProfile creates a cluster-scoped NodeProfile and registers cleanup that
// clears the finalizer so the object can actually be removed after the test.
func createProfile(t *testing.T, ctx context.Context, c client.Client, name string, spec nodev1alpha1.NodeProfileSpec) *nodev1alpha1.NodeProfile {
	t.Helper()
	p := &nodev1alpha1.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := c.Create(ctx, p); err != nil {
		t.Fatalf("creating profile: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		var cur nodev1alpha1.NodeProfile
		if err := c.Get(bg, types.NamespacedName{Name: name}, &cur); err == nil {
			cur.Finalizers = nil
			_ = c.Update(bg, &cur)
			_ = c.Delete(bg, &cur)
		}
	})
	return p
}

func reconcileProfile(t *testing.T, ctx context.Context, r *NodeProfileReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("Reconcile(profile %s): %v", name, err)
	}
	return res
}

func getProfile(t *testing.T, ctx context.Context, c client.Client, name string) nodev1alpha1.NodeProfile {
	t.Helper()
	var p nodev1alpha1.NodeProfile
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &p); err != nil {
		t.Fatalf("getting profile %s: %v", name, err)
	}
	return p
}

func TestNodeProfileReconcileCreatesDaemonSet(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "cloud.google.com/gke-nodepool"
	node := createNode(t, ctx, c, map[string]string{poolKey: "batch"})
	name := uniqueName("batch")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
		AppCDS:   &nodev1alpha1.AppCDSSpec{RegenerationEnabled: true},
	})

	reconcileProfile(t, ctx, r, name)

	// RuntimeClass ensured.
	var rc nodev1.RuntimeClass
	if err := c.Get(ctx, types.NamespacedName{Name: brewlet.RuntimeClassName}, &rc); err != nil {
		t.Fatalf("RuntimeClass not ensured: %v", err)
	}

	// Per-profile DaemonSet created, owned by the profile.
	var ds appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.ProfileDaemonSetName(name)}, &ds); err != nil {
		t.Fatalf("profile DaemonSet not created: %v", err)
	}
	if len(ds.OwnerReferences) != 1 || ds.OwnerReferences[0].Kind != "NodeProfile" {
		t.Fatalf("DaemonSet owner = %+v, want NodeProfile controller ref", ds.OwnerReferences)
	}
	var regenerationEnv string
	for _, env := range ds.Spec.Template.Spec.Containers[0].Env {
		if env.Name == "BREWLET_APP_CDS_REGENERATION_ENABLED" {
			regenerationEnv = env.Value
		}
	}
	if regenerationEnv != "true" {
		t.Fatalf("BREWLET_APP_CDS_REGENERATION_ENABLED = %q, want true", regenerationEnv)
	}

	// Status: one assigned node, not yet ready -> Provisioning.
	p := getProfile(t, ctx, c, name)
	if p.Status.AssignedNodes != 1 || p.Status.ReadyNodes != 0 {
		t.Fatalf("status assigned/ready = %d/%d, want 1/0", p.Status.AssignedNodes, p.Status.ReadyNodes)
	}
	if p.Status.ResolvedPoolKey != poolKey {
		t.Fatalf("resolvedPoolKey = %q, want %q", p.Status.ResolvedPoolKey, poolKey)
	}
	if r := conditionReason(p.Status.Conditions); r != nodev1alpha1.ReasonProvisioning {
		t.Fatalf("condition reason = %q, want Provisioning", r)
	}

	// Node advertises the runtime -> Ready.
	markNodeReady(t, ctx, c, node, name, p.Generation)
	reconcileProfile(t, ctx, r, name)
	p = getProfile(t, ctx, c, name)
	if p.Status.ReadyNodes != 1 {
		t.Fatalf("readyNodes = %d, want 1", p.Status.ReadyNodes)
	}
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonAllNodesProvisioned {
		t.Fatalf("condition reason = %q, want AllNodesProvisioned", reason)
	}
}

func TestNodeProfileEmptyPoolDegraded(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	// A node exists in a different pool, so the provider key resolves, but the
	// profile names a pool that matches nothing.
	createNode(t, ctx, c, map[string]string{"cloud.google.com/gke-nodepool": "general"})
	name := uniqueName("typo")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"does-not-exist"}, Key: "cloud.google.com/gke-nodepool"},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})

	reconcileProfile(t, ctx, r, name)

	p := getProfile(t, ctx, c, name)
	if p.Status.AssignedNodes != 0 {
		t.Fatalf("assignedNodes = %d, want 0", p.Status.AssignedNodes)
	}
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonEmptyPool {
		t.Fatalf("condition reason = %q, want EmptyPool", reason)
	}
}

func TestNodeProfileCRDAcceptsIPv6RuntimeSources(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	name := uniqueName("ipv6-sources")
	profile := nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: nodev1alpha1.NodeProfileSpec{
			JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "custom",
				Feature:      21,
				Source: nodev1alpha1.JDKSource{
					Image:    "[2001:db8::1]:5000/java/runtime@" + testImageDigest,
					JavaHome: "/opt/jdk",
				},
			}},
			Launchers: []nodev1alpha1.LauncherRef{{
				Name: "custom",
				Source: nodev1alpha1.LauncherSource{
					Image: "[2001:db8::2]/java/launcher@" + testImageDigest,
					Path:  "/usr/bin/custom",
				},
			}},
			Registry: &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{
				"docker.io": "[2001:db8::3]:5000/cache",
			}},
		},
	}
	if err := c.Create(ctx, &profile); err != nil {
		t.Fatalf("creating profile with IPv6 runtime sources: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), &profile) })
}

func TestNodeProfileInvalidUpdateWithdrawsProvisioning(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	r.Config.AllowedSourceMirrorHosts = []string{"registry.internal"}
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "cloud.google.com/gke-nodepool"
	nodeName := createNode(t, ctx, c, map[string]string{poolKey: "batch"})
	name := uniqueName("invalid-source")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	reconcileProfile(t, ctx, r, name)

	p := getProfile(t, ctx, c, name)
	markNodeReady(t, ctx, c, nodeName, name, p.Generation)
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("getting node: %v", err)
	}
	patch := client.MergeFrom(node.DeepCopy())
	node.Labels[brewlet.LabelJDKPrefix+"temurin-21"] = "true"
	node.Labels[brewlet.LabelJDKFeaturePrefix+"21"] = "true"
	node.Labels[brewlet.LabelLauncherPrefix+"jaz"] = "true"
	node.Annotations[brewlet.AnnotationJDKs] = "temurin-21"
	node.Annotations[brewlet.AnnotationJDKsInfo] = `{"jdks":[]}`
	node.Annotations[brewlet.AnnotationLaunchers] = "java,jaz"
	if err := c.Patch(ctx, &node, patch); err != nil {
		t.Fatalf("adding node capabilities: %v", err)
	}

	p = getProfile(t, ctx, c, name)
	p.Spec.Registry = &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{
		"docker.io": "unapproved.example/cache",
	}}
	if err := c.Update(ctx, &p); err != nil {
		t.Fatalf("updating profile with bypassed invalid policy: %v", err)
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      uniqueName("invalid-profile-provisioner"),
			Namespace: ns,
			Labels:    profileLabels(name),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "provisioner",
			Image: "ghcr.io/microsoft/brewlet-node-provisioner:test",
		}}},
	}
	if err := c.Create(ctx, &pod); err != nil {
		t.Fatalf("creating lingering provisioner pod: %v", err)
	}
	result := reconcileProfile(t, ctx, r, name)
	if result.RequeueAfter == 0 {
		t.Fatal("expected invalid profile reconciliation to wait for provisioner termination")
	}

	var ds appsv1.DaemonSet
	err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.ProfileDaemonSetName(name)}, &ds)
	if err == nil && ds.DeletionTimestamp.IsZero() {
		t.Fatal("invalid profile DaemonSet deletion did not use foreground propagation")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("getting deleting invalid profile DaemonSet: %v", err)
	}
	p = getProfile(t, ctx, c, name)
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonInvalidProfile {
		t.Fatalf("condition reason = %q, want InvalidProfile", reason)
	}
	if p.Status.ReadyNodes != 0 {
		t.Fatalf("readyNodes = %d, want 0", p.Status.ReadyNodes)
	}
	completeForegroundDaemonSetDeletion(t, ctx, c, ns, brewlet.ProfileDaemonSetName(name))
	markNodeReady(t, ctx, c, nodeName, name, p.Generation)
	result = reconcileProfile(t, ctx, r, name)
	if result.RequeueAfter == 0 {
		t.Fatal("expected invalid profile reconciliation to wait while a provisioner pod remains")
	}
	markNodeReady(t, ctx, c, nodeName, name, p.Generation)
	if err := c.Delete(ctx, &pod); err != nil {
		t.Fatalf("deleting lingering provisioner pod: %v", err)
	}
	reconcileProfile(t, ctx, r, name)
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("getting withdrawn node: %v", err)
	}
	for _, key := range []string{
		brewlet.LabelRuntimeReady,
		brewlet.LabelJDKPrefix + "temurin-21",
		brewlet.LabelJDKFeaturePrefix + "21",
		brewlet.LabelLauncherPrefix + "jaz",
	} {
		if _, exists := node.Labels[key]; exists {
			t.Errorf("node still advertises %s", key)
		}
	}
	for _, key := range []string{
		brewlet.AnnotationJDKs,
		brewlet.AnnotationJDKsInfo,
		brewlet.AnnotationLaunchers,
		brewlet.AnnotationProfile,
		brewlet.AnnotationProfileGeneration,
	} {
		if _, exists := node.Annotations[key]; exists {
			t.Errorf("node still carries %s", key)
		}
	}
}

func TestNeverValidNodeProfileHasNoCleanupFinalizer(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	r.Config.AllowedSourceMirrorHosts = []string{"registry.internal"}

	name := uniqueName("never-valid")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 21)},
		Registry: &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{
			"docker.io": "unapproved.example/cache",
		}},
	})
	reconcileProfile(t, ctx, r, name)

	p := getProfile(t, ctx, c, name)
	if containsString(p.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("never-valid profile received cleanup finalizer: %v", p.Finalizers)
	}
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonInvalidProfile {
		t.Fatalf("condition reason = %q, want InvalidProfile", reason)
	}
	if err := c.Delete(ctx, &p); err != nil {
		t.Fatalf("deleting never-valid profile: %v", err)
	}

	var cleanup appsv1.DaemonSet
	err := c.Get(ctx, types.NamespacedName{
		Namespace: ns,
		Name:      brewlet.CleanupDaemonSetName(name),
	}, &cleanup)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("never-valid profile created cleanup DaemonSet: %v", err)
	}
}

func TestDeletingPoolConflictingProfileSkipsHostCleanup(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "cloud.google.com/gke-nodepool"
	createNode(t, ctx, c, map[string]string{poolKey: "second"})
	firstName := uniqueName("first-profile")
	secondName := uniqueName("second-profile")
	createProfile(t, ctx, c, firstName, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"first"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	createProfile(t, ctx, c, secondName, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"second"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	reconcileProfile(t, ctx, r, firstName)
	reconcileProfile(t, ctx, r, secondName)

	first := getProfile(t, ctx, c, firstName)
	first.Spec.NodePool.Names = []string{"second"}
	if err := c.Update(ctx, &first); err != nil {
		t.Fatalf("updating first profile to conflicting pool: %v", err)
	}
	reconcileProfile(t, ctx, r, firstName)

	first = getProfile(t, ctx, c, firstName)
	if reason := conditionReason(first.Status.Conditions); reason != nodev1alpha1.ReasonInvalidProfile {
		t.Fatalf("condition reason = %q, want InvalidProfile", reason)
	}
	if !containsString(first.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("previously valid profile lost cleanup finalizer before deletion: %v", first.Finalizers)
	}
	if err := c.Delete(ctx, &first); err != nil {
		t.Fatalf("deleting conflicting profile: %v", err)
	}
	reconcileProfile(t, ctx, r, firstName)

	var cleanup appsv1.DaemonSet
	err := c.Get(ctx, types.NamespacedName{
		Namespace: ns,
		Name:      brewlet.CleanupDaemonSetName(firstName),
	}, &cleanup)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("conflicting profile created cleanup DaemonSet: %v", err)
	}
	var secondDS appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{
		Namespace: ns,
		Name:      brewlet.ProfileDaemonSetName(secondName),
	}, &secondDS); err != nil {
		t.Fatalf("valid profile DaemonSet was removed: %v", err)
	}
	completeForegroundDaemonSetDeletion(t, ctx, c, ns, brewlet.ProfileDaemonSetName(firstName))
	reconcileProfile(t, ctx, r, firstName)
	var gone nodev1alpha1.NodeProfile
	if err := c.Get(ctx, types.NamespacedName{Name: firstName}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("unprovisioned invalid profile should finalize without cleanup: %v", err)
	}
}

func TestDeletingNewlyConflictingProfileWaitsForProvisionerTermination(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "cloud.google.com/gke-nodepool"
	nodeName := createNode(t, ctx, c, map[string]string{poolKey: "first"})
	firstName := uniqueName("draining-profile")
	secondName := uniqueName("valid-profile")
	createProfile(t, ctx, c, firstName, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"first"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	createProfile(t, ctx, c, secondName, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"second"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	reconcileProfile(t, ctx, r, firstName)
	reconcileProfile(t, ctx, r, secondName)

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      uniqueName("old-provisioner"),
			Namespace: ns,
			Labels:    profileLabels(firstName),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "provisioner",
			Image: "ghcr.io/microsoft/brewlet-node-provisioner:test",
		}}},
	}
	if err := c.Create(ctx, &pod); err != nil {
		t.Fatalf("creating old provisioner pod: %v", err)
	}

	first := getProfile(t, ctx, c, firstName)
	first.Spec.NodePool.Names = []string{"second"}
	if err := c.Update(ctx, &first); err != nil {
		t.Fatalf("updating first profile to conflicting pool: %v", err)
	}
	first = getProfile(t, ctx, c, firstName)
	if err := c.Delete(ctx, &first); err != nil {
		t.Fatalf("deleting newly conflicting profile: %v", err)
	}

	result := reconcileProfile(t, ctx, r, firstName)
	if result.RequeueAfter == 0 {
		t.Fatal("expected invalid deletion to wait after stopping the provisioner DaemonSet")
	}
	first = getProfile(t, ctx, c, firstName)
	if !containsString(first.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("finalizer released before provisioner termination: %v", first.Finalizers)
	}
	completeForegroundDaemonSetDeletion(t, ctx, c, ns, brewlet.ProfileDaemonSetName(firstName))
	var cleanup appsv1.DaemonSet
	err := c.Get(ctx, types.NamespacedName{
		Namespace: ns,
		Name:      brewlet.CleanupDaemonSetName(firstName),
	}, &cleanup)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("newly conflicting profile created cleanup DaemonSet: %v", err)
	}

	result = reconcileProfile(t, ctx, r, firstName)
	if result.RequeueAfter == 0 {
		t.Fatal("expected invalid deletion to wait while an old provisioner pod remains")
	}
	first = getProfile(t, ctx, c, firstName)
	if !containsString(first.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("finalizer released while provisioner pod remained: %v", first.Finalizers)
	}
	if err := c.Delete(ctx, &pod); err != nil {
		t.Fatalf("deleting old provisioner pod: %v", err)
	}
	first = getProfile(t, ctx, c, firstName)
	markNodeReady(t, ctx, c, nodeName, firstName, first.Generation)

	reconcileProfile(t, ctx, r, firstName)
	first = getProfile(t, ctx, c, firstName)
	if !containsString(first.Finalizers, brewlet.FinalizerCleanup) ||
		conditionReason(first.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
		t.Fatalf("dirty invalid profile must remain blocked after worker termination: %+v", first.Status)
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatalf("getting node after invalid deletion: %v", err)
	}
	if _, exists := node.Labels[brewlet.LabelRuntimeReady]; exists {
		t.Fatal("late provisioner advertisement remained during blocked deletion")
	}
	if _, exists := node.Annotations[brewlet.AnnotationProfile]; exists {
		t.Fatal("late provisioner advertisement remained during blocked deletion")
	}
	if node.Labels[brewlet.LabelNodeOwner] != string(first.UID) {
		t.Fatal("dirty invalid profile lost its node ownership claim")
	}
}

func TestNodeProfileFinalizerBlocksGC(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	t.Cleanup(func() { cleanupRuntimeClass(c) })

	poolKey := "cloud.google.com/gke-nodepool"
	node := createNode(t, ctx, c, map[string]string{poolKey: "batch"})
	name := uniqueName("reversal")
	createProfile(t, ctx, c, name, nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Names: []string{"batch"}, Key: poolKey},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})

	// First reconcile adds the finalizer + the provisioner DaemonSet.
	reconcileProfile(t, ctx, r, name)
	p := getProfile(t, ctx, c, name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("finalizer not added: %v", p.Finalizers)
	}

	// Stop the provisioner before creating the cleanup DaemonSet.
	if err := c.Delete(ctx, &p); err != nil {
		t.Fatalf("deleting profile: %v", err)
	}
	reconcileProfile(t, ctx, r, name)
	completeForegroundDaemonSetDeletion(t, ctx, c, ns, brewlet.ProfileDaemonSetName(name))
	reconcileProfile(t, ctx, r, name)

	p = getProfile(t, ctx, c, name) // still present: finalizer blocks GC
	if p.DeletionTimestamp.IsZero() {
		t.Fatal("expected deletionTimestamp to be set")
	}
	var cleanup appsv1.DaemonSet
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.CleanupDaemonSetName(name)}, &cleanup); err != nil {
		t.Fatalf("cleanup DaemonSet not created: %v", err)
	}

	// Simulate the cleanup DaemonSet finishing on its node.
	pod := createDaemonSetPod(t, ctx, c, &cleanup, node, true)
	completeCleanupStatus(t, ctx, c, &cleanup, 1)

	// Completion is checkpointed, but the finalizer still protects teardown.
	reconcileProfile(t, ctx, r, name)
	p = getProfile(t, ctx, c, name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) || !cleanupCompleted(&p) {
		t.Fatal("cleanup completion must persist before releasing the finalizer")
	}
	if err := c.Delete(ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	completeForegroundDaemonSetDeletion(t, ctx, c, ns, cleanup.Name)

	// Once foreground GC has removed all cleanup workers, finalization is safe.
	reconcileProfile(t, ctx, r, name)
	var gone nodev1alpha1.NodeProfile
	err := c.Get(ctx, types.NamespacedName{Name: name}, &gone)
	if err == nil && !gone.DeletionTimestamp.IsZero() && len(gone.Finalizers) == 0 {
		// finalizer removed; apiserver will finalize deletion — acceptable.
	} else if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("unexpected error getting profile after cleanup: %v", err)
	} else if err == nil && containsString(gone.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("finalizer still present after cleanup completed: %v", gone.Finalizers)
	}
}

func completeForegroundDaemonSetDeletion(
	t *testing.T,
	ctx context.Context,
	c client.Client,
	namespace string,
	name string,
) {
	t.Helper()
	var ds appsv1.DaemonSet
	key := types.NamespacedName{Namespace: namespace, Name: name}
	if err := c.Get(ctx, key, &ds); apierrors.IsNotFound(err) {
		return
	} else if err != nil {
		t.Fatalf("getting foreground-deleting DaemonSet %s: %v", name, err)
	}
	if ds.DeletionTimestamp.IsZero() {
		t.Fatalf("DaemonSet %s is not pending foreground deletion", name)
	}

	base := ds.DeepCopy()
	ds.Finalizers = nil
	if err := c.Patch(ctx, &ds, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("simulating foreground garbage collection for DaemonSet %s: %v", name, err)
	}
}

func markNodeReady(t *testing.T, ctx context.Context, c client.Client, name, profile string, generation int64) {
	t.Helper()
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatalf("getting node: %v", err)
	}
	patch := client.MergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Labels[brewlet.LabelRuntimeReady] = brewlet.ValueReady
	node.Annotations[brewlet.AnnotationProfile] = profile
	node.Annotations[brewlet.AnnotationProfileGeneration] = strconv.FormatInt(generation, 10)
	if err := c.Patch(ctx, &node, patch); err != nil {
		t.Fatalf("marking node ready: %v", err)
	}
}

func conditionReason(conds []metav1.Condition) string {
	for _, c := range conds {
		if c.Type == nodev1alpha1.ConditionReady {
			return c.Reason
		}
	}
	return ""
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

var nameCounter int

func uniqueName(prefix string) string {
	nameCounter++
	return prefix + "-" + itoaTest(nameCounter)
}

func itoaTest(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
