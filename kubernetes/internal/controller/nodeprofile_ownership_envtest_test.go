// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func updateTargetNode(t *testing.T, f *cleanupFixture, name string, mutate func(*corev1.Node)) corev1.Node {
	t.Helper()
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	mutate(&node)
	if err := f.client.Update(f.ctx, &node); err != nil {
		t.Fatal(err)
	}
	return node
}

func TestNodeProfileRetargetCleansOnlyDepartingNodesAcrossRestart(t *testing.T) {
	f := newCleanupFixture(t, 2)
	original := f.profile.Spec.NodePool.Names[0]
	updateTargetNode(t, f, f.nodes[1], func(n *corev1.Node) { n.Labels["agentpool"] = "kept" })
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Spec.NodePool.Names = []string{original, "kept"}
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	for _, node := range f.nodes {
		markNodeReady(t, f.ctx, f.client, node, p.Name, p.Generation)
	}
	newNode := createNode(t, f.ctx, f.client, map[string]string{"agentpool": "new"})
	provisioner := f.daemonSet(t, brewlet.ProfileDaemonSetName(p.Name))
	oldPod := createDaemonSetPod(t, f.ctx, f.client, provisioner, f.nodes[0], false)
	p.Spec.NodePool.Names = []string{"kept", "new"}
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if p.Status.Retirement == nil || len(p.Status.Retirement.Targets) != 1 || p.Status.Retirement.Targets[0].Name != f.nodes[0] {
		t.Fatalf("wrong departing snapshot: %+v", p.Status.Retirement)
	}
	f.assertNoCleanup(t)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, provisioner.Namespace, provisioner.Name)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	f.assertNoCleanup(t)
	if err := f.client.Delete(f.ctx, oldPod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	// Both selector and role changes must not prevent reversal of old authority.
	oldNode := updateTargetNode(t, f, f.nodes[0], func(n *corev1.Node) {
		delete(n.Labels, "agentpool")
		n.Labels["node-role.kubernetes.io/control-plane"] = ""
	})
	reconcileProfile(t, f.ctx, f.r, p.Name)
	cleanup := f.daemonSet(t, brewlet.CleanupDaemonSetName(p.Name))
	if !legacyTemplateMatches(&cleanup.Spec.Template.Spec, &oldNode) {
		t.Fatal("cleanup lost the departing node after pool/role relabeling")
	}
	var kept corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[1]}, &kept); err != nil {
		t.Fatal(err)
	}
	if legacyTemplateMatches(&cleanup.Spec.Template.Spec, &kept) {
		t.Fatal("cleanup must never include retained targets")
	}
	cleanupPod := createDaemonSetPod(t, f.ctx, f.client, cleanup, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, cleanup, 1)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if p.Status.Retirement.Phase != nodev1alpha1.RetirementTeardown {
		t.Fatal("retirement completion checkpoint was not persisted")
	}
	frozenGeneration := p.Status.Retirement.Generation
	p.Spec.JDKs[0].Feature = 25
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	f.r = newProfileReconciler(f.client, cleanup.Namespace)
	f.r.APIReader = f.client
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if p.Status.Retirement.Generation != frozenGeneration || p.Status.Retirement.Phase != nodev1alpha1.RetirementTeardown {
		t.Fatal("new generation or controller restart discarded retirement progress")
	}
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, cleanup.Namespace, cleanup.Name)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	var old corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &old); err != nil {
		t.Fatal(err)
	}
	if old.Labels[brewlet.LabelNodeOwner] != string(p.UID) {
		t.Fatal("claim released while old cleanup pod can restart")
	}
	if err := f.client.Delete(f.ctx, cleanupPod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if p.Status.Retirement != nil || len(p.Status.Targets) != 2 {
		t.Fatalf("retarget failed to resume current targets: %+v", p.Status)
	}
	for _, name := range []string{f.nodes[0], f.nodes[1], newNode} {
		var node corev1.Node
		if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
			t.Fatal(err)
		}
		if name == f.nodes[0] {
			if node.Labels[brewlet.LabelNodeOwner] != "" || node.Labels[brewlet.LabelRuntimeReady] != "" || node.Annotations[brewlet.AnnotationProfile] != "" {
				t.Fatal("departing node retained ownership or runtime advertisements")
			}
		} else if node.Labels[brewlet.LabelNodeOwner] != string(p.UID) {
			t.Fatalf("current target %s not claimed", name)
		}
		if name == f.nodes[1] && node.Labels[brewlet.LabelRuntimeReady] != brewlet.ValueReady {
			t.Fatal("retarget cleanup removed retained-node readiness")
		}
	}
}

func TestNodeProfileRelabelingCapturesUnadvertisedProvisioner(t *testing.T) {
	for _, change := range []string{"pool", "role"} {
		t.Run(change, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			provisioner := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			pod := createDaemonSetPod(t, f.ctx, f.client, provisioner, f.nodes[0], false)
			updateTargetNode(t, f, f.nodes[0], func(n *corev1.Node) {
				if change == "pool" {
					delete(n.Labels, "agentpool")
				} else {
					n.Labels["node-role.kubernetes.io/control-plane"] = ""
				}
			})
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if p.Status.Retirement == nil || len(p.Status.Retirement.Targets) != 1 {
				t.Fatal("unadvertised in-progress node was forgotten after relabeling")
			}
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, provisioner.Namespace, provisioner.Name)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			f.assertNoCleanup(t)
			if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			f.daemonSet(t, brewlet.CleanupDaemonSetName(p.Name))
		})
	}
}

func TestNodeProfileRetirementRefusesReusedOrForeignNode(t *testing.T) {
	for _, change := range []string{"uid", "owner", "missing"} {
		t.Run(change, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			p.Spec.NodePool.Names = []string{"elsewhere"}
			if err := f.client.Update(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(p.Name))
			var node corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
				t.Fatal(err)
			}
			if change == "owner" {
				node.Labels[brewlet.LabelNodeOwner] = "different-owner"
				if err := f.client.Update(f.ctx, &node); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.client.Delete(f.ctx, &node); err != nil {
					t.Fatal(err)
				}
				if change == "uid" {
					node = corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}
					if err := f.client.Create(f.ctx, &node); err != nil {
						t.Fatal(err)
					}
				}
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked || p.Status.Retirement == nil {
				t.Fatal("foreign/missing node must block and retain its retirement ledger")
			}
			f.assertNoCleanup(t)
		})
	}
}

type interceptNodePatchClient struct {
	client.Client
	before func(context.Context, client.Object) error
}

func (c interceptNodePatchClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if err := c.before(ctx, obj); err != nil {
		return err
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestNodeProfileConcurrentClaimsUseNodeResourceVersion(t *testing.T) {
	f := newCleanupFixture(t, 0)
	name := createNode(t, f.ctx, f.client, nil)
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	target := nodev1alpha1.NodeTarget{Name: node.Name, UID: node.UID}
	other := createProfile(t, f.ctx, f.client, uniqueName("competitor"), f.profile.Spec)
	other.Status.Targets = []nodev1alpha1.NodeTarget{target}
	if err := f.client.Status().Update(f.ctx, other); err != nil {
		t.Fatal(err)
	}
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Status.Targets = []nodev1alpha1.NodeTarget{target}
	if err := f.client.Status().Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	competing := newProfileReconciler(f.client, f.r.Config.Namespace)
	competing.APIReader = f.client
	f.r.Client = interceptNodePatchClient{Client: f.client, before: func(ctx context.Context, obj client.Object) error {
		return competing.claimTarget(ctx, other, target, false)
	}}
	if err := f.r.claimTarget(f.ctx, &p, target, false); !apierrors.IsConflict(err) {
		t.Fatalf("stale claim must conflict instead of overwriting ownership: %v", err)
	}
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != string(other.UID) {
		t.Fatal("losing CAS overwrote the winning owner")
	}
}

func TestNodeProfileTargetLedgerFailurePreventsClaimsAndWorkers(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	pool := uniqueName("pool")
	nodeName := createNode(t, ctx, c, map[string]string{"agentpool": pool})
	p := createProfile(t, ctx, c, uniqueName("ledger"), nodev1alpha1.NodeProfileSpec{
		NodePool: nodev1alpha1.NodePoolRef{Key: "agentpool", Names: []string{pool}},
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	writeErr := errors.New("target ledger write rejected")
	r := newProfileReconciler(interceptProfileStatusClient{Client: c, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if len(obj.(*nodev1alpha1.NodeProfile).Status.Targets) > 0 {
			return writeErr
		}
		return c.Status().Update(ctx, obj, opts...)
	}}, ns)
	r.APIReader = c
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(p)}); !errors.Is(err, writeErr) {
		t.Fatalf("ledger status failure must propagate: %v", err)
	}
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != "" {
		t.Fatal("node claimed before its target ledger was durable")
	}
	f := &cleanupFixture{client: c, ctx: ctx, r: r, profile: *p}
	f.assertNoCleanup(t)
}

func TestNodeProfileMigratesLegacyUnadvertisedPodBeforeClaiming(t *testing.T) {
	f := newCleanupFixture(t, 1)
	cleanupLegacyDaemonSets(t, f.client, f.r.Config.Namespace)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	var env []corev1.EnvVar
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.Name != "BREWLET_REQUIRE_NODE_CLAIM" {
			env = append(env, e)
		}
	}
	ds.Spec.Template.Spec.Containers[0].Env = env
	ds.Spec.Template.Spec.Affinity = profileAffinity(&f.profile, "agentpool", nil)
	if err := f.client.Update(f.ctx, ds); err != nil {
		t.Fatal(err)
	}
	pod := createLegacyDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
	updateTargetNode(t, f, f.nodes[0], func(n *corev1.Node) {
		delete(n.Labels, brewlet.LabelNodeOwner)
		delete(n.Labels, brewlet.LabelNodeIdentity)
		delete(n.Annotations, brewlet.AnnotationNodeOwner)
		delete(n.Labels, "agentpool")
	})
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Status = nodev1alpha1.NodeProfileStatus{}
	if err := f.client.Status().Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if !p.Status.Migrating || len(p.Status.Targets) != 1 || p.Status.Targets[0].Name != f.nodes[0] {
		t.Fatalf("legacy in-flight target not captured before draining: %+v", p.Status)
	}
	observeLegacyFence(t, f, ds.Name)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	f.assertNoCleanup(t)
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if p.Status.Migrating || p.Status.Retirement == nil || !p.Status.Targets[0].Claimed {
		t.Fatalf("drained legacy target did not enter fenced retirement: %+v", p.Status)
	}
}

func TestNodeProfileDefaultWaitsForPriorOwnerAfterRelabel(t *testing.T) {
	f := newCleanupFixture(t, 1)
	defaultProfile := createProfile(t, f.ctx, f.client, uniqueName("default"), nodev1alpha1.NodeProfileSpec{
		JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	updateTargetNode(t, f, f.nodes[0], func(n *corev1.Node) { n.Labels["agentpool"] = "unassigned" })
	reconcileProfile(t, f.ctx, f.r, defaultProfile.Name)
	p := getProfile(t, f.ctx, f.client, defaultProfile.Name)
	if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonOwnershipConflict {
		t.Fatal("default must wait for the relabeled node's prior owner")
	}
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != string(f.profile.UID) {
		t.Fatal("default stole the old profile's in-progress host")
	}
	if !strings.Contains(p.Status.Conditions[0].Message, "cleanup") {
		t.Fatal("ownership conflict should explain the cleanup dependency")
	}
}

func TestNodeProfileRetirementCheckpointFailurePreservesCleanupEvidence(t *testing.T) {
	f := newCleanupFixture(t, 1)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Spec.NodePool.Names = []string{"elsewhere"}
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(p.Name))
	reconcileProfile(t, f.ctx, f.r, p.Name)
	cleanup := f.daemonSet(t, brewlet.CleanupDaemonSetName(p.Name))
	pod := createDaemonSetPod(t, f.ctx, f.client, cleanup, f.nodes[0], true)
	completeCleanupStatus(t, f.ctx, f.client, cleanup, 1)
	writeErr := errors.New("retirement checkpoint unavailable")
	f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		retirement := obj.(*nodev1alpha1.NodeProfile).Status.Retirement
		if retirement != nil && retirement.Phase == nodev1alpha1.RetirementTeardown {
			return writeErr
		}
		return f.client.Status().Update(ctx, obj, opts...)
	}}
	if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&p)}); !errors.Is(err, writeErr) {
		t.Fatalf("retirement checkpoint failure did not propagate: %v", err)
	}
	if ds := f.daemonSet(t, cleanup.Name); !ds.DeletionTimestamp.IsZero() {
		t.Fatal("cleanup evidence deleted before the retirement checkpoint persisted")
	}
	f.r.Client = f.client
	reconcileProfile(t, f.ctx, f.r, p.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, cleanup.Namespace, cleanup.Name)
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	// Simulate losing the status write after safe claim release. On restart,
	// the durable Teardown phase must not recreate cleanup on unclaimed nodes.
	f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if obj.(*nodev1alpha1.NodeProfile).Status.Retirement == nil {
			return writeErr
		}
		return f.client.Status().Update(ctx, obj, opts...)
	}}
	if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&p)}); !errors.Is(err, writeErr) {
		t.Fatalf("retirement release failure did not propagate: %v", err)
	}
	f.r = newProfileReconciler(f.client, cleanup.Namespace)
	f.r.APIReader = f.client
	reconcileProfile(t, f.ctx, f.r, p.Name)
	f.assertNoCleanup(t)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if p.Status.Retirement != nil || len(p.Status.Targets) != 0 {
		t.Fatal("restart did not finish the already completed retirement")
	}
}

func TestNodeProfileRetirementUnavailableNodeIsActionablyBlocked(t *testing.T) {
	f := newCleanupFixture(t, 1)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Spec.NodePool.Names = []string{"elsewhere"}
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(p.Name))
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
		t.Fatal(err)
	}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	if err := f.client.Status().Update(f.ctx, &node); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked ||
		!strings.Contains(p.Status.Conditions[0].Message, "restore node connectivity") {
		t.Fatalf("missing actionable unavailable-node status: %+v", p.Status.Conditions)
	}
	f.assertNoCleanup(t)
}

func TestNodeProfileUnavailableRetirementPausesWholeProfileAndPreservesRetainedNodes(t *testing.T) {
	for _, loss := range []string{"deleted", "replaced", "disconnected"} {
		t.Run(loss, func(t *testing.T) {
			f := newCleanupFixture(t, 2)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			retirementGeneration := p.Generation
			markNodeReady(t, f.ctx, f.client, f.nodes[1], p.Name, p.Generation)
			kept := updateTargetNode(t, f, f.nodes[1], func(node *corev1.Node) {
				node.Labels[brewlet.LabelJDKPrefix+"temurin-21"] = "true"
				node.Annotations[brewlet.AnnotationJDKs] = "temurin-21"
			})
			next := createNode(t, f.ctx, f.client, map[string]string{"agentpool": p.Spec.NodePool.Names[0]})
			provisioner := f.daemonSet(t, brewlet.ProfileDaemonSetName(p.Name))
			retainedWriter := createDaemonSetPod(t, f.ctx, f.client, provisioner, f.nodes[1], true)
			var old corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &old); err != nil {
				t.Fatal(err)
			}
			if loss == "disconnected" {
				old = updateTargetNode(t, f, old.Name, func(node *corev1.Node) {
					node.Labels["agentpool"] = p.Name + "-retiring"
				})
				old.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
				if err := f.client.Status().Update(f.ctx, &old); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := f.client.Delete(f.ctx, &old); err != nil {
					t.Fatal(err)
				}
				if loss == "replaced" {
					replacement := corev1.Node{ObjectMeta: metav1.ObjectMeta{
						Name: old.Name, Labels: map[string]string{"agentpool": p.Spec.NodePool.Names[0]},
					}}
					if err := f.client.Create(f.ctx, &replacement); err != nil {
						t.Fatal(err)
					}
				}
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, provisioner.Namespace, provisioner.Name)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			f.assertNoCleanup(t)
			if err := f.client.Delete(f.ctx, retainedWriter, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			assertPaused := func() {
				t.Helper()
				p = getProfile(t, f.ctx, f.client, p.Name)
				if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked ||
					!strings.Contains(p.Status.Conditions[0].Message, "profile-wide provisioning and upgrades are paused") {
					t.Fatalf("status must disclose the profile-wide pause: %+v", p.Status.Conditions)
				}
				if loss != "disconnected" && !strings.Contains(p.Status.Conditions[0].Message, "no supported in-place recovery") {
					t.Fatalf("lost UID status must not imply that recreating the Node recovers retirement: %+v", p.Status.Conditions)
				}
				if p.Status.Retirement == nil || len(p.Status.Retirement.Targets) != 1 ||
					p.Status.Retirement.Targets[0].UID != old.UID || p.Status.Retirement.Generation != retirementGeneration ||
					!containsString(p.Finalizers, brewlet.FinalizerCleanup) {
					t.Fatal("unproven host cleanup lost its original identity or finalizer")
				}
				f.assertNoCleanup(t)
				if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(provisioner), provisioner); !apierrors.IsNotFound(err) {
					t.Fatalf("retained/new-node provisioning must remain stopped: %v", err)
				}
				var current corev1.Node
				if err := f.client.Get(f.ctx, types.NamespacedName{Name: kept.Name}, &current); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(current.Labels, kept.Labels) || !reflect.DeepEqual(current.Annotations, kept.Annotations) {
					t.Fatal("blocked retirement changed retained-node claims, readiness, or capabilities")
				}
				current = corev1.Node{}
				if err := f.client.Get(f.ctx, types.NamespacedName{Name: next}, &current); err != nil {
					t.Fatal(err)
				}
				if current.Labels[brewlet.LabelNodeOwner] != "" {
					t.Fatal("new node was claimed while the profile's retirement was blocked")
				}
				if loss == "replaced" {
					current = corev1.Node{}
					if err := f.client.Get(f.ctx, types.NamespacedName{Name: old.Name}, &current); err != nil {
						t.Fatal(err)
					}
					if current.UID == old.UID || current.Labels[brewlet.LabelNodeOwner] != "" {
						t.Fatal("same-name replacement must not inherit the lost node's claim")
					}
				}
			}
			assertPaused()
			p.Spec.JDKs[0].Feature = 25
			if err := f.client.Update(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			f.r = newProfileReconciler(f.client, provisioner.Namespace)
			f.r.APIReader = f.client
			reconcileProfile(t, f.ctx, f.r, p.Name)
			assertPaused()
			if loss != "disconnected" {
				return
			}
			// Connectivity recovery preserves the original Node UID. It can
			// resume cleanup; recreating a deleted Node cannot do this.
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&old), &old); err != nil {
				t.Fatal(err)
			}
			old.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
			if err := f.client.Status().Update(f.ctx, &old); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			cleanup := f.daemonSet(t, brewlet.CleanupDaemonSetName(p.Name))
			if !legacyTemplateMatches(&cleanup.Spec.Template.Spec, &old) || legacyTemplateMatches(&cleanup.Spec.Template.Spec, &kept) {
				t.Fatal("recovered cleanup must target only the original departing node")
			}
			cleanupPod := createDaemonSetPod(t, f.ctx, f.client, cleanup, old.Name, true)
			completeCleanupStatus(t, f.ctx, f.client, cleanup, 1)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, cleanup.Namespace, cleanup.Name)
			if err := f.client.Delete(f.ctx, cleanupPod, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if p.Status.Retirement != nil || len(p.Status.Targets) != 2 || p.Status.ProvisioningGeneration != p.Generation {
				t.Fatalf("cleanup recovery did not resume retained/new-node provisioning: %+v", p.Status)
			}
			f.daemonSet(t, provisioner.Name)
		})
	}
}

func TestNodeProfileDeletionFinishesFrozenRetirementBeforeRemainingTargets(t *testing.T) {
	for _, phase := range []string{nodev1alpha1.RetirementCleaning, nodev1alpha1.RetirementTeardown} {
		t.Run(phase, func(t *testing.T) {
			f := newCleanupFixture(t, 2)
			updateTargetNode(t, f, f.nodes[0], func(n *corev1.Node) { delete(n.Labels, "agentpool") })
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, f.r.Config.Namespace, brewlet.ProfileDaemonSetName(f.profile.Name))
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			retiring := f.daemonSet(t, brewlet.CleanupDaemonSetName(f.profile.Name))
			retiringPod := createDaemonSetPod(t, f.ctx, f.client, retiring, f.nodes[0], true)
			completeCleanupStatus(t, f.ctx, f.client, retiring, 1)
			if phase == nodev1alpha1.RetirementTeardown {
				reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			}
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if err := f.client.Delete(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if p.Status.Retirement == nil || !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
				t.Fatal("deletion discarded an in-flight retirement")
			}
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, retiring.Namespace, retiring.Name)
			if err := f.client.Delete(f.ctx, retiringPod, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			cleanup := f.daemonSet(t, retiring.Name)
			if cleanup.UID == retiring.UID {
				t.Fatal("full deletion did not start cleanup for the remaining target")
			}
			var old corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &old); err != nil {
				t.Fatal(err)
			}
			if legacyTemplateMatches(&cleanup.Spec.Template.Spec, &old) {
				t.Fatal("full deletion tried to clean an already released retirement target")
			}
			pod := createDaemonSetPod(t, f.ctx, f.client, cleanup, f.nodes[1], true)
			completeCleanupStatus(t, f.ctx, f.client, cleanup, 1)
			f.assertTeardownHeld(t)
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, cleanup.Namespace, cleanup.Name)
			if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			f.assertFinalized(t)
		})
	}
}

func TestNodeProfileOldCRDPruningBlocksOwnershipActivation(t *testing.T) {
	f := newCleanupFixture(t, 0)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Status = nodev1alpha1.NodeProfileStatus{}
	if err := f.client.Status().Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		profile := obj.(*nodev1alpha1.NodeProfile)
		profile.Status.OwnershipInitialized = false
		profile.Status.Targets = nil
		profile.Status.ProvisioningSpec = nil
		profile.Status.ProvisioningGeneration = 0
		return f.client.Status().Update(ctx, obj, opts...)
	}}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) ||
		conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonOwnershipMigration ||
		!strings.Contains(p.Status.Conditions[0].Message, "CRD") {
		t.Fatalf("old CRD pruning must hold deletion with an upgrade instruction: %+v", p.Status)
	}
	if ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(p.Name)); !ds.DeletionTimestamp.IsZero() {
		t.Fatal("writer deleted before durable target schema was available")
	}
}

func TestNodeProfileCleanupPolicyDistinguishesPreviouslyMutatedAndNewImmutableNodes(t *testing.T) {
	f := newCleanupFixture(t, 1)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Spec.Rollout.ContainerdRestart = nodev1alpha1.ContainerdRestartNone
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	newNode := createNode(t, f.ctx, f.client, map[string]string{"agentpool": p.Spec.NodePool.Names[0]})
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if len(p.Status.Targets) != 2 {
		t.Fatalf("missing target snapshots: %+v", p.Status.Targets)
	}
	for _, target := range p.Status.Targets {
		want := nodev1alpha1.ContainerdRestartValidated
		if target.Name == newNode {
			want = nodev1alpha1.ContainerdRestartNone
		}
		if target.ContainerdRestart != want {
			t.Fatalf("node %s cleanup mode=%s, want %s", target.Name, target.ContainerdRestart, want)
		}
	}
}
