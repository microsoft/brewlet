// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func TestRemovedContainerdPolicyRetainedState(t *testing.T) {
	requireEnvtest(t)
	raw, err := os.ReadFile("../../deploy/nodeprofile-crd.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var strict, old apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &strict); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(strings.ReplaceAll(string(raw),
		"enum: [validated, none]", "enum: [validated, sighup, none]")), &old); err != nil {
		t.Fatal(err)
	}
	// Only this private API accepts the old enum to seed incompatible state.
	// Refusal after tightening is a safety test, not upgrade support.
	env := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{&old}}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	c, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newProfileReconciler(c, ns)
	r.APIReader = c
	var profiles []nodev1alpha1.NodeProfile
	for _, field := range []string{"provisioningSpec", "targets", "retirement.spec", "retirement.targets"} {
		for _, deleting := range []bool{false, true} {
			pool := uniqueName("removed-policy")
			createNode(t, ctx, c, map[string]string{"agentpool": pool})
			p := createProfile(t, ctx, c, uniqueName("removed-policy"), nodev1alpha1.NodeProfileSpec{
				NodePool: nodev1alpha1.NodePoolRef{Key: "agentpool", Names: []string{pool}},
				JDKs:     []nodev1alpha1.JDKRef{jdk("temurin", 21)},
			})
			reconcileProfile(t, ctx, r, p.Name)
			saved := getProfile(t, ctx, c, p.Name)
			// A valid current request must not erase unsupported host history.
			saved.Spec.Rollout.ContainerdRestart = "none"
			if err := c.Update(ctx, &saved); err != nil {
				t.Fatal(err)
			}
			setStoredRestartPolicy(&saved, field, "sighup")
			if err := c.Status().Update(ctx, &saved); err != nil {
				t.Fatal(err)
			}
			if deleting {
				if err := c.Delete(ctx, &saved); err != nil {
					t.Fatal(err)
				}
			}
			profiles = append(profiles, getProfile(t, ctx, c, p.Name))
		}
	}
	extensions, err := apiextensionsclient.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	crds := extensions.ApiextensionsV1().CustomResourceDefinitions()
	current, err := crds.Get(ctx, strict.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	current.Spec = strict.Spec
	if _, err := crds.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		p := profileNamed(uniqueName("rejected-mode"), []string{"rejected"}, jdk("temurin", 21))
		p.Spec.Rollout.ContainerdRestart = "sighup"
		err := c.Create(ctx, &p)
		if apierrors.IsInvalid(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, c.Delete(ctx, &p)
	}); err != nil {
		t.Fatalf("new schema did not reject removed policy: %v", err)
	}
	for _, before := range profiles {
		t.Run(before.Name, func(t *testing.T) {
			for attempt := 0; attempt < 2; attempt++ {
				restarted := newProfileReconciler(c, ns)
				restarted.APIReader = c
				_, err := restarted.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: before.Name}})
				if err != nil && (!apierrors.IsInvalid(err) || !strings.Contains(err.Error(), "sighup")) {
					t.Fatalf("unexpected refusal error: %v", err)
				}
				select {
				case event := <-restarted.Recorder.(*record.FakeRecorder).Events:
					if !strings.Contains(event, "CleanupBlocked") ||
						!strings.Contains(event, `"sighup" is invalid; want one of validated|none`) {
						t.Fatalf("missing blocked-policy validation error: %s", event)
					}
				default:
					t.Fatal("missing blocked-policy warning event")
				}
				after := getProfile(t, ctx, c, before.Name)
				if !reflect.DeepEqual(before.Status.Targets, after.Status.Targets) ||
					!reflect.DeepEqual(before.Status.ProvisioningSpec, after.Status.ProvisioningSpec) ||
					!reflect.DeepEqual(before.Status.Retirement, after.Status.Retirement) ||
					!reflect.DeepEqual(before.Finalizers, after.Finalizers) {
					t.Fatal("refusal changed cleanup obligations or finalizers")
				}
				if err == nil && conditionReason(after.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked {
					t.Fatal("successful status write did not report CleanupBlocked")
				}
				var worker appsv1.DaemonSet
				if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.ProfileDaemonSetName(before.Name)}, &worker); err != nil || !worker.DeletionTimestamp.IsZero() {
					t.Fatalf("refusal destroyed worker evidence: %v", err)
				}
				if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: brewlet.CleanupDaemonSetName(before.Name)}, &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
					t.Fatalf("refusal authorized cleanup: %v", err)
				}
				var node corev1.Node
				if err := c.Get(ctx, types.NamespacedName{Name: before.Status.Targets[0].Name}, &node); err != nil {
					t.Fatal(err)
				}
				if !nodeClaimedBy(&node, &before, before.Status.Targets[0]) {
					t.Fatal("refusal released node ownership")
				}
			}
		})
	}
}

func TestContainerdRestartCRDValidation(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	for _, mode := range []string{"", "validated", "none", "sighup", "reboot"} {
		t.Run(mode, func(t *testing.T) {
			p := profileNamed(uniqueName("restart-schema"), []string{"schema"}, jdk("temurin", 21))
			p.Spec.Rollout.ContainerdRestart = mode
			err := c.Create(ctx, &p)
			if mode == "sighup" || mode == "reboot" {
				if !apierrors.IsInvalid(err) {
					t.Fatalf("schema accepted %q: %v", mode, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Delete(context.Background(), &p) })
			p.Spec.Rollout.ContainerdRestart = "sighup"
			if err := c.Update(ctx, &p); !apierrors.IsInvalid(err) {
				t.Fatalf("schema accepted removed mode update: %v", err)
			}
			for _, field := range []string{"targets", "retirement.targets"} {
				saved := getProfile(t, ctx, c, p.Name)
				setStoredRestartPolicy(&saved, field, "sighup")
				if err := c.Status().Update(ctx, &saved); !apierrors.IsInvalid(err) {
					t.Fatalf("schema accepted %s removed policy: %v", field, err)
				}
			}
		})
	}
}

func TestFencedWorkerRemovedPolicyBlocksCleanup(t *testing.T) {
	for _, worker := range []string{"daemonset", "pod"} {
		t.Run(worker, func(t *testing.T) {
			f := newCleanupFixture(t, 1)
			ds := f.daemonSet(t, brewlet.ProfileDaemonSetName(f.profile.Name))
			setProvisionerEnv(&ds.Spec.Template.Spec, "BREWLET_CONTAINERD_RESTART", "sighup")
			var pod *corev1.Pod
			if worker == "daemonset" {
				if err := f.client.Update(f.ctx, ds); err != nil {
					t.Fatal(err)
				}
			} else {
				pod = createDaemonSetPod(t, f.ctx, f.client, ds, f.nodes[0], false)
			}
			before := getProfile(t, f.ctx, f.client, f.profile.Name)
			for _, deleting := range []bool{false, true} {
				if deleting {
					if err := f.client.Delete(f.ctx, &before); err != nil {
						t.Fatal(err)
					}
				}
				f.r = newProfileReconciler(f.client, ds.Namespace)
				f.r.APIReader = f.client
				reconcileProfile(t, f.ctx, f.r, before.Name)
				after := getProfile(t, f.ctx, f.client, before.Name)
				if conditionReason(after.Status.Conditions) != nodev1alpha1.ReasonCleanupBlocked ||
					!reflect.DeepEqual(before.Status.Targets, after.Status.Targets) ||
					!reflect.DeepEqual(before.Finalizers, after.Finalizers) {
					t.Fatal("unsupported worker policy did not preserve blocked cleanup")
				}
				if !f.daemonSet(t, ds.Name).DeletionTimestamp.IsZero() {
					t.Fatal("unsupported worker evidence was deleted")
				}
				if pod != nil {
					var saved corev1.Pod
					if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(pod), &saved); err != nil || !saved.DeletionTimestamp.IsZero() {
						t.Fatalf("unsupported pod evidence was deleted: %v", err)
					}
				}
				f.assertNoCleanup(t)
			}
		})
	}
}
