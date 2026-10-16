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
