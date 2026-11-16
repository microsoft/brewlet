package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/inventory"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestHumanSize(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 20: "5.0 MiB", 3 << 30: "3.0 GiB"} {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestOutputFlagRejectsUnknownFormat(t *testing.T) {
	var out, errOut bytes.Buffer
	err := dispatch(context.Background(), []string{"push", "registry.example.test/a:v1", "-o", "yaml"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "text|json") {
		t.Fatalf("unknown format accepted or unclear: %v", err)
	}
}

func TestWriteVerificationKeepsChecksDistinct(t *testing.T) {
	var out bytes.Buffer
	err := writeVerification(&out, "  ", artifact.CompositionInspection{
		Structural: true, TestReleaseApproval: "none", PolicyMode: "production",
		Predicate: artifact.CompositionPredicate{
			Platform:      v1.Platform{OS: "linux", Architecture: "arm64"},
			DependencySet: v1.Descriptor{Digest: digest.FromString("deps")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"Structure:", "Publisher provenance:", "Composition signature: not verified", "linux/arm64", digest.FromString("deps").String()} {
		if !strings.Contains(text, want) {
			t.Errorf("verification text missing %q:\n%s", want, text)
		}
	}
	if !regexp.MustCompile(`Structure:\s+verified`).MatchString(text) {
		t.Errorf("structural check must read as verified:\n%s", text)
	}
}

func TestWriteInventoryShowsUnverifiedReasons(t *testing.T) {
	report := inventory.Report{
		Cluster:    "https://cluster.example.test",
		Namespaces: []string{"payments"},
		FinishedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Containers: []inventory.Container{
			{Pod: inventory.ObjectIdentity{Namespace: "payments", Name: "api-1"}, Workload: inventory.ObjectIdentity{Kind: "Deployment", Name: "api"}, Name: "app", NodeName: "node-a", Status: "verified"},
			{Pod: inventory.ObjectIdentity{Namespace: "payments", Name: "api-2"}, Workload: inventory.ObjectIdentity{Kind: "Deployment", Name: "api"}, Name: "app", Status: "unverified", Reason: "no binding"},
		},
		Warnings: []string{"namespace payments partially visible"},
	}
	var out bytes.Buffer
	if err := writeInventory(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Inventory of https://cluster.example.test", "Complete:", "no", "Containers (2):", "payments/api-2", "unverified", "no binding", "Warnings:", "namespace payments partially visible"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("inventory text missing %q:\n%s", want, out.String())
		}
	}
}
