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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func setProvisionerEnv(spec *corev1.PodSpec, name, value string) {
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
	f := newCleanupFixture(t, 1)
	original := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
	cleanupTestDaemonSets(t, f.client, original.Namespace)
	pod := createDaemonSetPod(t, f.ctx, f.client, original, f.nodes[0], false)
	f.r.Config.Namespace = createNamespace(t, f.ctx, f.client)
	if err := f.client.Delete(f.ctx, &f.profile); err != nil {
		t.Fatal(err)
	}
	reconcileProfile(t, f.ctx, f.r, f.profile.Name)
	p := getProfile(t, f.ctx, f.client, f.profile.Name)
	if conditionReason(p.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked || !containsString(p.Finalizers, brewlet.FinalizerCleanup) {
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
}

func TestNodeProfileStandaloneWorkerBlocksNewOwnership(t *testing.T) {
	f := newCleanupFixture(t, 0)
	ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name)).DeepCopy()
	ds.ObjectMeta = metav1.ObjectMeta{Name: brewlet.ProvisionerName, Namespace: f.r.Config.Namespace}
	delete(ds.Spec.Template.Labels, brewlet.LabelNodeProfile)
	delete(ds.Spec.Selector.MatchLabels, brewlet.LabelNodeProfile)
	setProvisionerEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_UID", "")
	setProvisionerEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_NAME", "")
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
