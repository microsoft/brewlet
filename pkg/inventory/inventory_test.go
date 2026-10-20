package inventory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"encoding/json"
	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/node"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fixture struct {
	reader     client.Client
	pod        *corev1.Pod
	host       *corev1.Node
	deployment *appsv1.Deployment
	options    Options
	calls      *int
}

func makeFixture(t *testing.T) fixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	truth := true
	class := "brewlet"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orders-one", Namespace: "prod", UID: "pod-one",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "orders-rs", UID: "rs-one", Controller: &truth}}},
		Spec: corev1.PodSpec{NodeName: "worker", RuntimeClassName: &class, Containers: []corev1.Container{{Name: "main", Image: "desired-image-is-not-authority"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "main", ContainerID: "containerd://container-one",
			ImageID: digest.FromString("config").String(), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "prod", UID: "deployment-one"}}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "orders-rs", Namespace: "prod", UID: "rs-one",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "orders", UID: deployment.UID, Controller: &truth}}}}
	profile := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "pool", UID: "profile-one", Generation: 1}}
	host := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-one", Labels: map[string]string{
		api.OwnerUID: string(profile.UID), api.OwnerNodeUID: "node-one"}}}
	identity := node.ApplicationIdentity{Reference: "registry.example/apps/orders@" + digest.FromString("root").String(),
		RootDigest: digest.FromString("root").String(), ManifestDigest: digest.FromString("manifest").String(),
		ConfigDigest: digest.FromString("config").String(), Platform: v1.Platform{OS: "linux", Architecture: "arm64"}}
	binding := node.RuntimeBinding{Application: &identity, ContainerID: "container-one", ContainerdNamespace: "k8s.io",
		PodUID: string(pod.UID), PodName: pod.Name, PodNamespace: pod.Namespace, ContainerName: "main", ID: "runtime",
		InventoryUID: "old-runtime-uid", Generation: 7, Digest: digest.FromString("old-runtime-source").String(), State: "running"}
	data, err := json.Marshal(node.BindingObservation{NodeUID: string(host.UID), ProfileUID: string(profile.UID),
		ProfileGeneration: 1, ObservedAt: time.Now(), Complete: true, Bindings: []node.RuntimeBinding{binding}})
	if err != nil {
		t.Fatal(err)
	}
	host.Annotations = map[string]string{node.BindingsAnnotation: string(data), api.OwnerName: profile.Name}
	calls := new(int)
	options := Options{Cluster: "https://cluster.example", Namespaces: []string{"prod"}, MaxAge: time.Minute,
		ReadImage: func(ctx context.Context, identity node.ApplicationIdentity) (artifact.DependencyReport, error) {
			*calls++
			return artifact.DependencyReport{Verification: artifact.CompositionInspection{SchemaVersion: 1,
				Structural: true, PublisherProvenance: true, CompositionSignature: true, PolicyMode: "production",
				Predicate: artifact.CompositionPredicate{Output: digest.Digest(identity.ManifestDigest), Platform: identity.Platform,
					OutputConfig: v1.Descriptor{Digest: digest.Digest(identity.ConfigDigest)}}},
				Packages: []artifact.LockEntry{{Name: "shared", Version: "1", PURL: "pkg:generic/shared@1"}}}, nil
		}}
	return fixture{fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, host, profile, deployment, rs).Build(),
		pod, host, deployment, options, calls}
}

func TestCaptureUsesObservedGraphsAndUIDOwnership(t *testing.T) {
	f := makeFixture(t)
	result, err := Capture(context.Background(), f.reader, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Containers) != 1 || len(result.Artifacts) != 1 || *f.calls != 1 {
		t.Fatalf("incomplete inventory: %+v", result)
	}
	row := result.Containers[0]
	if row.Workload.UID != f.deployment.UID || row.Status != "verified" || row.Binding.Generation != 7 ||
		row.Binding.Application.ManifestDigest != digest.FromString("manifest").String() {
		t.Fatalf("desired state replaced actual running cohort: %+v", row)
	}
	if !result.FinishedAt.After(result.StartedAt) {
		t.Fatal("missing snapshot times")
	}
}

func TestCaptureFailsClosed(t *testing.T) {
	for name, mutate := range map[string]func(*fixture){
		"stale": func(f *fixture) {
			mutateObservation(t, f, func(o *node.BindingObservation) { o.ObservedAt = time.Now().Add(-time.Hour) })
		},
		"future": func(f *fixture) {
			mutateObservation(t, f, func(o *node.BindingObservation) { o.ObservedAt = time.Now().Add(time.Hour) })
		},
		"incomplete":      func(f *fixture) { mutateObservation(t, f, func(o *node.BindingObservation) { o.Complete = false }) },
		"foreign-node":    func(f *fixture) { mutateObservation(t, f, func(o *node.BindingObservation) { o.NodeUID = "other" }) },
		"foreign-profile": func(f *fixture) { mutateObservation(t, f, func(o *node.BindingObservation) { o.ProfileUID = "other" }) },
		"profile-generation": func(f *fixture) {
			mutateObservation(t, f, func(o *node.BindingObservation) { o.ProfileGeneration = 2 })
		},
		"pod-uid": func(f *fixture) {
			mutateObservation(t, f, func(o *node.BindingObservation) { o.Bindings[0].PodUID = "other" })
		},
		"container-id": func(f *fixture) {
			mutateObservation(t, f, func(o *node.BindingObservation) { o.Bindings[0].ContainerID = "other" })
		},
		"config-digest": func(f *fixture) {
			mutateObservation(t, f, func(o *node.BindingObservation) {
				o.Bindings[0].Application.ConfigDigest = digest.FromString("other").String()
			})
		},
		"missing-identity": func(f *fixture) {
			mutateObservation(t, f, func(o *node.BindingObservation) { o.Bindings[0].Application = nil })
		},
		"owner-uid": func(f *fixture) {
			f.deployment.UID = "recreated"
			f.deployment.ResourceVersion = ""
			if err := f.reader.Delete(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "prod"}}); err != nil {
				t.Fatal(err)
			}
			if err := f.reader.Create(context.Background(), f.deployment); err != nil {
				t.Fatal(err)
			}
		},
		"registry-unavailable": func(f *fixture) {
			f.options.ReadImage = func(context.Context, node.ApplicationIdentity) (artifact.DependencyReport, error) {
				return artifact.DependencyReport{}, fmt.Errorf("registry unavailable")
			}
		},
		"unsigned": func(f *fixture) {
			original := f.options.ReadImage
			f.options.ReadImage = func(ctx context.Context, i node.ApplicationIdentity) (artifact.DependencyReport, error) {
				r, e := original(ctx, i)
				r.Verification.CompositionSignature = false
				return r, e
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := makeFixture(t)
			mutate(&f)
			report, err := Capture(context.Background(), f.reader, f.options)
			if err != nil {
				t.Fatal(err)
			}
			if report.Complete || len(report.Containers) != 1 || report.Containers[0].Status != "unverified" || report.Containers[0].Reason == "" {
				t.Fatalf("unknown evidence became verified: %+v", report)
			}
		})
	}
}

func mutateObservation(t *testing.T, f *fixture, mutate func(*node.BindingObservation)) {
	t.Helper()
	var host corev1.Node
	if err := f.reader.Get(context.Background(), types.NamespacedName{Name: f.host.Name}, &host); err != nil {
		t.Fatal(err)
	}
	var observation node.BindingObservation
	if err := json.Unmarshal([]byte(host.Annotations[node.BindingsAnnotation]), &observation); err != nil {
		t.Fatal(err)
	}
	mutate(&observation)
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	host.Annotations[node.BindingsAnnotation] = string(data)
	if err = f.reader.Update(context.Background(), &host); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureScopeAndSharedArtifactDeduplication(t *testing.T) {
	f := makeFixture(t)
	second := f.pod.DeepCopy()
	second.Name, second.UID = "orders-two", "pod-two"
	second.ResourceVersion = ""
	second.Status.ContainerStatuses[0].ContainerID = "containerd://container-two"
	if err := f.reader.Create(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	mutateObservation(t, &f, func(o *node.BindingObservation) {
		copy := o.Bindings[0]
		copy.PodUID, copy.PodName, copy.ContainerID = string(second.UID), second.Name, "container-two"
		o.Bindings = append(o.Bindings, copy)
	})
	result, err := Capture(context.Background(), f.reader, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Complete || len(result.Containers) != 2 || len(result.Artifacts) != 1 || *f.calls != 1 {
		t.Fatalf("shared facts not deduplicated: %+v", result)
	}
	f.options.Workloads = []string{"Deployment/prod/different"}
	result, err = Capture(context.Background(), f.reader, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Containers) != 0 || !strings.Contains(strings.Join(result.Warnings, " "), "explicit scope") {
		t.Fatal("scope omission hidden")
	}
}
