package v2alpha1

import (
	"bytes"
	"io"
	"os/exec"
	"testing"

	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestChartDefaultRBACAndUnsupportedFeatureGuards(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm unavailable")
	}
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
