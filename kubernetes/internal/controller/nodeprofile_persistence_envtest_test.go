// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Emulate decoding an API response with omitted fields into an already
// populated object: metadata is acknowledged, but local status stays populated
// even though the API pruned its ownership data.
func prunedStatusAcknowledgement(c client.Client, prune func(*nodev1alpha1.NodeProfileStatus)) client.Client {
	return interceptProfileStatusClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		requested := obj.(*nodev1alpha1.NodeProfile)
		stored := requested.DeepCopy()
		prune(&stored.Status)
		if err := c.Status().Update(ctx, stored, opts...); err != nil {
			return err
		}
		requested.ResourceVersion = stored.ResourceVersion
		return nil
	}}
}

func requireCRDCheckpointError(t *testing.T, f *cleanupFixture) {
	t.Helper()
	_, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)})
	if err == nil || !strings.Contains(err.Error(), "CRD") {
		t.Fatalf("successful-but-pruned status write must fail closed with CRD upgrade guidance: %v", err)
	}
}

func TestNodeProfileFreshReadRejectsPrunedTargetBeforeClaimOrPublication(t *testing.T) {
	f := newCleanupFixture(t, 0)
	name := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	f.r.Client = prunedStatusAcknowledgement(f.client, func(status *nodev1alpha1.NodeProfileStatus) {
		status.Targets = nil
		status.ProvisioningSpec = nil
	})
	requireCRDCheckpointError(t, f)
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != "" {
		t.Fatal("node claim activated before authoritative target persistence")
	}
	current := f.daemonSet(t, ds.Name)
	if current.Generation != ds.Generation || legacyTemplateMatches(&current.Spec.Template.Spec, &node) {
		t.Fatal("privileged target published after its ledger was silently pruned")
	}
}

func TestNodeProfileFreshReadRejectsPrunedMigrationBeforeLegacyTeardown(t *testing.T) {
	f := newCleanupFixture(t, 1)
	cleanupLegacyDaemonSets(t, f.client, f.r.Config.Namespace)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	var env []corev1.EnvVar
	for _, value := range ds.Spec.Template.Spec.Containers[0].Env {
		if value.Name != "BREWLET_REQUIRE_NODE_CLAIM" {
			env = append(env, value)
		}
	}
	ds.Spec.Template.Spec.Containers[0].Env = env
	ds.Spec.Template.Spec.Affinity = profileAffinity(&f.profile, "agentpool", nil)
	if err := f.client.Update(f.ctx, ds); err != nil {
		t.Fatal(err)
	}
	pod := createLegacyDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
	updateTargetNode(t, f, f.nodes[0], func(node *corev1.Node) {
		delete(node.Labels, brewlet.LabelNodeOwner)
		delete(node.Labels, brewlet.LabelNodeIdentity)
		delete(node.Annotations, brewlet.AnnotationNodeOwner)
	})
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Status = nodev1alpha1.NodeProfileStatus{}
	if err := f.client.Status().Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	f.r.Client = prunedStatusAcknowledgement(f.client, func(status *nodev1alpha1.NodeProfileStatus) {
		status.Targets = nil
		status.ProvisioningSpec = nil
		status.ProvisioningGeneration = 0
		status.OwnershipInitialized = false
		status.Migrating = false
		status.MigrationDaemonSetUIDs = nil
	})
	requireCRDCheckpointError(t, f)
	if current := f.daemonSet(t, ds.Name); !current.DeletionTimestamp.IsZero() {
		t.Fatal("legacy DaemonSet deleted before its old targets were durably recorded")
	}
	var currentPod corev1.Pod
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), &currentPod); err != nil || !currentPod.DeletionTimestamp.IsZero() {
		t.Fatalf("unfenced legacy target evidence was destroyed: %v", err)
	}
	p = getProfile(t, f.ctx, f.client, p.Name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) ||
		conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonOwnershipMigration {
		t.Fatalf("migration did not preserve its finalizer and actionable status: %+v", p.Status)
	}
	// Once the current schema preserves the checkpoint, legacy teardown can
	// begin without losing the still-observable in-flight target.
	f.r.Client = f.client
	reconcileProfile(t, f.ctx, f.r, p.Name)
	observeLegacyFence(t, f, ds.Name)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if !p.Status.Migrating || len(p.Status.Targets) != 1 ||
		f.daemonSet(t, ds.Name).DeletionTimestamp.IsZero() {
		t.Fatal("migration did not resume after durable checkpoint storage became available")
	}
}

func TestNodeProfileFreshReadRejectsPrunedRetirementBeforeCleanupTeardown(t *testing.T) {
	f := newCleanupFixture(t, 1)
	updateTargetNode(t, f, f.nodes[0], func(node *corev1.Node) { delete(node.Labels, "agentpool") })
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	ds := f.daemonSet(t, brewlet.CleanupDaemonSetName(f.profile.Name))
	createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	f.r.Client = prunedStatusAcknowledgement(f.client, func(status *nodev1alpha1.NodeProfileStatus) {
		status.Retirement = nil
	})
	requireCRDCheckpointError(t, f)
	if current := f.daemonSet(t, ds.Name); !current.DeletionTimestamp.IsZero() {
		t.Fatal("cleanup evidence deleted before retirement completion was durably preserved")
	}
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != string(f.profile.UID) {
		t.Fatal("claim released after a silently discarded retirement checkpoint")
	}
}

type checkpointReadFailure struct {
	client.Reader
	afterWrite *bool
	err        error
}

func TestNodeProfileConfirmedRetirementInvalidatedByWriterDuringTeardown(t *testing.T) {
	f := newCleanupFixture(t, 1)
	updateTargetNode(t, f, f.nodes[0], func(node *corev1.Node) { delete(node.Labels, "agentpool") })
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	ds := f.daemonSet(t, brewlet.CleanupDaemonSetName(f.profile.Name))
	createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, ds, 1)
	f.r.Client = interceptDeleteClient{Client: f.client, beforeDelete: func(ctx context.Context, obj client.Object) error {
		profile := getProfile(t, ctx, f.client, f.profile.Name)
		if profile.Status.Retirement.Phase != nodev1alpha1.RetirementTeardown {
			t.Fatal("cleanup teardown started without a confirmed checkpoint")
		}
		provisioner := buildProfileDaemonSet(f.r.Config, &profile, "agentpool", nil)
		provisioner.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&profile, nodev1alpha1.GroupVersion.WithKind("NodeProfile"))}
		return f.client.Create(ctx, provisioner)
	}}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	profile := getProfile(t, f.ctx, f.client, f.profile.Name)
	if profile.Status.Retirement.Phase != nodev1alpha1.RetirementCleaning {
		t.Fatal("reappearing writer must invalidate the freshly confirmed retirement checkpoint")
	}
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != string(profile.UID) {
		t.Fatal("a reappearing writer must keep its target claim")
	}
}

func (r checkpointReadFailure) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*nodev1alpha1.NodeProfile); ok && *r.afterWrite {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestNodeProfileCheckpointConfirmationReadMustSucceed(t *testing.T) {
	f := newCleanupFixture(t, 0)
	name := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
	afterWrite := false
	readErr := errors.New("checkpoint confirmation unavailable")
	f.r.APIReader = checkpointReadFailure{Reader: f.client, afterWrite: &afterWrite, err: readErr}
	f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if err := f.client.Status().Update(ctx, obj, opts...); err != nil {
			return err
		}
		afterWrite = true
		return nil
	}}
	if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)}); !errors.Is(err, readErr) {
		t.Fatalf("confirmation read error must propagate: %v", err)
	}
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != "" {
		t.Fatal("a write acknowledgement alone must not authorize node claims")
	}
	f.r.Client, f.r.APIReader = f.client, f.client
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != string(f.profile.UID) {
		t.Fatal("claim activation did not resume after authoritative reads recovered")
	}
}
