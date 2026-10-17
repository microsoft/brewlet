//go:build linux

package artifact

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLinuxOverlayWhiteoutMaterialization(t *testing.T) {
	dir := testDir(t)
	store := Store{Dir: filepath.Join(dir, "store")}
	src := source(t, store, "gzip", []*tar.Header{fileHeader("app/.wh.deleted", "", 0), fileHeader("app/data/.wh..wh..opq", "", 0)}, []string{"", ""})
	cache := LayerCache{Dir: filepath.Join(dir, "cache")}
	root, e := cache.Materialize(context.Background(), src)
	if errors.Is(e, unix.EPERM) || errors.Is(e, unix.ENOTSUP) {
		t.Skipf("requires Linux native overlay storage, CAP_MKNOD and CAP_SYS_ADMIN: %v", e)
	}
	must(t, e)
	fi, e := os.Lstat(filepath.Join(root, "app/deleted"))
	must(t, e)
	if fi.Mode()&os.ModeCharDevice == 0 {
		t.Fatal("whiteout not converted to character device")
	}
	if _, e = os.Lstat(filepath.Join(root, "app/.wh.deleted")); !os.IsNotExist(e) {
		t.Fatal("ordinary whiteout file published")
	}
	buf := make([]byte, 16)
	n, e := unix.Getxattr(filepath.Join(root, "app/data"), "trusted.overlay.opaque", buf)
	must(t, e)
	if string(buf[:n]) != "y" {
		t.Fatal("missing overlay opaque xattr")
	}
}

func TestLinuxOverlayWhiteoutAndOpaqueEntriesHideOnlyLowerContent(t *testing.T) {
	dir := testDir(t)
	store := Store{Dir: filepath.Join(dir, "store")}
	lower := source(t, store, "gzip", []*tar.Header{
		fileHeader("app/deleted", "old", 0644),
		fileHeader("app/data/old", "old data", 0644),
		fileHeader("app/sibling", "retained", 0644),
	}, []string{"old", "old data", "retained"})
	upper := source(t, store, "zstd", []*tar.Header{
		fileHeader("app/.wh.deleted", "", 0),
		fileHeader("app/data/.wh..wh..opq", "", 0),
		fileHeader("app/data/new", "new data", 0644),
	}, []string{"", "", "new data"})
	must(t, ValidateComposition(context.Background(), []LayerSource{lower, upper}))
	cache := LayerCache{Dir: filepath.Join(dir, "cache")}
	lowerRoot, err := cache.Materialize(context.Background(), lower)
	must(t, err)
	upperRoot, err := cache.Materialize(context.Background(), upper)
	must(t, err)
	runtimeRoot, merged := filepath.Join(dir, "runtime"), filepath.Join(dir, "merged")
	must(t, os.Mkdir(runtimeRoot, 0755))
	must(t, os.Mkdir(merged, 0755))
	err = unix.Mount("overlay", merged, "overlay", unix.MS_RDONLY,
		"lowerdir="+upperRoot+":"+lowerRoot+":"+runtimeRoot)
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENODEV) {
		t.Skipf("requires actual Linux overlay mount capability: %v", err)
	}
	must(t, err)
	defer unix.Unmount(merged, 0)
	for _, name := range []string{"app/deleted", "app/data/old", "app/.wh.deleted", "app/data/.wh..wh..opq"} {
		if _, err := os.Lstat(filepath.Join(merged, name)); !os.IsNotExist(err) {
			t.Fatalf("whiteout/opaque metadata leaked lower content or literal markers at %s: %v", name, err)
		}
	}
	for name, want := range map[string]string{"app/data/new": "new data", "app/sibling": "retained"} {
		value, err := os.ReadFile(filepath.Join(merged, name))
		must(t, err)
		if string(value) != want {
			t.Fatalf("overlay changed surviving content %s: %q", name, value)
		}
	}
}

func TestLinuxOverlayInheritedSymlinkShadowing(t *testing.T) {
	dir := testDir(t)
	store := Store{Dir: filepath.Join(dir, "store")}
	lower := source(t, store, "gzip", []*tar.Header{
		{Name: "app/link", Typeflag: tar.TypeSymlink, Linkname: "sub", Mode: 0777},
		fileHeader("app/sub/old", "lower", 0644),
	}, []string{"", "lower"})
	upper := source(t, store, "zstd", []*tar.Header{fileHeader("app/link/new", "upper", 0644)}, []string{"upper"})
	must(t, ValidateComposition(context.Background(), []LayerSource{lower, upper}))
	cache := LayerCache{Dir: filepath.Join(dir, "cache")}
	lowerRoot, e := cache.Materialize(context.Background(), lower)
	must(t, e)
	upperRoot, e := cache.Materialize(context.Background(), upper)
	must(t, e)
	again, e := cache.Materialize(context.Background(), upper)
	must(t, e)
	if again != upperRoot {
		t.Fatal("layer cache became context-dependent")
	}
	runtimeRoot := filepath.Join(dir, "runtime")
	merged := filepath.Join(dir, "merged")
	must(t, os.Mkdir(runtimeRoot, 0755))
	must(t, os.Mkdir(merged, 0755))
	e = unix.Mount("overlay", merged, "overlay", unix.MS_RDONLY, "lowerdir="+upperRoot+":"+lowerRoot+":"+runtimeRoot)
	if errors.Is(e, unix.EPERM) || errors.Is(e, unix.ENODEV) {
		t.Skipf("requires Linux overlayfs and CAP_SYS_ADMIN: %v", e)
	}
	must(t, e)
	defer unix.Unmount(merged, 0)
	fi, e := os.Lstat(filepath.Join(merged, "app/link"))
	must(t, e)
	if !fi.IsDir() {
		t.Fatal("overlay did not shadow inherited symlink with upper implicit directory")
	}
	data, e := os.ReadFile(filepath.Join(merged, "app/link/new"))
	must(t, e)
	if string(data) != "upper" {
		t.Fatal("upper file absent")
	}
	if _, e = os.Lstat(filepath.Join(merged, "app/sub/new")); !os.IsNotExist(e) {
		t.Fatal("upper layer incorrectly followed inherited symlink")
	}
	data, e = os.ReadFile(filepath.Join(merged, "app/sub/old"))
	must(t, e)
	if string(data) != "lower" {
		t.Fatal("unrelated lower content changed")
	}
}
