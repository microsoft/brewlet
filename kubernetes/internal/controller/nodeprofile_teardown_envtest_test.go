// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (f *cleanupFixture) assertTeardownHeld(t *testing.T) {
	t.Helper()
	result := reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if result.RequeueAfter == 0 || !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatal("profile must retain its finalizer and requeue during cleanup worker teardown")
	}
	checkpoint := meta.FindStatusCondition(p.Status.Conditions, nodev1alpha1.ConditionCleanupComplete)
	if checkpoint == nil || checkpoint.Status != metav1.ConditionTrue ||
		checkpoint.Reason != nodev1alpha1.ReasonCleanupSucceeded ||
		checkpoint.ObservedGeneration != p.Generation {
		t.Fatalf("current-generation cleanup checkpoint missing: %+v", checkpoint)
	}
	if reason := conditionReason(p.Status.Conditions); reason != nodev1alpha1.ReasonCleanupTeardown {
		t.Fatalf("Ready reason = %q, want CleanupTeardown", reason)
	}
}

func (f *cleanupFixture) assertFinalized(t *testing.T) {
	t.Helper()
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	var p nodev1alpha1.NodeProfile
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&f.profile), &p); !apierrors.IsNotFound(err) {
		t.Fatalf("profile must finalize after worker teardown: %v", err)
	}
	f.assertNoCleanup(t)
}

func (f *cleanupFixture) startTeardown(t *testing.T) (*appsv1.DaemonSet, *corev1.Pod) {
	t.Helper()
	ds := f.startCleanup(t)
	pod := createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	f.assertTeardownHeld(t)
	return ds, pod
}

func TestNodeProfileCleanupTeardownWaitsForDaemonSetAndPods(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.startCleanup(t)
	pod := createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	pod.Finalizers = []string{"test.brewlet.sh/hold"}
	if err := f.client.Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	ds.Finalizers = []string{"test.brewlet.sh/hold"}
	if err := f.client.Update(f.ctx, ds); err != nil {
		t.Fatal(err)
	}
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	f.assertTeardownHeld(t)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
	f.assertTeardownHeld(t)
	f.assertNoCleanup(t)
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.assertTeardownHeld(t)
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Finalizers = nil
	if err := f.client.Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.assertFinalized(t)
}

func TestNodeProfileCleanupTeardownWaitsForDaemonSetAfterPodsGone(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds, pod := f.startTeardown(t)
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.assertTeardownHeld(t)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
	f.assertFinalized(t)
}

type staleProfileClient struct {
	client.Client
	profile nodev1alpha1.NodeProfile
}

func (c staleProfileClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if profile, ok := obj.(*nodev1alpha1.NodeProfile); ok && key.Name == c.profile.Name {
		c.profile.DeepCopyInto(profile)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestNodeProfileCleanupTeardownSurvivesRestartWithStaleCache(t *testing.T) {
	for _, cachedPhase := range []string{"before-deletion", "before-checkpoint"} {
		t.Run(cachedPhase, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			stale := f.profile
			ds := f.startCleanup(t)
			if cachedPhase == "before-checkpoint" {
				stale = getProfile(t, f.ctx, f.client, f.profile.Name)
			}
			pod := createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
			completeCleanupStatus(t, f.ctx, f.client, ds, 1)
			f.assertTeardownHeld(t)
			f.r = newProfileReconciler(staleProfileClient{Client: f.client, profile: stale}, ds.Namespace)
			f.r.APIReader = f.client
			f.r.Config.ProvisionerImage += "-new-operator"
			f.assertTeardownHeld(t)
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
			f.assertTeardownHeld(t)
			f.assertNoCleanup(t)
			if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			f.assertFinalized(t)
		})
	}
}

type interceptProfileStatusClient struct {
	client.Client
	update func(context.Context, client.Object, ...client.SubResourceUpdateOption) error
}

func (c interceptProfileStatusClient) Status() client.SubResourceWriter {
	return interceptProfileStatusWriter{SubResourceWriter: c.Client.Status(), update: c.update}
}

type interceptProfileStatusWriter struct {
	client.SubResourceWriter
	update func(context.Context, client.Object, ...client.SubResourceUpdateOption) error
}

func (w interceptProfileStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	return w.update(ctx, obj, opts...)
}

func TestNodeProfileCleanupCheckpointWriteFailuresBlockTeardown(t *testing.T) {
	for _, persistBeforeError := range []bool{false, true} {
		name := "rejected-write"
		if persistBeforeError {
			name = "lost-write-acknowledgement"
		}
		t.Run(name, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			ds := f.startCleanup(t)
			createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
			completeCleanupStatus(t, f.ctx, f.client, ds, 1)
			writeErr := errors.New("checkpoint write failed")
			f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if cleanupCompleted(obj.(*nodev1alpha1.NodeProfile)) {
					if persistBeforeError {
						if err := f.client.Status().Update(ctx, obj, opts...); err != nil {
							return err
						}
					}
					return writeErr
				}
				return f.client.Status().Update(ctx, obj, opts...)
			}}
			if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)}); !errors.Is(err, writeErr) {
				t.Fatalf("checkpoint error must propagate: %v", err)
			}
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if cleanupCompleted(&p) != persistBeforeError || !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
				t.Fatal("unexpected persisted checkpoint/finalizer after failed status update")
			}
			if got := f.daemonSet(t, ds.Name); !got.DeletionTimestamp.IsZero() {
				t.Fatal("cleanup DaemonSet was deleted before checkpoint acknowledgement")
			}
			f.r = newProfileReconciler(f.client, ds.Namespace)
			f.r.APIReader = f.client
			if persistBeforeError {
				ds.Status.NumberReady = 0
				if err := f.client.Status().Update(f.ctx, ds); err != nil {
					t.Fatal(err)
				}
			}
			f.assertTeardownHeld(t)
		})
	}
}

func TestNodeProfileCleanupCheckpointSurvivesTeardownFailure(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.startCleanup(t)
	createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	deleteErr := errors.New("cleanup deletion failed")
	f.r.Client = interceptDeleteClient{Client: f.client, beforeDelete: func(ctx context.Context, obj client.Object) error {
		p := getProfile(t, ctx, f.client, f.profile.Name)
		if !cleanupCompleted(&p) {
			t.Fatal("cleanup deletion attempted before persisting completion")
		}
		return deleteErr
	}}
	if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)}); !errors.Is(err, deleteErr) {
		t.Fatalf("expected teardown error: %v", err)
	}
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if !cleanupCompleted(&p) {
		t.Fatal("cleanup checkpoint lost after deletion failure")
	}
	f.r = newProfileReconciler(f.client, ds.Namespace)
	f.r.APIReader = f.client
	f.assertTeardownHeld(t)
}

func TestNodeProfileCleanupPendingStatusErrorsBlockWriterDeletion(t *testing.T) {
	f := newCleanupFixture(t, 1)
	if err := f.client.Delete(f.ctx, &f.profile); err != nil {
		t.Fatal(err)
	}
	writeErr := errors.New("pending status write failed")
	f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
		return writeErr
	}}
	if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)}); !errors.Is(err, writeErr) {
		t.Fatalf("pending status error must propagate: %v", err)
	}
	if ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name)); !ds.DeletionTimestamp.IsZero() {
		t.Fatal("provisioner deletion must wait for pending status to persist")
	}
	f.assertNoCleanup(t)
}

func TestNodeProfileCleanupCheckpointInvalidatedBeforeReappearedWritersStop(t *testing.T) {
	for _, kind := range []string{"daemonset", "orphan-pod"} {
		t.Run(kind, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			cleanup, cleanupPod := f.startTeardown(t)
			provisioner := buildProfileDaemonSet(f.r.Config, &f.profile, "agentpool", nil)
			provisioner.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&f.profile, nodev1alpha1.GroupVersion.WithKind("NodeProfile"))}
			if err := f.client.Create(f.ctx, provisioner); err != nil {
				t.Fatal(err)
			}
			pod := createDaemonSetPod(t, f.ctx, f.client, provisioner, f.nodes[0], false)
			if kind == "orphan-pod" {
				if err := f.client.Delete(f.ctx, provisioner); err != nil {
					t.Fatal(err)
				}
			}
			writeErr := errors.New("checkpoint invalidation failed")
			f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
				return writeErr
			}}
			if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)}); !errors.Is(err, writeErr) {
				t.Fatalf("checkpoint invalidation failure must propagate: %v", err)
			}
			if kind == "daemonset" && !f.daemonSet(t, provisioner.Name).DeletionTimestamp.IsZero() {
				t.Fatal("writer deleted before completion checkpoint could be invalidated")
			}
			f.r.Client = f.client
			f.assertHeld(t)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if meta.FindStatusCondition(p.Status.Conditions, nodev1alpha1.ConditionCleanupComplete) != nil {
				t.Fatal("reappeared provisioner must invalidate cleanup completion")
			}
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, cleanup.Namespace, cleanup.Name)
			if kind == "daemonset" {
				completeForegroundDaemonSetDeletion(t, f.ctx, f.client, provisioner.Namespace, provisioner.Name)
			}
			for _, writer := range []*corev1.Pod{pod, cleanupPod} {
				if err := f.client.Delete(f.ctx, writer, client.GracePeriodSeconds(0)); err != nil {
					t.Fatal(err)
				}
			}
			f.r = newProfileReconciler(f.client, cleanup.Namespace)
			f.r.APIReader = f.client
			f.assertHeld(t)
			if ds := f.daemonSet(t, cleanup.Name); ds.UID == cleanup.UID {
				t.Fatal("reappeared provisioner requires a new cleanup run")
			}
		})
	}
}

func TestNodeProfileCleanupCheckpointBoundToGeneration(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds, pod := f.startTeardown(t)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Spec.Rollout.ContainerdRestart = nodev1alpha1.ContainerdRestartNone
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	p = getProfile(t, f.ctx, f.client, f.profile.Name)
	if meta.FindStatusCondition(p.Status.Conditions, nodev1alpha1.ConditionCleanupComplete) != nil {
		t.Fatal("previous-generation completion must be invalidated")
	}
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.assertHeld(t)
	f.daemonSet(t, ds.Name)
}

func TestNodeProfileCleanupCheckpointInvalidatedByWriterDuringTeardown(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.startCleanup(t)
	createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	f.r.Client = interceptDeleteClient{Client: f.client, beforeDelete: func(ctx context.Context, obj client.Object) error {
		provisioner := buildProfileDaemonSet(f.r.Config, &f.profile, "agentpool", nil)
		provisioner.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&f.profile, nodev1alpha1.GroupVersion.WithKind("NodeProfile"))}
		return f.client.Create(ctx, provisioner)
	}}
	f.assertHeld(t)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if meta.FindStatusCondition(p.Status.Conditions, nodev1alpha1.ConditionCleanupComplete) != nil {
		t.Fatal("a writer found during teardown must immediately invalidate the checkpoint")
	}
}

func TestNodeProfileCleanupCheckpointBoundToProfileIdentity(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds, pod := f.startTeardown(t)
	oldProfile := getProfile(t, f.ctx, f.client, f.profile.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	f.assertFinalized(t)
	f.profile = *createProfile(t, f.ctx, f.client, oldProfile.Name, oldProfile.Spec)
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	f.profile = getProfile(t, f.ctx, f.client, f.profile.Name)
	if f.profile.UID == oldProfile.UID {
		t.Fatal("test must create a different profile incarnation")
	}
	cleanup := f.startCleanup(t)
	f.r = newProfileReconciler(staleProfileClient{Client: f.client, profile: oldProfile}, ds.Namespace)
	f.r.APIReader = f.client
	f.assertHeld(t)
	if got := f.daemonSet(t, cleanup.Name); !got.DeletionTimestamp.IsZero() {
		t.Fatal("previous profile's completion checkpoint must not tear down the new profile")
	}
}
