package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	noderuntime "github.com/microsoft/brewlet/pkg/node"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestGenuineOlderTaskObservationsPersistAcrossMissingAndIncompleteReports(t *testing.T) {
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	im.Status.Generation = 2
	now := time.Now().UTC()
	r.Now = func() time.Time { return now }
	snapshot := noderuntime.BindingObservation{NodeUID: string(n.UID), ProfileUID: string(p.UID), ProfileGeneration: 1, ObservedAt: now, Complete: true, Bindings: []noderuntime.RuntimeBinding{
		{ContainerID: "old-task", ContainerdNamespace: "k8s.io", PodUID: "old-pod", PodName: "api-old", PodNamespace: "apps", ContainerName: "app", ID: im.Name, InventoryUID: string(im.UID), Generation: 1, Digest: im.Status.Digest, State: "running"},
		{ContainerID: "current-task", ContainerdNamespace: "k8s.io", PodUID: "new-pod", PodName: "api-new", PodNamespace: "apps", ContainerName: "app", ID: im.Name, InventoryUID: string(im.UID), Generation: 2, Digest: im.Status.Digest, State: "running"},
	}}
	publish := func() {
		t.Helper()
		_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
		raw, _ := json.Marshal(snapshot)
		n.Annotations[noderuntime.BindingsAnnotation] = string(raw)
		if err := r.Update(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	if _, err := r.observeTasks(ctx, im); err != nil {
		t.Fatal(err)
	}
	if len(im.Status.TaskObservations) != 1 || len(im.Status.TaskObservations[0].Bindings) != 1 || im.Status.TaskObservations[0].Bindings[0].ContainerID != "old-task" {
		t.Fatal("actual old task/lease tuple not retained exactly")
	}
	if err := r.revocationCheckpoint(ctx, im); err != nil {
		t.Fatal(err)
	}
	snapshot.Complete = false
	snapshot.Bindings = nil
	publish()
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if _, err := r.observeTasks(ctx, im); err != nil {
		t.Fatal(err)
	}
	if im.Status.TaskObservations[0].Current || len(im.Status.TaskObservations[0].Bindings) != 1 {
		t.Fatal("incomplete report inferred disappearance of old task")
	}
	snapshot.Complete = true
	snapshot.ProfileUID = "foreign-profile"
	snapshot.ObservedAt = now.Add(time.Second)
	publish()
	if _, err := r.observeTasks(ctx, im); err != nil {
		t.Fatal(err)
	}
	if len(im.Status.TaskObservations[0].Bindings) != 1 {
		t.Fatal("foreign observation erased a genuine old task")
	}
	snapshot.ProfileUID = string(p.UID)
	snapshot.ObservedAt = now.Add(2 * time.Second)
	publish()
	if _, err := r.observeTasks(ctx, im); err != nil {
		t.Fatal(err)
	}
	if !im.Status.TaskObservations[0].Current || len(im.Status.TaskObservations[0].Bindings) != 0 {
		t.Fatal("fresh complete same-UID absence did not retire task diagnostic")
	}
}
func TestNativeRolloutAvailabilityDoesNotCompleteWithKnownOlderTask(t *testing.T) {
	f := restartSetup(t)
	d := f.deployment(t, "api")
	f.run(t)
	f.healthyDeployment(t, d)
	f.image.Status.TaskObservations = []api.RuntimeTaskObservation{{NodeName: "excluded-node", NodeUID: "node", ProfileUID: "profile", ProfileGeneration: 1, ObservedAt: time.Now(), Current: false, Reason: "ObservationUnavailable", Bindings: []api.RuntimeTaskBinding{{ContainerID: "old-task", ContainerdNamespace: "k8s.io", PodUID: "pod-api", PodName: "pod-api", PodNamespace: "apps", InventoryUID: f.image.UID, Generation: 6, Digest: "old-digest", State: "running"}}}}
	f.run(t)
	s := f.record(t, d.UID)
	if s.Phase != "Deferred" || s.CompletedAt != nil || len(s.BlockedPods) == 0 {
		t.Fatal("native availability falsely completed while genuine stale old-task tuple remained")
	}
	f.image.Status.TaskObservations[0].Bindings = nil
	f.image.Status.TaskObservations[0].Current = true
	f.run(t)
	if f.record(t, d.UID).Phase != "Complete" || f.patches != 1 {
		t.Fatal("genuine task disappearance did not permit completion without another patch")
	}
}

func TestTaskObservationBindingsAreSortedIndependentlyOfNodeReportOrder(t *testing.T) {
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	im.Status.Generation = 2
	now := time.Now().UTC()
	r.Now = func() time.Time { return now }
	binding := func(id string) noderuntime.RuntimeBinding {
		return noderuntime.RuntimeBinding{ContainerID: id, ContainerdNamespace: "k8s.io", PodUID: "pod-" + id, PodName: "api-" + id, PodNamespace: "apps", ContainerName: "app", ID: im.Name, InventoryUID: string(im.UID), Generation: 1, Digest: im.Status.Digest, State: "running"}
	}
	publish := func(order ...string) {
		t.Helper()
		snapshot := noderuntime.BindingObservation{NodeUID: string(n.UID), ProfileUID: string(p.UID), ProfileGeneration: 1, ObservedAt: now, Complete: true}
		for _, id := range order {
			snapshot.Bindings = append(snapshot.Bindings, binding(id))
		}
		_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
		raw, _ := json.Marshal(snapshot)
		n.Annotations[noderuntime.BindingsAnnotation] = string(raw)
		if err := r.Update(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	ids := func() []string {
		out := []string{}
		for _, b := range im.Status.TaskObservations[0].Bindings {
			out = append(out, b.ContainerID)
		}
		return out
	}
	publish("c", "a", "b")
	if _, err := r.observeTasks(ctx, im); err != nil {
		t.Fatal(err)
	}
	if got := ids(); len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("bindings not sorted by containerd namespace/ID: %v", got)
	}
	publish("b", "c", "a")
	changed, err := r.observeTasks(ctx, im)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("reordered but identical node report produced a status change: %v", ids())
	}
}
