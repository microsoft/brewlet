// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runnablestage

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRootAndBytes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BREWLET_RUNNABLE_STAGE", root)
	if Root() != root {
		t.Fatal("environment override not used")
	}
	t.Setenv("BREWLET_RUNNABLE_STAGE", "")
	t.Setenv("TMPDIR", t.TempDir())
	want := filepath.Join(os.TempDir(), "brewlet-runnable")
	if runtime.GOOS == "linux" {
		want = "/tmp/brewlet-runnable"
	}
	if Root() != want {
		t.Fatalf("default root = %q, want %q", Root(), want)
	}
	for name, data := range map[string]string{
		"unmanaged/app.jar":                                    "123",
		"immutable-v2/.pending/app.jar":                        "12345",
		"immutable-v2/" + strings.Repeat("a", 64) + "/app.jar": "1234567",
	} {
		writeFile(t, filepath.Join(root, name), data)
	}
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "large.jar"), strings.Repeat("x", 1024))
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "large.jar"), filepath.Join(root, "linked.jar")); err != nil {
		t.Fatal(err)
	}
	got, err := Bytes(root)
	if err != nil || got != 15 {
		t.Fatalf("Bytes = %d, %v; want 15", got, err)
	}
	if got, err := Bytes(filepath.Join(root, "missing")); got != 0 || err != nil {
		t.Fatalf("missing Bytes = %d, %v", got, err)
	}
	if _, err := Bytes(filepath.Join(root, "linked")); err == nil {
		t.Fatal("accepted symlink root")
	}
	if _, err := Bytes(filepath.Join(outside, "large.jar")); err == nil {
		t.Fatal("accepted regular-file root")
	}
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCanonical(t *testing.T) {
	for _, name := range []string{"", ".pending", strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63), strings.Repeat("a", 65)} {
		if canonical(name) {
			t.Errorf("accepted %q", name)
		}
	}
	if !canonical(strings.Repeat("0123456789abcdef", 4)) {
		t.Fatal("rejected lowercase digest")
	}
}
