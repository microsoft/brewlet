// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/microsoft/brewlet/internal/artifact"
)

// traversalDigest is the reported attack value: filepath.Join cleans it away and
// the supposed content-store blob path collapses to "/", so an unvalidated
// resolver would hand the root shim the host root filesystem to bind-mount.
const traversalDigest = "sha256:../../../../../.."

// tamperManifest rewrites the manifest blob at manifestDigest with mutate
// applied and republishes it under its new (correct) digest, returning that
// digest. This models a real publisher: the manifest is internally consistent
// and hashes to what it claims — only the descriptors inside it are hostile.
func tamperManifest(t *testing.T, root, manifestDigest string, mutate func(*artifact.Manifest)) string {
	t.Helper()
	path, err := contentBlobPath(root, manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var man artifact.Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		t.Fatal(err)
	}
	mutate(&man)
	out, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	return writeContentBlob(t, root, out)
}

// writeNativeContentStore lays down a native Brewlet artifact (config + jar,
// classpath, modulepath and CDS layers) in an OCI-style content store and
// returns the store root and the manifest digest.
func writeNativeContentStore(t *testing.T) (root, manifestDigest string) {
	t.Helper()
	root = t.TempDir()
	jarDigest := writeContentBlob(t, root, []byte("PK\x03\x04 jar"))
	depsDigest := writeContentBlob(t, root, []byte("deps-tar"))
	modsDigest := writeContentBlob(t, root, []byte("mods-tar"))
	jsaDigest := writeContentBlob(t, root, []byte("cds-archive"))

	cfg := artifact.JVMConfig{
		SchemaVersion: 1,
		MainJar:       "app.jar",
		Entry:         artifact.Entry{Mode: "jar"},
		CDS:           &artifact.CDS{Archive: "app.jsa"},
	}
	cfgBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgDigest := writeContentBlob(t, root, cfgBytes)

	man := artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		ArtifactType:  artifact.ArtifactType,
		Config:        artifact.Descriptor{MediaType: artifact.ConfigMediaType, Digest: cfgDigest, Size: int64(len(cfgBytes))},
		Layers: []artifact.Descriptor{
			{MediaType: artifact.JarLayerMediaType, Digest: jarDigest},
			{MediaType: artifact.ClasspathLayerMediaType, Digest: depsDigest},
			{MediaType: artifact.ModulepathLayerMediaType, Digest: modsDigest},
			{MediaType: artifact.CDSLayerMediaType, Digest: jsaDigest},
		},
	}
	manBytes, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	return root, writeContentBlob(t, root, manBytes)
}

// TestContentStoreBlobsRejectsTraversalDescriptorDigests is the regression test
// for the reported container-escape-equivalent host read: a tenant-published
// manifest whose layer descriptors carry traversal digests must fail resolution
// outright, and must never yield a host path for the shim to bind-mount.
func TestContentStoreBlobsRejectsTraversalDescriptorDigests(t *testing.T) {
	root, manifestDigest := writeNativeContentStore(t)
	baseline, err := contentStoreBlobs(root, manifestDigest)
	if err != nil {
		t.Fatalf("untampered artifact must resolve: %v", err)
	}
	if baseline.JarHostPath == "" {
		t.Fatal("baseline resolution produced no jar path")
	}

	// Every case must be rejected *by digest validation*, not incidentally.
	// Before the fix some of these digests resolved to "/", and reads of "/"
	// fail on their own with EISDIR — so asserting only "an error occurred"
	// would pass against the vulnerable code. Requiring the error to name the
	// invalid digest is what makes these genuine regression tests.
	cases := []struct {
		name    string
		mutate  func(*artifact.Manifest)
		wantErr string
	}{
		{"jar layer", func(m *artifact.Manifest) { m.Layers[0].Digest = traversalDigest }, "invalid digest"},
		{"classpath layer", func(m *artifact.Manifest) { m.Layers[1].Digest = traversalDigest }, "invalid digest"},
		{"modulepath layer", func(m *artifact.Manifest) { m.Layers[2].Digest = traversalDigest }, "invalid digest"},
		{"cds layer", func(m *artifact.Manifest) { m.Layers[3].Digest = traversalDigest }, "invalid digest"},
		{"config blob", func(m *artifact.Manifest) { m.Config.Digest = traversalDigest; m.Config.Size = 0 }, "invalid digest"},
		{"uppercase jar digest", func(m *artifact.Manifest) {
			m.Layers[0].Digest = strings.ToUpper(m.Layers[0].Digest[len("sha256:"):])
			m.Layers[0].Digest = "sha256:" + m.Layers[0].Digest
		}, "invalid digest"},
		{"truncated jar digest", func(m *artifact.Manifest) {
			m.Layers[0].Digest = m.Layers[0].Digest[:len(m.Layers[0].Digest)-1]
		}, "invalid digest"},
		{"unsupported algorithm", func(m *artifact.Manifest) {
			m.Layers[0].Digest = "sha512:" + strings.Repeat("a", 128)
		}, "invalid digest"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tampered := tamperManifest(t, root, manifestDigest, tc.mutate)
			blobs, err := contentStoreBlobs(root, tampered)
			if err == nil {
				t.Fatalf("resolution succeeded for a hostile %s digest: %+v", tc.name, blobs)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q (rejection must come from digest validation, not an incidental read failure)", err, tc.wantErr)
			}
			assertNoHostPaths(t, blobs)
		})
	}
}

// TestContentStoreBlobsRejectsMismatchedLayerContent covers the second half of
// the remediation: a canonical digest that does not describe the bytes on disk
// must not be mounted either.
func TestContentStoreBlobsRejectsMismatchedLayerContent(t *testing.T) {
	root, manifestDigest := writeNativeContentStore(t)

	// Point the JAR layer at a well-formed digest whose blob holds other bytes.
	otherDigest := writeContentBlob(t, root, []byte("unrelated content"))
	tampered := tamperManifest(t, root, manifestDigest, func(m *artifact.Manifest) {
		m.Layers[0].Digest = otherDigest
	})
	jarPath, err := contentBlobPath(root, otherDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jarPath, []byte("swapped payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	blobs, err := contentStoreBlobs(root, tampered)
	if err == nil {
		t.Fatalf("resolution succeeded for a layer whose bytes do not match its digest: %+v", blobs)
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("error = %v, want a digest mismatch", err)
	}
	assertNoHostPaths(t, blobs)
}

// TestRunnableImageRejectsTraversalLayerDigest exercises the Kubernetes path:
// runnable-image layers are staged rather than mounted directly, so a hostile
// descriptor must be refused before any host file is read into the stage tree.
func TestRunnableImageRejectsTraversalLayerDigest(t *testing.T) {
	root, ref, _ := buildRunnableStore(t)
	stage := t.TempDir()
	t.Setenv("BREWLET_RUNNABLE_STAGE", stage)

	_, manifestDigest, err := (artifact.Store{Root: root}).ResolveManifestByRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	tampered := tamperManifest(t, root, manifestDigest, func(m *artifact.Manifest) {
		for i := range m.Layers {
			if m.Layers[i].Annotations[artifact.LayerRoleAnnotation] == artifact.LayerRoleApp {
				m.Layers[i].Digest = traversalDigest
			}
		}
	})

	blobs, err := loadArtifactBlobs(imageConfig{Backend: "containerd", ContentRoot: root, ManifestDigest: tampered})
	if err == nil {
		t.Fatalf("runnable image resolved with a traversal app-layer digest: %+v", blobs)
	}
	// The pre-fix code read this layer with os.ReadFile, which would fail on
	// "/" by itself, so the error must specifically name the invalid digest for
	// this to be a real regression test.
	if !strings.Contains(err.Error(), "invalid digest") {
		t.Errorf("error = %v, want it to contain %q (rejection must come from digest validation, not an incidental read failure)", err, "invalid digest")
	}
	assertNoHostPaths(t, blobs)

	entries, err := os.ReadDir(filepath.Join(stage))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		app := filepath.Join(stage, e.Name(), "app")
		if _, err := os.Stat(app); err == nil {
			t.Errorf("hostile layer was staged into %s", app)
		}
	}
}

// TestContentBlobPathRefusesHostileDigests asserts the shim's own digest-to-path
// mapping never returns a path outside the content store.
func TestContentBlobPathRefusesHostileDigests(t *testing.T) {
	root := t.TempDir()
	for _, digest := range []string{
		traversalDigest,
		"sha256:..",
		"sha256:/",
		"sha256:",
		"",
		"not-a-digest",
		"sha256:" + strings.Repeat("a", 63),
	} {
		got, err := contentBlobPath(root, digest)
		if err == nil {
			t.Errorf("contentBlobPath(%q) = %q, want an error", digest, got)
		}
		if got != "" {
			t.Errorf("contentBlobPath(%q) returned path %q alongside an error", digest, got)
		}
	}
}

// assertNoHostPaths fails when a rejected resolution still handed back a path
// the caller could mount.
func assertNoHostPaths(t *testing.T, blobs artifactBlobs) {
	t.Helper()
	if blobs.JarHostPath != "" {
		t.Errorf("JarHostPath = %q after rejection, want empty", blobs.JarHostPath)
	}
	if blobs.CDSHostPath != "" {
		t.Errorf("CDSHostPath = %q after rejection, want empty", blobs.CDSHostPath)
	}
	if len(blobs.ClasspathHostPaths) != 0 {
		t.Errorf("ClasspathHostPaths = %v after rejection, want none", blobs.ClasspathHostPaths)
	}
	if len(blobs.ModulepathHostPaths) != 0 {
		t.Errorf("ModulepathHostPaths = %v after rejection, want none", blobs.ModulepathHostPaths)
	}
}
