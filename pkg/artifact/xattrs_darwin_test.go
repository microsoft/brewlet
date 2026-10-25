//go:build darwin

package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRejectSourceXattrsDarwin(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "host")
	must(t, os.WriteFile(host, []byte("x"), 0o644))
	if e := unix.Setxattr(host, "com.apple.quarantine", []byte("0081;00000000;test;"), 0); e != nil {
		t.Skipf("cannot set com.apple.quarantine: %v", e)
	}
	_ = unix.Setxattr(host, "com.apple.provenance", []byte{1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0)
	if e := rejectSourceXattrs(host); e != nil {
		t.Fatalf("host-local macOS xattrs rejected: %v", e)
	}

	bad := filepath.Join(dir, "bad")
	must(t, os.WriteFile(bad, []byte("x"), 0o644))
	must(t, unix.Setxattr(bad, "user.test", []byte("v"), 0))
	e := rejectSourceXattrs(bad)
	if Reason(e) != "LayerPathViolation" || !strings.Contains(e.Error(), "user.test") || !strings.Contains(e.Error(), bad) {
		t.Fatalf("want LayerPathViolation naming user.test and path, got %v", e)
	}
}
