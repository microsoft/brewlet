// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func setRolloutStatus(t *testing.T, c client.Client, key client.ObjectKey, status appsv1.DeploymentStatus) {
	t.Helper()
	ctx := testContext(t)
	var dep appsv1.Deployment
	if err := c.Get(ctx, key, &dep); err != nil {
		t.Fatal(err)
	}
	status.ObservedGeneration = dep.Generation
	dep.Status = status
	if err := c.Status().Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
}

func assertApplicationReady(t *testing.T, c client.Client, key client.ObjectKey, ready bool, replicas int32) {
	t.Helper()
	var app appsv1alpha1.JavaApplication
	if err := c.Get(testContext(t), key, &app); err != nil {
		t.Fatal(err)
	}
	condition := meta.FindStatusCondition(app.Status.Conditions, appsv1alpha1.ConditionReady)
	want := metav1.ConditionFalse
	if ready {
		want = metav1.ConditionTrue
	}
	if condition == nil || condition.Status != want {
		t.Fatalf("Ready condition = %+v, want %s", condition, want)
	}
	if condition.ObservedGeneration != app.Generation || app.Status.ObservedGeneration != app.Generation {
		t.Fatalf("status does not describe current application generation: %+v", app.Status)
	}
	if app.Status.ReadyReplicas != replicas {
		t.Fatalf("readyReplicas=%d, want observed count %d", app.Status.ReadyReplicas, replicas)
	}
}

func TestJavaApplicationWaitsForCurrentRollout(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)
	app := newJavaApp(ns, "rollout")
	app.Spec.Artifact.Image = "registry.example.com/app@sha256:" + strings.Repeat("1", 64)
	if err := c.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(app)
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	complete := appsv1.DeploymentStatus{Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2}
	setRolloutStatus(t, c, key, complete)
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, true, 2)

	if err := c.Get(ctx, key, app); err != nil {
		t.Fatal(err)
	}
	app.Spec.Artifact.Image = "registry.example.com/app@sha256:" + strings.Repeat("2", 64)
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, false, 2)

	failed := appsv1.DeploymentStatus{
		Replicas: 2, ReadyReplicas: 2, AvailableReplicas: 2,
		Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded", Message: "replacement image cannot start",
		}},
	}
	setRolloutStatus(t, c, key, failed)
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, false, 2)

	for _, status := range []appsv1.DeploymentStatus{
		{Replicas: 3, UpdatedReplicas: 1, ReadyReplicas: 2, AvailableReplicas: 2},
		{Replicas: 3, UpdatedReplicas: 2, ReadyReplicas: 3, AvailableReplicas: 3},
		{Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 1},
	} {
		setRolloutStatus(t, c, key, status)
		reconcileJavaApp(t, ctx, r, ns, app.Name)
		assertApplicationReady(t, c, key, false, status.ReadyReplicas)
	}
	setRolloutStatus(t, c, key, complete)
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, true, 2)
	if err := c.Get(ctx, key, app); err != nil {
		t.Fatal(err)
	}
	version := app.ResourceVersion
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	if err := c.Get(ctx, key, app); err != nil {
		t.Fatal(err)
	}
	if app.ResourceVersion != version {
		t.Fatal("unchanged completed rollout caused another status write")
	}
}

type laggingDeploymentClient struct {
	client.Client
	snapshot *appsv1.Deployment
	reads    int
}

func (c *laggingDeploymentClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if dep, ok := obj.(*appsv1.Deployment); ok && key == client.ObjectKeyFromObject(c.snapshot) {
		c.snapshot.DeepCopyInto(dep)
		c.reads++
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestJavaApplicationReadinessDoesNotReusePreUpdateCache(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)
	app := newJavaApp(ns, "cached-rollout")
	if err := c.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(app)
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	setRolloutStatus(t, c, key, appsv1.DeploymentStatus{
		Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2,
	})
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	var old appsv1.Deployment
	if err := c.Get(ctx, key, &old); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, app); err != nil {
		t.Fatal(err)
	}
	app.Spec.JVM.Args = []string{"-Xmx128m"}
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	cache := &laggingDeploymentClient{Client: c, snapshot: &old}
	r.Client = cache
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	var current appsv1.Deployment
	if err := c.Get(ctx, key, &current); err != nil {
		t.Fatal(err)
	}
	if current.Generation <= old.Generation || cache.reads == 0 {
		t.Fatal("fixture did not exercise a stale read after updating the Deployment")
	}
	assertApplicationReady(t, c, key, false, 2)

	// Even clients constructed without a separate reader must not certify a
	// cached revision older than the successful Deployment write.
	r.APIReader = nil
	if err := c.Get(ctx, key, app); err != nil {
		t.Fatal(err)
	}
	if err := r.updateStatus(ctx, app, &current); err != nil {
		t.Fatal(err)
	}
	assertApplicationReady(t, c, key, false, 2)
}

func TestJavaApplicationReadinessFollowsHPATarget(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)
	app := newJavaApp(ns, "autoscaled")
	app.Spec.Autoscaling.Enabled = true
	app.Spec.Autoscaling.MaxReplicas = 6
	if err := c.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(app)
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	setRolloutStatus(t, c, key, appsv1.DeploymentStatus{
		Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
	})
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, true, 1)

	var dep appsv1.Deployment
	if err := c.Get(ctx, key, &dep); err != nil {
		t.Fatal(err)
	}
	four := int32(4)
	dep.Spec.Replicas = &four
	if err := c.Update(ctx, &dep); err != nil {
		t.Fatal(err)
	}
	setRolloutStatus(t, c, key, appsv1.DeploymentStatus{
		Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
	})
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, false, 1)
	setRolloutStatus(t, c, key, appsv1.DeploymentStatus{
		Replicas: 4, UpdatedReplicas: 4, ReadyReplicas: 4, AvailableReplicas: 4,
	})
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, true, 4)
	if err := c.Get(ctx, key, &dep); err != nil {
		t.Fatal(err)
	}
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != four {
		t.Fatal("readiness reconciliation changed the HPA replica target")
	}
}

func TestJavaApplicationReadinessWaitsForScaleToZero(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)
	app := newJavaApp(ns, "scale-zero")
	if err := c.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	key := client.ObjectKeyFromObject(app)
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	setRolloutStatus(t, c, key, appsv1.DeploymentStatus{
		Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2,
	})
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, true, 2)

	if err := c.Get(ctx, key, app); err != nil {
		t.Fatal(err)
	}
	zero := int32(0)
	app.Spec.Replicas = &zero
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, false, 2)
	setRolloutStatus(t, c, key, appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1})
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, false, 1)
	setRolloutStatus(t, c, key, appsv1.DeploymentStatus{
		Conditions: []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded", Message: "earlier rollout timed out",
		}},
	})
	reconcileJavaApp(t, ctx, r, ns, app.Name)
	assertApplicationReady(t, c, key, true, 0)
}

type deploymentReadHook struct {
	client.Reader
	beforeRead func(context.Context, client.ObjectKey) error
}

func (r deploymentReadHook) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*appsv1.Deployment); ok {
		if err := r.beforeRead(ctx, key); err != nil {
			return err
		}
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestJavaApplicationReadinessRejectsConcurrentDeploymentChanges(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*appsv1.Deployment)
		ready   int32
		message string
	}{
		{"template", func(dep *appsv1.Deployment) {
			dep.Spec.Template.Spec.Containers[0].Image = "registry.example.com/unexpected@sha256:" + strings.Repeat("3", 64)
		}, 2, "pod template"},
		{"replica target", func(dep *appsv1.Deployment) {
			three := int32(3)
			dep.Spec.Replicas = &three
		}, 3, "replica target"},
		{"ownership", func(dep *appsv1.Deployment) {
			dep.OwnerReferences = nil
		}, 0, "ownership"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := requireEnvtest(t)
			ctx := testContext(t)
			ns := createNamespace(t, ctx, c)
			r := newJavaAppReconciler(c)
			app := newJavaApp(ns, "changed")
			if err := c.Create(ctx, app); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKeyFromObject(app)
			reconcileJavaApp(t, ctx, r, ns, app.Name)
			setRolloutStatus(t, c, key, appsv1.DeploymentStatus{
				Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2,
			})
			r.APIReader = deploymentReadHook{Reader: c, beforeRead: func(ctx context.Context, key client.ObjectKey) error {
				var dep appsv1.Deployment
				if err := c.Get(ctx, key, &dep); err != nil {
					return err
				}
				tt.mutate(&dep)
				if err := c.Update(ctx, &dep); err != nil {
					return err
				}
				want := *dep.Spec.Replicas
				dep.Status = appsv1.DeploymentStatus{
					ObservedGeneration: dep.Generation, Replicas: want, UpdatedReplicas: want,
					ReadyReplicas: want, AvailableReplicas: want,
				}
				return c.Status().Update(ctx, &dep)
			}}
			reconcileJavaApp(t, ctx, r, ns, app.Name)
			assertApplicationReady(t, c, key, false, tt.ready)
			if err := c.Get(ctx, key, app); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(app.Status.Conditions, appsv1alpha1.ConditionReady)
			if !strings.Contains(condition.Message, tt.message) {
				t.Fatalf("missing mismatch diagnostic: %+v", condition)
			}
		})
	}
}

func TestJavaApplicationReadinessPropagatesReadFailure(t *testing.T) {
	c := requireEnvtest(t)
	ctx := testContext(t)
	ns := createNamespace(t, ctx, c)
	r := newJavaAppReconciler(c)
	app := newJavaApp(ns, "unreadable")
	if err := c.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	expected := errors.New("authoritative Deployment read unavailable")
	r.APIReader = deploymentReadHook{Reader: c, beforeRead: func(context.Context, client.ObjectKey) error {
		return expected
	}}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if !errors.Is(err, expected) {
		t.Fatalf("Reconcile error=%v, want read failure", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	if condition := meta.FindStatusCondition(app.Status.Conditions, appsv1alpha1.ConditionReady); condition != nil &&
		condition.Status == metav1.ConditionTrue {
		t.Fatalf("failed read manufactured Ready: %+v", condition)
	}
}
