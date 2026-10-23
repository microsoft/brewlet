package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func jsonBytes(v any) ([]byte, error) { return json.Marshal(v) }

// LayerSpec describes one producer layer. A nil Platform applies the layer to
// every declared platform; otherwise it belongs only to that platform manifest.
type LayerSpec struct {
	Role, Source, Destination string
	Platform                  *v1.Platform
}

// ParseLayerSpec parses [OS/ARCH[/VARIANT]:]ROLE=SOURCE:/app/PATH.
func ParseLayerSpec(s string) (LayerSpec, error) {
	role, rest, ok := strings.Cut(s, "=")
	var platform *v1.Platform
	if i := strings.LastIndex(role, ":"); ok && i >= 0 {
		p, e := ParsePlatform(role[:i])
		if e != nil {
			return LayerSpec{}, e
		}
		platform, role = &p, role[i+1:]
	}
	if !ok || (role != "app" && role != "dependency") {
		return LayerSpec{}, fmt.Errorf("layer must be [OS/ARCH[/VARIANT]:]app=SOURCE:/app/PATH or [OS/ARCH[/VARIANT]:]dependency=SOURCE:/app/PATH")
	}
	i := strings.LastIndex(rest, ":")
	if i < 1 {
		return LayerSpec{}, fmt.Errorf("missing layer destination")
	}
	dest := rest[i+1:]
	if !strings.HasPrefix(dest, "/app") {
		return LayerSpec{}, fail("LayerPathViolation", "destination outside /app")
	}
	if _, e := archivePath(strings.TrimPrefix(dest, "/")); e != nil {
		return LayerSpec{}, e
	}
	return LayerSpec{Role: role, Source: rest[:i], Destination: strings.TrimPrefix(dest, "/"), Platform: platform}, nil
}

type BuiltLayer struct {
	Descriptor v1.Descriptor `json:"descriptor"`
	DiffID     digest.Digest `json:"diffId"`
}

func writeLayer(store Store, role string, write func(*tar.Writer) error) (BuiltLayer, error) {
	pr, pw := io.Pipe()
	result := make(chan struct {
		d v1.Descriptor
		e error
	}, 1)
	go func() {
		d, e := store.Put(v1.MediaTypeImageLayerGzip, pr)
		pr.CloseWithError(e)
		result <- struct {
			d v1.Descriptor
			e error
		}{d, e}
	}()
	gz := gzip.NewWriter(pw)
	ud := digest.SHA256.Digester()
	tw := tar.NewWriter(io.MultiWriter(gz, ud.Hash()))
	e := write(tw)
	if e == nil {
		e = tw.Close()
	}
	if e == nil {
		e = gz.Close()
	}
	pw.CloseWithError(e)
	r := <-result
	if e != nil {
		return BuiltLayer{}, e
	}
	if r.e != nil {
		return BuiltLayer{}, r.e
	}
	r.d.Annotations = map[string]string{"brewlet.sh/layer": role}
	return BuiltLayer{r.d, ud.Digest()}, nil
}
func BuildLayer(ctx context.Context, store Store, spec LayerSpec) (BuiltLayer, error) {
	if spec.Role != "app" && spec.Role != "dependency" {
		return BuiltLayer{}, fail("UnsupportedArtifact", "invalid layer role")
	}
	if _, e := archivePath(spec.Destination); e != nil {
		return BuiltLayer{}, e
	}
	sourceAbs, e := filepath.EvalSymlinks(spec.Source)
	if e != nil {
		return BuiltLayer{}, e
	}
	sourceAbs, e = filepath.Abs(sourceAbs)
	if e != nil {
		return BuiltLayer{}, e
	}
	storeAbs, e := canonicalOutput(filepath.Join(store.Dir, "blobs", "sha256"))
	if e != nil {
		return BuiltLayer{}, e
	}
	relative, e := filepath.Rel(sourceAbs, storeAbs)
	if e != nil {
		return BuiltLayer{}, e
	}
	if relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return BuiltLayer{}, fmt.Errorf("layer source contains output store; choose a separate --store directory")
	}
	fi, e := os.Lstat(spec.Source)
	if e != nil {
		return BuiltLayer{}, e
	}
	if !fi.IsDir() {
		return BuiltLayer{}, fmt.Errorf("layer source must be a directory")
	}
	sourceRoot, e := os.OpenRoot(spec.Source)
	if e != nil {
		return BuiltLayer{}, e
	}
	defer sourceRoot.Close()
	return writeLayer(store, spec.Role, func(tw *tar.Writer) error {
		var names []string
		e := fs.WalkDir(sourceRoot.FS(), ".", func(p string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			names = append(names, p)
			return nil
		})
		if e != nil {
			return e
		}
		sort.Strings(names)
		hardlinks := map[fileIdentity]string{}
		for _, p := range names {
			if e = ctx.Err(); e != nil {
				return e
			}
			fi, e := sourceRoot.Lstat(p)
			if e != nil {
				return e
			}
			if e = rejectSourceXattrs(filepath.Join(spec.Source, filepath.FromSlash(p))); e != nil {
				return e
			}
			rel := p
			name := spec.Destination
			if rel != "." {
				name = path.Join(name, filepath.ToSlash(rel))
			}
			link := ""
			if fi.Mode()&os.ModeSymlink != 0 {
				link, e = sourceRoot.Readlink(p)
				if e != nil {
					return e
				}
			}
			h, e := tar.FileInfoHeader(fi, link)
			if e != nil {
				return fail("LayerPathViolation", "%v", e)
			}
			h.Name = name
			h.Uid = 0
			h.Gid = 0
			h.Uname = ""
			h.Gname = ""
			h.AccessTime = fi.ModTime()
			h.ChangeTime = fi.ModTime()
			h.Format = tar.FormatPAX
			if fi.Mode().IsRegular() {
				id := sourceIdentity(fi)
				if id.Ino != 0 {
					if previous, ok := hardlinks[id]; ok {
						h.Typeflag = tar.TypeLink
						h.Linkname = previous
						h.Size = 0
					} else {
						hardlinks[id] = name
					}
				}
			}
			if e = tw.WriteHeader(h); e != nil {
				return e
			}
			if h.Typeflag == tar.TypeReg {
				f, e := sourceRoot.Open(p)
				if e != nil {
					return e
				}
				_, e = io.Copy(tw, f)
				f.Close()
				if e != nil {
					return e
				}
			}
		}
		return nil
	})
}

func canonicalOutput(p string) (string, error) {
	p, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	var suffix []string
	for {
		real, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return real, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", e
		}
		suffix = append(suffix, filepath.Base(p))
		p = parent
	}
}

type BuildOptions struct {
	Launch        []byte
	Platforms     []v1.Platform
	Layers        []LayerSpec
	LayerSets     []v1.Descriptor
	LayerSetTrust LayerSetTrust
}

func Build(ctx context.Context, store Store, o BuildOptions) (v1.Descriptor, error) {
	l, e := ParseLaunch(o.Launch)
	if e != nil {
		return v1.Descriptor{}, e
	}
	if len(o.Platforms) == 0 {
		return v1.Descriptor{}, fail("InvalidPlatform", "explicit platforms required")
	}
	var layers []BuiltLayer
	var evidence []LayerSetEvidence
	for _, set := range o.LayerSets {
		ls, ev, e := ConsumeLayerSet(ctx, store, set, o.LayerSetTrust)
		if e != nil {
			return v1.Descriptor{}, e
		}
		layers = append(layers, ls...)
		evidence = append(evidence, ev)
	}
	seenPlatforms := map[string]bool{}
	for _, p := range o.Platforms {
		key := p.OS + "/" + p.Architecture + "/" + p.Variant
		if seenPlatforms[key] {
			return v1.Descriptor{}, fail("InvalidPlatform", "duplicate platform")
		}
		seenPlatforms[key] = true
		if _, e = ParsePlatform(strings.TrimSuffix(key, "/")); e != nil {
			return v1.Descriptor{}, e
		}
	}
	built := make([]BuiltLayer, len(o.Layers))
	for i, s := range o.Layers {
		if s.Role != "app" && s.Role != "dependency" {
			return v1.Descriptor{}, fail("UnsupportedArtifact", "invalid source role")
		}
		if s.Platform != nil {
			declared := false
			for _, p := range o.Platforms {
				declared = declared || sameDeclaredPlatform(p, *s.Platform)
			}
			if !declared {
				return v1.Descriptor{}, fail("InvalidPlatform", "layer platform %s/%s is not a declared --platform", s.Platform.OS, s.Platform.Architecture)
			}
		}
		layer, e := BuildLayer(ctx, store, s)
		if e != nil {
			return v1.Descriptor{}, e
		}
		built[i] = layer
	}
	launch, e := writeLayer(store, "launch", func(t *tar.Writer) error {
		h := &tar.Header{Name: "launch.json", Mode: 0644, Size: int64(len(o.Launch)), Typeflag: tar.TypeReg}
		if e := t.WriteHeader(h); e != nil {
			return e
		}
		_, e := io.Copy(t, bytes.NewReader(o.Launch))
		return e
	})
	if e != nil {
		return v1.Descriptor{}, e
	}
	walked := map[digest.Digest]LayerSource{}
	source := func(b BuiltLayer) (LayerSource, error) {
		if src, ok := walked[b.Descriptor.Digest]; ok {
			return src, nil
		}
		d := b.Descriptor
		src := LayerSource{Descriptor: d, DiffID: b.DiffID, Open: func() (io.ReadCloser, error) { return store.Open(d) }}
		if _, e := WalkLayer(ctx, src, nil); e != nil {
			return LayerSource{}, e
		}
		walked[d.Digest] = src
		return src, nil
	}
	idx := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex}
	for _, p := range o.Platforms {
		platformLayers := append([]BuiltLayer(nil), layers...)
		apps := 0
		for i, s := range o.Layers {
			if s.Platform != nil && !sameDeclaredPlatform(*s.Platform, p) {
				continue
			}
			if s.Role == "app" {
				apps++
			}
			platformLayers = append(platformLayers, built[i])
		}
		if apps == 0 {
			return v1.Descriptor{}, fail("UnsupportedArtifact", "at least one app layer required")
		}
		platformLayers = append(platformLayers, launch)
		var descs []v1.Descriptor
		var diffs []digest.Digest
		var sources []LayerSource
		for _, b := range platformLayers {
			src, e := source(b)
			if e != nil {
				return v1.Descriptor{}, e
			}
			sources = append(sources, src)
			descs = append(descs, b.Descriptor)
			diffs = append(diffs, b.DiffID)
		}
		if e = ValidateComposition(ctx, sources); e != nil {
			return v1.Descriptor{}, e
		}
		image := v1.Image{Platform: p, Config: l.ImageConfig(), RootFS: v1.RootFS{Type: "layers", DiffIDs: diffs}}
		config, e := store.PutJSON(v1.MediaTypeImageConfig, image)
		if e != nil {
			return v1.Descriptor{}, e
		}
		m := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: descs, Annotations: map[string]string{"brewlet.sh/artifact-version": "1"}}
		if len(evidence) > 0 {
			b, _ := jsonBytes(evidence)
			m.Annotations["brewlet.sh/layerset-evidence"] = string(b)
		}
		d, e := store.PutJSON(v1.MediaTypeImageManifest, m)
		if e != nil {
			return v1.Descriptor{}, e
		}
		d.Platform = &p
		idx.Manifests = append(idx.Manifests, d)
	}
	if len(idx.Manifests) == 1 {
		return idx.Manifests[0], nil
	}
	return store.PutJSON(v1.MediaTypeImageIndex, idx)
}
