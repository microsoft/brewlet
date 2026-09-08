// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

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

func legacyEnv(spec *corev1.PodSpec, name, value string) {
	var env []corev1.EnvVar
	for _, item := range spec.Containers[0].Env {
		if item.Name != name {
			env = append(env, item)
		}
	}
	if value != "" {
		env = append(env, corev1.EnvVar{Name: name, Value: value})
	}
	spec.Containers[0].Env = env
}

func createLegacyDaemonSetPod(t *testing.T, ctx context.Context, c client.Client, ds *appsv1.DaemonSet, node string, ready bool) *corev1.Pod {
	t.Helper()
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		t.Fatal(err)
	}
	spec := ds.Spec.Template.Spec.DeepCopy()
	spec.NodeName = node
	names, _ := migrationPodNames(spec, nodes.Items)
	// Model an existing legacy node unambiguously predating its old worker.
	// API creation timestamps have one-second precision.
	for i := range nodes.Items {
		for _, name := range names {
			if nodes.Items[i].Name == name {
				if delay := time.Until(nodes.Items[i].CreationTimestamp.Add(time.Second)); delay > 0 {
					time.Sleep(delay)
				}
			}
		}
	}
	return createDaemonSetPod(t, ctx, c, ds, node, ready)
}

func clearLegacyClaims(t *testing.T, f *cleanupFixture) {
	t.Helper()
	cleanupLegacyDaemonSets(t, f.client, f.r.Config.Namespace)
	for _, name := range f.nodes {
		updateTargetNode(t, f, name, func(node *corev1.Node) {
			delete(node.Labels, brewlet.LabelNodeOwner)
			delete(node.Labels, brewlet.LabelNodeIdentity)
			delete(node.Annotations, brewlet.AnnotationNodeOwner)
		})
	}
	profile := getProfile(t, f.ctx, f.client, f.profile.Name)
	profile.Status = nodev1alpha1.NodeProfileStatus{}
	if err := f.client.Status().Update(f.ctx, &profile); err != nil {
		t.Fatal(err)
	}
}

func cleanupLegacyDaemonSets(t *testing.T, c client.Client, namespace string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		var sets appsv1.DaemonSetList
		if err := c.List(ctx, &sets, client.InNamespace(namespace)); err != nil {
			t.Error(err)
			return
		}
		for i := range sets.Items {
			ds := &sets.Items[i]
			ds.Finalizers = nil
			_ = c.Update(ctx, ds)
			_ = c.Delete(ctx, ds, client.PropagationPolicy(metav1.DeletePropagationBackground))
		}
	})
}

func observeLegacyFence(t *testing.T, f *cleanupFixture, name string) {
	t.Helper()
	ds := f.daemonSet(t, name)
	if !migrationFencePresent(ds) || !ds.DeletionTimestamp.IsZero() {
		t.Fatal("migration must gate future scheduling without deleting the DaemonSet or old pods")
	}
	ds.Status.ObservedGeneration = ds.Generation
	if err := f.client.Status().Update(f.ctx, ds); err != nil {
		t.Fatal(err)
	}
}

func requireLegacyTarget(t *testing.T, profile *nodev1alpha1.NodeProfile, name string, uid types.UID, mode string) {
	t.Helper()
	for _, target := range profile.Status.Targets {
		if target.Name == name {
			if target.UID != uid || target.ContainerdRestart != mode {
				t.Fatalf("target %s lost identity/policy: %+v; want uid=%s mode=%s", name, target, uid, mode)
			}
			return
		}
	}
	t.Fatalf("missing legacy target %s: %+v", name, profile.Status.Targets)
}

func TestNodeProfileLegacyAdvertisementsPersistAfterWritersDisappear(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleting", true: "invalid-deleting"}[invalid], func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			markNodeReady(t, f.ctx, f.client, f.nodes[0], f.profile.Name, f.profile.Generation)
			ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			if err := f.client.Delete(f.ctx, ds, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
				t.Fatal(err)
			}
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
			clearLegacyClaims(t, f)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if invalid {
				p.Spec.Registry = &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{"docker.io": "unapproved.example/cache"}}
				if err := f.client.Update(f.ctx, &p); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.client.Delete(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			if !p.Status.Migrating || !containsString(p.Finalizers, brewlet.FinalizerCleanup) ||
				conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
				t.Fatalf("advertisement-only host state must block finalization: %+v", p.Status)
			}
			requireLegacyTarget(t, &p, f.nodes[0], "", "")
			f.assertNoCleanup(t)
			updateTargetNode(t, f, f.nodes[0], removeNodeAdvertisements)
			f.r = newProfileReconciler(f.client, ds.Namespace)
			f.r.APIReader = f.client
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			requireLegacyTarget(t, &p, f.nodes[0], "", "")
			if !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
				t.Fatal("withdrawing readiness erased the durable legacy obligation")
			}
			var node corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
				t.Fatal(err)
			}
			if node.Labels[brewlet.LabelNodeOwner] != "" {
				t.Fatal("name-only evidence must not grant host ownership")
			}
			other := createProfile(t, f.ctx, f.client, uniqueName("competitor"), f.profile.Spec)
			if err := f.r.claimTarget(f.ctx, other, nodev1alpha1.NodeTarget{Name: node.Name, UID: node.UID}, false); err == nil {
				t.Fatal("a different profile must not steal a durable unclaimed migration obligation after ads disappear")
			}
		})
	}
}

func TestNodeProfileUnprovenInvalidProfileCannotEraseLegacyAdvertisements(t *testing.T) {
	f := newCleanupFixture(t, 0)
	p := createProfile(t, f.ctx, f.client, uniqueName("never-valid"), nodev1alpha1.NodeProfileSpec{
		JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
		Registry: &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{"docker.io": "unapproved.example/cache"}},
	})
	name := createNode(t, f.ctx, f.client, nil)
	markNodeReady(t, f.ctx, f.client, name, p.Name, 1)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelRuntimeReady] != brewlet.ValueReady || node.Annotations[brewlet.AnnotationProfile] != p.Name {
		t.Fatal("an unproven same-name profile must not erase someone else's legacy evidence")
	}
}

func TestNodeProfileLegacyPodNodeUIDRejectsAmbiguousCreationTimes(t *testing.T) {
	created := time.Unix(1700000000, 0)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)}}
	for _, tc := range []struct {
		name    string
		created metav1.Time
		known   bool
	}{
		{"original-node", metav1.NewTime(created.Add(-time.Second)), true},
		{"same-clock-tick", metav1.NewTime(created), false},
		{"replacement-node", metav1.NewTime(created.Add(time.Second)), false},
		{"missing-time", metav1.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "observed-node-uid", CreationTimestamp: tc.created}}
			uid := legacyPodNodeUID(pod, node)
			if (uid != "") != tc.known {
				t.Fatalf("unproven legacy binding identity: uid=%q known=%v", uid, tc.known)
			}
		})
	}
}

func TestNodeProfileLegacyOldPodPolicyRemainsPerNode(t *testing.T) {
	for _, mode := range []string{nodev1alpha1.ContainerdRestartValidated, nodev1alpha1.ContainerdRestartSIGHUP, ""} {
		name := mode
		if name == "" {
			name = "unknown"
		}
		t.Run(name, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_CONTAINERD_RESTART", mode)
			oldPod := createLegacyDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			p.Spec.Rollout.ContainerdRestart = nodev1alpha1.ContainerdRestartNone
			if err := f.client.Update(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			next := createNode(t, f.ctx, f.client, map[string]string{"agentpool": p.Spec.NodePool.Names[0]})
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_CONTAINERD_RESTART", nodev1alpha1.ContainerdRestartNone)
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_GENERATION", strconv.FormatInt(p.Generation, 10))
			ds.Spec.Template.Spec.Affinity = profileAffinity(&p, "agentpool", nil)
			if err := f.client.Update(f.ctx, ds); err != nil {
				t.Fatal(err)
			}
			clearLegacyClaims(t, f)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			var oldNode, nextNode corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &oldNode); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: next}, &nextNode); err != nil {
				t.Fatal(err)
			}
			requireLegacyTarget(t, &p, oldNode.Name, oldNode.UID, mode)
			requireLegacyTarget(t, &p, nextNode.Name, nextNode.UID, nodev1alpha1.ContainerdRestartNone)
			var stillRunning corev1.Pod
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(oldPod), &stillRunning); err != nil || !stillRunning.DeletionTimestamp.IsZero() {
				t.Fatalf("scheduling fence destroyed old pod evidence: %v", err)
			}
			observeLegacyFence(t, f, ds.Name)
			reconcileProfile(t, f.ctx, f.r, p.Name)
			if mode == "" {
				if !f.daemonSet(t, ds.Name).DeletionTimestamp.IsZero() {
					t.Fatal("unknown old policy must not be discarded by draining its last evidence")
				}
			} else {
				completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
			}
			if err := f.client.Delete(f.ctx, oldPod, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			f.r = newProfileReconciler(f.client, ds.Namespace)
			f.r.APIReader = f.client
			reconcileProfile(t, f.ctx, f.r, p.Name)
			p = getProfile(t, f.ctx, f.client, p.Name)
			requireLegacyTarget(t, &p, oldNode.Name, oldNode.UID, mode)
			requireLegacyTarget(t, &p, nextNode.Name, nextNode.UID, nodev1alpha1.ContainerdRestartNone)
			if mode == "" {
				if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked || !p.Status.Migrating {
					t.Fatal("a newer none template must not erase previously observed unknown policy")
				}
			} else if p.Status.Migrating || !p.Status.Targets[0].Claimed || !p.Status.Targets[1].Claimed {
				t.Fatal("verified per-node policy migration did not finish after all writers drained")
			}
		})
	}
}

func TestNodeProfileLegacyPendingAffinitySurvivesBindingAndTerminationRace(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	legacyEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
	ds.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{f.nodes[0]}}},
		}}},
	}}
	pending := createLegacyDaemonSetPod(t, f.ctx, f.client, ds, "", false)
	pending.Status = corev1.PodStatus{Phase: corev1.PodPending}
	if err := f.client.Status().Update(f.ctx, pending); err != nil {
		t.Fatal(err)
	}
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Spec.NodePool.Names = []string{p.Name + "-new"}
	p.Spec.Rollout.ContainerdRestart = nodev1alpha1.ContainerdRestartNone
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	next := createNode(t, f.ctx, f.client, map[string]string{"agentpool": p.Spec.NodePool.Names[0]})
	ds.Spec.Template.Spec.Affinity = profileAffinity(&p, "agentpool", nil)
	legacyEnv(&ds.Spec.Template.Spec, "BREWLET_CONTAINERD_RESTART", nodev1alpha1.ContainerdRestartNone)
	legacyEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_GENERATION", strconv.FormatInt(p.Generation, 10))
	if err := f.client.Update(f.ctx, ds); err != nil {
		t.Fatal(err)
	}
	clearLegacyClaims(t, f)
	reconcileProfile(t, f.ctx, f.r, p.Name)
	var old corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &old); err != nil {
		t.Fatal(err)
	}
	p = getProfile(t, f.ctx, f.client, p.Name)
	requireLegacyTarget(t, &p, old.Name, old.UID, nodev1alpha1.ContainerdRestartValidated)
	if !f.daemonSet(t, ds.Name).DeletionTimestamp.IsZero() {
		t.Fatal("pending evidence was drained before the scheduling fence was observed")
	}
	observeLegacyFence(t, f, ds.Name)
	f.r.Client = interceptDeleteClient{Client: f.client, beforeDelete: func(ctx context.Context, obj client.Object) error {
		saved := getProfile(t, ctx, f.client, p.Name)
		requireLegacyTarget(t, &saved, old.Name, old.UID, nodev1alpha1.ContainerdRestartValidated)
		binding := &corev1.Binding{ObjectMeta: metav1.ObjectMeta{Name: pending.Name, Namespace: pending.Namespace},
			Target: corev1.ObjectReference{Kind: "Node", Name: old.Name, UID: old.UID}}
		if err := f.client.SubResource("binding").Create(ctx, pending, binding); err != nil {
			return err
		}
		markNodeReady(t, ctx, f.client, old.Name, p.Name, 1)
		return f.client.Delete(ctx, pending, client.GracePeriodSeconds(0))
	}}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	f.r.Client = f.client
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
	f.r = newProfileReconciler(f.client, ds.Namespace)
	f.r.APIReader = f.client
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if p.Status.Retirement == nil || len(p.Status.Retirement.Targets) != 1 ||
		p.Status.Retirement.Targets[0].UID != old.UID {
		t.Fatal("a binding/termination race lost the departing node's retirement")
	}
	cleanup := f.daemonSet(t, brewlet.CleanupDaemonSetName(p.Name))
	var currentOld, currentNext corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: old.Name}, &currentOld); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: next}, &currentNext); err != nil {
		t.Fatal(err)
	}
	if !legacyTemplateMatches(&cleanup.Spec.Template.Spec, &currentOld) || legacyTemplateMatches(&cleanup.Spec.Template.Spec, &currentNext) {
		t.Fatal("migration cleanup must reach only the old pending pod's departing node")
	}
	requireLegacyTarget(t, &p, currentOld.Name, currentOld.UID, nodev1alpha1.ContainerdRestartValidated)
	requireLegacyTarget(t, &p, currentNext.Name, currentNext.UID, nodev1alpha1.ContainerdRestartNone)
}

func TestNodeProfileLegacyOrphanPodCannotBeAdoptedByName(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	legacyEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
	ds.UID = "unknown-old-daemonset-uid"
	pod := createLegacyDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
	actual := f.daemonSet(t, ds.Name)
	if err := f.client.Delete(f.ctx, actual, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		t.Fatal(err)
	}
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, actual.Namespace, actual.Name)
	clearLegacyClaims(t, f)
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if !p.Status.Migrating || conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
		t.Fatal("an orphan's matching labels/env/name are not its missing controller UID chain")
	}
	requireLegacyTarget(t, &p, f.nodes[0], "", "")
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	requireLegacyTarget(t, &p, f.nodes[0], "", "")
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatal("orphaned worker disappearance erased its repairable ownership record")
	}
}

func TestNodeProfileClaimsBlockForeignNamespaceLegacyWorkers(t *testing.T) {
	for _, kind := range []string{"daemonset", "running-pod", "pending-pod"} {
		t.Run(kind, func(t *testing.T) {
			f := newCleanupFixture(t, 0)
			nodeName := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
			foreignNamespace := createNamespace(t, f.ctx, f.client)
			cleanupLegacyDaemonSets(t, f.client, foreignNamespace)
			cfg := f.r.Config
			cfg.Namespace = foreignNamespace
			ds := buildProfileDaemonSet(cfg, &f.profile, "agentpool", nil)
			ds.Name = brewlet.ProvisionerName
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_UID", "")
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_NAME", "")
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_GENERATION", "")
			delete(ds.Labels, brewlet.LabelNodeProfile)
			delete(ds.Spec.Template.Labels, brewlet.LabelNodeProfile)
			var foreign client.Object
			if kind == "daemonset" {
				if err := f.client.Create(f.ctx, ds); err != nil {
					t.Fatal(err)
				}
				foreign = ds
			} else {
				ds.UID = "foreign-original-controller"
				name := nodeName
				if kind == "pending-pod" {
					name = ""
				}
				foreign = createDaemonSetPod(t, f.ctx, f.client, ds, name, false)
			}
			before := foreign.DeepCopyObject().(client.Object)
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonOwnershipConflict ||
				!strings.Contains(p.Status.Conditions[0].Message, foreignNamespace) {
				t.Fatalf("claim must disclose the blocking foreign namespace worker: %+v", p.Status.Conditions)
			}
			var node corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: nodeName}, &node); err != nil {
				t.Fatal(err)
			}
			if node.Labels[brewlet.LabelNodeOwner] != "" {
				t.Fatal("standalone readiness withdrawal must not permit a concurrent managed claim")
			}
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil {
				t.Fatal(err)
			}
			if foreign.GetResourceVersion() != before.GetResourceVersion() || !foreign.GetDeletionTimestamp().IsZero() {
				t.Fatal("read-only global discovery mutated or adopted a foreign worker")
			}
		})
	}
}

type denyGlobalDaemonSetList struct{ client.Reader }

func (r denyGlobalDaemonSetList) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	options := &client.ListOptions{}
	for _, opt := range opts {
		opt.ApplyToList(options)
	}
	if _, ok := list.(*appsv1.DaemonSetList); ok && options.Namespace == "" {
		return errors.New("cluster-wide daemonset list denied")
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestNodeProfileGlobalLegacyReadFailurePreventsClaims(t *testing.T) {
	f := newCleanupFixture(t, 0)
	name := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
	f.r.APIReader = denyGlobalDaemonSetList{Reader: f.client}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if !strings.Contains(p.Status.Conditions[0].Message, "cluster-wide daemonset list denied") {
		t.Fatal("global inspection errors must not be treated as no legacy writers")
	}
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != "" {
		t.Fatal("node claimed without a successful global writer inspection")
	}
}

func TestNodeProfileForeignOwnedLegacyWriterBlocksInvalidFinalization(t *testing.T) {
	f := newCleanupFixture(t, 1)
	local := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	if err := f.client.Delete(f.ctx, local, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		t.Fatal(err)
	}
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, local.Namespace, local.Name)
	foreignNamespace := createNamespace(t, f.ctx, f.client)
	cleanupLegacyDaemonSets(t, f.client, foreignNamespace)
	cfg := f.r.Config
	cfg.Namespace = foreignNamespace
	foreign := buildProfileDaemonSet(cfg, &f.profile, "agentpool", nil)
	foreign.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(&f.profile, nodev1alpha1.GroupVersion.WithKind("NodeProfile"))}
	foreign.Spec.Template.Spec.Affinity = profileAffinity(&f.profile, "agentpool", nil)
	legacyEnv(&foreign.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
	if err := f.client.Create(f.ctx, foreign); err != nil {
		t.Fatal(err)
	}
	before := foreign.ResourceVersion
	clearLegacyClaims(t, f)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	p.Spec.Registry = &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{"docker.io": "unapproved.example/cache"}}
	if err := f.client.Update(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, p.Name)
	p = getProfile(t, f.ctx, f.client, p.Name)
	if !containsString(p.Finalizers, brewlet.FinalizerCleanup) || !p.Status.Migrating ||
		len(p.Status.Targets) != 1 || conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
		t.Fatal("moving the operator namespace must not orphan its old UID-owned host state during invalid deletion")
	}
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil {
		t.Fatal(err)
	}
	if foreign.ResourceVersion != before || !foreign.DeletionTimestamp.IsZero() {
		t.Fatal("migration must neither patch nor delete foreign namespace workers")
	}
	f.assertNoCleanup(t)
}

func TestNodeProfileFencedForeignWriterBlocksCleanupAfterNamespaceMove(t *testing.T) {
	f := newCleanupFixture(t, 1)
	originalNamespace := f.r.Config.Namespace
	cleanupLegacyDaemonSets(t, f.client, originalNamespace)
	original := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	f.r.Config.Namespace = createNamespace(t, f.ctx, f.client)
	if err := f.client.Delete(f.ctx, &f.profile); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked ||
		!containsString(p.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatal("an initialized ledger must not hide a still-running writer in the old operator namespace")
	}
	var current appsv1.DaemonSet
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(original), &current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != original.ResourceVersion || !current.DeletionTimestamp.IsZero() {
		t.Fatal("a namespace move must not grant authority to mutate the old namespace")
	}
	f.assertNoCleanup(t)
}

func TestNodeProfileFencedNamespaceMoveWaitsForDaemonSetAndPodsBeforeProvisioning(t *testing.T) {
	f := newCleanupFixture(t, 1)
	originalNamespace := f.r.Config.Namespace
	cleanupLegacyDaemonSets(t, f.client, originalNamespace)
	original := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	pod := createDaemonSetPod(t, f.ctx, f.client, original, f.nodes[0], true)
	if !claimFenced(original) || !claimFencedPod(&pod.Spec) {
		t.Fatal("namespace-move regression must use modern, fully fenced workers")
	}
	generation := f.profile.Generation
	markNodeReady(t, f.ctx, f.client, f.nodes[0], f.profile.Name, generation)
	next := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
	f.r.Config.Namespace = createNamespace(t, f.ctx, f.client)
	cleanupLegacyDaemonSets(t, f.client, f.r.Config.Namespace)
	assertBlocked := func() {
		t.Helper()
		reconcileProfile(t, f.ctx, f.r, f.profile.Name)
		profile := getProfile(t, f.ctx, f.client, f.profile.Name)
		if profile.Generation != generation || conditionReason(profile.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
			t.Fatal("matching UID/generation/fence flags must not bypass the foreign-namespace writer barrier")
		}
		var created appsv1.DaemonSet
		if err := f.client.Get(f.ctx, types.NamespacedName{Namespace: f.r.Config.Namespace, Name: original.Name}, &created); !apierrors.IsNotFound(err) {
			t.Fatalf("new namespace published a provisioner before old workers disappeared: %v", err)
		}
		var node corev1.Node
		if err := f.client.Get(f.ctx, types.NamespacedName{Name: next}, &node); err != nil {
			t.Fatal(err)
		}
		if node.Labels[brewlet.LabelNodeOwner] != "" {
			t.Fatal("new target claim activated while old-namespace writers retain the same profile authority")
		}
		f.assertNoCleanup(t)
	}
	assertBlocked()
	var untouched appsv1.DaemonSet
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(original), &untouched); err != nil {
		t.Fatal(err)
	}
	if untouched.ResourceVersion != original.ResourceVersion {
		t.Fatal("namespace migration must not modify the foreign DaemonSet")
	}
	if err := f.client.Delete(f.ctx, original, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		t.Fatal(err)
	}
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, original.Namespace, original.Name)
	assertBlocked()
	var untouchedPod corev1.Pod
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), &untouchedPod); err != nil {
		t.Fatal(err)
	}
	if untouchedPod.ResourceVersion != pod.ResourceVersion || !untouchedPod.DeletionTimestamp.IsZero() {
		t.Fatal("a fenced foreign pod must block without receiving cross-namespace writes")
	}
	if err := f.client.Delete(f.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	f.daemonSet(t, original.Name)
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: next}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != string(f.profile.UID) {
		t.Fatal("provisioning did not resume after the original owner removed all foreign workers")
	}
}

func TestNodeProfileLegacyInventoryNeverSubstitutesReplacementUID(t *testing.T) {
	for _, beforeInventory := range []bool{false, true} {
		t.Run(map[bool]string{false: "after-inventory", true: "missing-before-inventory"}[beforeInventory], func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			legacyEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
			ds.Spec.Template.Spec.Affinity = profileAffinity(&f.profile, "agentpool", nil)
			if err := f.client.Update(f.ctx, ds); err != nil {
				t.Fatal(err)
			}
			createLegacyDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
			clearLegacyClaims(t, f)
			var old corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &old); err != nil {
				t.Fatal(err)
			}
			if !beforeInventory {
				reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			}
			if err := f.client.Delete(f.ctx, &old); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			replacement := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: old.Name, Labels: map[string]string{
				"agentpool": f.profile.Spec.NodePool.Names[0],
			}}}
			if err := f.client.Create(f.ctx, &replacement); err != nil {
				t.Fatal(err)
			}
			observeLegacyFence(t, f, ds.Name)
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if !p.Status.Migrating || len(p.Status.Targets) != 1 ||
				p.Status.Targets[0].UID == replacement.UID ||
				conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
				t.Fatal("migration replaced an original/unknown Node UID with a newer same-name Node")
			}
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&replacement), &replacement); err != nil {
				t.Fatal(err)
			}
			if replacement.Labels[brewlet.LabelNodeOwner] != "" || !f.daemonSet(t, ds.Name).DeletionTimestamp.IsZero() {
				t.Fatal("unproven node identity must not grant ownership or destroy original worker evidence")
			}
		})
	}
}

func TestNodeProfileLegacySchedulingFenceFailureKeepsEvidence(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	legacyEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
	ds.Spec.Template.Spec.Affinity = profileAffinity(&f.profile, "agentpool", nil)
	if err := f.client.Update(f.ctx, ds); err != nil {
		t.Fatal(err)
	}
	pod := createLegacyDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
	clearLegacyClaims(t, f)
	denied := errors.New("fence patch denied")
	f.r.Client = interceptNodePatchClient{Client: f.client, before: func(_ context.Context, obj client.Object) error {
		if _, ok := obj.(*appsv1.DaemonSet); ok {
			return denied
		}
		return nil
	}}
	result, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("fence failure should report blocked retry without destroying evidence: %v", err)
	}
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if !p.Status.Migrating || len(p.Status.Targets) != 1 || !strings.Contains(p.Status.Conditions[0].Message, denied.Error()) {
		t.Fatal("failed scheduling fence lost its durable target evidence or actionable status")
	}
	if !f.daemonSet(t, ds.Name).DeletionTimestamp.IsZero() {
		t.Fatal("DaemonSet was deleted before scheduling could be fenced")
	}
	var current corev1.Pod
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), &current); err != nil || !current.DeletionTimestamp.IsZero() {
		t.Fatalf("fence failure destroyed old pod evidence: %v", err)
	}
}
