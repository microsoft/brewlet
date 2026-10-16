package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/brewlet/pkg/artifact"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestInspectLayerSetExposesOnlyVerifiedNeutralLock(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".test-inspect-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	store := artifact.Store{Dir: filepath.Join(dir, "layout")}
	metadata := []byte(`{"sourceBom":"explicit-test-coordinate","artifacts":[]}`)
	filePath := "/app/.brewlet/maven-lock.json"
	hash := digest.FromBytes(metadata).Encoded()
	var raw, compressed bytes.Buffer
	tw := tar.NewWriter(&raw)
	check(tw.WriteHeader(&tar.Header{Name: filePath[1:], Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(metadata))}))
	_, err = tw.Write(metadata)
	check(err)
	check(tw.Close())
	gz := gzip.NewWriter(&compressed)
	_, err = gz.Write(raw.Bytes())
	check(err)
	check(gz.Close())
	layer, err := store.Put(v1.MediaTypeImageLayerGzip, bytes.NewReader(compressed.Bytes()))
	check(err)
	layer.Annotations = map[string]string{"brewlet.sh/layer": "dependency"}
	diff := digest.FromBytes(raw.Bytes())
	config, err := store.PutJSON(artifact.MediaTypeLayerSetConfig, artifact.LayerSetConfig{SchemaVersion: 1, Name: "explicit", Version: "1", Ecosystem: "java", Layers: []artifact.LayerBinding{{Digest: layer.Digest, DiffID: diff}}})
	check(err)
	lock := artifact.LayerSetLock{SchemaVersion: 1, Entries: []artifact.LockEntry{{Ecosystem: "java", Name: "graph", Version: "1", Path: filePath, SHA256: hash}}}
	lockDescriptor, err := store.PutJSON(artifact.MediaTypeLayerSetLock, lock)
	check(err)
	manifest := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifact.MediaTypeLayerSet, Config: config, Layers: []v1.Descriptor{layer, lockDescriptor}}
	root, err := store.PutJSON(v1.MediaTypeImageManifest, manifest)
	check(err)
	sbom, err := store.PutJSON(artifact.MediaTypeCycloneDX, map[string]any{
		"bomFormat": "CycloneDX", "specVersion": "1.5", "version": 1,
		"components": []any{map[string]any{
			"type": "library", "name": "graph", "version": "1", "purl": "pkg:generic/java/graph@1",
			"hashes":     []any{map[string]string{"alg": "SHA-256", "content": hash}},
			"properties": []any{map[string]string{"name": "brewlet:ecosystem", "value": "java"}, map[string]string{"name": "brewlet:path", "value": filePath}},
		}},
	})
	check(err)
	empty, err := store.Put(v1.MediaTypeEmptyJSON, bytes.NewReader([]byte("{}")))
	check(err)
	ref, err := store.PutJSON(v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifact.MediaTypeCycloneDX, Subject: &root, Config: empty, Layers: []v1.Descriptor{sbom}})
	check(err)
	ref.ArtifactType = artifact.MediaTypeCycloneDX
	check(store.RecordReferrer(root, ref))
	check(store.WriteIndex(root, "explicit"))
	inspect := func() (out []byte, err error) {
		reader, writer, e := os.Pipe()
		if e != nil {
			return nil, e
		}
		previous := os.Stdout
		os.Stdout = writer
		defer func() { os.Stdout = previous; writer.Close(); reader.Close() }()
		err = run(context.Background(), []string{"inspect", "explicit", "--store", store.Dir})
		writer.Close()
		os.Stdout = previous
		out, e = io.ReadAll(reader)
		if err == nil {
			err = e
		}
		return
	}
	out, err := inspect()
	check(err)
	var result struct {
		Digest   string                    `json:"digest"`
		Lock     artifact.LayerSetLock     `json:"lock"`
		Evidence artifact.LayerSetEvidence `json:"evidence"`
	}
	check(json.Unmarshal(out, &result))
	if result.Digest != root.Digest.String() || len(result.Lock.Entries) != 1 || result.Lock.Entries[0].Path != filePath || result.Lock.Entries[0].SHA256 != hash || result.Evidence.LockDigest != lockDescriptor.Digest {
		t.Fatalf("verified neutral lock missing from response: %s", out)
	}
	lock.Entries[0].SHA256 = digest.FromString("wrong graph bytes").Encoded()
	badLock, err := store.PutJSON(artifact.MediaTypeLayerSetLock, lock)
	check(err)
	manifest.Layers[1] = badLock
	badRoot, err := store.PutJSON(v1.MediaTypeImageManifest, manifest)
	check(err)
	check(store.WriteIndex(badRoot, "explicit"))
	out, err = inspect()
	if err == nil || len(out) != 0 {
		t.Fatalf("unverified lock exposed: err=%v output=%s", err, out)
	}
}
