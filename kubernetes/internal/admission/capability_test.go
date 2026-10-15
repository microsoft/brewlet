// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"testing"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
)

func node(name string, ready bool, jdks string) corev1.Node {
	n := corev1.Node{}
	n.Name = name
	n.Labels = map[string]string{}
	n.Annotations = map[string]string{}
	if ready {
		n.Labels[brewlet.LabelRuntimeReady] = brewlet.ValueReady
	}
	if jdks != "" {
		n.Annotations[brewlet.AnnotationJDKs] = jdks
	}
	return n
}

func TestNodeCapabilityFrom(t *testing.T) {
	n := node("n1", true, "temurin-21,microsoft-25")
	c := NodeCapabilityFrom(&n)
	if !c.Ready {
		t.Fatal("expected node ready")
	}
	if len(c.JDKs) != 2 || c.JDKs[0] != "temurin-21" || c.JDKs[1] != "microsoft-25" {
		t.Fatalf("jdks = %v", c.JDKs)
	}
	n.Spec.Taints = []corev1.Taint{{Key: brewlet.StartupTaintKey, Effect: corev1.TaintEffectNoSchedule}}
	if NodeCapabilityFrom(&n).Ready {
		t.Fatal("a node still carrying the startup taint is not ready")
	}
}

func TestCompatibilityUsesCompactJDKInventory(t *testing.T) {
	for _, info := range []string{"", "{broken", `[{"distribution":"temurin","feature":17}]`} {
		n := node("worker", true, "temurin-21,microsoft-25")
		if info != "" {
			n.Annotations[brewlet.AnnotationJDKsInfo] = info
		}
		fleet := []NodeCapability{NodeCapabilityFrom(&n)}
		for _, request := range []string{"temurin-21", "21", "microsoft-25", "25"} {
			if result := CheckInventory(fleet, nil, request, nil); !result.Compatible {
				t.Errorf("compact JDK %q rejected with structured annotation %q: %+v", request, info, result)
			}
		}
		for _, request := range []string{"temurin-17", "17", "temurin-25"} {
			if result := CheckInventory(fleet, nil, request, nil); result.Compatible || result.DenyReason != brewlet.ReasonNoCompatibleJDK {
				t.Errorf("incompatible JDK %q accepted with structured annotation %q: %+v", request, info, result)
			}
		}
	}
}

func TestSupportsJDK(t *testing.T) {
	c := NodeCapability{JDKs: []string{"temurin-21", "microsoft-25"}}
	cases := []struct {
		req  string
		want bool
	}{
		{"", true},
		{"temurin-21", true},
		{"microsoft-25", true},
		{"temurin-25", false}, // dist mismatch
		{"21", true},          // bare feature
		{"25", true},
		{"17", false},
	}
	for _, tc := range cases {
		if got := c.supportsJDK(tc.req); got != tc.want {
			t.Errorf("supportsJDK(%q) = %v, want %v", tc.req, got, tc.want)
		}
	}
}

func TestCheckFleet(t *testing.T) {
	fleet := []NodeCapability{
		{Name: "ready-a", Ready: true, Arch: "amd64", JDKs: []string{"temurin-21"}},
		{Name: "ready-b", Ready: true, Arch: "arm64", JDKs: []string{"microsoft-25"}},
		{Name: "not-ready", Ready: false, Arch: "amd64", JDKs: []string{"temurin-17"}},
	}

	// No explicit request: always compatible.
	if r := CheckInventory(fleet, nil, "", nil); !r.Compatible {
		t.Errorf("empty request should be compatible: %+v", r)
	}

	// Satisfiable requests.
	if r := CheckInventory(fleet, nil, "temurin-21", nil); !r.Compatible {
		t.Errorf("temurin-21 should be compatible: %+v", r)
	}

	// Unsatisfiable JDK -> NoCompatibleJDK.
	if r := CheckInventory(fleet, nil, "temurin-17", nil); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleJDK {
		t.Errorf("temurin-17 only on not-ready node: got %+v", r)
	}
	if r := CheckInventory(fleet, nil, "17", nil); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleJDK {
		t.Errorf("feature 17 not on ready fleet: got %+v", r)
	}

	// Arch constraint (non-portable artifact).
	if r := CheckInventory(fleet, nil, "", []string{"amd64"}); !r.Compatible {
		t.Errorf("amd64 available on ready-a: got %+v", r)
	}
	if r := CheckInventory(fleet, nil, "", []string{"amd64", "arm64"}); !r.Compatible {
		t.Errorf("amd64/arm64 both available: got %+v", r)
	}
	// Architecture never denies: an arch no ready node has is admitted with a
	// warning and left to node affinity + the autoscaler.
	if r := CheckInventory(fleet, nil, "", []string{"ppc64le"}); !r.Compatible || r.Warning == "" {
		t.Errorf("ppc64le absent -> admitted with warning: got %+v", r)
	}
	if r := CheckInventory(fleet, nil, "temurin-21", []string{"arm64"}); !r.Compatible || r.Warning == "" {
		t.Errorf("temurin-21 is amd64-only, arm64 requested -> admitted with warning: got %+v", r)
	}
	if r := CheckInventory(fleet, nil, "temurin-21", []string{"amd64"}); !r.Compatible || r.Warning != "" {
		t.Errorf("temurin-21 on amd64 ready node -> silent admit: got %+v", r)
	}
}

// Issue #239 regression: an empty ready fleet must not deny a request that a
// valid NodeProfile declares, or autoscaled pools can never scale from zero.
func TestCheckInventoryScaleFromZero(t *testing.T) {
	if r := CheckInventory(nil, nil, "21", nil); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleJDK {
		t.Fatalf("no nodes and no profiles must still deny: %+v", r)
	}
	profiles := []ProfileCapability{{Name: "java21", JDKs: []string{"microsoft-21"}}}
	r := CheckInventory(nil, profiles, "21", nil)
	if !r.Compatible || len(r.PendingProfiles) != 1 || r.PendingProfiles[0] != "java21" || r.Warning == "" {
		t.Fatalf("profile-declared JDK must be admitted with a warning: %+v", r)
	}
	if r := CheckInventory(nil, profiles, "temurin-21", nil); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleJDK {
		t.Fatalf("undeclared distribution must deny: %+v", r)
	}
}

// Profiles that are not yet backed by ready nodes still count when only some
// of the fleet is provisioned, and incompatible ready nodes don't mask them.
func TestCheckInventoryIncompatibleReadyNodesAndProfile(t *testing.T) {
	fleet := []NodeCapability{{Name: "a", Ready: true, Arch: "amd64", JDKs: []string{"temurin-17"}}}
	profiles := []ProfileCapability{{Name: "java25", JDKs: []string{"microsoft-25"}}}
	if r := CheckInventory(fleet, profiles, "microsoft-25", nil); !r.Compatible || r.Warning == "" {
		t.Fatalf("profile must admit despite incompatible ready node: %+v", r)
	}
}
