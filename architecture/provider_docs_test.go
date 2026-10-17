package architecture

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestProviderTemplatesMatchInventoryAndStartupFence(t *testing.T) {
	data, err := os.ReadFile("../docs/README.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```yaml\\n(.*?)\\n```").FindAllSubmatch(data, -1)
	if len(blocks) != 2 {
		t.Fatalf("expected reviewed NodeProfile and NodePool examples, got %d", len(blocks))
	}
	var profile api.NodeProfile
	if err := yaml.UnmarshalStrict(blocks[0][1], &profile); err != nil {
		t.Fatal(err)
	}
	if profile.APIVersion != "node.brewlet.sh/v2alpha1" || profile.Kind != "NodeProfile" {
		t.Fatal("provider example restored an obsolete or unrelated profile API")
	}
	if err := policy.Profile(&profile, nil, nil); err != nil {
		t.Fatal("documented NodeProfile violates production policy", err)
	}
	if profile.Spec.NodePool.Key != "eks.amazonaws.com/nodegroup" ||
		len(profile.Spec.RuntimeImages) != 1 || profile.Spec.RuntimeImages[0].ID != "static" ||
		profile.Spec.RuntimeImages[0].Mode() != "OnDemand" {
		t.Fatal("simulation recipe and offered inventory disagree")
	}
	want := corev1.Toleration{Key: api.StartupTaint, Operator: corev1.TolerationOpEqual,
		Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}
	if len(profile.Spec.Tolerations) != 1 || profile.Spec.Tolerations[0] != want {
		t.Fatal("provisioner startup tolerance is missing or broader than the exact fence")
	}
	var pool map[string]interface{}
	if err := yaml.UnmarshalStrict(blocks[1][1], &pool); err != nil {
		t.Fatal(err)
	}
	object := &unstructured.Unstructured{Object: pool}
	if object.GetAPIVersion() != "karpenter.sh/v1" || object.GetKind() != "NodePool" {
		t.Fatal("real-node template is not the declared Karpenter NodePool API")
	}
	labels, found, err := unstructured.NestedStringMap(pool, "spec", "template", "metadata", "labels")
	if err != nil || !found || len(labels) != 2 ||
		labels[api.RuntimeLabel] != "ready" || labels["brewlet.sh/runtime.static"] != "available" {
		t.Fatal("real-node template capabilities do not match the OnDemand inventory", labels, err)
	}
	raw, found, err := unstructured.NestedSlice(pool, "spec", "template", "spec", "startupTaints")
	if err != nil || !found || len(raw) != 1 {
		t.Fatal("real-node template lacks the unique startup fence", err)
	}
	encoded, err := json.Marshal(raw[0])
	if err != nil {
		t.Fatal(err)
	}
	var taint corev1.Taint
	if err := json.Unmarshal(encoded, &taint); err != nil {
		t.Fatal(err)
	}
	if taint.Key != want.Key || taint.Value != want.Value || taint.Effect != want.Effect {
		t.Fatal("real-node template startup taint differs from the provisioner tolerance")
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "Key=k8s.io/cluster-autoscaler/node-template/label/brewlet.sh/") {
			continue
		}
		count++
		if !strings.Contains(line, "PropagateAtLaunch=false") ||
			(!strings.Contains(line, "/brewlet.sh/runtime,Value=ready,") &&
				!strings.Contains(line, "/brewlet.sh/runtime.static,Value=available,")) {
			t.Fatal("EKS simulation tag leaks readiness or disagrees with inventory", line)
		}
	}
	if count != 2 {
		t.Fatal("EKS simulation recipe lost the baseline or per-ID capability", count)
	}
}

func TestProviderDocsKeepInitializationAndVerificationBoundaries(t *testing.T) {
	data, err := os.ReadFile("../docs/README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, required := range []string{
		"No EKS, Karpenter or AKS provider scale-from-zero has been verified",
		"managed-cluster-level", "node-image-only", "NodeInitializationTaintsPreview",
		"Ordinary AKS `--node-taints` are pinned", "An OnDemand per-ID pin is not supported",
	} {
		if !strings.Contains(text, required) {
			t.Fatal("provider contract or unverified-infrastructure boundary missing", required)
		}
	}
}
