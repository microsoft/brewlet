package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestApplicationDeadlineWithRetainedOlderTaskIsFailedNotInstallingDeferred(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "a-app-failure")
	next := f.deployment(t, "z-next")
	ctx := context.Background()
	zero, one := intstr.FromInt32(0), intstr.FromInt32(1)
	d.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxUnavailable: &zero, MaxSurge: &one}}
	if err := f.engine.Client.Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	// Native zero-unavailable strategy retains the old healthy Pod while the
	// new application is Running but not Ready. This is not a runtime wait.
	old := new(corev1.Pod)
	if err := f.engine.Client.Get(ctx, client.ObjectKey{Name: "pod-a-app-failure", Namespace: "apps"}, old); err != nil {
		t.Fatal(err)
	}
	after := old.DeepCopy()
	after.Name = "after-a-app-failure"
	after.UID = "after-pod"
	after.ResourceVersion = ""
	after.Status.StartTime = timePtr(f.now)
	after.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	after.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", Ready: false, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(f.now)}}}}
	if err := f.engine.Client.Create(ctx, after); err != nil {
		t.Fatal(err)
	}
	f.image.Status.TaskObservations = []api.RuntimeTaskObservation{{NodeName: "node", NodeUID: "node", ProfileUID: "profile", ProfileGeneration: 1, ObservedAt: f.now, Current: true, Reason: "CompleteTaskAndLeaseObservation", Bindings: []api.RuntimeTaskBinding{{ContainerID: "old-task", ContainerdNamespace: "k8s.io", PodUID: old.UID, PodName: old.Name, PodNamespace: old.Namespace, InventoryUID: f.image.UID, Generation: 6, Digest: "old", State: "running"}}}}
	f.now = f.now.Add(11 * time.Minute)
	f.run(t)
	status := f.record(t, d.UID)
	if status.Phase != "Failed" || status.Reason != "RestartFailed" || strings.Contains(status.Message, "installation") || len(status.BlockedPods) == 0 {
		t.Fatalf("old task diagnostic misclassified app rollout failure: %+v", status)
	}
	if f.record(t, next.UID).Phase != "Rolling" || f.patches != 2 {
		t.Fatal("failed application deadline did not free its rollout slot")
	}
}
