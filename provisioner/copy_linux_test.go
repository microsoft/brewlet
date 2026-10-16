//go:build linux

package provisioner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeCopyPreservesDirectorySGIDStickyAndMtime(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to preserve image ownership")
	}
	base, err := os.MkdirTemp(".", ".copy-root-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	source := filepath.Join(base, "source")
	destination := filepath.Join(base, "destination")
	for _, name := range []string{source, destination, filepath.Join(source, "var/local"), filepath.Join(source, "tmp")} {
		if err := os.MkdirAll(name, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(source, "var/local"), os.ModeSetgid|02775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "tmp"), os.ModeSticky|01777); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1000, 0)
	if err := os.Chtimes(filepath.Join(source, "var/local"), mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := copyRoot(context.Background(), source, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"var/local", "tmp"} {
		from, err := os.Stat(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		to, err := os.Stat(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if from.Mode() != to.Mode() || !from.ModTime().Equal(to.ModTime()) {
			t.Fatal("directory bytes/metadata silently changed", from.Mode(), to.Mode(), from.ModTime(), to.ModTime())
		}
	}
}
