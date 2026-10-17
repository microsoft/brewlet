//go:build linux

package provisioner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func copySafetyFixture(t *testing.T) (string, string, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root to preserve image ownership")
	}
	base, err := os.MkdirTemp(".", ".copy-safety-")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	source, destination := filepath.Join(base, "source"), filepath.Join(base, "destination")
	for _, dir := range []string{source, destination} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return base, source, destination
}

func TestRuntimeCopyNeverTraversesAnImageSymlinkIntoHostFiles(t *testing.T) {
	base, source, destination := copySafetyFixture(t)
	outside := filepath.Join(base, "host")
	if err := os.Mkdir(outside, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("host bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("image bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := copyRoot(context.Background(), source, destination); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(destination, "link"))
	if err != nil || link != outside {
		t.Fatal("image symlink was followed or rewritten during copy", link, err)
	}
	for path, want := range map[string]string{sentinel: "host bytes", filepath.Join(destination, "payload"): "image bytes"} {
		bytes, err := os.ReadFile(path)
		if err != nil || string(bytes) != want {
			t.Fatal("copy changed host or ordinary image bytes", path, string(bytes), err)
		}
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatal("copy wrote through an image symlink", entries, err)
	}
}

func TestRuntimeCopyRejectsUnsafeEntriesWithoutSanitizingThem(t *testing.T) {
	for _, kind := range []string{"fifo", "setuid", "setgid"} {
		t.Run(kind, func(t *testing.T) {
			_, source, destination := copySafetyFixture(t)
			entry := filepath.Join(source, "unsafe")
			if kind == "fifo" {
				if err := unix.Mkfifo(entry, 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(entry, []byte("image bytes"), 0755); err != nil {
					t.Fatal(err)
				}
				mode := os.ModeSetuid
				if kind == "setgid" {
					mode = os.ModeSetgid
				}
				if err := os.Chmod(entry, mode|0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := copyRoot(context.Background(), source, destination); err == nil {
				t.Fatal("unsafe source entry copied or silently sanitized")
			}
			if _, err := os.Lstat(filepath.Join(destination, "unsafe")); !os.IsNotExist(err) {
				t.Fatal("rejected source entry was published", err)
			}
		})
	}
}
