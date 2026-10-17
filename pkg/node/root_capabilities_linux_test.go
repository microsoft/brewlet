//go:build linux

package node

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRuntimeRootRejectsActualFileCapabilities(t *testing.T) {
	root, m := runtimeRootFixture(t)
	file := filepath.Join(root, "usr/bin/tool")
	if err := os.WriteFile(file, []byte("never executed"), 0755); err != nil {
		t.Fatal(err)
	}
	capabilities := make([]byte, 20)
	binary.LittleEndian.PutUint32(capabilities[0:4], 0x02000001)
	binary.LittleEndian.PutUint32(capabilities[4:8], 1<<10)
	if err := unix.Setxattr(file, "security.capability", capabilities, 0); err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) {
			t.Skipf("requires Linux file capabilities and test-only CAP_SETFCAP: %v", err)
		}
		t.Fatal(err)
	}
	if err := ValidateRoot(root, &m); err == nil || !strings.Contains(err.Error(), "file capability") {
		t.Fatalf("actual runtime file capability was not rejected: %v", err)
	}
}
