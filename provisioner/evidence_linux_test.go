//go:build linux

package provisioner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostEvidenceFallbackAfterShimRemoval(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("trusted evidence collectors must be root-owned")
	}
	dir := t.TempDir()
	nsenter := filepath.Join(dir, "nsenter")
	if err := os.WriteFile(nsenter, []byte("#!/bin/sh\nwhile [ \"$1\" != -- ]; do shift; done\nshift\nexec \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	bundled := filepath.Join(dir, "collector")
	if err := os.WriteFile(bundled, []byte("#!/bin/sh\nprintf '{\"gone\":true}\\n'\n"), 0755); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(dir, "host")
	evidence := HostEvidence{HostRoot: host, FallbackBinary: bundled}
	gone, err := evidence.MountsGone(context.Background(), "/opt/brewlet/runtimes/test")
	if err != nil || !gone {
		t.Fatalf("bundled collector did not produce evidence: gone=%v err=%v", gone, err)
	}

	if err := os.Chmod(bundled, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.MountsGone(context.Background(), "/opt/brewlet/runtimes/test"); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("writable bundled collector accepted: %v", err)
	}
	if err := os.Chmod(bundled, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "collector-link")
	if err := os.Symlink(bundled, link); err != nil {
		t.Fatal(err)
	}
	evidence.FallbackBinary = link
	if _, err := evidence.MountsGone(context.Background(), "/opt/brewlet/runtimes/test"); err == nil {
		t.Fatal("symlinked bundled collector accepted")
	}
	evidence.FallbackBinary = bundled
	installed := filepath.Join(host, "usr/local/bin/containerd-shim-brewlet-v2")
	if err := os.MkdirAll(filepath.Dir(installed), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("untrusted"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(installed, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := evidence.MountsGone(context.Background(), "/opt/brewlet/runtimes/test"); err == nil || !strings.Contains(err.Error(), "untrusted") {
		t.Fatalf("untrusted installed collector was bypassed: %v", err)
	}
}
