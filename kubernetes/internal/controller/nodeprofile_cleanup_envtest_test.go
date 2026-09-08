// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type cleanupFixture struct {
	client  client.Client
	ctx     context.Context
	r       *NodeProfileReconciler
	profile nodev1alpha1.NodeProfile
	nodes   []string
}

func newCleanupFixture(t *testing.T, nodeCount int) *cleanupFixture {
	t.Helper()
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	r.APIReader = c
	t.Cleanup(func() { cleanupRuntimeClass(c) })
	pool := uniqueName("cleanup-pool")
	var nodes []string
	for i := 0; i < nodeCount; i++ {
		nodes = append(nodes, createNode(t, ctx, c, map[string]string{"agentpool": pool}))
	}
	p := createProfile(t, ctx, c, uniqueName("cleanup-profile"), nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Key: "agentpool", Names: []string{pool}},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	reconcileProfile(t, ctx, r, p.Name)
	return &cleanupFixture{client: c, ctx: ctx, r: r, profile: getProfile(t, ctx, c, p.Name), nodes: nodes}
}

func (f *cleanupFixture) daemonSet(t *testing.T, name string) *appsv1.DaemonSet {
	t.Helper()
	var ds appsv1.DaemonSet
	if err := f.client.Get(f.ctx, types.NamespacedName{Namespace: f.r.Config.Namespace, Name: name}, &ds); err != nil {
		t.Fatal(err)
	}
	return &ds
}

func (f *cleanupFixture) startCleanup(t *testing.T) *appsv1.DaemonSet {
	t.Helper()
	if err := f.client.Delete(f.ctx, &f.profile); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	return f.daemonSet(t, brewlet.CleanupDaemonSetName(f.profile.Name))
}

func (f *cleanupFixture) assertHeld(t *testing.T) {
	t.Helper()
	result := reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	if result.RequeueAfter == 0 {
		t.Fatal("incomplete cleanup must requeue")
	}
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatal("cleanup finalizer released prematurely")
	}
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonCleanupPending {
		t.Fatalf("condition reason = %q, want CleanupPending", reason)
	}
}

func (f *cleanupFixture) assertNoCleanup(t *testing.T) {
	t.Helper()
	var ds appsv1.DaemonSet
	err := f.client.Get(f.ctx, types.NamespacedName{
		Namespace: f.r.Config.Namespace, Name: brewlet.CleanupDaemonSetName(f.profile.Name),
	}, &ds)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("cleanup must not start while provisioning remains: %v", err)
	}
}

func createDaemonSetPod(t *testing.T, ctx context.Context, c client.Client, ds *appsv1.DaemonSet, node string, ready bool) *corev1.Pod {
	t.Helper()
	template := ds.Spec.Template.DeepCopy()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: uniqueName("profile-pod"), Namespace: ds.Namespace,
			Labels: template.Labels, Annotations: template.Annotations,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ds, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))},
		},
		Spec: template.Spec,
	}
	pod.Spec.NodeName = node
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var current corev1.Pod
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &current); err == nil {
			current.Finalizers = nil
			_ = c.Update(context.Background(), &current)
			_ = c.Delete(context.Background(), &current, client.GracePeriodSeconds(0))
		}
	})
	condition := corev1.ConditionFalse
	if ready {
		condition = corev1.ConditionTrue
	}
	pod.Status = corev1.PodStatus{
		Phase:      corev1.PodRunning,
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: condition}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "provisioner", Image: pod.Spec.Containers[0].Image, ImageID: "test-image",
			Ready: ready, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	if err := c.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func completeCleanupStatus(t *testing.T, ctx context.Context, c client.Client, ds *appsv1.DaemonSet, count int32) {
	t.Helper()
	if err := c.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		t.Fatal(err)
	}
	ds.Status = appsv1.DaemonSetStatus{
		ObservedGeneration: ds.Generation, DesiredNumberScheduled: count,
		CurrentNumberScheduled: count, UpdatedNumberScheduled: count,
		NumberReady: count, NumberAvailable: count,
	}
	if err := c.Status().Update(ctx, ds); err != nil {
		t.Fatal(err)
	}
}

func TestNodeProfileDeletionWaitsForRunningAndTerminatingProvisioners(t *testing.T) {
	f := newCleanupFixture(t, 1)
	provisioner := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	pod := createDaemonSetPod(t, f.ctx, f.client, provisioner, f.nodes[0], false)
	if err := f.client.Delete(f.ctx, &f.profile); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	f.assertNoCleanup(t)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, provisioner.Name)
	f.assertHeld(t)
	f.assertNoCleanup(t)

	pod.Finalizers = []string{"test.brewlet.sh/hold"}
	if err := f.client.Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	f.assertNoCleanup(t)
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Finalizers = nil
	if err := f.client.Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	f.daemonSet(t, brewlet.CleanupDaemonSetName(f.profile.Name))
}

func TestNodeProfileDeletionStopsLegacyConcurrentCleanup(t *testing.T) {
	f := newCleanupFixture(t, 1)
	provisioner := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	provisionerPod := createDaemonSetPod(t, f.ctx, f.client, provisioner, f.nodes[0], false)
	cleanup := buildCleanupDaemonSet(f.r.Config, &f.profile, "agentpool", nil)
	cleanup.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&f.profile, nodev1alpha1.GroupVersion.WithKind("NodeProfile"))}
	if err := f.client.Create(f.ctx, cleanup); err != nil {
		t.Fatal(err)
	}
	cleanupPod := createDaemonSetPod(t, f.ctx, f.client, cleanup, f.nodes[0], true)
	if err := f.client.Delete(f.ctx, &f.profile); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	if got := f.daemonSet(t, cleanup.Name); got.DeletionTimestamp.IsZero() {
		t.Fatal("legacy concurrent cleanup was not stopped")
	}
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, provisioner.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, cleanup.Name)
	if err := f.client.Delete(f.ctx, provisionerPod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	f.assertNoCleanup(t)
	if err := f.client.Delete(f.ctx, cleanupPod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	if got := f.daemonSet(t, cleanup.Name); got.UID == cleanup.UID {
		t.Fatal("cleanup must restart only after the old writers are gone")
	}
}

func TestNodeProfileCleanupRejectsIncompleteStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*appsv1.DaemonSetStatus)
	}{
		{"pending", func(s *appsv1.DaemonSetStatus) { *s = appsv1.DaemonSetStatus{} }},
		{"stale-generation", func(s *appsv1.DaemonSetStatus) { s.ObservedGeneration-- }},
		{"partial-assignment", func(s *appsv1.DaemonSetStatus) {
			s.DesiredNumberScheduled = 1
			s.CurrentNumberScheduled = 1
			s.UpdatedNumberScheduled = 1
			s.NumberReady = 1
			s.NumberAvailable = 1
		}},
		{"old-template-ready", func(s *appsv1.DaemonSetStatus) { s.UpdatedNumberScheduled = 1 }},
		{"not-ready", func(s *appsv1.DaemonSetStatus) { s.NumberReady = 1 }},
		{"not-available", func(s *appsv1.DaemonSetStatus) { s.NumberAvailable = 1 }},
		{"unavailable", func(s *appsv1.DaemonSetStatus) { s.NumberUnavailable = 1 }},
		{"misscheduled", func(s *appsv1.DaemonSetStatus) { s.NumberMisscheduled = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCleanupFixture(t, 2)
			ds := f.startCleanup(t)
			for _, node := range f.nodes {
				createDaemonSetPod(t, f.ctx, f.client, ds, node, true)
			}
			completeCleanupStatus(t, f.ctx, f.client, ds, 2)
			tc.mutate(&ds.Status)
			if err := f.client.Status().Update(f.ctx, ds); err != nil {
				t.Fatal(err)
			}
			f.assertHeld(t)
		})
	}
}

func TestNodeProfileCleanupRejectsIncompletePods(t *testing.T) {
	for _, name := range []string{"missing", "pending", "not-ready", "failed", "container-restarting", "terminating", "old-template", "other-owner", "duplicate-node"} {
		t.Run(name, func(t *testing.T) {
			f := newCleanupFixture(t, 2)
			ds := f.startCleanup(t)
			createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
			if name != "missing" {
				node := f.nodes[1]
				if name == "duplicate-node" {
					node = f.nodes[0]
				}
				pod := createDaemonSetPod(t, f.ctx, f.client, ds, node, name != "not-ready")
				switch name {
				case "pending", "failed":
					pod.Status.Phase = corev1.PodFailed
					pod.Status.Conditions = nil
					pod.Status.ContainerStatuses[0].Ready = false
					pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
					}
					if name == "pending" {
						pod.Status.Phase = corev1.PodPending
						pod.Status.ContainerStatuses = nil
					}
					if err := f.client.Status().Update(f.ctx, pod); err != nil {
						t.Fatal(err)
					}
				case "container-restarting":
					pod.Status.ContainerStatuses[0].Ready = false
					pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
					}
					if err := f.client.Status().Update(f.ctx, pod); err != nil {
						t.Fatal(err)
					}
				case "terminating":
					pod.Finalizers = []string{"test.brewlet.sh/hold"}
					if err := f.client.Update(f.ctx, pod); err != nil {
						t.Fatal(err)
					}
					if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
						t.Fatal(err)
					}
				case "old-template", "other-owner":
					if name == "old-template" {
						delete(pod.Annotations, cleanupTemplateAnnotation)
					} else {
						pod.OwnerReferences[0].UID = types.UID("old-daemonset")
					}
					if err := f.client.Update(f.ctx, pod); err != nil {
						t.Fatal(err)
					}
				}
			}
			completeCleanupStatus(t, f.ctx, f.client, ds, 2)
			f.assertHeld(t)
		})
	}
}

func TestNodeProfileCleanupUpgradeWaitsForCurrentTemplate(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.startCleanup(t)
	oldPod := createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	f.r.Config.ProvisionerImage += "-updated"
	f.assertHeld(t)
	ds = f.daemonSet(t, ds.Name)
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	f.assertHeld(t)
	currentPod := createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	f.assertHeld(t)
	if err := f.client.Delete(f.ctx, oldPod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if !cleanupCompleted(&p) {
		t.Fatal("current-template cleanup must checkpoint completion")
	}
	if err := f.client.Delete(f.ctx, currentPod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, ds.Name)
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	var gone nodev1alpha1.NodeProfile
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&f.profile), &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("profile should finalize after current-template cleanup: %v", err)
	}
}

func TestNodeProfileCleanupMissingAssignedNodeBlocks(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.startCleanup(t)
	pod := createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
	if err := f.client.Delete(f.ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: f.nodes[0]}}); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
		t.Fatal("a missing previously claimed node must block cleanup, not count as an empty pool")
	}
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p = getProfile(t, f.ctx, f.client, f.profile.Name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) || len(p.Status.Targets) != 1 {
		t.Fatal("missing node identity and finalizer must be retained for recovery")
	}
	f.daemonSet(t, ds.Name)
}

func TestNodeProfileDeletionPreservesUnownedDaemonSet(t *testing.T) {
	for _, kind := range []string{"provisioner", "cleanup"} {
		t.Run(kind, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			if kind == "cleanup" {
				ds = f.startCleanup(t)
			} else if err := f.client.Delete(f.ctx, &f.profile); err != nil {
				t.Fatal(err)
			}
			ds.OwnerReferences[0].UID = types.UID("previous-profile-incarnation")
			if err := f.client.Update(f.ctx, ds); err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)}); err == nil {
				t.Fatal("unexpected DaemonSet owner must block host cleanup")
			}
			if got := f.daemonSet(t, ds.Name); !got.DeletionTimestamp.IsZero() {
				t.Fatal("DaemonSet owned by another profile incarnation was deleted")
			}
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
				t.Fatal("ownership conflict must preserve cleanup finalizer")
			}
		})
	}
}

func TestNodeProfileDeletionZeroAssignedSkipsCleanup(t *testing.T) {
	f := newCleanupFixture(t, 0)
	if err := f.client.Delete(f.ctx, &f.profile); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	f.assertNoCleanup(t)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	var gone nodev1alpha1.NodeProfile
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&f.profile), &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("empty profile should finalize without cleanup pods: %v", err)
	}
	f.assertNoCleanup(t)
}

func TestNodeProfileDeletionPreservesDaemonSetChangedBeforeDelete(t *testing.T) {
	for _, change := range []string{"ownership", "replacement"} {
		t.Run(change, func(t *testing.T) {
			f := newCleanupFixture(t, 0)
			f.r.Client = interceptDeleteClient{Client: f.client, beforeDelete: func(ctx context.Context, obj client.Object) error {
				var ds appsv1.DaemonSet
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(obj), &ds); err != nil {
					return err
				}
				if change == "ownership" {
					ds.OwnerReferences = nil
					return f.client.Update(ctx, &ds)
				}
				if err := f.client.Delete(ctx, &ds); err != nil {
					return err
				}
				replacement := buildProfileDaemonSet(f.r.Config, &f.profile, "agentpool", nil)
				return f.client.Create(ctx, replacement)
			}}
			if _, err := f.r.deleteProfileDaemonSetIfExists(f.ctx, &f.profile); !apierrors.IsConflict(err) {
				t.Fatalf("delete after concurrent %s change returned %v, want Conflict", change, err)
			}
			if got := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name)); !got.DeletionTimestamp.IsZero() {
				t.Fatal("changed DaemonSet was deleted")
			}
		})
	}
}
