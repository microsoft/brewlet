package provisioner

import (
	"strings"
	"testing"
)

func TestDigestPreservingMirror(t *testing.T) {
	source := "registry.example.com/golden/runtime@sha256:" + strings.Repeat("a", 64)
	got, err := SourceReference(source, map[string]string{"registry.example.com": "mirror.internal/base"}, []string{"mirror.internal"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "mirror.internal/base/golden/runtime@sha256:"+strings.Repeat("a", 64) {
		t.Fatal(got)
	}
}
func TestSourcePolicyRejectsBeforePull(t *testing.T) {
	for _, source := range []string{"registry.example.com/golden/runtime:latest", "runtime", "https://registry.example.com/runtime@sha256:" + strings.Repeat("a", 64)} {
		if _, err := SourceReference(source, nil, nil); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	source := "registry.example.com/golden/runtime@sha256:" + strings.Repeat("a", 64)
	if _, err := SourceReference(source, map[string]string{"registry.example.com": "evil.internal/base"}, nil); err == nil {
		t.Fatal("accepted unauthorized mirror")
	}
}

func TestSourceReferenceUsesSharedMirrorPolicy(t *testing.T) {
	source := "registry.example.com/golden/runtime@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		name    string
		mirrors map[string]string
		allowed []string
	}{
		{"self mirror", map[string]string{"registry.example.com": "registry.example.com/base"}, []string{"registry.example.com"}},
		{"duplicate allowlist", nil, []string{"mirror.internal", "mirror.internal"}},
		{"malformed unused allowlist", nil, []string{"mirror.internal/base"}},
		{"unapproved host", map[string]string{"registry.example.com": "evil.internal/base"}, []string{"mirror.internal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved, err := SourceReference(source, tc.mirrors, tc.allowed)
			if err == nil || resolved != "" {
				t.Fatalf("invalid policy produced a pull reference: %q, %v", resolved, err)
			}
		})
	}
	if resolved, err := SourceReference(source, nil, []string{"mirror.internal:5000"}); err != nil || resolved != source {
		t.Fatalf("valid unused allowlist changed source: %q, %v", resolved, err)
	}
}
