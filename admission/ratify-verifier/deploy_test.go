// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestDeployedVerifierUsesV145ChartFields(t *testing.T) {
	raw, err := os.ReadFile("../deploy/20-ratify-verifier.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Spec map[string]any `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	// The pinned v1.4.5 chart CRD omits type, even though its Go SDK has it.
	// The live API server rejects the entire Verifier when this field is set.
	if _, exists := manifest.Spec["type"]; exists {
		t.Fatal("Ratify v1.4.5 chart CRD rejects spec.type; select the plugin with spec.name")
	}
	if manifest.Spec["name"] != pluginName {
		t.Fatalf("spec.name = %v, want %q", manifest.Spec["name"], pluginName)
	}
}

const deployDir = "../deploy"

type deployManifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec map[string]any `json:"spec"`
}

// expectedDeploy pins every shipped admission manifest's identity, so a new,
// renamed, or retyped file cannot reach users without a contract here.
var expectedDeploy = map[string][3]string{
	"10-ratify-store.yaml":                  {"config.ratify.deislabs.io/v1beta1", "Store", "store-oras"},
	"20-ratify-verifier.yaml":               {"config.ratify.deislabs.io/v1beta1", "Verifier", "verifier-brewlet-managed-dependencies"},
	"30-ratify-policy.yaml":                 {"config.ratify.deislabs.io/v1beta1", "Policy", "ratify-policy"},
	"40-gatekeeper-constrainttemplate.yaml": {"templates.gatekeeper.sh/v1", "ConstraintTemplate", "brewletmanageddependencies"},
	"50-gatekeeper-constraint.yaml":         {"constraints.gatekeeper.sh/v1beta1", "BrewletManagedDependencies", "brewlet-managed-dependencies"},
}

func loadDeploy(t *testing.T, name string) deployManifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(deployDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\n---") {
		t.Fatalf("%s: one resource per file is expected", name)
	}
	var m deployManifest
	if err := yaml.UnmarshalStrict(raw, &m); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m
}

func nested(t *testing.T, name string, v any, path ...string) any {
	t.Helper()
	for _, key := range path {
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("%s: %s is not an object", name, strings.Join(path, "."))
		}
		if v, ok = obj[key]; !ok {
			t.Fatalf("%s: missing %s", name, strings.Join(path, "."))
		}
	}
	return v
}

func TestDeployInventoryAndIdentities(t *testing.T) {
	entries, err := os.ReadDir(deployDir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	want := []string{"config.json"}
	for name := range expectedDeploy {
		want = append(want, name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("admission/deploy contains %v, want %v; add contracts for new manifests", got, want)
	}
	for name, id := range expectedDeploy {
		m := loadDeploy(t, name)
		if m.APIVersion != id[0] || m.Kind != id[1] || m.Metadata.Name != id[2] {
			t.Errorf("%s: %s %s/%s, want %s %s/%s", name, m.APIVersion, m.Kind, m.Metadata.Name, id[0], id[1], id[2])
		}
		if len(m.Spec) == 0 {
			t.Errorf("%s: empty spec", name)
		}
	}
}

func TestDeployedStoreDiscoversNativeReferrers(t *testing.T) {
	const name = "10-ratify-store.yaml"
	m := loadDeploy(t, name)
	if m.Spec["name"] != "oras" {
		t.Fatalf("spec.name = %v, want oras", m.Spec["name"])
	}
	// Brewlet publishes no cosign signatures; enabling cosign adds lookups only.
	if nested(t, name, m.Spec, "parameters", "cosignEnabled") != false {
		t.Fatal("spec.parameters.cosignEnabled must be false")
	}
}

func TestConstraintBindsTemplateAndFailsClosed(t *testing.T) {
	const name = "50-gatekeeper-constraint.yaml"
	tmpl := loadDeploy(t, "40-gatekeeper-constrainttemplate.yaml")
	constraint := loadDeploy(t, name)

	kind := nested(t, "template", tmpl.Spec, "crd", "spec", "names", "kind")
	if constraint.Kind != kind {
		t.Fatalf("constraint kind %q does not match template CRD kind %v", constraint.Kind, kind)
	}
	// Gatekeeper requires the template name to be the lowercase CRD kind.
	if tmpl.Metadata.Name != strings.ToLower(constraint.Kind) {
		t.Fatalf("template name %q, want %q", tmpl.Metadata.Name, strings.ToLower(constraint.Kind))
	}
	targets, _ := tmpl.Spec["targets"].([]any)
	if len(targets) != 1 || nested(t, "template", targets[0], "target") != "admission.k8s.gatekeeper.sh" {
		t.Fatalf("template targets = %v, want one admission.k8s.gatekeeper.sh target", targets)
	}
	pkg := "package " + tmpl.Metadata.Name + "\n"
	if rego, _ := nested(t, "template", targets[0], "rego").(string); !strings.HasPrefix(rego, pkg) {
		t.Fatalf("template rego must start with %q", pkg)
	}

	if constraint.Spec["enforcementAction"] != "deny" {
		t.Fatalf("enforcementAction = %v, want deny", constraint.Spec["enforcementAction"])
	}
	kinds, _ := nested(t, name, constraint.Spec, "match", "kinds").([]any)
	matchesPods := false
	for _, k := range kinds {
		groups, _ := nested(t, name, k, "apiGroups").([]any)
		names, _ := nested(t, name, k, "kinds").([]any)
		if slices.Contains(groups, any("")) && slices.Contains(names, any("Pod")) {
			matchesPods = true
		}
	}
	if !matchesPods {
		t.Fatalf("match.kinds = %v, must include core Pods", kinds)
	}
	excluded, _ := nested(t, name, constraint.Spec, "match").(map[string]any)["excludedNamespaces"].([]any)
	for _, ns := range excluded {
		if !slices.Contains([]any{"kube-system", "gatekeeper-system", "ratify-service"}, ns) {
			t.Errorf("excludedNamespaces includes %v; only bootstrap infrastructure may bypass admission", ns)
		}
	}
}

func TestCLIConfigMatchesClusterStoreAndVerifier(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(deployDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	plugins, _ := nested(t, "config.json", cfg, "store", "plugins").([]any)
	if len(plugins) != 1 || nested(t, "config.json", plugins[0], "name") != "oras" ||
		nested(t, "config.json", plugins[0], "cosignEnabled") != false {
		t.Fatalf("store.plugins = %v, want one oras store with cosignEnabled false", plugins)
	}
	verifiers, _ := nested(t, "config.json", cfg, "verifier", "plugins").([]any)
	found := false
	for _, v := range verifiers {
		if nested(t, "config.json", v, "name") == pluginName {
			found = true
		}
	}
	if !found {
		t.Fatalf("verifier.plugins = %v, must include %s", verifiers, pluginName)
	}
}
