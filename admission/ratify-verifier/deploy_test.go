// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
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
