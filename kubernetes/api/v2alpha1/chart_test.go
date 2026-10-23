package v2alpha1

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"slices"
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

func TestChartRotationRestartAdmissionPolicyAndNamespacedRBAC(t *testing.T) {
	requireHelm(t)
	command := func(args ...string) ([]byte, error) {
		base := []string{"template", "test", "../../charts/brewlet", "--namespace", "brewlet-system", "--set", "operator.rotationRestarts.enabled=true"}
		return exec.Command("helm", append(base, args...)...).CombinedOutput()
	}
	docs := func(out []byte) []map[string]any {
		t.Helper()
		var all []map[string]any
		decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
		for {
			var doc map[string]any
			if err := decoder.Decode(&doc); err == io.EOF {
				return all
			} else if err != nil {
				t.Fatal(err)
			}
			if doc != nil {
				all = append(all, doc)
			}
		}
	}
	kinds := func(all []map[string]any, kind string) []map[string]any {
		var out []map[string]any
		for _, d := range all {
			if d["kind"] == kind {
				out = append(out, d)
			}
		}
		return out
	}
	out, err := command()
	if err != nil {
		t.Fatalf("template: %v: %s", err, out)
	}
	all := docs(out)
	policies := kinds(all, "ValidatingAdmissionPolicy")
	bindings := kinds(all, "ValidatingAdmissionPolicyBinding")
	if len(policies) != 1 || len(bindings) != 1 {
		t.Fatalf("restart opt-in must ship exactly one policy and binding: %d/%d", len(policies), len(bindings))
	}
	spec := policies[0]["spec"].(map[string]any)
	if spec["failurePolicy"] != "Fail" {
		t.Fatal("restart policy must fail closed")
	}
	if actions := bindings[0]["spec"].(map[string]any)["validationActions"].([]any); len(actions) != 1 || actions[0] != "Deny" {
		t.Fatalf("restart policy binding must deny: %v", actions)
	}
	for _, want := range []string{
		`request.userInfo.username == "system:serviceaccount:brewlet-system:brewlet-operator"`,
		`values: ["brewlet-system"]`,
		`request.operation == 'UPDATE'`,
		`["kube-system","kube-public","kube-node-lease"]`,
		`'brewlet.sh/restart-on-rotation' in variables.oldMeta`,
		`object.spec.template.spec == oldObject.spec.template.spec`,
		`k == 'template'`,
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("restart policy lacks %q", want)
		}
	}
	if bytes.Contains(out, []byte("name: test-brewlet-rotation-restarts\n  namespace:")) || bytes.Contains(out, []byte("--restart-namespaces")) {
		t.Fatal("cluster-wide restart mode rendered namespaced restart RBAC")
	}

	scoped, err := command("--set", "operator.rotationRestarts.namespaces={apps,payments}")
	if err != nil {
		t.Fatalf("namespaced restarts: %v: %s", err, scoped)
	}
	all = docs(scoped)
	for _, cr := range kinds(all, "ClusterRole") {
		for _, value := range cr["rules"].([]any) {
			rule := value.(map[string]any)
			for _, v := range rule["verbs"].([]any) {
				if slices.Contains(rule["apiGroups"].([]any), any("apps")) && v != "get" && v != "list" && v != "watch" {
					t.Fatalf("namespaced restarts kept cluster-wide workload mutation: %v", rule)
				}
			}
		}
	}
	patchRoles := map[string]bool{}
	for _, role := range kinds(all, "Role") {
		meta := role["metadata"].(map[string]any)
		if meta["name"] != "test-brewlet-rotation-restarts" {
			continue
		}
		rules := role["rules"].([]any)
		if len(rules) != 1 || len(rules[0].(map[string]any)["verbs"].([]any)) != 1 {
			t.Fatalf("restart role grants more than patch: %v", rules)
		}
		patchRoles[meta["namespace"].(string)] = true
	}
	if len(patchRoles) != 2 || !patchRoles["apps"] || !patchRoles["payments"] {
		t.Fatalf("restart patch roles not rendered per allowlisted namespace: %v", patchRoles)
	}
	if !bytes.Contains(scoped, []byte("--restart-namespaces=apps,payments")) || !bytes.Contains(scoped, []byte(`request.namespace in ["apps","payments"]`)) {
		t.Fatal("namespace allowlist not passed to operator and admission policy")
	}

	for _, ns := range []string{"brewlet-system", "kube-system"} {
		if out, err := command("--set", "operator.rotationRestarts.namespaces={"+ns+"}"); err == nil {
			t.Fatalf("restart allowlist accepted %s: %s", ns, out)
		}
	}
	if out, err := command("--kube-version", "1.29.0"); err == nil || !bytes.Contains(out, []byte("requireAdmissionPolicy=false")) {
		t.Fatalf("restarts enabled without ValidatingAdmissionPolicy support: %v: %s", err, out)
	}
	unbounded, err := command("--kube-version", "1.29.0", "--set", "operator.rotationRestarts.requireAdmissionPolicy=false")
	if err != nil || bytes.Contains(unbounded, []byte("ValidatingAdmissionPolicy")) || !bytes.Contains(unbounded, []byte("--rotation-restarts=true")) {
		t.Fatalf("explicit policy waiver not honoured: %v", err)
	}
	if defaults, err := exec.Command("helm", "template", "test", "../../charts/brewlet", "--namespace", "brewlet-system").CombinedOutput(); err != nil || bytes.Contains(defaults, []byte("ValidatingAdmissionPolicy")) {
		t.Fatalf("restart policy rendered without restart opt-in: %v", err)
	}
}
