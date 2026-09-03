// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAppCDSRegenerationJSON(t *testing.T) {
	disabled, err := json.Marshal(NodeProfileSpec{})
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(disabled), "regenerationEnabled") {
		t.Fatalf("false policy must remain omitted, got %s", disabled)
	}
	if strings.Contains(string(disabled), "appCDS") {
		t.Fatalf("unset AppCDS policy must remain omitted, got %s", disabled)
	}

	enabled, err := json.Marshal(NodeProfileSpec{
		AppCDS: &AppCDSSpec{RegenerationEnabled: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(enabled), `"appCDS":{"regenerationEnabled":true}`) {
		t.Fatalf("enabled policy did not use the approved API contract: %s", enabled)
	}
}

func TestAppCDSPolicyManifests(t *testing.T) {
	cases := map[string][]string{
		"../../../deploy/nodeprofile-crd.yaml": {
			"appCDS:", "regenerationEnabled:", "default: false",
		},
		"../../../charts/brewlet/crds/nodeprofile-crd.yaml": {
			"appCDS:", "regenerationEnabled:", "default: false",
		},
		"../../../charts/brewlet/values.yaml": {
			"appCDS:", "regenerationEnabled: false",
		},
		"../../../charts/brewlet/templates/nodeprofiles.yaml": {
			"appCDS:", "regenerationEnabled: true",
		},
	}
	for path, expected := range cases {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, want := range expected {
			if !strings.Contains(string(content), want) {
				t.Errorf("%s does not contain %q", path, want)
			}
		}
	}
}
