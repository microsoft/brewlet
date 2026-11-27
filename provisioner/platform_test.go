package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type platformStore struct {
	blobs      map[digest.Digest][]byte
	opens      map[digest.Digest]int
	layerOpens int
}

func (s *platformStore) put(t *testing.T, media string, value any) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	d := ocispec.Descriptor{MediaType: media, Digest: digest.FromBytes(data), Size: int64(len(data))}
	s.blobs[d.Digest] = data
	return d
}
func (s *platformStore) manifest(t *testing.T, p ocispec.Platform) ocispec.Descriptor {
	t.Helper()
	config := ocispec.Image{Platform: p, RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.Digest("sha256:" + strings.Repeat("a", 64))}}}
	c := s.put(t, ocispec.MediaTypeImageConfig, config)
	m := s.put(t, ocispec.MediaTypeImageManifest, ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: c, Layers: []ocispec.Descriptor{{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.Digest("sha256:" + strings.Repeat("b", 64)), Size: 123}}})
	m.Platform = &p
	return m
}
func (s *platformStore) open(d ocispec.Descriptor) (io.ReadCloser, error) {
	if s.opens == nil {
		s.opens = map[digest.Digest]int{}
	}
	s.opens[d.Digest]++
	if d.MediaType == ocispec.MediaTypeImageLayerGzip {
		s.layerOpens++
		return nil, io.ErrUnexpectedEOF
	}
	data, exists := s.blobs[d.Digest]
	if !exists {
		return nil, io.EOF
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestScopedRuntimePlatformsSkipUnrequiredGraphs(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "manifest"
		if nested {
			name = "nested-index"
		}
		t.Run(name, func(t *testing.T) {
			s := &platformStore{blobs: map[digest.Digest][]byte{}}
			amd := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
			foreignPlatform := ocispec.Platform{OS: "linux", Architecture: "riscv64"}
			foreign := s.manifest(t, foreignPlatform)
			if nested {
				foreign = s.put(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{foreign}})
				foreign.Platform = &foreignPlatform
			}
			delete(s.blobs, foreign.Digest)
			index := s.put(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{amd, foreign}})
			required := []ocispec.Platform{{OS: "linux", Architecture: "x86_64", Variant: "v1"}}
			result, err := inspectRuntimePlatforms(context.Background(), index, s.open, required)
			if err != nil {
				t.Fatal(err)
			}
			if err := result.Covers(required); err != nil {
				t.Fatal(err)
			}
			if s.opens[foreign.Digest] != 0 || s.layerOpens != 0 {
				t.Fatal("scoped inspection opened an unrelated graph or layer", s.opens)
			}
			if _, err := InspectRuntimePlatforms(context.Background(), index, s.open); err == nil {
				t.Fatal("full-index inspection did not inspect the missing foreign graph")
			}
		})
	}
}

func TestScopedRuntimePlatformsDiscoverUndeclaredEntries(t *testing.T) {
	s := &platformStore{blobs: map[digest.Digest][]byte{}}
	amd := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	arm := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	amd.Platform, arm.Platform = nil, nil
	index := s.put(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{amd, arm}})
	required := []ocispec.Platform{{OS: "linux", Architecture: "amd64"}}
	result, err := inspectRuntimePlatforms(context.Background(), index, s.open, required)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Covers(required); err != nil {
		t.Fatal(err)
	}
	if s.opens[amd.Digest] != 1 || s.opens[arm.Digest] != 1 || len(result.entries) != 1 {
		t.Fatal("undeclared entries were not discovered or scoped", s.opens, result.entries)
	}
}

func TestScopedRuntimePlatformsRejectInvalidRequiredMetadata(t *testing.T) {
	for _, failure := range []string{"missing", "config-mismatch", "ambiguous-alias", "nested-mismatch", "corrupt"} {
		t.Run(failure, func(t *testing.T) {
			s := &platformStore{blobs: map[digest.Digest][]byte{}}
			p := ocispec.Platform{OS: "linux", Architecture: "amd64"}
			manifest := s.manifest(t, p)
			children := []ocispec.Descriptor{manifest}
			switch failure {
			case "missing":
				children = []ocispec.Descriptor{s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "arm64"})}
			case "config-mismatch":
				wrong := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "arm64"})
				wrong.Platform = &p
				children = []ocispec.Descriptor{wrong}
			case "ambiguous-alias":
				alias := manifest
				alias.Platform = &ocispec.Platform{OS: "linux", Architecture: "x86_64", Variant: "v1"}
				children = append(children, alias)
			case "nested-mismatch":
				child := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "arm64"})
				nested := s.put(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{child}})
				nested.Platform = &p
				children = []ocispec.Descriptor{nested}
			case "corrupt":
				s.blobs[manifest.Digest] = []byte(`{"schemaVersion":2}`)
			}
			index := s.put(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: children})
			required := []ocispec.Platform{p}
			result, err := inspectRuntimePlatforms(context.Background(), index, s.open, required)
			if err == nil {
				err = result.Covers(required)
			}
			if err == nil {
				t.Fatal("invalid required platform metadata accepted")
			}
		})
	}
}
func TestRuntimePlatformMetadataCoversPoolWithoutOpeningLayers(t *testing.T) {
	s := &platformStore{blobs: map[digest.Digest][]byte{}}
	amd := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "amd64", Variant: "v1"})
	arm := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"})
	index := s.put(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{amd, arm}})
	declared, err := InspectRuntimePlatforms(context.Background(), index, s.open)
	if err != nil {
		t.Fatal(err)
	}
	if err := declared.Covers([]ocispec.Platform{{OS: "linux", Architecture: "amd64"}, {OS: "linux", Architecture: "arm64"}}); err != nil {
		t.Fatal(err)
	}
	selected, err := declared.Select(ocispec.Platform{OS: "linux", Architecture: "aarch64", Variant: "v8.0"})
	if err != nil || selected.Manifest.Digest != arm.Digest || selected.Platform.Variant != "" {
		t.Fatal(selected, err)
	}
	if s.layerOpens != 0 {
		t.Fatal("cold metadata validation fetched root layer bytes")
	}
	if err := declared.Covers([]ocispec.Platform{{OS: "linux", Architecture: "s390x"}}); err == nil {
		t.Fatal("uncovered pool architecture accepted")
	}
	if err := declared.Covers(nil); err == nil {
		t.Fatal("unknown offered platforms accepted")
	}
}
func TestRuntimePlatformVariantsRemainStrictAfterNormalization(t *testing.T) {
	s := &platformStore{blobs: map[digest.Digest][]byte{}}
	manifest := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v7"})
	declared, err := InspectRuntimePlatforms(context.Background(), manifest, s.open)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := declared.Select(ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v7"}); err != nil {
		t.Fatal(err)
	}
	if _, err := declared.Select(ocispec.Platform{OS: "linux", Architecture: "arm", Variant: "v6"}); err == nil {
		t.Fatal("unsupported variant fallback accepted")
	}
	v9 := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v9"})
	declared, err = InspectRuntimePlatforms(context.Background(), v9, s.open)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := declared.Select(ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v9"}); err != nil {
		t.Fatal("explicit non-default variant blanket rejected", err)
	}
	if _, err := declared.Select(ocispec.Platform{OS: "linux", Architecture: "arm64"}); err == nil {
		t.Fatal("v9-only runtime treated as default v8")
	}
}
func TestRuntimeMetadataRejectsDigestPlatformMismatchAndAmbiguity(t *testing.T) {
	s := &platformStore{blobs: map[digest.Digest][]byte{}}
	manifest := s.manifest(t, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	wrong := manifest
	p := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	wrong.Platform = &p
	index := s.put(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{wrong}})
	if _, err := InspectRuntimePlatforms(context.Background(), index, s.open); err == nil {
		t.Fatal("index/config mismatch accepted")
	}
	duplicate := manifest
	alias := ocispec.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}
	duplicate.Platform = &alias
	index = s.put(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, Manifests: []ocispec.Descriptor{manifest, duplicate}})
	if _, err := InspectRuntimePlatforms(context.Background(), index, s.open); err == nil {
		t.Fatal("ambiguous normalized aliases accepted")
	}
	s.blobs[manifest.Digest] = []byte(`{"schemaVersion":2}`)
	if _, err := InspectRuntimePlatforms(context.Background(), manifest, s.open); err == nil {
		t.Fatal("corrupt pinned metadata accepted")
	}
	if s.layerOpens != 0 {
		t.Fatal("invalid metadata opened layers")
	}
}
