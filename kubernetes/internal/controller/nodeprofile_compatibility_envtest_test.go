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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func preClaimEnv(spec *corev1.PodSpec, name, value string) {
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

func clearPreClaimOwnership(t *testing.T, f *cleanupFixture) {
	t.Helper()
	cleanupTestDaemonSets(t, f.client, f.r.Config.Namespace)
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

func cleanupTestDaemonSets(t *testing.T, c client.Client, namespace string) {
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
			if err := c.Update(ctx, ds); err != nil {
				t.Error(err)
			}
			if err := c.Delete(ctx, ds, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				t.Error(err)
			}
		}
	})
}

func requirePreClaimRefusal(t *testing.T, f *cleanupFixture) nodev1alpha1.NodeProfile {
	t.Helper()
	result := reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if result.RequeueAfter == 0 || conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonUnsupportedPreClaimState ||
		!containsString(p.Finalizers, brewlet.FinalizerCleanup) {
		t.Fatalf("expected persistent, finalizer-held pre-claim refusal: %+v", p.Status)
	}
	if !strings.Contains(p.Status.Conditions[0].Message, "original release") {
		t.Fatal("refusal must explain safe original-release recovery")
	}
	f.assertNoCleanup(t)
	return p
}

func TestNodeProfilePreClaimRefusalPreservesWorkersAcrossLifecycle(t *testing.T) {
	for _, lifecycle := range []string{"valid", "invalid", "deleting", "invalid-deleting", "empty-pool"} {
		t.Run(lifecycle, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			preClaimEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
			if err := f.client.Update(f.ctx, ds); err != nil {
				t.Fatal(err)
			}
			pod := createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
			clearPreClaimOwnership(t, f)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			if strings.Contains(lifecycle, "invalid") {
				p.Spec.Registry = &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{"docker.io": "unapproved.example/cache"}}
			}
			if lifecycle == "empty-pool" {
				p.Spec.NodePool.Names = []string{"no-matching-nodes"}
			}
			if err := f.client.Update(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(lifecycle, "deleting") {
				if err := f.client.Delete(f.ctx, &p); err != nil {
					t.Fatal(err)
				}
			}
			p = requirePreClaimRefusal(t, f)
			if p.Status.Migrating || len(p.Status.MigrationDaemonSetUIDs) != 0 ||
				len(p.Status.Targets) != 0 || p.Status.OwnershipInitialized || p.Status.ProvisioningSpec != nil {
				t.Fatalf("refusal manufactured migration or host authority: %+v", p.Status)
			}
			if current := f.daemonSet(t, ds.Name); current.ResourceVersion != ds.ResourceVersion {
				t.Fatal("refusal changed pre-claim scheduling, deletion, or evidence")
			}
			var current corev1.Pod
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), &current); err != nil {
				t.Fatal(err)
			}
			if current.ResourceVersion != pod.ResourceVersion {
				t.Fatal("refusal changed pre-claim pod evidence")
			}
			var node corev1.Node
			if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
				t.Fatal(err)
			}
			if node.Labels[brewlet.LabelNodeOwner] != "" {
				t.Fatal("refusal adopted a pre-claim host")
			}
		})
	}
}

func TestNodeProfilePreClaimAdvertisementsRemainBlockedAfterEvidenceDisappears(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	if err := f.client.Delete(f.ctx, ds, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
		t.Fatal(err)
	}
	completeForegroundDaemonSetDeletion(t, f.ctx, f.client, ds.Namespace, ds.Name)
	markNodeReady(t, f.ctx, f.client, f.nodes[0], f.profile.Name, f.profile.Generation)
	clearPreClaimOwnership(t, f)
	before := requirePreClaimRefusal(t, f)
	updateTargetNode(t, f, f.nodes[0], removeNodeAdvertisements)
	f.r = newProfileReconciler(f.client, f.r.Config.Namespace)
	f.r.APIReader = f.client
	p := requirePreClaimRefusal(t, f)
	if !reflect.DeepEqual(before.Status.Conditions, p.Status.Conditions) {
		t.Fatal("restart or evidence disappearance lost the original refusal diagnostic")
	}
	if len(p.Status.Targets) != 0 {
		t.Fatal("advertisements must not infer cleanup authority")
	}
	other := createProfile(t, f.ctx, f.client, uniqueName("competitor"), f.profile.Spec)
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &node); err != nil {
		t.Fatal(err)
	}
	if err := f.r.claimTarget(f.ctx, other, nodev1alpha1.NodeTarget{Name: node.Name, UID: node.UID}); err == nil {
		t.Fatal("another profile adopted a host with unresolved historical obligations")
	}
	if err := f.client.Delete(f.ctx, &p); err != nil {
		t.Fatal(err)
	}
	requirePreClaimRefusal(t, f)
}

func TestNodeProfileRetainsInertMigrationEvidence(t *testing.T) {
	for _, evidence := range []string{"migrating", "daemonset-uids", "unknown-target", "old-condition"} {
		t.Run(evidence, func(t *testing.T) {
			f := newCleanupFixture(t, 0)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			switch evidence {
			case "migrating":
				p.Status.Migrating = true
			case "daemonset-uids":
				p.Status.MigrationDaemonSetUIDs = []types.UID{"vanished-worker-uid"}
			case "unknown-target":
				p.Status.Targets = []nodev1alpha1.NodeTarget{{Name: "old-host"}}
			case "old-condition":
				p.Status.Conditions[0].Reason = "OwnershipMigration"
			}
			if err := f.client.Status().Update(f.ctx, &p); err != nil {
				t.Fatal(err)
			}
			before := p.Status.DeepCopy()
			current := requirePreClaimRefusal(t, f)
			if current.Status.Migrating != before.Migrating ||
				!reflect.DeepEqual(current.Status.MigrationDaemonSetUIDs, before.MigrationDaemonSetUIDs) ||
				!reflect.DeepEqual(current.Status.Targets, before.Targets) {
				t.Fatal("refusal changed inert evidence")
			}
			if !HasNodeProfileCleanupObligations(&current) {
				t.Fatal("inert evidence must prevent no-host finalization")
			}
		})
	}
}

func TestNodeProfilePreClaimPendingAndOrphanPodsAreNotAdopted(t *testing.T) {
	for _, state := range []string{"pending", "orphan", "terminating"} {
		t.Run(state, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			preClaimEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
			pod := createDaemonSetPod(t, f.ctx, f.client, ds, "", false)
			if state == "orphan" {
				pod.OwnerReferences = nil
			}
			if state == "terminating" {
				pod.Finalizers = []string{"test.brewlet.sh/hold"}
			}
			if err := f.client.Update(f.ctx, pod); err != nil {
				t.Fatal(err)
			}
			if state == "terminating" {
				if err := f.client.Delete(f.ctx, pod); err != nil {
					t.Fatal(err)
				}
				if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
					t.Fatal(err)
				}
			}
			clearPreClaimOwnership(t, f)
			requirePreClaimRefusal(t, f)
			var current corev1.Pod
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), &current); err != nil {
				t.Fatal(err)
			}
			if current.ResourceVersion != pod.ResourceVersion {
				t.Fatal("pending, orphaned, or terminating evidence was modified")
			}
		})
	}
}

type denyGlobalWorkerList struct {
	client.Reader
	pods bool
}

func (r denyGlobalWorkerList) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	_, podList := list.(*corev1.PodList)
	_, dsList := list.(*appsv1.DaemonSetList)
	if (r.pods && podList) || (!r.pods && dsList) {
		return errors.New("cluster-wide worker list denied")
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestNodeProfileGlobalWorkerReadFailurePreventsClaimsAndTeardown(t *testing.T) {
	for _, pods := range []bool{false, true} {
		for _, deleting := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "daemonsets", true: "pods"}[pods], map[bool]string{false: "active", true: "deleting"}[deleting]}, "/"), func(t *testing.T) {
				f := newCleanupFixture(t, 0)
				name := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
				ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
				if deleting {
					if err := f.client.Delete(f.ctx, &f.profile); err != nil {
						t.Fatal(err)
					}
				}
				f.r.APIReader = denyGlobalWorkerList{Reader: f.client, pods: pods}
				reconcileProfile(t, f.ctx, f.r, f.profile.Name)
				p := getProfile(t, f.ctx, f.client, f.profile.Name)
				if !strings.Contains(p.Status.Conditions[0].Message, "cluster-wide worker list denied") {
					t.Fatal("read errors must be actionable")
				}
				if f.daemonSet(t, ds.Name).ResourceVersion != ds.ResourceVersion {
					t.Fatal("read failure permitted worker teardown or publication")
				}
				var node corev1.Node
				if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
					t.Fatal(err)
				}
				if node.Labels[brewlet.LabelNodeOwner] != "" {
					t.Fatal("read failure permitted a claim")
				}
			})
		}
	}
}

func TestNodeProfileForeignWritersBlockWithoutMutation(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		t.Run(map[bool]string{false: "pre-claim", true: "current"}[fenced], func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			original := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			cleanupTestDaemonSets(t, f.client, original.Namespace)
			if !fenced {
				preClaimEnv(&original.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
				if err := f.client.Update(f.ctx, original); err != nil {
					t.Fatal(err)
				}
			}
			pod := createDaemonSetPod(t, f.ctx, f.client, original, f.nodes[0], false)
			f.r.Config.Namespace = createNamespace(t, f.ctx, f.client)
			if err := f.client.Delete(f.ctx, &f.profile); err != nil {
				t.Fatal(err)
			}
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			p := getProfile(t, f.ctx, f.client, f.profile.Name)
			want := nodev1alpha1.ReasonCleanupBlocked
			if !fenced {
				want = nodev1alpha1.ReasonUnsupportedPreClaimState
			}
			if conditionReason(p.Status.Conditions) != want || !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
				t.Fatalf("namespace move did not block: %+v", p.Status)
			}
			var current appsv1.DaemonSet
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(original), &current); err != nil {
				t.Fatal(err)
			}
			if current.ResourceVersion != original.ResourceVersion {
				t.Fatal("namespace move mutated the original worker")
			}
			if err := f.client.Delete(f.ctx, original, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil {
				t.Fatal(err)
			}
			completeForegroundDaemonSetDeletion(t, f.ctx, f.client, original.Namespace, original.Name)
			reconcileProfile(t, f.ctx, f.r, f.profile.Name)
			f.assertNoCleanup(t)
			var currentPod corev1.Pod
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), &currentPod); err != nil {
				t.Fatal(err)
			}
			if currentPod.ResourceVersion != pod.ResourceVersion {
				t.Fatal("foreign pod was mutated after its DaemonSet disappeared")
			}
		})
	}
}

func TestNodeProfileStandaloneWorkerBlocksNewOwnership(t *testing.T) {
	f := newCleanupFixture(t, 0)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name)).DeepCopy()
	ds.ObjectMeta = metav1.ObjectMeta{Name: brewlet.ProvisionerName, Namespace: f.r.Config.Namespace}
	delete(ds.Spec.Template.Labels, brewlet.LabelNodeProfile)
	delete(ds.Spec.Selector.MatchLabels, brewlet.LabelNodeProfile)
	preClaimEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
	preClaimEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_UID", "")
	preClaimEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_NAME", "")
	if err := f.client.Create(f.ctx, ds); err != nil {
		t.Fatal(err)
	}
	cleanupTestDaemonSets(t, f.client, ds.Namespace)
	name := createNode(t, f.ctx, f.client, map[string]string{"agentpool": f.profile.Spec.NodePool.Names[0]})
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	if current := f.daemonSet(t, ds.Name); current.ResourceVersion != ds.ResourceVersion {
		t.Fatal("standalone worker was adopted or removed")
	}
	var node corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: name}, &node); err != nil {
		t.Fatal(err)
	}
	if node.Labels[brewlet.LabelNodeOwner] != "" {
		t.Fatal("standalone conflict permitted a claim")
	}
}

func TestNodeProfilePreClaimRefusalWriteFailurePreservesEvidence(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	preClaimEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
	if err := f.client.Update(f.ctx, ds); err != nil {
		t.Fatal(err)
	}

	cleanupTestDaemonSets(t, f.client, ds.Namespace)
	denied := errors.New("refusal status denied")
	f.r.Client = interceptProfileStatusClient{Client: f.client, update: func(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
		return denied
	}}
	if _, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&f.profile)}); !errors.Is(err, denied) {
		t.Fatalf("refusal write failure must propagate: %v", err)
	}
	if f.daemonSet(t, ds.Name).ResourceVersion != ds.ResourceVersion {
		t.Fatal("failed refusal persistence changed workers")
	}
	var cleanup appsv1.DaemonSet
	if err := f.client.Get(f.ctx, client.ObjectKey{Namespace: ds.Namespace, Name: brewlet.CleanupDaemonSetName(f.profile.Name)}, &cleanup); !apierrors.IsNotFound(err) {
		t.Fatal("failed refusal persistence started cleanup")
	}
}

func TestNodeProfilePreClaimEvidenceNeverAdoptsReplacementNode(t *testing.T) {
	f := newCleanupFixture(t, 1)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	preClaimEnv(&ds.Spec.Template.Spec, "BREWLET_REQUIRE_NODE_CLAIM", "")
	if err := f.client.Update(f.ctx, ds); err != nil {
		t.Fatal(err)
	}
	clearPreClaimOwnership(t, f)
	requirePreClaimRefusal(t, f)
	var old corev1.Node
	if err := f.client.Get(f.ctx, types.NamespacedName{Name: f.nodes[0]}, &old); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(f.ctx, &old); err != nil {
		t.Fatal(err)
	}
	replacement := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: old.Name, Labels: map[string]string{
		"agentpool": f.profile.Spec.NodePool.Names[0],
	}}}
	if err := f.client.Create(f.ctx, &replacement); err != nil {
		t.Fatal(err)
	}
	p := requirePreClaimRefusal(t, f)
	if len(p.Status.Targets) != 0 || f.daemonSet(t, ds.Name).ResourceVersion != ds.ResourceVersion {
		t.Fatal("replacement Node caused historical inventory or worker mutation")
	}
	if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(&replacement), &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.UID == old.UID || replacement.Labels[brewlet.LabelNodeOwner] != "" {
		t.Fatal("pre-claim state adopted a replacement Node")
	}
}
