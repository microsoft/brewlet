//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/brewlet/pkg/artifact"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestWarmCreateRetainsOriginalLaunchMetadataAndPayloadBytes(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".warm-source-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &platformFixture{blobs: map[digest.Digest][]byte{}}
	host := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	manifest := f.manifest(t, host, host)
	target := f.json(t, ocispec.MediaTypeImageIndex, ocispec.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: []ocispec.Descriptor{manifest}})
	source := func(d ocispec.Descriptor) (io.ReadCloser, error) {
		if _, exists := f.blobs[d.Digest]; !exists {
			return nil, os.ErrNotExist
		}
		return f.open(d)
	}
	cache := &artifact.ContentCache{Dir: filepath.Join(dir, ".content")}
	open := func(d ocispec.Descriptor) (io.ReadCloser, error) { return cache.Open(context.Background(), source, d) }
	first, err := resolveNativeArtifact(context.Background(), open, target, host)
	if err != nil {
		t.Fatal(err)
	}
	for _, layer := range first.Layers {
		if _, err := (artifact.LayerCache{Dir: dir}).Materialize(context.Background(), layer); err != nil {
			t.Fatal(err)
		}
	}
	launch := first.Manifest.Layers[len(first.Manifest.Layers)-1]
	delete(f.blobs, launch.Digest)
	second, err := resolveNativeArtifact(context.Background(), open, target, host)
	if err != nil || second.Manifest.Config.Digest != first.Manifest.Config.Digest || string(second.LaunchJSON) != string(first.LaunchJSON) {
		t.Fatal("discarded final launch layer broke repeat Create", err)
	}
	// Also cover legitimate loss of all compressed and protected metadata
	// content. Every fallback byte must be the original verified object.
	clear(f.blobs)
	cachedTarget, err := cache.Target(context.Background(), target.Digest)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := resolveNativeArtifact(context.Background(), open, cachedTarget, host)
	if err != nil {
		t.Fatal(err)
	}
	if warm.Target.Digest != target.Digest || warm.ManifestDescriptor.Digest != manifest.Digest || warm.Manifest.Config.Digest != first.Manifest.Config.Digest {
		t.Fatal("warm metadata identities were reconstructed or changed")
	}
	for _, layer := range warm.Layers {
		if _, err := (artifact.LayerCache{Dir: dir}).Materialize(context.Background(), layer); err != nil {
			t.Fatal(err)
		}
	}
	if len(cache.References()) != 5 {
		t.Fatal("retained graph references incomplete", cache.References())
	}
	layer := warm.Layers[0]
	receiptPath := filepath.Join(dir, artifact.PolicyVersion+"-"+layer.Descriptor.Digest.Encoded(), "receipt.json")
	data, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatal(err)
	}
	var receipt artifact.CacheReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt.DiffID = digest.FromString("different uncompressed payload").String()
	data, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(receiptPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, data, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(receiptPath, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := (artifact.LayerCache{Dir: dir}).Materialize(context.Background(), layer); err == nil {
		t.Fatal("warm content fallback bypassed payload cache diff ID receipt")
	}
}

func TestWarmSourceMissingTamperedAndRejectedContentFailClosed(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".warm-fail-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &platformFixture{blobs: map[digest.Digest][]byte{}}
	host := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	target := f.manifest(t, host, host)
	missing := false
	source := func(d ocispec.Descriptor) (io.ReadCloser, error) {
		if missing {
			return nil, os.ErrNotExist
		}
		return f.open(d)
	}
	cache := &artifact.ContentCache{Dir: filepath.Join(dir, ".content")}
	open := func(d ocispec.Descriptor) (io.ReadCloser, error) { return cache.Open(context.Background(), source, d) }
	first, err := resolveNativeArtifact(context.Background(), open, target, host)
	if err != nil {
		t.Fatal(err)
	}
	launch := first.Manifest.Layers[len(first.Manifest.Layers)-1]
	original := append([]byte(nil), f.blobs[launch.Digest]...)
	f.blobs[launch.Digest] = []byte("present corrupt bytes")
	if _, err := resolveNativeArtifact(context.Background(), open, target, host); artifact.Reason(err) != "DigestMismatch" {
		t.Fatal("corrupt present source fell back to cache", err)
	}
	f.blobs[launch.Digest] = original
	rejected := func(ocispec.Descriptor) (io.ReadCloser, error) { return nil, os.ErrPermission }
	if _, err := cache.Open(context.Background(), rejected, launch); !errors.Is(err, os.ErrPermission) {
		t.Fatal("rejected source fell back", err)
	}
	missing = true
	entry := filepath.Join(cache.Dir, artifact.PolicyVersion+"-"+launch.Digest.Encoded())
	blob := filepath.Join(entry, "blob")
	if err := os.Chmod(blob, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blob, []byte("tampered"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blob, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveNativeArtifact(context.Background(), open, target, host); err == nil {
		t.Fatal("tampered retained original accepted")
	}
	if err := os.RemoveAll(entry); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveNativeArtifact(context.Background(), open, target, host); err == nil {
		t.Fatal("missing launch/cache accepted")
	}
}
