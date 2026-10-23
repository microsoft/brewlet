//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/microsoft/brewlet/pkg/artifact"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type platformFixture struct{ blobs map[digest.Digest][]byte }

func (f *platformFixture) put(media string, data []byte) ocispec.Descriptor {
	d := ocispec.Descriptor{MediaType: media, Digest: digest.FromBytes(data), Size: int64(len(data))}
	f.blobs[d.Digest] = data
	return d
}
func (f *platformFixture) json(t *testing.T, media string, value any) ocispec.Descriptor {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return f.put(media, data)
}
func (f *platformFixture) layer(t *testing.T, name string, data []byte, role string) (ocispec.Descriptor, digest.Digest) {
	t.Helper()
	var raw bytes.Buffer
	writer := tar.NewWriter(&raw)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0444, Typeflag: tar.TypeReg, Size: int64(len(data))}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	d := f.put(ocispec.MediaTypeImageLayerGzip, compressed.Bytes())
	d.Annotations = map[string]string{"brewlet.sh/layer": role}
	return d, digest.FromBytes(raw.Bytes())
}
func (f *platformFixture) manifest(t *testing.T, declared, configPlatform ocispec.Platform) ocispec.Descriptor {
	t.Helper()
	launch := []byte(`{"schemaVersion":1,"process":{"entrypoint":["tool"],"workingDir":"/app"}}`)
	app, appDiff := f.layer(t, "app/code", []byte("neutral bytes"), "app")
	final, launchDiff := f.layer(t, "launch.json", launch, "launch")
	config := ocispec.Image{Platform: configPlatform, Config: ocispec.ImageConfig{Entrypoint: []string{"tool"}, WorkingDir: "/app", Labels: map[string]string{"sh.brewlet.app": "1"}}, RootFS: ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{appDiff, launchDiff}}}
	c := f.json(t, ocispec.MediaTypeImageConfig, config)
	m := f.json(t, ocispec.MediaTypeImageManifest, ocispec.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageManifest, Config: c, Layers: []ocispec.Descriptor{app, final}, Annotations: map[string]string{"brewlet.sh/artifact-version": "1"}})
	m.Platform = &declared
	return m
}
func (f *platformFixture) open(d ocispec.Descriptor) (io.ReadCloser, error) {
	value, ok := f.blobs[d.Digest]
	if !ok {
		return nil, io.EOF
	}
	return io.NopCloser(bytes.NewReader(value)), nil
}
func TestShimArtifactPlatformSelectionRetainsExactHostVariant(t *testing.T) {
	for _, test := range []struct {
		name, arch, host, declared, config string
		accepted                           bool
	}{
		{"arm v7", "arm", "v7", "v7", "v7", true},
		{"arm v7 rejects v6", "arm", "v7", "v6", "v6", false},
		{"arm64 equivalent v8", "arm64", "", "v8", "v8", true},
		{"descriptor default alias mismatch", "arm64", "", "v8", "", false},
		{"arm64 rejects higher v9", "arm64", "", "v9", "v9", false},
		{"amd64 equivalent v1", "amd64", "", "v1", "v1", true},
		{"amd64 rejects v3 default", "amd64", "", "v3", "v3", false},
		{"amd64 v3 exact", "amd64", "v3", "v3", "v3", true},
		{"descriptor config mismatch", "amd64", "v2", "v2", "v3", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := &platformFixture{blobs: map[digest.Digest][]byte{}}
			host := ocispec.Platform{OS: "linux", Architecture: test.arch, Variant: test.host}
			m := f.manifest(t, ocispec.Platform{OS: "linux", Architecture: test.arch, Variant: test.declared}, ocispec.Platform{OS: "linux", Architecture: test.arch, Variant: test.config})
			target := f.json(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{m}})
			resolved, err := resolveNativeArtifact(context.Background(), f.open, target, host)
			if test.accepted {
				if err != nil || resolved.ManifestDescriptor.Digest != m.Digest {
					t.Fatal(resolved, err)
				}
			} else if err == nil {
				t.Fatal("incompatible platform/mirror selected")
			}
			runtimePlatform := ocispec.Platform{OS: "linux", Architecture: test.arch, Variant: test.declared}
			runtimeErr := validateRuntimeHostPlatform(runtimePlatform, host)
			if test.declared == test.config && (runtimeErr == nil) != test.accepted {
				t.Fatal("leased runtime host variant gate differs", runtimeErr)
			}
			if test.declared != test.config && artifact.Reason(err) != "LaunchConfigMismatch" {
				t.Fatal("config declaration mismatch was not preserved", err)
			}
		})
	}
}

func TestShimRejectsAmbiguousEquivalentDefaultPlatformDescriptors(t *testing.T) {
	f := &platformFixture{blobs: map[digest.Digest][]byte{}}
	p := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	a := f.manifest(t, p, p)
	alias := a
	v8 := p
	v8.Variant = "v8"
	alias.Platform = &v8
	index := f.json(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{a, alias}})
	if _, err := resolveNativeArtifact(context.Background(), f.open, index, p); artifact.Reason(err) != "InvalidPlatform" {
		t.Fatal("ambiguous normalized selection accepted", err)
	}
}
