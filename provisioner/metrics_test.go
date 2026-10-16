package provisioner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSharedDiskBytesDoNotFollowLinks(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".disk-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "bytes"), []byte("12345"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bytes", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	n, err := logicalBytes(dir)
	if err != nil || n != 5 {
		t.Fatal(n, err)
	}
	if n, err := logicalBytes(filepath.Join(dir, "absent")); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
