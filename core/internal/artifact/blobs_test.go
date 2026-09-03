// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type manifestBlobSource struct {
	blobs map[string][]byte
	reads int
}

func (s *manifestBlobSource) ReadBlob(digest string) ([]byte, error) {
	s.reads++
	return s.blobs[digest], nil
}

func (*manifestBlobSource) BlobPath(string) (string, error) { return "", nil }

func testPlatform() *Platform {
	target := currentRunnablePlatform()
	return &target
}

func testManifestBytes(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config:        Descriptor{Digest: digestOf([]byte("config"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestResolveManifestFollowingIndexVerifiesDirectManifest(t *testing.T) {
	raw := testManifestBytes(t)
	digest := digestOf(raw)
	src := &manifestBlobSource{blobs: map[string][]byte{digest: raw}}

	man, gotDigest, err := ResolveManifestFollowingIndex(src, digest)
	if err != nil {
		t.Fatalf("ResolveManifestFollowingIndex: %v", err)
	}
	if man.SchemaVersion != 2 {
		t.Errorf("schemaVersion = %d, want 2", man.SchemaVersion)
	}
	if gotDigest != digest {
		t.Errorf("resolved digest = %q, want %q", gotDigest, digest)
	}
}

func TestResolveManifestFollowingIndexVerifiesSelectedPlatformManifest(t *testing.T) {
	manifestRaw := testManifestBytes(t)
	manifestDigest := digestOf(manifestRaw)
	indexRaw, err := json.Marshal(Index{
		SchemaVersion: 2,
		MediaType:     OCIImageIndexMediaType,
		Manifests: []Descriptor{{
			Digest:   manifestDigest,
			Size:     int64(len(manifestRaw)),
			Platform: testPlatform(),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexDigest := digestOf(indexRaw)
	src := &manifestBlobSource{blobs: map[string][]byte{
		indexDigest:    indexRaw,
		manifestDigest: manifestRaw,
	}}

	_, gotDigest, err := ResolveManifestFollowingIndex(src, indexDigest)
	if err != nil {
		t.Fatalf("ResolveManifestFollowingIndex: %v", err)
	}
	if gotDigest != manifestDigest {
		t.Errorf("resolved digest = %q, want platform manifest %q", gotDigest, manifestDigest)
	}
	if src.reads != 2 {
		t.Errorf("source reads = %d, want outer index and platform manifest", src.reads)
	}
}

func TestResolveManifestFollowingIndexUsesExactPlatformWithoutFallback(t *testing.T) {
	config := Descriptor{Digest: digestOf([]byte("shared-config"))}
	manifestBytes := func(mainJar string) []byte {
		t.Helper()
		raw, err := json.Marshal(Manifest{
			SchemaVersion: 2,
			MediaType:     ociManifestMediaType,
			Config:        config,
			Annotations: map[string]string{
				JVMConfigAnnotation: `{"schemaVersion":1,"mainJar":"` + mainJar + `","entry":{"mode":"jar"}}`,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	maliciousRaw := manifestBytes("malicious.jar")
	maliciousDigest := digestOf(maliciousRaw)
	safeRaw := manifestBytes("safe.jar")
	safeDigest := digestOf(safeRaw)
	target := currentRunnablePlatform()
	wrongOS := target
	wrongOS.OS = "windows"

	indexRaw, err := json.Marshal(Index{
		SchemaVersion: 2,
		MediaType:     OCIImageIndexMediaType,
		Manifests: []Descriptor{
			{Digest: maliciousDigest, Size: int64(len(maliciousRaw)), Platform: &wrongOS},
			{Digest: safeDigest, Size: int64(len(safeRaw)), Platform: &target},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexDigest := digestOf(indexRaw)
	src := &manifestBlobSource{blobs: map[string][]byte{
		indexDigest:     indexRaw,
		maliciousDigest: maliciousRaw,
		safeDigest:      safeRaw,
	}}

	man, gotDigest, err := ResolveManifestFollowingIndex(src, indexDigest)
	if err != nil {
		t.Fatalf("ResolveManifestFollowingIndex: %v", err)
	}
	if gotDigest != safeDigest {
		t.Fatalf("resolved digest = %q, want matching platform manifest %q", gotDigest, safeDigest)
	}
	if got := man.Annotations[JVMConfigAnnotation]; !strings.Contains(got, `"mainJar":"safe.jar"`) {
		t.Fatalf("resolved launch config = %q, want safe platform manifest", got)
	}
	if src.reads != 2 {
		t.Fatalf("source reads = %d, want index plus matching platform manifest", src.reads)
	}

	indexRaw, err = json.Marshal(Index{
		SchemaVersion: 2,
		MediaType:     OCIImageIndexMediaType,
		Manifests: []Descriptor{
			{Digest: maliciousDigest, Size: int64(len(maliciousRaw)), Platform: &wrongOS},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexDigest = digestOf(indexRaw)
	src = &manifestBlobSource{blobs: map[string][]byte{
		indexDigest:     indexRaw,
		maliciousDigest: maliciousRaw,
	}}
	if _, _, err := ResolveManifestFollowingIndex(src, indexDigest); err == nil || !strings.Contains(err.Error(), "has no manifest for") {
		t.Fatalf("error = %v, want unmatched platform rejection", err)
	}
	if src.reads != 1 {
		t.Fatalf("source reads = %d, want no unmatched manifest fallback", src.reads)
	}
}

func TestResolveManifestFollowingIndexRejectsDigestMismatch(t *testing.T) {
	t.Run("outer index", func(t *testing.T) {
		expectedRaw, err := json.Marshal(Index{
			SchemaVersion: 2,
			MediaType:     OCIImageIndexMediaType,
			Manifests:     []Descriptor{{Digest: digestOf(testManifestBytes(t))}},
		})
		if err != nil {
			t.Fatal(err)
		}
		requested := digestOf(expectedRaw)
		src := &manifestBlobSource{blobs: map[string][]byte{
			requested: append(expectedRaw, '\n'),
		}}

		_, gotDigest, err := ResolveManifestFollowingIndex(src, requested)
		if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("error = %v, want digest mismatch", err)
		}
		if gotDigest != "" {
			t.Errorf("resolved digest = %q after mismatch, want empty", gotDigest)
		}
	})

	t.Run("selected platform manifest", func(t *testing.T) {
		expectedManifest := testManifestBytes(t)
		manifestDigest := digestOf(expectedManifest)
		indexRaw, err := json.Marshal(Index{
			SchemaVersion: 2,
			MediaType:     OCIImageIndexMediaType,
			Manifests:     []Descriptor{{Digest: manifestDigest, Platform: testPlatform()}},
		})
		if err != nil {
			t.Fatal(err)
		}
		indexDigest := digestOf(indexRaw)
		src := &manifestBlobSource{blobs: map[string][]byte{
			indexDigest:    indexRaw,
			manifestDigest: append(expectedManifest, '\n'),
		}}

		_, gotDigest, err := ResolveManifestFollowingIndex(src, indexDigest)
		if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
			t.Fatalf("error = %v, want selected manifest digest mismatch", err)
		}
		if gotDigest != "" {
			t.Errorf("resolved digest = %q after mismatch, want empty", gotDigest)
		}
	})
}

func TestResolveManifestFollowingIndexRejectsInvalidDigestBeforeSourceAccess(t *testing.T) {
	for _, digest := range []string{
		"sha256:deadbeef",
		"sha256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"SHA256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	} {
		t.Run(digest, func(t *testing.T) {
			src := &manifestBlobSource{blobs: map[string][]byte{}}
			if _, _, err := ResolveManifestFollowingIndex(src, digest); err == nil {
				t.Fatal("expected invalid digest error")
			}
			if src.reads != 0 {
				t.Errorf("source reads = %d, want 0", src.reads)
			}
		})
	}

	t.Run("selected platform manifest", func(t *testing.T) {
		indexRaw, err := json.Marshal(Index{
			SchemaVersion: 2,
			MediaType:     OCIImageIndexMediaType,
			Manifests:     []Descriptor{{Digest: "sha256:invalid", Platform: testPlatform()}},
		})
		if err != nil {
			t.Fatal(err)
		}
		indexDigest := digestOf(indexRaw)
		src := &manifestBlobSource{blobs: map[string][]byte{indexDigest: indexRaw}}

		if _, _, err := ResolveManifestFollowingIndex(src, indexDigest); err == nil {
			t.Fatal("expected invalid selected digest error")
		}
		if src.reads != 1 {
			t.Errorf("source reads = %d, want only the outer index read", src.reads)
		}
	})
}

func TestResolveManifestFollowingIndexRejectsDescriptorSizeMismatch(t *testing.T) {
	manifestRaw := testManifestBytes(t)
	manifestDigest := digestOf(manifestRaw)
	indexRaw, err := json.Marshal(Index{
		SchemaVersion: 2,
		MediaType:     OCIImageIndexMediaType,
		Manifests:     []Descriptor{{Digest: manifestDigest, Size: int64(len(manifestRaw) + 1), Platform: testPlatform()}},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexDigest := digestOf(indexRaw)
	src := &manifestBlobSource{blobs: map[string][]byte{
		indexDigest:    indexRaw,
		manifestDigest: manifestRaw,
	}}

	_, gotDigest, err := ResolveManifestFollowingIndex(src, indexDigest)
	if err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("error = %v, want descriptor size mismatch", err)
	}
	if gotDigest != "" {
		t.Errorf("resolved digest = %q after mismatch, want empty", gotDigest)
	}
}

func TestStoreResolveBlobsRejectsManifestDigestMismatch(t *testing.T) {
	root := t.TempDir()
	jarPath := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: root}
	cfg := JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "jar"}}
	desc, err := store.Push("demo/corrupt:1", cfg, jarPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath, err := store.BlobPath(desc.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(strings.Repeat("x", int(desc.Size))), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := store.ResolveBlobs("demo/corrupt:1")
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want manifest digest mismatch", err)
	}
	if got.ManifestDigest != "" {
		t.Errorf("ManifestDigest = %q after mismatch, want empty", got.ManifestDigest)
	}
}

// storeBlobSource resolves blobs from a local OCI layout, which is what the
// CLI's run/bundle path uses.
func TestResolveNativeBlobsRejectsTraversalLayerDigest(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	jarPath := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "jar"}}
	if _, err := store.Push("demo/hostile:1", cfg, jarPath); err != nil {
		t.Fatal(err)
	}
	man, manifestDigest, err := store.ResolveManifestByRef("demo/hostile:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveNativeBlobs(store, man, manifestDigest); err != nil {
		t.Fatalf("untampered artifact must resolve: %v", err)
	}

	for _, digest := range []string{
		"sha256:../../../../../..",
		"sha256:..",
		"sha256:" + strings.Repeat("a", 63),
		"",
	} {
		hostile := man
		hostile.Layers = append([]Descriptor(nil), man.Layers...)
		for i := range hostile.Layers {
			if hostile.Layers[i].MediaType == JarLayerMediaType {
				hostile.Layers[i].Digest = digest
			}
		}
		got, err := ResolveNativeBlobs(store, hostile, manifestDigest)
		if err == nil {
			t.Errorf("ResolveNativeBlobs accepted jar digest %q", digest)
		}
		if got.JarHostPath != "" {
			t.Errorf("jar digest %q produced host path %q, want none", digest, got.JarHostPath)
		}
	}
}

// TestResolveNativeBlobsRejectsTamperedJarBytes covers a blob that is mounted
// rather than read: its bytes must still match the descriptor that named it.
func TestResolveNativeBlobsRejectsTamperedJarBytes(t *testing.T) {
	root := t.TempDir()
	store := Store{Root: root}
	jarPath := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: Entry{Mode: "jar"}}
	if _, err := store.Push("demo/swapped:1", cfg, jarPath); err != nil {
		t.Fatal(err)
	}
	man, manifestDigest, err := store.ResolveManifestByRef("demo/swapped:1")
	if err != nil {
		t.Fatal(err)
	}
	layer, err := man.JarLayer()
	if err != nil {
		t.Fatal(err)
	}
	blobPath, err := store.BlobPath(layer.Digest)
	if err != nil {
		t.Fatal(err)
	}
	// Same length as the original payload, so the size check cannot mask the
	// content hash check.
	if err := os.WriteFile(blobPath, []byte("swapped!"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveNativeBlobs(store, man, manifestDigest)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("error = %v, want a jar blob digest mismatch", err)
	}
	if got.JarHostPath != "" {
		t.Errorf("JarHostPath = %q after rejection, want empty", got.JarHostPath)
	}
}
