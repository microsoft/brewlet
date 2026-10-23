package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/microsoft/brewlet/pkg/artifact"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestPushPreservesExistingOCIOverAuthenticatedTLS(t *testing.T) {
	dir, e := os.MkdirTemp(".", ".test-push-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	check := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	store := artifact.Store{Dir: filepath.Join(dir, "layout")}
	var tarBytes, compressed bytes.Buffer
	tw := tar.NewWriter(&tarBytes)
	check(tw.WriteHeader(&tar.Header{Name: "usr/bin/runner", Mode: 0755, Typeflag: tar.TypeReg, Size: 6}))
	_, e = tw.Write([]byte("runner"))
	check(e)
	check(tw.Close())
	gz := gzip.NewWriter(&compressed)
	_, e = gz.Write(tarBytes.Bytes())
	check(e)
	check(gz.Close())
	layer, e := store.Put(v1.MediaTypeImageLayerGzip, bytes.NewReader(compressed.Bytes()))
	check(e)
	index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex}
	for _, arch := range []string{"amd64", "arm64"} {
		platform := v1.Platform{OS: "linux", Architecture: arch}
		image := v1.Image{Platform: platform, Config: v1.ImageConfig{User: "0", Env: []string{"PATH=/usr/bin"}}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(tarBytes.Bytes())}}}
		config, e := store.PutJSON(v1.MediaTypeImageConfig, image)
		check(e)
		manifest, e := store.PutJSON(v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{layer}})
		check(e)
		manifest.Platform = &platform
		index.Manifests = append(index.Manifests, manifest)
	}
	target, e := store.PutJSON(v1.MediaTypeImageIndex, index)
	check(e)
	check(store.WriteIndex(target, "golden-source"))
	layoutBefore, e := os.ReadFile(filepath.Join(store.Dir, "index.json"))
	check(e)
	type object struct {
		media string
		body  []byte
	}
	objects := map[string]object{}
	var mu sync.Mutex
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		username, password, ok := r.BasicAuth()
		if !ok || username != "test-builder" || password != "test-only-password" {
			w.WriteHeader(401)
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/v2/team/golden/")
		if key == "blobs/uploads/" && r.Method == "POST" {
			w.Header().Set("Location", "/v2/team/golden/blobs/uploads/id")
			w.WriteHeader(202)
			return
		}
		if key == "blobs/uploads/id" && r.Method == "PUT" {
			b, _ := io.ReadAll(r.Body)
			hash := r.URL.Query().Get("digest")
			if digest.FromBytes(b).String() != hash {
				w.WriteHeader(400)
				return
			}
			objects["blobs/"+hash] = object{"application/octet-stream", b}
			w.WriteHeader(201)
			return
		}
		if strings.HasPrefix(key, "manifests/") && r.Method == "PUT" {
			b, _ := io.ReadAll(r.Body)
			value := object{r.Header.Get("Content-Type"), b}
			objects[key] = value
			objects["manifests/"+digest.FromBytes(b).String()] = value
			w.WriteHeader(201)
			return
		}
		if value, ok := objects[key]; ok {
			w.Header().Set("Content-Type", value.media)
			if r.Method != "HEAD" {
				w.Write(value.body)
			}
			return
		}
		w.WriteHeader(404)
	}))
	defer server.Close()
	ca := filepath.Join(dir, "ca.pem")
	check(os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600))
	t.Setenv("SSL_CERT_FILE", ca)
	t.Setenv("BREWLET_REGISTRY_USERNAME", "test-builder")
	t.Setenv("BREWLET_REGISTRY_PASSWORD", "test-only-password")
	reference := strings.TrimPrefix(server.URL, "https://") + "/team/golden:v1"
	resultFile := filepath.Join(dir, "result.json")
	args := []string{"push", reference, "--store", store.Dir, "--source", target.Digest.String(), "--registry-scheme", "https", "--push-result", resultFile}
	check(run(context.Background(), args))
	var result struct {
		Digest string `json:"digest"`
	}
	b, e := os.ReadFile(resultFile)
	check(e)
	check(json.Unmarshal(b, &result))
	if result.Digest != target.Digest.String() {
		t.Fatal("push changed the selected target")
	}
	layoutAfter, e := os.ReadFile(filepath.Join(store.Dir, "index.json"))
	check(e)
	if !bytes.Equal(layoutBefore, layoutAfter) {
		t.Fatal("remote push rewrote source layout")
	}
	rootBytes, e := store.Read(target)
	check(e)
	mu.Lock()
	published := objects["manifests/v1"]
	mu.Unlock()
	if published.media != v1.MediaTypeImageIndex || !bytes.Equal(published.body, rootBytes) {
		t.Fatal("published root bytes changed")
	}
	for _, descriptor := range index.Manifests {
		raw, e := store.Read(descriptor)
		check(e)
		mu.Lock()
		published := objects["manifests/"+descriptor.Digest.String()]
		mu.Unlock()
		if !bytes.Equal(raw, published.body) {
			t.Fatal("platform manifest changed")
		}
		var manifest v1.Manifest
		check(json.Unmarshal(raw, &manifest))
		for _, blob := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
			reader, e := store.Open(blob)
			check(e)
			expected, e := io.ReadAll(reader)
			reader.Close()
			check(e)
			mu.Lock()
			published := objects["blobs/"+blob.Digest.String()]
			mu.Unlock()
			if !bytes.Equal(expected, published.body) {
				t.Fatal("published blob changed")
			}
		}
	}
	// Raw publication must also carry a malformed launch fixture unchanged. The
	// runtime validator, rather than this transport command, rejects that image.
	fixtureLayer := func(name string, data []byte, role string) (v1.Descriptor, digest.Digest) {
		var raw, packed bytes.Buffer
		writer := tar.NewWriter(&raw)
		check(writer.WriteHeader(&tar.Header{Name: name, Mode: 0755, Typeflag: tar.TypeReg, Size: int64(len(data))}))
		_, e := writer.Write(data)
		check(e)
		check(writer.Close())
		compressor := gzip.NewWriter(&packed)
		_, e = compressor.Write(raw.Bytes())
		check(e)
		check(compressor.Close())
		descriptor, e := store.Put(v1.MediaTypeImageLayerGzip, bytes.NewReader(packed.Bytes()))
		check(e)
		descriptor.Annotations = map[string]string{"brewlet.sh/layer": role}
		return descriptor, digest.FromBytes(raw.Bytes())
	}
	appLayer, appDiff := fixtureLayer("app/runner", []byte("runner"), "app")
	launchLayer, launchDiff := fixtureLayer("launch.json", []byte(`{"schemaVersion":1,"process":{"entrypoint":["runner"]}}`), "launch")
	platform := v1.Platform{OS: "linux", Architecture: "arm64"}
	fixtureConfig, e := store.PutJSON(v1.MediaTypeImageConfig, v1.Image{Platform: platform, Config: v1.ImageConfig{Entrypoint: []string{"runner"}, WorkingDir: "/app"}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{appDiff, launchDiff}}})
	check(e)
	fixture, e := store.PutJSON(v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: fixtureConfig, Layers: []v1.Descriptor{appLayer, launchLayer}, Annotations: map[string]string{"brewlet.sh/artifact-version": "2"}})
	check(e)
	if _, e = artifact.Resolve(context.Background(), store.Open, fixture, platform); artifact.Reason(e) != "InvalidLaunchConfig" {
		t.Fatalf("fixture does not target launch validation: %v", e)
	}
	fixtureRef := strings.TrimSuffix(reference, ":v1") + ":invalid-launch"
	check(run(context.Background(), []string{"push", fixtureRef, "--store", store.Dir, "--source", fixture.Digest.String(), "--registry-scheme", "https"}))
	expected, e := store.Read(fixture)
	check(e)
	mu.Lock()
	fixtureBytes := objects["manifests/invalid-launch"].body
	requestCount := requests
	mu.Unlock()
	if !bytes.Equal(expected, fixtureBytes) {
		t.Fatal("malformed fixture was rewritten or rejected")
	}
	// Identity failure must occur before any registry request or result update.
	blobPath := filepath.Join(store.Dir, "blobs", "sha256", layer.Digest.Encoded())
	check(os.WriteFile(blobPath, []byte("corrupt compressed layer"), 0600))
	if e = run(context.Background(), args); artifact.Reason(e) != "DigestMismatch" {
		t.Fatalf("corrupt graph publication allowed: %v", e)
	}
	mu.Lock()
	after := requests
	mu.Unlock()
	if after != requestCount {
		t.Fatal("corrupt source contacted registry before graph verification")
	}
}

func TestPushRejectsAmbiguousValidationOptions(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"push", "registry.example.test/golden:v1", "--store", "missing-layout"}, "does not exist"},
		{[]string{"push", "registry.example.test/golden:v1", "--store", "unused", "--platform", "linux/arm64"}, "run `brewlet build` first"},
		{[]string{"push", "registry.example.test/golden:v1", "--store", "unused", "--launch=launch.json"}, "run `brewlet build` first"},
		{[]string{"push", "registry.example.test/golden:v1", "--store", "unused", "--require-provenance"}, "not defined"},
		{[]string{"push", "registry.example.test/golden:v1", "--store", "unused", "--registry-scheme", "https", "--insecure"}, "conflicts"},
	} {
		if e := run(context.Background(), test.args); e == nil || !strings.Contains(e.Error(), test.want) {
			t.Fatalf("%v: got %v; want %s", test.args, e, test.want)
		}
	}
}
