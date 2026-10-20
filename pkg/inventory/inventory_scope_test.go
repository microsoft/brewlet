package inventory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/node"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestScopedAffectedControlStagingAndUnknownApplicationsRemainDistinct(t *testing.T) {
	f := makeFixture(t)
	var additions []node.RuntimeBinding
	for _, name := range []string{"staging", "control", "unknown"} {
		pod := f.pod.DeepCopy()
		pod.Name, pod.UID, pod.ResourceVersion = name, types.UID(name+"-pod"), ""
		pod.OwnerReferences = nil
		if name == "staging" {
			pod.Namespace = "staging"
		}
		container := name + "-container"
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + container
		identity := node.ApplicationIdentity{Reference: "registry.example/apps/orders@" + digest.FromString("root").String(),
			RootDigest: digest.FromString("root").String(), ManifestDigest: digest.FromString("manifest").String(),
			ConfigDigest: digest.FromString("config").String(), Platform: v1.Platform{OS: "linux", Architecture: "arm64"}}
		if name != "staging" {
			identity.RootDigest = digest.FromString(name + "-root").String()
			identity.ManifestDigest = digest.FromString(name + "-manifest").String()
			identity.ConfigDigest = digest.FromString(name + "-config").String()
			identity.Reference = "registry.example/apps/" + name + "@" + identity.RootDigest
		}
		pod.Status.ContainerStatuses[0].ImageID = identity.ConfigDigest
		if err := f.reader.Create(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
		additions = append(additions, node.RuntimeBinding{Application: &identity,
			ContainerID: container, ContainerdNamespace: "k8s.io", PodUID: string(pod.UID),
			PodName: pod.Name, PodNamespace: pod.Namespace, ContainerName: "main",
			State: "running", ID: "runtime", InventoryUID: "old-runtime-uid", Generation: 7})
	}
	mutateObservation(t, &f, func(observation *node.BindingObservation) {
		observation.Bindings = append(observation.Bindings, additions...)
	})
	original := f.options.ReadImage
	f.options.Namespaces = []string{"prod", "staging"}
	f.options.ReadImage = func(ctx context.Context, identity node.ApplicationIdentity) (artifact.DependencyReport, error) {
		if strings.Contains(identity.Reference, "/unknown@") {
			return artifact.DependencyReport{}, fmt.Errorf("composition evidence missing")
		}
		facts, err := original(ctx, identity)
		if strings.Contains(identity.Reference, "/control@") {
			facts.Packages[0].Version, facts.Packages[0].PURL = "2", "pkg:generic/shared@2"
		}
		return facts, err
	}
	report, err := Capture(context.Background(), f.reader, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || len(report.Containers) != 4 || len(report.Artifacts) != 2 {
		t.Fatalf("coverage or distinct image cohorts lost: %+v", report)
	}
	affected := map[string]bool{}
	for _, row := range report.Containers {
		if row.Pod.Name == "unknown" {
			if row.Status != "unverified" || row.Reason == "" || row.EvidenceKey != "" {
				t.Fatal("unknown image became an unaffected control")
			}
			continue
		}
		// An exact fixture advisory join, not a CVE database or version comparator.
		for _, pkg := range report.Artifacts[row.EvidenceKey].Packages {
			if pkg.PURL == "pkg:generic/shared@1" {
				affected[row.Pod.Namespace+"/"+row.Workload.Name] = true
			}
		}
	}
	if len(affected) != 2 || !affected["prod/orders"] || !affected["staging/staging"] || affected["prod/control"] {
		t.Fatalf("external package-to-application join lost scope/version: %v", affected)
	}
}

type unavailableNamespaceReader struct{ client.Reader }

func (r unavailableNamespaceReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	settings := &client.ListOptions{}
	for _, option := range options {
		option.ApplyToList(settings)
	}
	if settings.Namespace == "staging" {
		return fmt.Errorf("forbidden: no Pod list permission in staging")
	}
	return r.Reader.List(ctx, list, options...)
}

func TestNamespaceReadFailureDoesNotProduceAnEmptyVerifiedScope(t *testing.T) {
	f := makeFixture(t)
	f.options.Namespaces = []string{"prod", "staging"}
	report, err := Capture(context.Background(), unavailableNamespaceReader{f.reader}, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Complete || len(report.Containers) != 1 ||
		!strings.Contains(strings.Join(report.Warnings, " "), "staging unavailable") {
		t.Fatal("unreadable namespace omitted without uncertainty")
	}
}

func TestMatchingLabelsNeverReplaceControllerUIDOwnership(t *testing.T) {
	f := makeFixture(t)
	var pod corev1.Pod
	if err := f.reader.Get(context.Background(), client.ObjectKeyFromObject(f.pod), &pod); err != nil {
		t.Fatal(err)
	}
	pod.OwnerReferences = nil
	pod.Labels = map[string]string{"app": f.deployment.Name}
	if err := f.reader.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	report, err := Capture(context.Background(), f.reader, f.options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.Containers[0].Workload.Kind != "Pod" ||
		report.Containers[0].Workload.UID != pod.UID {
		t.Fatal("labels incorrectly assigned standalone Pod to Deployment")
	}
}
