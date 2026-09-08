// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestNodeProfileOwnershipStatusDeepCopy(t *testing.T) {
	profile := NodeProfile{Status: NodeProfileStatus{
		Targets:                []NodeTarget{{Name: "worker", UID: types.UID("node-uid"), Claimed: true}},
		MigrationDaemonSetUIDs: []types.UID{"legacy-uid"},
		ProvisioningSpec:       &NodeProfileSpec{NodePool: NodePoolRef{Names: []string{"old"}}},
		Retirement: &NodeRetirement{
			Targets: []NodeTarget{{Name: "worker", UID: types.UID("node-uid"), Claimed: true}},
			Phase:   RetirementCleaning, Generation: 3,
			Spec: NodeProfileSpec{Tolerations: []corev1.Toleration{{Key: "dedicated"}}},
		},
	}}
	copy := profile.DeepCopy()
	copy.Status.Targets[0].UID = "different"
	copy.Status.MigrationDaemonSetUIDs[0] = "different"
	copy.Status.ProvisioningSpec.NodePool.Names[0] = "new"
	copy.Status.Retirement.Targets[0].Name = "different"
	copy.Status.Retirement.Spec.Tolerations[0].Key = "different"
	if profile.Status.Targets[0].UID != "node-uid" ||
		profile.Status.MigrationDaemonSetUIDs[0] != "legacy-uid" ||
		profile.Status.ProvisioningSpec.NodePool.Names[0] != "old" ||
		profile.Status.Retirement.Targets[0].Name != "worker" ||
		profile.Status.Retirement.Spec.Tolerations[0].Key != "dedicated" {
		t.Fatal("ownership status snapshots must not alias informer data")
	}
	encoded, err := json.Marshal(profile.Status)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"targets":[{"name":"worker","uid":"node-uid","claimed":true}]`, `"phase":"Cleaning"`, `"generation":3`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Fatalf("missing ownership protocol %s in %s", fragment, encoded)
		}
	}
}

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
