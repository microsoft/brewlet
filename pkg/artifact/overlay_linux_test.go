//go:build linux

package artifact

import (
	"archive/tar"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLinuxLayerCacheENOSPCDoesNotPublishPartialEntry(t *testing.T) {
	dir := testDir(t)
	store := Store{Dir: filepath.Join(dir, "store")}
	payload := strings.Repeat("bounded cache capacity fixture\n", 300000)
	src := source(t, store, "gzip", []*tar.Header{fileHeader("app/data", payload, 0644)}, []string{payload})
	cache := LayerCache{Dir: filepath.Join(dir, "cache")}
	must(t, os.Mkdir(cache.Dir, 0700))
	err := unix.Mount("tmpfs", cache.Dir, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=4m,mode=0700")
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENODEV) {
		t.Skipf("requires isolated Linux tmpfs and CAP_SYS_ADMIN: %v", err)
	}
	must(t, err)
	defer func() { must(t, unix.Unmount(cache.Dir, 0)) }()
	root, err := cache.Materialize(context.Background(), src)
	if !errors.Is(err, unix.ENOSPC) || root != "" {
		t.Fatalf("bounded extraction did not fail with real ENOSPC: root=%q error=%v", root, err)
	}
	entries, err := os.ReadDir(cache.Dir)
	must(t, err)
	key := PolicyVersion + "-" + src.Descriptor.Digest.Encoded()
	for _, entry := range entries {
		if entry.Name() != key+".lock" {
			t.Fatalf("failed extraction exposed partial cache state: %s", entry.Name())
		}
	}
	if _, err := os.Lstat(filepath.Join(cache.Dir, key)); !os.IsNotExist(err) {
		t.Fatalf("failed extraction published a digest entry: %v", err)
	}
	must(t, unix.Mount("tmpfs", cache.Dir, "tmpfs", unix.MS_REMOUNT|unix.MS_NOSUID|unix.MS_NODEV, "size=16m,mode=0700"))
	root, err = cache.Materialize(context.Background(), src)
	must(t, err)
	got, err := os.ReadFile(filepath.Join(root, "app/data"))
	must(t, err)
	if string(got) != payload {
		t.Fatal("retry after restoring isolated capacity changed payload")
	}
	_, receipt, err := VerifyCachedEntry(filepath.Dir(root))
	must(t, err)
	if receipt.Digest != src.Descriptor.Digest.String() || receipt.DiffID != src.DiffID.String() {
		t.Fatal("retry published an unrelated receipt")
	}
}

func TestLinuxLayerExtractionNormalizesOwnershipAndPreservesMetadata(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires isolated root-owned Linux extraction")
	}
	for _, format := range []string{"gzip", "zstd"} {
		t.Run(format, func(t *testing.T) {
			dir := testDir(t)
			store := Store{Dir: filepath.Join(dir, "store")}
			stamp := time.Unix(1700000000, 123000000)
			headers := []*tar.Header{
				{Name: "app/bin", Typeflag: tar.TypeDir, Mode: 0755, Uid: 1234, Gid: 2345, ModTime: stamp, Format: tar.FormatPAX},
				{Name: "app/server", Typeflag: tar.TypeLink, Linkname: "app/bin/server", Mode: 0644, Uid: 3456, Gid: 4567},
				fileHeader("app/bin/server", "executable payload", 0555),
			}
			headers[2].Uid, headers[2].Gid = 5678, 6789
			src := source(t, store, format, headers, []string{"", "", "executable payload"})
			cache := LayerCache{Dir: filepath.Join(dir, "cache")}
			root, err := cache.Materialize(context.Background(), src)
			must(t, err)
			for name, mode := range map[string]os.FileMode{"app/bin": 0755, "app/bin/server": 0555, "app/server": 0555} {
				p := filepath.Join(root, name)
				var st unix.Stat_t
				must(t, unix.Lstat(p, &st))
				fi, err := os.Stat(p)
				must(t, err)
				if st.Uid != 0 || st.Gid != 0 || fi.Mode().Perm() != mode || !fi.ModTime().Equal(stamp) {
					t.Fatalf("%s changed required metadata: uid=%d gid=%d mode=%v mtime=%v", name, st.Uid, st.Gid, fi.Mode(), fi.ModTime())
				}
			}
			target, err := os.Stat(filepath.Join(root, "app/bin/server"))
			must(t, err)
			alias, err := os.Stat(filepath.Join(root, "app/server"))
			must(t, err)
			if !os.SameFile(target, alias) {
				t.Fatal("forward hardlink did not retain the same regular inode")
			}
		})
	}
}

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
	if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) {
		t.Skipf("requires native whiteout/opaque materialization capabilities: %v", err)
	}
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
