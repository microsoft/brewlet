package node

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeDirectorySGIDAllowedButSetIDRegularFileRejected(t *testing.T) {
	root := testDirectory(t)
	for _, name := range []string{"usr/bin", "bin", "etc", "home/app", "var/local"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("app:x:1000:1000:app:/home/app:/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:1000:\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "var/local"), os.ModeSetgid|0755); err != nil {
		t.Fatal(err)
	}
	m := testMetadata()
	m.Platform.OS = "linux"
	m.Platform.Architecture = "amd64"
	if err := ValidateRoot(root, &m); err != nil {
		t.Fatal("directory SGID rejected", err)
	}
	file := filepath.Join(root, "usr/bin/bad")
	if err := os.WriteFile(file, []byte("not executed"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, os.ModeSetuid|0755); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoot(root, &m); err == nil {
		t.Fatal("setuid regular file accepted")
	}
}
