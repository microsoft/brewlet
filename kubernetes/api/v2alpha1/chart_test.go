package v2alpha1

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"testing"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestRuntimeClassOverhead(t *testing.T) {
	requireHelm(t)
	render := func(args ...string) ([]byte, error) {
		base := []string{"template", "test", "../../charts/brewlet", "--namespace", "brewlet-system"}
		return exec.Command("helm", append(base, args...)...).CombinedOutput()
	}
	decode := func(t *testing.T, data []byte) nodev1.RuntimeClass {
		t.Helper()
		decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var item nodev1.RuntimeClass
			if err := decoder.Decode(&item); err != nil {
				t.Fatalf("decode RuntimeClass: %v", err)
			}
			if item.Kind == "RuntimeClass" {
				return item
			}
		}
	}
	raw, err := os.ReadFile("../../deploy/runtimeclass.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defaults := decode(t, raw)
	if defaults.Overhead == nil {
		t.Fatal("raw RuntimeClass has no default overhead")
	}
	for _, tc := range []struct {
		name, cpu, memory string
		override          bool
	}{
		{"defaults", "50m", "64Mi", false},
		{"overrides", "125m", "128Mi", true},
		{"zero-is-explicit", "0", "0", true},
		{"exponent-and-fraction", "1e-3", "1.5Gi", true},
		{"positive-sign", "+.5", "+2Mi", true},
		{"sub-milli-quantity", "1u", "1n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var args []string
			if tc.override {
				args = []string{"--set-string", "runtimeClass.overhead.cpu=" + tc.cpu,
					"--set-string", "runtimeClass.overhead.memory=" + tc.memory}
			}
			data, err := render(args...)
			if err != nil {
				t.Fatalf("template: %v: %s", err, data)
			}
			actual := decode(t, data)
			if actual.Overhead == nil || len(actual.Overhead.PodFixed) != 2 {
				t.Fatalf("missing or unexpected overhead: %+v", actual.Overhead)
			}
			for name, value := range map[string]string{"cpu": tc.cpu, "memory": tc.memory} {
				expected := resource.MustParse(value)
				quantity, ok := actual.Overhead.PodFixed[corev1.ResourceName(name)]
				if !ok || quantity.Cmp(expected) != 0 {
					t.Fatalf("%s: got %s, expected %s", name, quantity.String(), expected.String())
				}
				if !tc.override {
					rawQuantity := defaults.Overhead.PodFixed[corev1.ResourceName(name)]
					if quantity.Cmp(rawQuantity) != 0 {
						t.Fatalf("chart/raw default mismatch for %s", name)
					}
				}
			}
			if actual.Name != "brewlet" || actual.Handler != "brewlet" ||
				actual.Annotations["helm.sh/resource-policy"] != "keep" ||
				actual.Scheduling == nil || len(actual.Scheduling.NodeSelector) != 1 ||
				actual.Scheduling.NodeSelector["brewlet.sh/runtime"] != "ready" {
				t.Fatalf("overhead changed the RuntimeClass contract: %+v", actual)
			}
		})
	}
	for _, option := range []string{
		"runtimeClass=null", "runtimeClass.overhead=null",
		"runtimeClass.overhead.cpu=true", "runtimeClass.overhead.cpu=1",
		"runtimeClass.overhead.memory=128", "runtimeClass.overhead.memory=false",
		"runtimeClass.overhead.cpu=", "runtimeClass.overhead.memory=",
		"runtimeClass.overhead.cpu=-1m", "runtimeClass.overhead.memory=-1Gi",
		"runtimeClass.overhead.cpu=not-a-quantity", "runtimeClass.overhead.memory=1.2.3Gi",
		"runtimeClass.overhead.cpu=1K", "runtimeClass.overhead.cup=50m",
	} {
		t.Run("invalid/"+option, func(t *testing.T) {
			if data, err := render("--set", option); err == nil {
				t.Fatalf("invalid overhead accepted: %s: %s", option, data)
			}
		})
	}
}

func TestChartDefaultRBACAndUnsupportedFeatureGuards(t *testing.T) {
	requireHelm(t)
	command := func(args ...string) ([]byte, error) {
		base := []string{"template", "test", "../../charts/brewlet", "--namespace", "brewlet-system"}
		return exec.Command("helm", append(base, args...)...).CombinedOutput()
	}
	out, err := command()
	if err != nil {
		t.Fatalf("template: %v: %s", err, out)
	}
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if doc["kind"] != "ClusterRole" {
			continue
		}
		rules, _ := doc["rules"].([]any)
		for _, value := range rules {
			rule := value.(map[string]any)
			groups, _ := rule["apiGroups"].([]any)
			apps := false
			for _, g := range groups {
				if g == "apps" {
					apps = true
				}
			}
			if !apps {
				continue
			}
			verbs, _ := rule["verbs"].([]any)
			for _, v := range verbs {
				if v != "get" && v != "list" && v != "watch" {
					t.Fatalf("default cluster RBAC can mutate workload: %v", rule)
				}
			}
		}
	}
	enabled, err := command("--set", "operator.rotationRestarts.enabled=true")
	if err != nil {
		t.Fatalf("restart opt-in chart: %v: %s", err, enabled)
	}
	if !bytes.Contains(enabled, []byte("--rotation-restarts=true")) || !bytes.Contains(enabled, []byte("resources: [deployments, statefulsets, daemonsets]")) {
		t.Fatal("restart opt-in did not enable explicit manager/RBAC")
	}
	if bytes.Contains(out, []byte("brewlet-node-metrics")) || bytes.Contains(out, []byte("--node-metrics-address")) {
		t.Fatal("node metrics were enabled without opt-in")
	}
	withMetrics, err := command("--set", "metrics.enabled=true")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(withMetrics, []byte("brewlet-node-metrics")) || !bytes.Contains(withMetrics, []byte("--node-metrics-address=:9090")) {
		t.Fatal("metrics opt-in did not expose genuine node endpoint")
	}
	for _, args := range [][]string{{"--set", "uninstall.timeoutSeconds=1.5"}, {"--set", "uninstall.timeoutSeconds=0"}, {"--set", "uninstall.timeoutSeconds=86401"}} {
		if _, err := command(args...); err == nil {
			t.Fatalf("unsupported/unsafe chart option accepted: %v", args)
		}
	}
}
