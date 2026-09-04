// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"testing"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
)

func node(name string, ready bool, jdks, launchers string) corev1.Node {
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
	if launchers != "" {
		n.Annotations[brewlet.AnnotationLaunchers] = launchers
	}
	return n
}

func TestNodeCapabilityFrom(t *testing.T) {
	n := node("n1", true, "temurin-21,microsoft-25", "java,jaz")
	n.Labels[brewlet.LabelAppCDSRegeneration] = "true"
	c := NodeCapabilityFrom(&n)
	if !c.Ready {
		t.Fatal("expected node ready")
	}
	if len(c.JDKs) != 2 || c.JDKs[0] != "temurin-21" || c.JDKs[1] != "microsoft-25" {
		t.Fatalf("jdks = %v", c.JDKs)
	}
	if len(c.Launchers) != 2 || c.Launchers[1] != "jaz" {
		t.Fatalf("launchers = %v", c.Launchers)
	}
	if !c.AppCDSRegeneration {
		t.Fatal("expected AppCDS regeneration policy to be projected")
	}
}

// The AppCDS capability key is a boolean-presence label. CAPABILITY_LABELS.md
// ("Contract v1") states the value is not part of the scheduling test and that
// consumers MUST NOT require "=true". A node bootstrapped from an immutable
// image or an autoscaler template may publish the key with any value, and the
// affinity Brewlet injects uses Operator: Exists, so the fleet pre-check must
// agree or it would deny pods the scheduler could have placed.
func TestNodeCapabilityAppCDSIsPresenceNotValue(t *testing.T) {
	for _, value := range []string{"true", "", "1", "enabled", "True"} {
		n := node("n1", true, "temurin-21", "java")
		n.Labels[brewlet.LabelAppCDSRegeneration] = value
		if c := NodeCapabilityFrom(&n); !c.AppCDSRegeneration {
			t.Errorf("label present with value %q must satisfy the presence contract", value)
		}
	}

	n := node("n1", true, "temurin-21", "java")
	if c := NodeCapabilityFrom(&n); c.AppCDSRegeneration {
		t.Error("absent label must not advertise AppCDS regeneration")
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

func TestSupportsLauncher(t *testing.T) {
	c := NodeCapability{Launchers: []string{"java", "jaz"}}
	for req, want := range map[string]bool{"": true, "java": true, "jaz": true, "graal": false} {
		if got := c.supportsLauncher(req); got != want {
			t.Errorf("supportsLauncher(%q) = %v, want %v", req, got, want)
		}
	}
	// Vanilla java is available even on a node with no launcher layers.
	bare := NodeCapability{}
	if !bare.supportsLauncher("java") || !bare.supportsLauncher("") {
		t.Error("vanilla java must be available on every node")
	}
	if bare.supportsLauncher("jaz") {
		t.Error("jaz must not be available without a launcher layer")
	}
}

func TestCheckFleet(t *testing.T) {
	fleet := []NodeCapability{
		{Name: "ready-a", Ready: true, Arch: "amd64", JDKs: []string{"temurin-21"}, Launchers: []string{"java"}},
		{Name: "ready-b", Ready: true, Arch: "arm64", JDKs: []string{"microsoft-25"}, Launchers: []string{"java", "jaz"}, AppCDSRegeneration: true},
		{Name: "not-ready", Ready: false, Arch: "amd64", JDKs: []string{"temurin-17"}, Launchers: []string{"java", "graal"}},
	}

	// No explicit request: always compatible.
	if r := CheckFleet(fleet, "", "", nil, false); !r.Compatible {
		t.Errorf("empty request should be compatible: %+v", r)
	}
	if r := CheckFleet(fleet, "", "java", nil, false); !r.Compatible {
		t.Errorf("vanilla launcher should be compatible: %+v", r)
	}

	// Satisfiable requests.
	if r := CheckFleet(fleet, "temurin-21", "", nil, false); !r.Compatible {
		t.Errorf("temurin-21 should be compatible: %+v", r)
	}
	if r := CheckFleet(fleet, "25", "jaz", nil, false); !r.Compatible {
		t.Errorf("feature 25 + jaz should be compatible (ready-b): %+v", r)
	}

	// Unsatisfiable JDK -> NoCompatibleJDK.
	if r := CheckFleet(fleet, "temurin-17", "", nil, false); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleJDK {
		t.Errorf("temurin-17 only on not-ready node: got %+v", r)
	}
	if r := CheckFleet(fleet, "17", "", nil, false); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleJDK {
		t.Errorf("feature 17 not on ready fleet: got %+v", r)
	}

	// JDK exists but not with the requested launcher -> NoCompatibleLauncher.
	if r := CheckFleet(fleet, "temurin-21", "jaz", nil, false); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleLauncher {
		t.Errorf("temurin-21 has no jaz: got %+v", r)
	}

	// Launcher exists nowhere ready -> NoCompatibleLauncher.
	if r := CheckFleet(fleet, "", "graal", nil, false); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleLauncher {
		t.Errorf("graal only on not-ready node: got %+v", r)
	}

	// Arch constraint (non-portable artifact).
	if r := CheckFleet(fleet, "", "", []string{"amd64"}, false); !r.Compatible {
		t.Errorf("amd64 available on ready-a: got %+v", r)
	}
	if r := CheckFleet(fleet, "", "", []string{"amd64", "arm64"}, false); !r.Compatible {
		t.Errorf("amd64/arm64 both available: got %+v", r)
	}
	// arm64 only ready node is ready-b (microsoft-25), so amd64-only JDK + arm64 must be denied by arch.
	if r := CheckFleet(fleet, "", "", []string{"ppc64le"}, false); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleArch {
		t.Errorf("ppc64le not on ready fleet -> NoCompatibleArch: got %+v", r)
	}
	// JDK + arch that can't be jointly satisfied: temurin-21 is amd64 only; arm64 request -> arch denial.
	if r := CheckFleet(fleet, "temurin-21", "", []string{"arm64"}, false); r.Compatible || r.DenyReason != brewlet.ReasonNoCompatibleArch {
		t.Errorf("temurin-21 is amd64-only, arm64 requested -> NoCompatibleArch: got %+v", r)
	}

	// Regeneration must be authorized on the same ready node that satisfies all
	// other requested capabilities.
	if r := CheckFleet(fleet, "25", "jaz", []string{"arm64"}, true); !r.Compatible {
		t.Errorf("ready-b authorizes regeneration and satisfies the request: %+v", r)
	}
	if r := CheckFleet(fleet, "temurin-21", "", []string{"amd64"}, true); r.Compatible ||
		r.DenyReason != brewlet.ReasonAppCDSRegenerationDisabled {
		t.Errorf("otherwise-compatible ready-a is not policy-authorized: %+v", r)
	}
	if r := CheckFleet(nil, "", "", nil, true); r.Compatible ||
		r.DenyReason != brewlet.ReasonAppCDSRegenerationDisabled {
		t.Errorf("regeneration-only request without an authorized node: %+v", r)
	}
}
