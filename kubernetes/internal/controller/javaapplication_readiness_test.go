// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"strings"
	"testing"

	appsv1alpha1 "brewlet-operator/api/v1alpha1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDeploymentReadyRequiresCurrentRollout(t *testing.T) {
	tests := []struct {
		name   string
		change func(*appsv1.Deployment)
		ready  bool
	}{
		{"complete rollout", func(*appsv1.Deployment) {}, true},
		{"not observed", func(d *appsv1.Deployment) { d.Status.ObservedGeneration = 1 }, false},
		{"old healthy replicas", func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 0 }, false},
		{"partly updated", func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 1 }, false},
		{"surge still present", func(d *appsv1.Deployment) { d.Status.Replicas = 3 }, false},
		{"not all ready", func(d *appsv1.Deployment) { d.Status.ReadyReplicas = 1 }, false},
		{"minimum ready time pending", func(d *appsv1.Deployment) {
			d.Spec.MinReadySeconds = 30
			d.Status.AvailableReplicas = 1
		}, false},
		{"unavailable replicas", func(d *appsv1.Deployment) { d.Status.UnavailableReplicas = 1 }, false},
		{"terminating deployment", func(d *appsv1.Deployment) {
			now := metav1.Now()
			d.DeletionTimestamp = &now
		}, false},
		{"deadline exceeded", func(d *appsv1.Deployment) {
			d.Status.UpdatedReplicas = 0
			d.Status.Conditions = []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "ProgressDeadlineExceeded", Message: "new ReplicaSet did not become available",
			}}
		}, false},
		{"paused but already complete", func(d *appsv1.Deployment) { d.Spec.Paused = true }, true},
		{"zero replicas complete", func(d *appsv1.Deployment) {
			*d.Spec.Replicas = 0
			d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation}
		}, true},
		{"zero complete after an earlier timeout", func(d *appsv1.Deployment) {
			*d.Spec.Replicas = 0
			d.Status = appsv1.DeploymentStatus{
				ObservedGeneration: d.Generation,
				Conditions: []appsv1.DeploymentCondition{{
					Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
					Reason: "ProgressDeadlineExceeded", Message: "earlier rollout timed out",
				}},
			}
		}, true},
		{"zero still has old replicas", func(d *appsv1.Deployment) {
			*d.Spec.Replicas = 0
			d.Status.UpdatedReplicas = 0
		}, false},
		{"zero not observed", func(d *appsv1.Deployment) {
			*d.Spec.Replicas = 0
			d.Status = appsv1.DeploymentStatus{}
		}, false},
		{"default one replica", func(d *appsv1.Deployment) {
			d.Spec.Replicas = nil
			d.Status = appsv1.DeploymentStatus{
				ObservedGeneration: d.Generation, Replicas: 1,
				UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1,
			}
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			two := int32(2)
			dep := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 2},
				Spec:       appsv1.DeploymentSpec{Replicas: &two},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 2,
					ReadyReplicas: 2, AvailableReplicas: 2,
				},
			}
			tt.change(dep)
			ready, reason, message := deploymentReady(dep, true)
			if ready != tt.ready {
				t.Fatalf("ready=%t, want %t (%s: %s)", ready, tt.ready, reason, message)
			}
			wantReason := appsv1alpha1.ReasonProgressing
			if tt.ready {
				wantReason = appsv1alpha1.ReasonReconciled
			}
			if reason != wantReason || strings.TrimSpace(message) == "" {
				t.Fatalf("condition = %s/%q, want %s with a diagnostic", reason, message, wantReason)
			}
		})
	}
}
