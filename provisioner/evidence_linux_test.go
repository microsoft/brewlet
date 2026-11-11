//go:build linux

package provisioner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const bundledCollectorMarker = "bundled-collector"

// TestEvidenceHelperProcess stands in for nsenter running the collector passed
// as fd 3, so the test never executes files from noexec temporary mounts.
func TestEvidenceHelperProcess(t *testing.T) {
	if os.Getenv("BREWLET_EVIDENCE_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	content, err := io.ReadAll(os.NewFile(3, "collector"))
	if err != nil || string(content) != bundledCollectorMarker || len(args) != 3 || args[1] != "/proc/self/fd/3" || args[2] != "--brewlet-evidence" {
		fmt.Println(`{"error":"unexpected collector invocation"}`)
		os.Exit(0)
	}
	fmt.Println(`{"gone":true}`)
	os.Exit(0)
}

func TestHostEvidenceFallbackAfterShimRemoval(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("trusted evidence collectors must be root-owned")
	}
	original := evidenceCommand
	t.Cleanup(func() { evidenceCommand = original })
	evidenceCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if name != "nsenter" {
			t.Fatalf("unexpected evidence command %q", name)
		}
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestEvidenceHelperProcess$", "--"}, args[len(args)-2:]...)...)
		cmd.Env = append(os.Environ(), "BREWLET_EVIDENCE_HELPER=1")
		return cmd
	}
	dir := t.TempDir()
	bundled := filepath.Join(dir, "collector")
	if err := os.WriteFile(bundled, []byte(bundledCollectorMarker), 0755); err != nil {
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
