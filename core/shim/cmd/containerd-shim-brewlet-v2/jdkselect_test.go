// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkJDK creates a fake node-resident JDK root with a bin/java under rootsDir and
// activates it in the JDK inventory. Selection is fail-closed on the
// administrator-owned inventory, so an installed-but-unlisted root is invisible;
// tests that exercise that path write the inventory themselves afterwards.
func mkJDK(t *testing.T, rootsDir, name string) {
	t.Helper()
	bin := filepath.Join(rootsDir, name, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "java"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	activate(t, rootsDir, jdkActiveInventory, name)
}

// activate appends name to a runtime roots directory's active inventory.
func activate(t *testing.T, rootsDir, inventory, name string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(rootsDir, inventory), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(name + "\n"); err != nil {
		t.Fatal(err)
	}
}

// wantRoot is the symlink-resolved path selection must return for a runtime root:
// the shim returns the resolved directory so the path that reaches mount
// construction is exactly the one whose containment it proved.
func wantRoot(t *testing.T, rootsDir, name string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(filepath.Join(rootsDir, name))
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestSelectJDKBareFeaturePicksLexicallyFirst(t *testing.T) {
	dir := t.TempDir()
	// Install two distributions of feature 21, out of lexical order, plus an
	// unrelated feature to prove filtering.
	mkJDK(t, dir, "temurin-21")
	mkJDK(t, dir, "microsoft-21")
	mkJDK(t, dir, "temurin-25")

	got, err := selectJDK(dir, "21")
	if err != nil {
		t.Fatalf("selectJDK: %v", err)
	}

	// No vendor preference: lexically-first among {microsoft-21, temurin-21}.
	if want := wantRoot(t, dir, "microsoft-21"); got != want {
		t.Errorf("selectJDK(21) = %q, want %q (lexically-first, not temurin)", got, want)
	}
}

func TestSelectJDKResolvesImageRootJavaHome(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "zulu-21")
	home := filepath.Join(root, "usr", "lib", "jvm", "zulu21")
	if err := os.MkdirAll(filepath.Join(home, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "bin", "java"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, jdkHomeMetadata), []byte("/usr/lib/jvm/zulu21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	activate(t, dir, jdkActiveInventory, "zulu-21")

	gotRoot, err := selectJDK(dir, "zulu-21")
	if err != nil {
		t.Fatal(err)
	}
	if want := wantRoot(t, dir, "zulu-21"); gotRoot != want {
		t.Fatalf("selectJDK() = %q, want %q", gotRoot, want)
	}
	gotHome, err := resolveJDKHome(root)
	if err != nil {
		t.Fatal(err)
	}
	wantHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	if gotHome != wantHome {
		t.Fatalf("resolveJDKHome() = %q, want %q", gotHome, wantHome)
	}
}

func TestResolveJDKHomeRejectsInvalidMetadata(t *testing.T) {
	for _, value := range []string{"", "usr/lib/jvm/zulu21", "/", "/usr/../etc"} {
		t.Run(value, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, jdkHomeMetadata), []byte(value), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveJDKHome(root); err == nil {
				t.Fatalf("resolveJDKHome() accepted invalid metadata %q", value)
			}
		})
	}
}

func TestResolveJDKHomeRejectsSymlinkOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "bin", "java"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "runtime")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, jdkHomeMetadata), []byte("/runtime\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJDKHome(root); err == nil {
		t.Fatal("resolveJDKHome() accepted a Java home symlink outside the runtime root")
	}
}

func TestSelectJDKEmptyRequestDefaultsFeature21(t *testing.T) {
	dir := t.TempDir()
	mkJDK(t, dir, "zulu-21")

	got, err := selectJDK(dir, "")
	if err != nil {
		t.Fatalf("selectJDK: %v", err)
	}
	if want := wantRoot(t, dir, "zulu-21"); got != want {
		t.Errorf("selectJDK(\"\") = %q, want %q (feature 21, only installed distro)", got, want)
	}
}

func TestSelectJDKExplicitDistributionIsExact(t *testing.T) {
	dir := t.TempDir()
	mkJDK(t, dir, "temurin-21")

	got, err := selectJDK(dir, "temurin-21")
	if err != nil {
		t.Fatalf("selectJDK: %v", err)
	}
	if want := wantRoot(t, dir, "temurin-21"); got != want {
		t.Errorf("selectJDK(temurin-21) = %q, want %q", got, want)
	}

	// A pinned distribution must NOT fall back to another vendor's build.
	if _, err := selectJDK(dir, "microsoft-21"); err == nil {
		t.Error("selectJDK(microsoft-21) succeeded, want NoCompatibleJDK (no silent temurin fallback)")
	}
}

func TestSelectJDKNoMatchingFeature(t *testing.T) {
	dir := t.TempDir()
	mkJDK(t, dir, "temurin-25")

	if _, err := selectJDK(dir, "21"); err == nil {
		t.Error("selectJDK(21) succeeded with only feature 25 installed, want NoCompatibleJDK")
	}
}

func TestSelectJDKIgnoresInactiveRoots(t *testing.T) {
	dir := t.TempDir()
	mkJDK(t, dir, "microsoft-21")
	mkJDK(t, dir, "temurin-21")
	if err := os.WriteFile(filepath.Join(dir, jdkActiveInventory), []byte("temurin-21\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := selectJDK(dir, "21")
	if err != nil {
		t.Fatal(err)
	}
	if want := wantRoot(t, dir, "temurin-21"); got != want {
		t.Fatalf("selectJDK(21) = %q, want active root %q", got, want)
	}
	if _, err := selectJDK(dir, "microsoft-21"); err == nil {
		t.Fatal("selectJDK(microsoft-21) selected an inactive root")
	}
}

// unsafeRuntimeNames are the traversal, separator and dot-segment variants a
// tenant can put in the brewlet.sh/jdk annotation. None
// may reach mount construction.
var unsafeRuntimeNames = []string{
	"..",
	"../..",
	"../../..",
	"../outside",
	"jdk/../../outside",
	"sub/jdk",
	`sub\jdk`,
	"/etc",
	"/opt/brewlet/jdks/jdk",
	".",
	"./jdk",
	"jdk*",
	"jdk?",
	" jdk",
	"jdk ",
	"JDK",
	"-jdk",
	"jdk-",
	"jdk\n../outside",
	strings.Repeat("j", 64),
}

// TestSelectJDKRejectsUnsafeDistribution applies the same rule to the JDK half
// of the annotation pair: the distribution becomes a directory name under the
// JDK roots, so it must be a safe token.
func TestSelectJDKRejectsUnsafeDistribution(t *testing.T) {
	for _, dist := range []string{"..", "../..", "sub/temurin", `sub\temurin`, "/etc", ".", "TEMURIN", "temurin*"} {
		t.Run(dist, func(t *testing.T) {
			dir := t.TempDir()
			mkJDK(t, dir, "temurin-21")
			mkJDK(t, filepath.Dir(dir), "outside-21")
			request := dist + "-21"
			if err := os.WriteFile(filepath.Join(dir, jdkActiveInventory), []byte("temurin-21\n"+request+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, err := selectJDK(dir, request); err == nil {
				t.Fatalf("selectJDK(%q) selected %q, want rejection", request, got)
			}
		})
	}
}

// TestSelectJDKRequiresActiveInventory: an installed-but-unlisted JDK root is
// invisible, so selection cannot fall back to joining an unvetted name onto the
// roots directory on a node with no inventory.
func TestSelectJDKRequiresActiveInventory(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "temurin-21", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "java"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := selectJDK(dir, "temurin-21"); err == nil {
		t.Fatal("selectJDK(temurin-21) selected a root without an active inventory")
	}
	if _, err := selectJDK(dir, "21"); err == nil {
		t.Fatal("selectJDK(21) selected a root without an active inventory")
	}
}

// TestSelectJDKRejectsRootSymlinkOutsideRoots covers a
// listed JDK root that symlinks out of the roots directory is refused.
func TestSelectJDKRejectsRootSymlinkOutsideRoots(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	mkJDK(t, outside, "temurin-21")
	if err := os.Symlink(filepath.Join(outside, "temurin-21"), filepath.Join(dir, "temurin-21")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, jdkActiveInventory), []byte("temurin-21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := selectJDK(dir, "temurin-21"); err == nil {
		t.Fatalf("selectJDK(temurin-21) selected symlinked root %q outside %s", got, dir)
	}
	if got, err := selectJDK(dir, "21"); err == nil {
		t.Fatalf("selectJDK(21) selected symlinked root %q outside %s", got, dir)
	}
}
