package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type Store struct{ Dir string }

func (s Store) blobPath(d digest.Digest) (string, error) {
	if d.Validate() != nil || d.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("invalid sha256 digest")
	}
	return filepath.Join(s.Dir, "blobs", "sha256", d.Encoded()), nil
}
func (s Store) Open(d v1.Descriptor) (io.ReadCloser, error) {
	p, e := s.blobPath(d.Digest)
	if e != nil {
		return nil, e
	}
	return os.Open(p)
}
func (s Store) Read(d v1.Descriptor) ([]byte, error) {
	if d.Size < 0 || d.Size > 16<<20 {
		return nil, fmt.Errorf("metadata size exceeds limit")
	}
	r, e := s.Open(d)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	b, e := io.ReadAll(io.LimitReader(r, d.Size+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) != d.Size || digest.FromBytes(b) != d.Digest {
		return nil, fail("DigestMismatch", "metadata identity mismatch")
	}
	return b, nil
}
func (s Store) Put(media string, r io.Reader) (v1.Descriptor, error) {
	dir := filepath.Join(s.Dir, "blobs", "sha256")
	if e := os.MkdirAll(dir, 0755); e != nil {
		return v1.Descriptor{}, e
	}
	f, e := os.CreateTemp(dir, ".blob-")
	if e != nil {
		return v1.Descriptor{}, e
	}
	defer os.Remove(f.Name())
	d := digest.SHA256.Digester()
	n, e := io.Copy(io.MultiWriter(f, d.Hash()), r)
	if e != nil {
		f.Close()
		return v1.Descriptor{}, e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return v1.Descriptor{}, e
	}
	if e = f.Close(); e != nil {
		return v1.Descriptor{}, e
	}
	desc := v1.Descriptor{MediaType: media, Digest: d.Digest(), Size: n}
	p, _ := s.blobPath(desc.Digest)
	if e = os.Link(f.Name(), p); e != nil && !os.IsExist(e) {
		return v1.Descriptor{}, e
	}
	return desc, nil
}
func (s Store) PutJSON(media string, v any) (v1.Descriptor, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return v1.Descriptor{}, e
	}
	return s.Put(media, bytes.NewReader(b))
}
func (s Store) WriteIndex(target v1.Descriptor, ref string) error {
	if e := os.MkdirAll(s.Dir, 0755); e != nil {
		return e
	}
	if e := os.WriteFile(filepath.Join(s.Dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0644); e != nil {
		return e
	}
	target.Annotations = cloneAnnotations(target.Annotations)
	target.Annotations[v1.AnnotationRefName] = ref
	refs, e := s.Referrers(target)
	if e != nil {
		return e
	}
	b, e := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: append([]v1.Descriptor{target}, refs...)})
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(s.Dir, ".index-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), filepath.Join(s.Dir, "index.json"))
}
func (s Store) Target(ref string) (v1.Descriptor, error) {
	b, e := os.ReadFile(filepath.Join(s.Dir, "index.json"))
	if e != nil {
		return v1.Descriptor{}, e
	}
	var idx v1.Index
	if e = json.Unmarshal(b, &idx); e != nil {
		return v1.Descriptor{}, e
	}
	for _, d := range idx.Manifests {
		if ref != "" && (d.Annotations[v1.AnnotationRefName] == ref || string(d.Digest) == ref) {
			return d, nil
		}
	}
	if ref == "" {
		var named []v1.Descriptor
		for _, d := range idx.Manifests {
			if d.Annotations[v1.AnnotationRefName] != "" {
				named = append(named, d)
			}
		}
		if len(named) == 1 {
			return named[0], nil
		}
		if len(idx.Manifests) == 1 {
			return idx.Manifests[0], nil
		}
	}
	return v1.Descriptor{}, fmt.Errorf("reference not in layout: %s", ref)
}

type Resolved struct {
	Target, ManifestDescriptor v1.Descriptor
	Manifest                   v1.Manifest
	Image                      v1.Image
	Launch                     *Launch
	LaunchJSON                 []byte
	Layers                     []LayerSource
}
type BlobOpener func(v1.Descriptor) (io.ReadCloser, error)

func readVerified(open BlobOpener, d v1.Descriptor) ([]byte, error) {
	if d.Size < 0 || d.Size > 16<<20 {
		return nil, fmt.Errorf("metadata too large")
	}
	r, e := open(d)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	b, e := io.ReadAll(io.LimitReader(r, d.Size+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) != d.Size || digest.FromBytes(b) != d.Digest {
		return nil, fail("DigestMismatch", "metadata identity mismatch")
	}
	return b, nil
}

// Resolve verifies a strict platform selection, config mirror and every streamed
// layer. open must read local content only; Resolve never accesses a registry.
func Resolve(ctx context.Context, open BlobOpener, target v1.Descriptor, platform v1.Platform) (*Resolved, error) {
	out := &Resolved{Target: target}
	selected := target
	b, e := readVerified(open, target)
	if e != nil {
		return nil, e
	}
	if target.MediaType == v1.MediaTypeImageIndex {
		var idx v1.Index
		if e = json.Unmarshal(b, &idx); e != nil {
			return nil, e
		}
		if idx.SchemaVersion != 2 || idx.MediaType != v1.MediaTypeImageIndex {
			return nil, fail("UnsupportedArtifact", "invalid OCI image index")
		}
		found := false
		for _, d := range idx.Manifests {
			if d.Platform == nil || d.MediaType != v1.MediaTypeImageManifest {
				return nil, fail("InvalidPlatform", "explicit platform manifest descriptors required")
			}
			if d.Platform != nil && samePlatform(*d.Platform, platform) {
				if found {
					return nil, fail("InvalidPlatform", "ambiguous platform")
				}
				selected = d
				found = true
			}
		}
		if !found {
			return nil, fail("InvalidPlatform", "no exact platform match")
		}
		b, e = readVerified(open, selected)
		if e != nil {
			return nil, e
		}
	}
	if selected.MediaType != v1.MediaTypeImageManifest {
		return nil, fail("UnsupportedArtifact", "OCI image manifest required")
	}
	if e = json.Unmarshal(b, &out.Manifest); e != nil {
		return nil, e
	}
	m := out.Manifest
	if m.SchemaVersion != 2 || m.MediaType != v1.MediaTypeImageManifest || m.Annotations["brewlet.sh/artifact-version"] != "2" || m.Config.MediaType != v1.MediaTypeImageConfig || len(m.Layers) < 2 {
		return nil, fail("UnsupportedArtifact", "not a version 2 application artifact")
	}
	if _, old := m.Annotations["brewlet.sh/jvm-config"]; old {
		return nil, fail("UnsupportedArtifact", "legacy launch annotation")
	}
	b, e = readVerified(open, m.Config)
	if e != nil {
		return nil, e
	}
	if out.Image, e = ParseImage(b); e != nil {
		return nil, e
	}
	if !samePlatform(out.Image.Platform, platform) || out.Image.RootFS.Type != "layers" || len(out.Image.RootFS.DiffIDs) != len(m.Layers) {
		return nil, fail("LaunchConfigMismatch", "platform or rootfs mismatch")
	}
	apps := 0
	for i, d := range m.Layers {
		role := d.Annotations["brewlet.sh/layer"]
		if role == "app" {
			apps++
		} else if role != "dependency" && role != "launch" {
			return nil, fail("UnsupportedArtifact", "invalid layer role")
		}
		if (role == "launch") != (i == len(m.Layers)-1) {
			return nil, fail("UnsupportedArtifact", "launch must be last and unique")
		}
		desc := d
		src := LayerSource{Descriptor: d, DiffID: out.Image.RootFS.DiffIDs[i], Open: func() (io.ReadCloser, error) { return open(desc) }}
		info, e := WalkLayer(ctx, src, nil)
		if e != nil {
			return nil, e
		}
		if role == "launch" {
			out.LaunchJSON = info.Launch
			out.Launch, e = ParseLaunch(info.Launch)
			if e != nil {
				return nil, e
			}
		} else {
			out.Layers = append(out.Layers, src)
		}
	}
	if apps == 0 {
		return nil, fail("UnsupportedArtifact", "application layer required")
	}
	if e = ValidateComposition(ctx, out.Layers); e != nil {
		return nil, e
	}
	if e = ValidateEvidence(out.Manifest); e != nil {
		return nil, e
	}
	if e = validateEvidenceDiffIDs(out.Manifest, out.Image); e != nil {
		return nil, e
	}
	if e = ValidateMirror(out.Launch, out.Image); e != nil {
		return nil, e
	}
	out.ManifestDescriptor = selected
	return out, nil
}
func samePlatform(a, b v1.Platform) bool {
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant && a.OSVersion == b.OSVersion
}
