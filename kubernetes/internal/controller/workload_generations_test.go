package controller

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	noderuntime "github.com/microsoft/brewlet/pkg/node"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestWorkloadGenerationChangesAreRecordedPerOwningWorkload(t *testing.T) {
	r, im, p, n := revocationSetup(t)
	ctx := context.Background()
	revokeStep(t, r, im)
	_ = r.Get(ctx, client.ObjectKeyFromObject(im), im)
	if !policy.Approved(im) || len(im.Status.RevocationTargets) != 1 {
		t.Fatal("fixture is not an approved runtime with a claimed node")
	}
	now := time.Now().UTC()
	r.Now = func() time.Time { return now }
	recorder := record.NewFakeRecorder(8)
	r.Recorder = recorder

	controller := true
	owned := func(kind, apiVersion, name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: &controller}}
	}
	labels := map[string]string{"app": "api"}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "apps", UID: "deployment"}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}}}
	objects := []client.Object{
		deployment,
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "apps", UID: "rs-1", OwnerReferences: owned("Deployment", "apps/v1", "api", "deployment")}},
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "api-2", Namespace: "apps", UID: "rs-2", OwnerReferences: owned("Deployment", "apps/v1", "api", "deployment")}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-1-a", Namespace: "apps", UID: "pod-old", OwnerReferences: owned("ReplicaSet", "apps/v1", "api-1", "rs-1")}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-2-a", Namespace: "apps", UID: "pod-new", OwnerReferences: owned("ReplicaSet", "apps/v1", "api-2", "rs-2")}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "apps", UID: "pod-solo"}},
	}
	for _, o := range objects {
		if err := r.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	binding := func(pod, name string, generation int64, digest string) noderuntime.RuntimeBinding {
		return noderuntime.RuntimeBinding{ContainerID: pod + "-task", ContainerdNamespace: "k8s.io", PodUID: pod, PodName: name, PodNamespace: "apps", ContainerName: "app", ID: im.Name, InventoryUID: string(im.UID), Generation: generation, Digest: digest, State: "running"}
	}
	publish := func(observedAt time.Time, bindings ...noderuntime.RuntimeBinding) {
		t.Helper()
		_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
		raw, _ := json.Marshal(noderuntime.BindingObservation{NodeUID: string(n.UID), ProfileUID: string(p.UID), ProfileGeneration: 1, ObservedAt: observedAt, Complete: true, Bindings: bindings})
		n.Annotations[noderuntime.BindingsAnnotation] = string(raw)
		if err := r.Update(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	observe := func() []workloadGenerationChange {
		t.Helper()
		changes, err := r.observeWorkloadGenerations(ctx, im)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.revocationCheckpoint(ctx, im); err != nil {
			t.Fatal(err)
		}
		r.recordWorkloadGenerationChanges(changes)
		return changes
	}
	find := func(name string) *api.WorkloadGeneration {
		for i := range im.Status.WorkloadGenerations {
			if im.Status.WorkloadGenerations[i].Name == name {
				return &im.Status.WorkloadGenerations[i]
			}
		}
		return nil
	}
	expectQuiet := func() {
		t.Helper()
		select {
		case e := <-recorder.Events:
			t.Fatalf("unexpected event: %s", e)
		default:
		}
	}

	publish(now, binding("pod-old", "api-1-a", 1, "sha256:old"), binding("pod-solo", "solo", 1, "sha256:old"),
		noderuntime.RuntimeBinding{ContainerID: "other", ContainerdNamespace: "k8s.io", PodUID: "pod-other", PodName: "other", PodNamespace: "apps", ID: "java-21", InventoryUID: "other-epoch", Generation: 9, Digest: "sha256:java"})
	if changes := observe(); len(changes) != 0 {
		t.Fatalf("first observation is not a change: %+v", changes)
	}
	expectQuiet()
	api1 := find("api")
	if len(im.Status.WorkloadGenerations) != 2 || api1 == nil || api1.Kind != "Deployment" || api1.UID != "deployment" || len(api1.Generations) != 1 || api1.Generations[0].Generation != 1 || api1.ChangedAt != nil {
		t.Fatalf("ReplicaSet pod not attributed to its Deployment: %+v", im.Status.WorkloadGenerations)
	}
	if solo := find("solo"); solo == nil || solo.Kind != "Pod" || solo.APIVersion != "v1" {
		t.Fatal("uncontrolled pod is not its own workload")
	}

	// Missing or stale evidence must never look like a generation change.
	publish(now.Add(-time.Hour), binding("pod-new", "api-2-a", 2, "sha256:new"))
	if changes := observe(); len(changes) != 0 || find("api").Generations[0].Generation != 1 || find("api").Generations[0].Pods != 1 {
		t.Fatal("stale observation changed workload generation records")
	}

	now = now.Add(time.Minute)
	publish(now, binding("pod-old", "api-1-a", 1, "sha256:old"), binding("pod-new", "api-2-a", 2, "sha256:new"), binding("pod-solo", "solo", 1, "sha256:old"))
	if changes := observe(); len(changes) != 1 {
		t.Fatalf("mid-rollout mixed generations not reported: %+v", changes)
	}
	<-recorder.Events
	now = now.Add(time.Minute)
	publish(now, binding("pod-new", "api-2-a", 2, "sha256:new"))
	if changes := observe(); len(changes) != 1 {
		t.Fatalf("completed rollout not reported once: %+v", changes)
	}
	select {
	case e := <-recorder.Events:
		for _, want := range []string{"Normal RuntimeGenerationChanged", im.Name, "from generation 1 (sha256:old), generation 2 (sha256:new) to generation 2 (sha256:new)", "1 pod(s)"} {
			if !strings.Contains(e, want) {
				t.Fatalf("event %q omits %q", e, want)
			}
		}
	default:
		t.Fatal("generation change emitted no workload event")
	}
	expectQuiet()
	api1 = find("api")
	if len(api1.Generations) != 1 || api1.Generations[0].Generation != 2 || len(api1.PreviousGenerations) != 2 || api1.ChangedAt == nil || !api1.ChangedAt.Time.Equal(now.Truncate(time.Second)) {
		t.Fatalf("durable previous/current generations absent: %+v", api1)
	}
	if solo := find("solo"); solo == nil || solo.Generations[0].Pods != 0 || solo.Generations[0].Generation != 1 || solo.ChangedAt != nil {
		t.Fatal("unobserved but existing workload lost its last observed generation")
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewRuntimeMetrics(r.Reader))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	series := 0
	for _, f := range families {
		if f.GetName() != "brewlet_runtime_workload_generation_pods" {
			continue
		}
		for _, m := range f.Metric {
			series++
			got := map[string]string{}
			for _, l := range m.Label {
				got[l.GetName()] = l.GetValue()
			}
			if got["name"] != "api" || got["kind"] != "Deployment" || got["generation"] != "2" || got["digest"] != "sha256:new" || m.GetGauge().GetValue() != 1 {
				t.Fatalf("unexpected workload generation series: %v = %v", got, m.GetGauge().GetValue())
			}
		}
	}
	if series != 1 {
		t.Fatalf("expected only the observed workload generation series, got %d", series)
	}

	// Scaling is not a generation change; deleted workloads are pruned.
	if err := r.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "solo", Namespace: "apps"}}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	publish(now)
	if changes := observe(); len(changes) != 0 || find("solo") != nil || find("api") == nil || find("api").Generations[0].Pods != 0 {
		t.Fatalf("scale-to-zero or deletion mishandled: %+v", im.Status.WorkloadGenerations)
	}
	expectQuiet()
	assertRuntimeStatusConformsToCRD(t, im)
}

func assertRuntimeStatusConformsToCRD(t *testing.T, im *api.RuntimeImage) {
	t.Helper()
	for _, path := range []string{"../../deploy/crds/runtimeimages.yaml", "../../charts/brewlet/crds/runtimeimages.yaml"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		crd := new(extv1.CustomResourceDefinition)
		if err := yaml.UnmarshalStrict(data, crd); err != nil {
			t.Fatal(err)
		}
		props := new(ext.JSONSchemaProps)
		if err := extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, props, nil); err != nil {
			t.Fatal(err)
		}
		schema, _, err := validation.NewSchemaValidator(props)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(im)
		var object map[string]any
		if err := kjson.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		if errs := validation.ValidateCustomResource(field.NewPath("runtimeimage"), object, schema); len(errs) > 0 {
			t.Fatalf("%s rejects controller-produced status: %v", path, errs)
		}
	}
}
