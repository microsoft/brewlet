package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

func testDir(t *testing.T) string {
	t.Helper()
	p, e := os.MkdirTemp(".", ".test-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(p) })
	p, e = filepath.Abs(p)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func cleanSource(t *testing.T, dir string) {
	t.Helper()
	must(t, filepath.Walk(dir, func(p string, fi os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		n, e := unix.Llistxattr(p, nil)
		if e == unix.ENOTSUP {
			return nil
		}
		if e != nil {
			return e
		}
		buf := make([]byte, n)
		n, e = unix.Llistxattr(p, buf)
		if e != nil {
			return e
		}
		for _, name := range strings.Split(string(buf[:n]), "\x00") {
			if name != "" {
				if e = unix.Lremovexattr(p, name); e != nil {
					return e
				}
			}
		}
		return nil
	}))
	if e := rejectSourceXattrs(dir); e != nil {
		t.Skipf("host retains synthetic source xattrs after explicit removal; full producer cases run on Linux: %v", e)
	}
}
func source(t *testing.T, store Store, format string, headers []*tar.Header, payloads []string) LayerSource {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for i, h := range headers {
		must(t, tw.WriteHeader(h))
		if h.Typeflag == tar.TypeReg && h.Size > 0 {
			_, e := io.WriteString(tw, payloads[i])
			must(t, e)
		}
	}
	must(t, tw.Close())
	diff := digest.FromBytes(raw.Bytes())
	var compressed bytes.Buffer
	switch format {
	case "gzip":
		w := gzip.NewWriter(&compressed)
		_, e := w.Write(raw.Bytes())
		must(t, e)
		must(t, w.Close())
	case "zstd":
		w, e := zstd.NewWriter(&compressed)
		must(t, e)
		_, e = w.Write(raw.Bytes())
		must(t, e)
		must(t, w.Close())
	}
	media := v1.MediaTypeImageLayerGzip
	if format == "zstd" {
		media = v1.MediaTypeImageLayerZstd
	}
	d, e := store.Put(media, bytes.NewReader(compressed.Bytes()))
	must(t, e)
	d.Annotations = map[string]string{"brewlet.sh/layer": "app"}
	return LayerSource{Descriptor: d, DiffID: diff, Open: func() (io.ReadCloser, error) { return store.Open(d) }}
}
func fileHeader(name, data string, mode int64) *tar.Header {
	return &tar.Header{Name: name, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 123000000), Format: tar.FormatPAX}
}
func TestConformance(t *testing.T) {
	b, e := os.ReadFile("../../conformance/artifact-v2.json")
	must(t, e)
	r, e := RunFixtures(context.Background(), b)
	must(t, e)
	if len(r) < 40 {
		t.Fatal("missing shared cases")
	}
	for _, f := range r {
		if !f.Passed {
			t.Errorf("fixture failed: %+v", f)
		}
	}
}
func TestStreamingMaterializationAndLinks(t *testing.T) {
	for _, format := range []string{"gzip", "zstd"} {
		t.Run(format, func(t *testing.T) {
			dir := testDir(t)
			store := Store{Dir: filepath.Join(dir, "store")}
			ctx := context.Background()
			first := source(t, store, format, []*tar.Header{fileHeader("app/bin/server", "hello", 0755), {Name: "app/server", Typeflag: tar.TypeLink, Linkname: "app/bin/server", Mode: 0644}, {Name: "app/alias", Typeflag: tar.TypeSymlink, Linkname: "bin", Mode: 0777}, fileHeader("app/gone", "bye", 0644)}, []string{"hello", "", "", "bye"})
			second := source(t, store, format, []*tar.Header{fileHeader("app/bin/new", "new", 0644), fileHeader("app/.wh.gone", "", 0)}, []string{"new", ""})
			dest := filepath.Join(dir, "published")
			must(t, MaterializeLayers(ctx, []LayerSource{first, second}, dest))
			data, e := os.ReadFile(filepath.Join(dest, "app/alias/new"))
			must(t, e)
			if string(data) != "new" {
				t.Fatal("safe composed symlink not followed")
			}
			if _, e = os.Lstat(filepath.Join(dest, "app/gone")); !os.IsNotExist(e) {
				t.Fatal("whiteout not applied")
			}
			if _, e = os.Lstat(filepath.Join(dest, "app/.wh.gone")); !os.IsNotExist(e) {
				t.Fatal("whiteout became ordinary file")
			}
			a, e := os.Stat(filepath.Join(dest, "app/server"))
			must(t, e)
			b, e := os.Stat(filepath.Join(dest, "app/bin/server"))
			must(t, e)
			if !os.SameFile(a, b) {
				t.Fatal("hardlink not preserved")
			}
			if b.Mode().Perm() != 0755 || !b.ModTime().Equal(time.Unix(1700000000, 123000000)) {
				t.Fatalf("metadata changed: %v %v", b.Mode(), b.ModTime())
			}
		})
	}
}
func TestOpaqueOnlyDeletesLower(t *testing.T) {
	dir := testDir(t)
	s := Store{Dir: filepath.Join(dir, "store")}
	lower := source(t, s, "gzip", []*tar.Header{fileHeader("app/data/old", "old", 0644)}, []string{"old"})
	upper := source(t, s, "gzip", []*tar.Header{fileHeader("app/data/new", "new", 0644), fileHeader("app/data/.wh..wh..opq", "", 0)}, []string{"new", ""})
	dest := filepath.Join(dir, "root")
	must(t, MaterializeLayers(context.Background(), []LayerSource{lower, upper}, dest))
	if _, e := os.Stat(filepath.Join(dest, "app/data/old")); !os.IsNotExist(e) {
		t.Fatal("opaque marker retained lower")
	}
	b, e := os.ReadFile(filepath.Join(dest, "app/data/new"))
	must(t, e)
	if string(b) != "new" {
		t.Fatal("opaque removed same-layer file")
	}
}
func TestCorruptionNeverPublishes(t *testing.T) {
	dir := testDir(t)
	s := Store{Dir: filepath.Join(dir, "store")}
	good := source(t, s, "gzip", []*tar.Header{fileHeader("app/a", "data", 0644)}, []string{"data"})
	for _, kind := range []string{"digest", "size", "diff", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			bad := good
			switch kind {
			case "digest":
				bad.Descriptor.Digest = digest.FromString("wrong")
			case "size":
				bad.Descriptor.Size++
			case "diff":
				bad.DiffID = digest.FromString("wrong")
			case "truncated":
				r, e := good.Open()
				must(t, e)
				b, e := io.ReadAll(r)
				r.Close()
				must(t, e)
				b = b[:len(b)-8]
				bad.Open = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }
			}
			dest := filepath.Join(dir, kind)
			if e := MaterializeLayers(context.Background(), []LayerSource{bad}, dest); e == nil {
				t.Fatal("corruption accepted")
			}
			if _, e := os.Lstat(dest); !os.IsNotExist(e) {
				t.Fatal("published invalid tree")
			}
		})
	}
}
func TestConcurrentPublication(t *testing.T) {
	dir := testDir(t)
	s := Store{Dir: filepath.Join(dir, "store")}
	l := source(t, s, "gzip", []*tar.Header{fileHeader("app/a", "data", 0644)}, []string{"data"})
	dest := filepath.Join(dir, "root")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- MaterializeLayers(context.Background(), []LayerSource{l}, dest) }()
	}

	wg.Wait()
	close(results)
	success := 0
	for e := range results {
		if e == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("expected one atomic winner, got %d", success)
	}
	b, e := os.ReadFile(filepath.Join(dest, "app/a"))
	must(t, e)
	if string(b) != "data" {
		t.Fatal("invalid published content")
	}
}
func TestInheritedSymlinkParentIsShadowed(t *testing.T) {
	dir := testDir(t)
	s := Store{Dir: filepath.Join(dir, "store")}
	lower := source(t, s, "gzip", []*tar.Header{{Name: "app/deep", Typeflag: tar.TypeSymlink, Linkname: ".", Mode: 0777}, fileHeader("app/file", "lower", 0644)}, []string{"", "lower"})
	upper := source(t, s, "gzip", []*tar.Header{fileHeader("app/deep/file", "upper", 0644), {Name: "app/deep/link", Typeflag: tar.TypeSymlink, Linkname: "../outside", Mode: 0777}}, []string{"upper", ""})
	must(t, ValidateComposition(context.Background(), []LayerSource{lower, upper}))
	dest := filepath.Join(dir, "root")
	must(t, MaterializeLayers(context.Background(), []LayerSource{lower, upper}, dest))
	fi, e := os.Lstat(filepath.Join(dest, "app/deep"))
	must(t, e)
	if !fi.IsDir() {
		t.Fatal("upper implicit directory did not shadow lower symlink")
	}
	for p, want := range map[string]string{"app/deep/file": "upper", "app/file": "lower"} {
		b, e := os.ReadFile(filepath.Join(dest, p))
		must(t, e)
		if string(b) != want {
			t.Fatalf("%s: got %q want %q", p, b, want)
		}
	}
}
func TestComposedSymlinkDotDotEscape(t *testing.T) {
	dir := testDir(t)
	s := Store{Dir: filepath.Join(dir, "store")}
	lower := source(t, s, "gzip", []*tar.Header{{Name: "app/a", Typeflag: tar.TypeSymlink, Linkname: ".", Mode: 0777}}, []string{""})
	upper := source(t, s, "gzip", []*tar.Header{{Name: "app/b", Typeflag: tar.TypeSymlink, Linkname: "a/../outside", Mode: 0777}}, []string{""})
	same := source(t, s, "gzip", []*tar.Header{{Name: "app/a", Typeflag: tar.TypeSymlink, Linkname: ".", Mode: 0777}, {Name: "app/b", Typeflag: tar.TypeSymlink, Linkname: "a/../outside", Mode: 0777}}, []string{"", ""})
	for _, sources := range [][]LayerSource{{lower, upper}, {same}} {
		if e := ValidateComposition(context.Background(), sources); Reason(e) != "LayerPathViolation" {
			t.Fatalf("composed escape accepted: %v", e)
		}
		dest := filepath.Join(dir, "root")
		if e := MaterializeLayers(context.Background(), sources, dest); Reason(e) != "LayerPathViolation" {
			t.Fatalf("materialized composed escape: %v", e)
		}
		if _, e := os.Lstat(dest); !os.IsNotExist(e) {
			t.Fatal("unsafe tree published")
		}
	}
}
func TestSourceCannotContainOutputStore(t *testing.T) {
	dir := testDir(t)
	input := filepath.Join(dir, "input")
	must(t, os.Mkdir(input, 0755))
	alias := filepath.Join(dir, "alias")
	must(t, os.Symlink(input, alias))
	for _, storeDir := range []string{filepath.Join(input, "store"), filepath.Join(alias, "store")} {
		_, e := BuildLayer(context.Background(), Store{Dir: storeDir}, LayerSpec{"app", input, "app"})
		if e == nil || !strings.Contains(e.Error(), "contains output store") {
			t.Fatalf("overlapping source accepted: %v", e)
		}
	}
}
func TestBuildDeterministicAndResolve(t *testing.T) {
	dir := testDir(t)
	app := filepath.Join(dir, "input")
	must(t, os.Mkdir(app, 0755))
	must(t, os.WriteFile(filepath.Join(app, "server"), []byte("native"), 0755))
	stamp := time.Unix(1700000000, 321000000)
	must(t, os.Chtimes(filepath.Join(app, "server"), stamp, stamp))
	must(t, os.Chtimes(app, stamp, stamp))
	cleanSource(t, app)
	s := Store{Dir: filepath.Join(dir, "store")}
	p1, _ := ParsePlatform("linux/amd64")
	p2, _ := ParsePlatform("linux/arm64")
	o := BuildOptions{Launch: []byte(`{"schemaVersion":2,"process":{"entrypoint":["/app/server"]}}`), Platforms: []v1.Platform{p1, p2}, Layers: []LayerSpec{{"app", app, "app"}}}
	first, e := Build(context.Background(), s, o)
	must(t, e)
	second, e := Build(context.Background(), s, o)
	must(t, e)
	if first.Digest != second.Digest {
		t.Fatal("nondeterministic build")
	}
	a, e := Resolve(context.Background(), s.Open, first, p1)
	must(t, e)
	b, e := Resolve(context.Background(), s.Open, first, p2)
	must(t, e)
	if a.Manifest.Layers[0].Digest != b.Manifest.Layers[0].Digest {
		t.Fatal("neutral layers not reused")
	}
	rawIndex, e := s.Read(first)
	must(t, e)
	var explicit struct {
		Manifests []v1.Descriptor `json:"manifests"`
	}
	must(t, json.Unmarshal(rawIndex, &explicit))
	var descriptorFiles []string
	for i, d := range explicit.Manifests {
		file := filepath.Join(dir, []string{"amd64.json", "arm64.json"}[i])
		raw, e := json.Marshal(d)
		must(t, e)
		must(t, os.WriteFile(file, raw, 0644))
		descriptorFiles = append(descriptorFiles, file)
	}
	combined, e := BuildIndex(context.Background(), s, descriptorFiles)
	must(t, e)
	if combined.Digest != first.Digest {
		t.Fatal("explicit index descriptor composition changed manifest ordering")
	}
	if _, e = BuildIndex(context.Background(), s, []string{descriptorFiles[0], descriptorFiles[0]}); Reason(e) != "InvalidPlatform" {
		t.Fatalf("duplicate explicit platform accepted: %v", e)
	}
	if !bytes.Equal(a.LaunchJSON, o.Launch) {
		t.Fatal("launch bytes rewritten")
	}
	wrong, _ := ParsePlatform("linux/s390x")
	if _, e = Resolve(context.Background(), s.Open, first, wrong); Reason(e) != "InvalidPlatform" {
		t.Fatalf("platform fallback accepted: %v", e)
	}
	image := a.Image
	image.Config.Cmd = []string{"override"}
	cd, e := s.PutJSON(v1.MediaTypeImageConfig, image)
	must(t, e)
	m := a.Manifest
	m.Config = cd
	md, e := s.PutJSON(v1.MediaTypeImageManifest, m)
	must(t, e)
	if _, e = Resolve(context.Background(), s.Open, md, p1); Reason(e) != "LaunchConfigMismatch" {
		t.Fatalf("bad mirror accepted: %v", e)
	}
}
func TestLayerSetByteReuseAndLock(t *testing.T) {
	dir := testDir(t)
	deps := filepath.Join(dir, "deps")
	app := filepath.Join(dir, "app")
	must(t, os.Mkdir(deps, 0755))
	must(t, os.Mkdir(app, 0755))
	must(t, os.WriteFile(filepath.Join(deps, "library"), []byte("lib"), 0644))
	must(t, os.WriteFile(filepath.Join(app, "main"), []byte("app"), 0644))
	cleanSource(t, deps)
	cleanSource(t, app)
	s := Store{Dir: filepath.Join(dir, "store")}
	lock := &LayerSetLock{Entries: []LockEntry{{Ecosystem: "generic", Name: "library", Version: "1", Path: "/app/lib/library", SHA256: digest.FromString("lib").Encoded()}}}
	set, e := BuildLayerSet(context.Background(), s, LayerSetConfig{Name: "approved", Version: "1", Ecosystem: "generic"}, []LayerSpec{{"dependency", deps, "app/lib"}}, lock)
	must(t, e)
	layers, ev, e := ConsumeLayerSet(context.Background(), s, set)
	must(t, e)
	p, _ := ParsePlatform("linux/amd64")
	target, e := Build(context.Background(), s, BuildOptions{Launch: []byte(`{"schemaVersion":2,"process":{"entrypoint":["runner"]}}`), Platforms: []v1.Platform{p}, Layers: []LayerSpec{{"app", app, "app"}}, LayerSets: []v1.Descriptor{set}})
	must(t, e)
	resolved, e := Resolve(context.Background(), s.Open, target, p)
	must(t, e)
	if resolved.Manifest.Layers[0].Digest != layers[0].Descriptor.Digest || resolved.Image.RootFS.DiffIDs[0] != layers[0].DiffID {
		t.Fatal("layer set bytes/diff IDs rewritten")
	}
	var evidence []LayerSetEvidence
	must(t, json.Unmarshal([]byte(resolved.Manifest.Annotations["brewlet.sh/layerset-evidence"]), &evidence))
	if len(evidence) != 1 || evidence[0].ManifestDigest != ev.ManifestDigest {
		t.Fatal("missing binding evidence")
	}
	must(t, ValidateEvidence(resolved.Manifest))
	lock.Entries[0].SHA256 = strings.Repeat("0", 64)
	_, e = BuildLayerSet(context.Background(), s, LayerSetConfig{Name: "bad", Version: "1", Ecosystem: "generic"}, []LayerSpec{{"dependency", deps, "app/lib"}}, lock)
	if e == nil {
		t.Fatal("invalid lock accepted")
	}
}
