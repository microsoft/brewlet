// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/brewlet/internal/artifact"
)

// writeContentBlob writes b into an OCI-style content store and returns its
// "sha256:<hex>" digest, mirroring how containerd lays blobs on disk.
func writeContentBlob(t *testing.T, root string, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	digest := fmt.Sprintf("sha256:%x", sum)
	dir := filepath.Join(root, "blobs", "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, digest[len("sha256:"):]), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return digest
}

// mustContentBlobPath resolves a digest to its content-store path, failing the
// test if the digest is not canonical.
func mustContentBlobPath(t *testing.T, root, digest string) string {
	t.Helper()
	p, err := contentBlobPath(root, digest)
	if err != nil {
		t.Fatalf("contentBlobPath(%q): %v", digest, err)
	}
	return p
}

func TestContentStoreBlobs(t *testing.T) {
	root := t.TempDir()

	jarDigest := writeContentBlob(t, root, []byte("PK\x03\x04 fake-jar-bytes"))

	cfg := artifact.JVMConfig{
		SchemaVersion: 1,
		MainJar:       "app.jar",
		Entry:         artifact.Entry{Mode: "jar"},
	}
	cfgBytes, _ := json.Marshal(cfg)
	cfgDigest := writeContentBlob(t, root, cfgBytes)

	man := artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		ArtifactType:  artifact.ArtifactType,
		Config:        artifact.Descriptor{MediaType: artifact.ConfigMediaType, Digest: cfgDigest},
		Layers: []artifact.Descriptor{
			{MediaType: artifact.JarLayerMediaType, Digest: jarDigest},
		},
	}
	manBytes, _ := json.Marshal(man)
	manDigest := writeContentBlob(t, root, manBytes)

	blobs, err := contentStoreBlobs(root, manDigest)
	if err != nil {
		t.Fatalf("contentStoreBlobs: %v", err)
	}
	if blobs.Config.MainJar != "app.jar" {
		t.Errorf("MainJar = %q, want app.jar", blobs.Config.MainJar)
	}
	if want := mustContentBlobPath(t, root, jarDigest); blobs.JarHostPath != want {
		t.Errorf("JarHostPath = %q, want %q", blobs.JarHostPath, want)
	}
	if _, err := os.Stat(blobs.JarHostPath); err != nil {
		t.Errorf("resolved jar path not on disk: %v", err)
	}
	if blobs.ManifestDigest != manDigest {
		t.Errorf("ManifestDigest = %q, want %q", blobs.ManifestDigest, manDigest)
	}
}

func TestContentStoreBlobsErrors(t *testing.T) {
	root := t.TempDir()

	if _, err := contentStoreBlobs(root, ""); err == nil {
		t.Error("expected error for empty manifest digest")
	}
	if _, err := contentStoreBlobs(root, "sha256:deadbeef"); err == nil {
		t.Error("expected error for missing manifest blob")
	}

	// A non-Brewlet artifactType must be rejected.
	man := artifact.Manifest{ArtifactType: "application/vnd.oci.image.config.v1+json"}
	manBytes, _ := json.Marshal(man)
	badDigest := writeContentBlob(t, root, manBytes)
	if _, err := contentStoreBlobs(root, badDigest); err == nil {
		t.Error("expected error for non-Brewlet artifactType")
	}
}

func TestLoadArtifactBlobsBackendSelection(t *testing.T) {
	// Layout backend round-trips through the local OCI layout store.
	layoutRoot := t.TempDir()
	jarPath := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 layout-jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := artifact.Store{Root: layoutRoot}
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	manifestDesc, err := store.Push("demo/hello:1.0.0", cfg, jarPath)
	if err != nil {
		t.Fatal(err)
	}

	got, err := loadArtifactBlobs(imageConfig{StoreRoot: layoutRoot, Ref: "demo/hello:1.0.0"})
	if err != nil {
		t.Fatalf("layout backend: %v", err)
	}
	if got.Config.MainJar != "app.jar" {
		t.Errorf("layout MainJar = %q", got.Config.MainJar)
	}
	if got.ManifestDigest != manifestDesc.Digest {
		t.Errorf("layout ManifestDigest = %q, want %q", got.ManifestDigest, manifestDesc.Digest)
	}

	// Unknown backend is rejected.
	if _, err := loadArtifactBlobs(imageConfig{Backend: "bogus"}); err == nil {
		t.Error("expected error for unknown backend")
	}
}

func TestResolveArtifactPropagatesManifestDigest(t *testing.T) {
	layoutRoot := t.TempDir()
	jarPath := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 layout-jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := artifact.Store{Root: layoutRoot}
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	manifestDesc, err := store.Push("demo/resolve:1", cfg, jarPath)
	if err != nil {
		t.Fatal(err)
	}

	jdkRoot := filepath.Join(t.TempDir(), "temurin-21")
	if err := os.MkdirAll(filepath.Join(jdkRoot, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jdkRoot, "bin", "java"), []byte("java"), 0o755); err != nil {
		t.Fatal(err)
	}
	activate(t, filepath.Dir(jdkRoot), jdkActiveInventory, "temurin-21")

	got, err := resolveArtifact(imageConfig{
		StoreRoot:   layoutRoot,
		Ref:         "demo/resolve:1",
		JDKRootsDir: filepath.Dir(jdkRoot),
	})
	if err != nil {
		t.Fatalf("resolveArtifact: %v", err)
	}
	if got.ManifestDigest != manifestDesc.Digest {
		t.Errorf("ManifestDigest = %q, want %q", got.ManifestDigest, manifestDesc.Digest)
	}
}
