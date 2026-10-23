package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type restartFixture struct {
	engine  *RestartEngine
	image   *api.RuntimeImage
	now     time.Time
	patches int
}

func restartSetup(t *testing.T) *restartFixture {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	f := &restartFixture{now: time.Date(2026, 10, 16, 14, 0, 0, 0, time.UTC)}
	complete := metav1.NewTime(f.now.Add(-time.Minute))
	digest := "sha256:" + strings.Repeat("a", 64)
	source := "registry.example.com/runtime@" + digest
	f.image = &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12", UID: "epoch", Generation: 1}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: source}, User: api.User{UID: 1000, GID: 1000}}, Status: api.RuntimeImageStatus{Generation: 7, Digest: digest, SourceImage: source, Rotation: api.RotationStatus{Generation: 7, Digest: digest}, RotationCompletedAt: &complete, Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}, {Type: "RotationComplete", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	_ = encodeJournal(f.image, journalFor(f.image, 7))
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&api.RuntimeImage{}, &appsv1.Deployment{}, &appsv1.StatefulSet{}, &appsv1.DaemonSet{}, &corev1.Pod{}).WithObjects(f.image).WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		switch obj.(type) {
		case *appsv1.Deployment, *appsv1.StatefulSet, *appsv1.DaemonSet:
			f.patches++
			data, err := p.Data(obj)
			if err != nil {
				return err
			}
			var patch map[string]any
			if err := json.Unmarshal(data, &patch); err != nil {
				return err
			}
			metadata, _ := patch["metadata"].(map[string]any)
			if metadata["uid"] == "" || metadata["resourceVersion"] == "" {
				return fmt.Errorf("restart patch lacks UID/resourceVersion fence")
			}
		}
		return c.Patch(ctx, obj, p, opts...)
	}}).Build()
	f.engine = &RestartEngine{Client: c, Reader: c, MaxConcurrent: 1, WorkloadProgressDeadline: 10 * time.Minute, Now: func() time.Time { return f.now }}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(f.image), f.image)
	return f
}
func (f *restartFixture) deployment(t *testing.T, name string) *appsv1.Deployment {
	t.Helper()
	controller := true
	runtimeClass := "brewlet"
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "apps", UID: types.UID("deployment-" + name), Generation: 1, Annotations: map[string]string{"brewlet.sh/restart-on-rotation": "true"}}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}, Annotations: map[string]string{api.RuntimeAnnotation: f.image.Name}}, Spec: corev1.PodSpec{RuntimeClassName: &runtimeClass, Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/app@sha256:" + strings.Repeat("a", 64)}}}}}}
	if err := f.engine.Client.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs-" + name, Namespace: "apps", UID: types.UID("rs-" + name), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: name, UID: d.UID, Controller: &controller}}}}
	if err := f.engine.Client.Create(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-" + name, Namespace: "apps", UID: types.UID("pod-" + name), Labels: map[string]string{"app": name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: "node"}, Status: corev1.PodStatus{Phase: corev1.PodRunning, StartTime: timePtr(f.now.Add(-time.Hour))}}
	if err := f.engine.Client.Create(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return d
}
func timePtr(t time.Time) *metav1.Time { v := metav1.NewTime(t); return &v }
func (f *restartFixture) run(t *testing.T) {
	t.Helper()
	if err := f.engine.Reconcile(context.Background(), f.image); err != nil {
		t.Fatal(err)
	}
}
func (f *restartFixture) record(t *testing.T, uid types.UID) api.WorkloadRestart {
	t.Helper()
	for _, r := range f.image.Status.Restarts.Workloads {
		if r.UID == uid && r.InventoryUID == f.image.UID && r.Generation == f.image.Status.Generation {
			return r
		}
	}
	t.Fatalf("no restart for %s: %+v", uid, f.image.Status.Restarts)
	return api.WorkloadRestart{}
}
func (f *restartFixture) healthyDeployment(t *testing.T, d *appsv1.Deployment) {
	t.Helper()
	ctx := context.Background()
	_ = f.engine.Client.Get(ctx, client.ObjectKeyFromObject(d), d)
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1}
	if err := f.engine.Client.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	p := new(corev1.Pod)
	_ = f.engine.Client.Get(ctx, client.ObjectKey{Name: "pod-" + d.Name, Namespace: d.Namespace}, p)
	p.Status.Phase = corev1.PodRunning
	p.Status.StartTime = timePtr(f.now)
	p.Status.ContainerStatuses = nil
	if err := f.engine.Client.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
}
func TestRestartSelectionPatchIdempotenceAndRecovery(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "api")
	f.run(t)
	r := f.record(t, d.UID)
	if f.patches != 1 || r.Phase != "Rolling" || !r.Patched || r.InventoryUID != f.image.UID {
		t.Fatalf("restart not durably recorded: patches=%d record=%+v", f.patches, r)
	}
	_ = f.engine.Client.Get(context.Background(), client.ObjectKeyFromObject(d), d)
	b, err := barrierValue(d.Spec.Template.Annotations[api.BarrierAnnotation], f.image.Name, f.image.UID)
	if err != nil || b.Generation != 7 {
		t.Fatalf("wrong barrier: %+v %v", b, err)
	}
	// A fresh controller observes persisted status and never reapplies a patch.
	f.engine = &RestartEngine{Client: f.engine.Client, Reader: f.engine.Reader, MaxConcurrent: 1, Now: func() time.Time { return f.now }}
	f.run(t)
	if f.patches != 1 {
		t.Fatal("controller restart repeated template patch")
	}
	f.healthyDeployment(t, d)
	f.run(t)
	if r := f.record(t, d.UID); r.Phase != "Complete" || r.CompletedAt == nil {
		t.Fatalf("rollout not observed: %+v", r)
	}
	f.run(t)
	if f.patches != 1 || f.image.Status.Restarts.Complete != 1 {
		t.Fatal("completed restart repeated")
	}
}
func TestRestartIgnoresNewPodsAndForeignOwnerChains(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "new")
	ctx := context.Background()
	p := new(corev1.Pod)
	_ = f.engine.Client.Get(ctx, client.ObjectKey{Name: "pod-new", Namespace: "apps"}, p)
	p.Status.StartTime = timePtr(f.now)
	_ = f.engine.Client.Status().Update(ctx, p)
	foreign := f.deployment(t, "foreign")
	rs := new(appsv1.ReplicaSet)
	_ = f.engine.Client.Get(ctx, client.ObjectKey{Name: "rs-foreign", Namespace: "apps"}, rs)
	rs.OwnerReferences[0].UID = "other-deployment"
	_ = f.engine.Client.Update(ctx, rs)
	f.run(t)
	if f.patches != 0 || len(f.image.Status.Restarts.Workloads) != 0 {
		t.Fatalf("new/foreign pods selected: %s %s", d.Name, foreign.Name)
	}
}
func TestRestartEpochMismatchRequiresOwnerAcknowledgement(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "api")
	ctx := context.Background()
	d.Spec.Template.Annotations[api.BarrierAnnotation] = `{"id":"python-3.12","inventoryUID":"retired-epoch","generation":6}`
	_ = f.engine.Client.Update(ctx, d)
	f.run(t)
	if f.patches != 0 || f.record(t, d.UID).Phase != "EpochMismatch" {
		t.Fatal("old epoch silently rewritten")
	}
	_ = f.engine.Client.Get(ctx, client.ObjectKeyFromObject(d), d)
	if !strings.Contains(d.Spec.Template.Annotations[api.BarrierAnnotation], "retired-epoch") {
		t.Fatal("owner acknowledgement bypassed")
	}
	delete(d.Spec.Template.Annotations, api.BarrierAnnotation)
	_ = f.engine.Client.Update(ctx, d)
	f.run(t)
	if f.patches != 1 || f.record(t, d.UID).Phase != "Rolling" {
		t.Fatal("explicit acknowledgement did not permit new epoch")
	}
}
func TestRestartGlobalConcurrencyAndExpiredSlotRelease(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "api")
	other := f.image.DeepCopy()
	other.Name = "java-21"
	other.UID = "other-epoch"
	other.ResourceVersion = ""
	other.Status.Restarts.Workloads = []api.WorkloadRestart{{Kind: "Deployment", Name: "other", Namespace: "apps", UID: "other-workload", InventoryUID: other.UID, Generation: other.Status.Generation, Phase: "Rolling", Reason: "RestartProgressing", StartedAt: timePtr(f.now)}}
	if err := f.engine.Client.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	f.run(t)
	if f.patches != 0 || f.record(t, d.UID).Phase != "Pending" {
		t.Fatal("cluster concurrency limit exceeded")
	}
	f.now = f.now.Add(11 * time.Minute)
	f.run(t)
	if f.patches != 1 {
		t.Fatal("expired rollout retained global slot")
	}
}
func TestRestartColdAndExcludedPodDeadlineDeferredThenRecovery(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		t.Run(fmt.Sprint(excluded), func(t *testing.T) {
			f := restartSetup(t)
			d := f.deployment(t, "a-cold")
			next := f.deployment(t, "z-next")
			f.run(t)
			if f.patches != 1 {
				t.Fatal("initial concurrency limit broken")
			}
			p := new(corev1.Pod)
			ctx := context.Background()
			_ = f.engine.Client.Get(ctx, client.ObjectKey{Name: "pod-a-cold", Namespace: "apps"}, p)
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CreateContainerError", Message: "RuntimeImageInstalling"}}}}
			if excluded {
				p.Status.ContainerStatuses = nil
				f.image.Status.Rotation.ExcludedNodes = []api.RotationNode{{Name: "node", UID: "node", Reason: "NodeWideStartupFence"}}
			}
			_ = f.engine.Client.Status().Update(ctx, p)
			f.now = f.now.Add(11 * time.Minute)
			f.run(t)
			r := f.record(t, d.UID)
			if r.Phase != "Deferred" || len(r.BlockedPods) != 1 || r.BlockedPods[0].UID != p.UID || r.BlockedPods[0].Node != "node" || r.BlockedPods[0].MinimumGeneration != 7 {
				t.Fatalf("cold/barrier timeout misreported: %+v", r)
			}
			if f.patches != 2 || f.record(t, next.UID).Phase != "Rolling" {
				t.Fatal("deferred installation retained rollout slot")
			}
			f.healthyDeployment(t, d)
			f.run(t)
			if f.record(t, d.UID).Phase != "Complete" || f.patches != 2 {
				t.Fatal("eventual recovery repatched template or failed to observe completion")
			}
		})
	}
}
func TestRestartNativeDeadlineFailsAndReleasesSlot(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "a-failing")
	next := f.deployment(t, "z-next")
	f.run(t)
	ctx := context.Background()
	_ = f.engine.Client.Get(ctx, client.ObjectKeyFromObject(d), d)
	d.Status.ObservedGeneration = d.Generation
	d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded", Message: "application unhealthy"}}
	_ = f.engine.Client.Status().Update(ctx, d)
	f.run(t)
	if f.record(t, d.UID).Phase != "Failed" || f.record(t, next.UID).Phase != "Rolling" || f.patches != 2 {
		t.Fatal("native deadline did not free slot")
	}
}

func TestRestartSelectionUsesLeaseEvidenceBeforeStartTime(t *testing.T) {
	setPod := func(t *testing.T, f *restartFixture, start time.Time) {
		t.Helper()
		p := new(corev1.Pod)
		_ = f.engine.Client.Get(context.Background(), client.ObjectKey{Name: "pod-api", Namespace: "apps"}, p)
		p.Status.StartTime = timePtr(start)
		if err := f.engine.Client.Status().Update(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	observation := func(f *restartFixture, at time.Time, current bool, reason string, bindings ...api.RuntimeTaskBinding) []api.RuntimeTaskObservation {
		return []api.RuntimeTaskObservation{{NodeName: "node", NodeUID: "node", ProfileUID: "profile", ProfileGeneration: 1, ObservedAt: at, Current: current, Reason: reason, Bindings: bindings}}
	}
	oldBinding := api.RuntimeTaskBinding{ContainerID: "old-task", ContainerdNamespace: "k8s.io", PodUID: "pod-api", PodName: "pod-api", PodNamespace: "apps", InventoryUID: "epoch", Generation: 6, Digest: "old-digest", State: "running"}

	t.Run("fresh complete observation without binding spares pre-completion pod", func(t *testing.T) {
		f := restartSetup(t)
		d := f.deployment(t, "api")
		f.image.Status.TaskObservations = observation(f, f.now.Add(-10*time.Second), true, "CompleteTaskAndLeaseObservation")
		f.run(t)
		_ = f.engine.Client.Get(context.Background(), client.ObjectKeyFromObject(f.image), f.image)
		if f.patches != 0 || len(f.image.Status.Restarts.Workloads) != 0 {
			t.Fatalf("observed-current pod %s was restarted: patches=%d restarts=%+v", d.Name, f.patches, f.image.Status.Restarts)
		}
	})
	t.Run("older binding restarts pod that started after completion", func(t *testing.T) {
		f := restartSetup(t)
		d := f.deployment(t, "api")
		setPod(t, f, f.now)
		f.image.Status.TaskObservations = observation(f, f.now, false, "ObservationUnavailable", oldBinding)
		f.run(t)
		if f.patches != 1 || f.record(t, d.UID).Phase != "Rolling" {
			t.Fatalf("pod with genuine older binding not restarted: patches=%d record=%+v", f.patches, f.record(t, d.UID))
		}
	})
	for name, obs := range map[string]func(f *restartFixture) []api.RuntimeTaskObservation{
		"no observation": func(*restartFixture) []api.RuntimeTaskObservation { return nil },
		"stale observation": func(f *restartFixture) []api.RuntimeTaskObservation {
			return observation(f, f.now.Add(-3*time.Minute), true, "CompleteTaskAndLeaseObservation")
		},
		"non-current observation": func(f *restartFixture) []api.RuntimeTaskObservation {
			return observation(f, f.now.Add(-10*time.Second), false, "CompleteTaskAndLeaseObservation")
		},
		"retirement resolution": func(f *restartFixture) []api.RuntimeTaskObservation {
			return observation(f, f.now.Add(-10*time.Second), true, "IndependentRetirementResolution")
		},
		"observation predates container start": func(f *restartFixture) []api.RuntimeTaskObservation {
			return observation(f, f.now.Add(-90*time.Minute), true, "CompleteTaskAndLeaseObservation")
		},
		"other node": func(f *restartFixture) []api.RuntimeTaskObservation {
			o := observation(f, f.now.Add(-10*time.Second), true, "CompleteTaskAndLeaseObservation")
			o[0].NodeName = "other"
			return o
		},
	} {
		t.Run(name+" falls back to start-time rule", func(t *testing.T) {
			f := restartSetup(t)
			d := f.deployment(t, "api")
			f.image.Status.TaskObservations = obs(f)
			f.run(t)
			if f.patches != 1 || f.record(t, d.UID).Phase != "Rolling" {
				t.Fatalf("uncertain pre-completion pod not restarted: patches=%d record=%+v", f.patches, f.record(t, d.UID))
			}
		})
	}
}

func TestRestartNamespacesScopeCandidates(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "api")
	f.engine.Namespaces = []string{"payments"}
	f.run(t)
	_ = f.engine.Client.Get(context.Background(), client.ObjectKeyFromObject(f.image), f.image)
	if f.patches != 0 || len(f.image.Status.Restarts.Workloads) != 0 {
		t.Fatalf("workload %s outside restart namespaces was selected: %+v", d.Name, f.image.Status.Restarts)
	}
	f.engine.Namespaces = []string{"payments", "apps"}
	f.run(t)
	if f.patches != 1 || f.record(t, d.UID).Phase != "Rolling" {
		t.Fatalf("allowlisted workload not restarted: patches=%d", f.patches)
	}
}

func TestParseRestartNamespaces(t *testing.T) {
	got, err := ParseRestartNamespaces(" apps, payments,,apps ")
	if err != nil || len(got) != 2 || got[0] != "apps" || got[1] != "payments" {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := ParseRestartNamespaces(""); err != nil || len(got) != 0 {
		t.Fatalf("empty allowlist must mean cluster-wide: %v, %v", got, err)
	}
	for _, bad := range []string{"Apps", "a_b", "-x", strings.Repeat("a", 64)} {
		if _, err := ParseRestartNamespaces(bad); err == nil {
			t.Fatalf("accepted invalid namespace %q", bad)
		}
	}
}
