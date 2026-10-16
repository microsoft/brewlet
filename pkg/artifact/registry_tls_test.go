package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryExplicitCAFileFailsClosed(t *testing.T) {
	dir := testDir(t)
	file := filepath.Join(dir, "ca.pem")
	t.Setenv("SSL_CERT_FILE", file)
	if _, e := NewRegistry("registry.example.test/image:v1", false); e == nil || !strings.Contains(e.Error(), "SSL_CERT_FILE") {
		t.Fatalf("missing explicit trust silently ignored: %v", e)
	}
	must(t, os.WriteFile(file, []byte("not a CA certificate"), 0600))
	if _, e := NewRegistry("registry.example.test/image:v1", false); e == nil || !strings.Contains(e.Error(), "SSL_CERT_FILE") {
		t.Fatalf("unusable explicit trust silently ignored: %v", e)
	}
}
