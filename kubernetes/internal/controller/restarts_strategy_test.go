package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRestartBarrierRejectsAmbiguousMembersWithoutLoweringThem(t *testing.T) {
	for _, raw := range []string{
		`{"id":"foreign","id":"runtime","inventoryUID":"epoch","generation":7}`,
		`{"id":"runtime","inventoryUID":"old","inventoryUID":"epoch","generation":7}`,
		`{"id":"runtime","inventoryUID":"epoch","generation":99,"generation":7}`,
		`{"id":"runtime","inventoryUID":"epoch","generation":99,"generat\u0069on":7}`,
	} {
		if _, err := barrierValue(raw, "runtime", "epoch"); err == nil ||
			!strings.HasPrefix(err.Error(), "InvalidRuntimeGeneration") {
			t.Fatalf("ambiguous barrier was interpreted as a normal rotation: %s: %v", raw, err)
		}
	}
	barrier, err := barrierValue(`{"id":"runtime","inventoryUID":"epoch","generation":9007199254740993}`,
		"runtime", "epoch")
	if err != nil || barrier.Generation != 9007199254740993 {
		t.Fatal("restart barrier rounded exact generation", barrier, err)
	}
}

func TestRestartBlockersRetainNeverStartedRuntimeFailureDuringBackoff(t *testing.T) {
	for _, role := range []string{"main", "init", "ephemeral"} {
		t.Run(role, func(t *testing.T) {
			status := corev1.ContainerStatus{Name: "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "StartError", ExitCode: 128, StartedAt: metav1.NewTime(time.Unix(0, 0)),
					Message: "failed to create task: RuntimeImageInstalling: example"}}}
			pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "blocked", Namespace: "apps", UID: "pod-one"},
				Spec: corev1.PodSpec{NodeName: "cold-node"}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
			switch role {
			case "main":
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
			case "init":
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
			case "ephemeral":
				pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{status}
			}
			image := &api.RuntimeImage{Status: api.RuntimeImageStatus{Generation: 2}}
			blocked := restartBlockers([]corev1.Pod{pod}, image)
			if len(blocked) != 1 || blocked[0].Reason != "RuntimeImageInstalling" ||
				blocked[0].UID != pod.UID || blocked[0].Node != "cold-node" ||
				blocked[0].MinimumGeneration != 2 {
				t.Fatalf("real Create backoff lost runtime blocker: %+v", blocked)
			}
		})
	}
}

func TestRestartBlockersIgnoreStaleFailureAfterRealExecution(t *testing.T) {
	for _, state := range []string{"running", "previously-started", "application-exit"} {
		t.Run(state, func(t *testing.T) {
			status := corev1.ContainerStatus{Name: "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					Reason: "StartError", StartedAt: metav1.NewTime(time.Unix(0, 0)),
					Message: "RuntimeImageInstalling"}}}
			switch state {
			case "running":
				status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			case "previously-started":
				status.LastTerminationState.Terminated.StartedAt = metav1.NewTime(time.Unix(100, 0))
			case "application-exit":
				status.LastTerminationState.Terminated.Reason = "Error"
			}
			pod := corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{status}}}
			if blocked := restartBlockers([]corev1.Pod{pod}, &api.RuntimeImage{}); len(blocked) != 0 {
				t.Fatalf("actual execution was deferred using stale runtime text: %+v", blocked)
			}
		})
	}
}

func TestRestartOnDeletePausedPartitionedHaveBarrierWithoutSlot(t *testing.T) {
	for _, mode := range []string{"daemon-ondelete", "stateful-ondelete", "partitioned", "paused"} {
		t.Run(mode, func(t *testing.T) {
			f := restartSetup(t)
			ctx := context.Background()
			d := f.deployment(t, "z-actionable")
			name := "a-deferred"
			runtimeClass := "brewlet"
			controller := true
			metadata := metav1.ObjectMeta{Name: name, Namespace: "apps", UID: types.UID(mode), Generation: 1, Annotations: map[string]string{"brewlet.sh/restart-on-rotation": "true"}}
			template := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}, Annotations: map[string]string{api.RuntimeAnnotation: f.image.Name}}, Spec: corev1.PodSpec{RuntimeClassName: &runtimeClass, Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/app@sha256:" + strings.Repeat("a", 64)}}}}
			selector := &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}
			var obj client.Object
			kind := ""
			switch mode {
			case "daemon-ondelete":
				obj = &appsv1.DaemonSet{ObjectMeta: metadata, Spec: appsv1.DaemonSetSpec{Selector: selector, Template: template, UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}}}
				kind = "DaemonSet"
			case "stateful-ondelete":
				obj = &appsv1.StatefulSet{ObjectMeta: metadata, Spec: appsv1.StatefulSetSpec{Selector: selector, Template: template, UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}}}
				kind = "StatefulSet"
			case "partitioned":
				partition := int32(1)
				obj = &appsv1.StatefulSet{ObjectMeta: metadata, Spec: appsv1.StatefulSetSpec{Selector: selector, Template: template, UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType, RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: &partition}}}}
				kind = "StatefulSet"
			case "paused":
				obj = f.deployment(t, name)
				dep := obj.(*appsv1.Deployment)
				dep.Spec.Paused = true
				_ = f.engine.Client.Update(ctx, dep)
				kind = "Deployment"
			}
			if mode != "paused" {
				if err := f.engine.Client.Create(ctx, obj); err != nil {
					t.Fatal(err)
				}
				p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + name, Namespace: "apps", UID: "deferred-pod", Labels: map[string]string{"app": name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, UID: obj.GetUID(), Name: name, Controller: &controller}}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: timePtr(f.now.Add(-2 * time.Hour))}}
				if err := f.engine.Client.Create(ctx, p); err != nil {
					t.Fatal(err)
				}
			}
			deletes := 0
			f.engine.Client = interceptor.NewClient(f.engine.Client.(client.WithWatch), interceptor.Funcs{Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				deletes++
				return nil
			}})
			f.run(t)
			r := f.record(t, obj.GetUID())
			want := "Deferred"
			if strings.Contains(mode, "ondelete") {
				want = "NotApplicable"
			}
			if r.Phase != want || !r.Patched || r.CompletedAt != nil || f.record(t, d.UID).Phase != "Rolling" {
				t.Fatalf("strategy misreported or retained a slot: %+v", r)
			}
			if deletes != 0 {
				t.Fatal("operator deleted a Pod")
			}
			fresh, err := refreshWorkload(ctx, f.engine.Reader, restartWorkload{kind: kind, object: obj})
			if err != nil {
				t.Fatal(err)
			}
			if fresh.template.Annotations[api.BarrierAnnotation] == "" {
				t.Fatal("non-actionable controller did not receive barrier")
			}
			if strings.Contains(mode, "ondelete") && !fresh.onDelete || mode == "paused" && fresh.deferred == "" || mode == "partitioned" && fresh.deferred == "" {
				t.Fatal("controller update strategy changed")
			}
		})
	}
}

func TestRestartPrunedIntentCannotPatchWorkload(t *testing.T) {
	f := restartSetup(t)
	_ = f.deployment(t, "api")
	f.engine.Reader = interceptor.NewClient(f.engine.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if err := c.Get(ctx, key, obj, opts...); err != nil {
			return err
		}
		if im, ok := obj.(*api.RuntimeImage); ok {
			im.Status.Restarts = api.RestartSummary{}
		}
		return nil
	}})
	err := f.engine.Reconcile(context.Background(), f.image)
	if err == nil || !strings.Contains(err.Error(), "checkpoint") || f.patches != 0 {
		t.Fatalf("pruned intent authorized mutation: patches=%d error=%v", f.patches, err)
	}
}

func TestRestartNewTargetSupersedesOlderPendingAndRolling(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "api")
	_ = f.deployment(t, "queued")
	f.run(t)
	if f.patches != 1 {
		t.Fatal("fixture concurrency failed")
	}
	ctx := context.Background()
	f.image.Spec.Source.Image = "registry.example.com/runtime@sha256:" + strings.Repeat("b", 64)
	f.image.Generation++
	if err := f.engine.Client.Update(ctx, f.image); err != nil {
		t.Fatal(err)
	}
	f.image.Status.Generation = 8
	f.image.Status.SourceImage = f.image.Spec.Source.Image
	f.image.Status.Digest = "sha256:" + strings.Repeat("b", 64)
	f.image.Status.Rotation.Generation = 8
	f.image.Status.Rotation.Digest = f.image.Status.Digest
	f.image.Status.RotationCompletedAt = timePtr(f.now)
	for i := range f.image.Status.Conditions {
		f.image.Status.Conditions[i].ObservedGeneration = f.image.Generation
	}
	if err := f.engine.Client.Status().Update(ctx, f.image); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	if f.record(t, d.UID).Generation != 8 || f.record(t, d.UID).Phase != "Rolling" || f.patches != 2 {
		t.Fatalf("new target retained old slot: %+v", f.image.Status.Restarts)
	}
	for _, s := range f.image.Status.Restarts.Workloads {
		if s.Generation == 7 && s.Phase != "Superseded" {
			t.Fatal("obsolete restart not superseded")
		}
	}
}
