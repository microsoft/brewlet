// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/microsoft/brewlet/internal/artifact"
	"github.com/microsoft/brewlet/internal/registry"
	"github.com/microsoft/brewlet/internal/registry/registrytest"
)

const localRef = "example.test/team/orders:1.0.0"

func buildImage(t *testing.T) (artifact.Store, artifact.Descriptor) {
	t.Helper()
	dir := t.TempDir()
	jar := filepath.Join(dir, "orders.jar")
	if err := os.WriteFile(jar, []byte("PK\x03\x04 orders-fat-jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := artifact.Store{Root: filepath.Join(dir, "oci")}
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "orders.jar", Entry: artifact.Entry{Mode: "jar"}}
	desc, err := s.PushRunnableImage(localRef, cfg, jar, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return s, desc
}

func push(t *testing.T, reg *registrytest.Registry, cred *registry.Credential, policy registry.TrustPolicy, s artifact.Store) (registry.PushResult, error) {
	t.Helper()
	c, err := registry.NewClient(reg.Host(), "team/orders", cred, policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	return registry.PushLayout(context.Background(), s, localRef, c, "1.0.0", registry.PushOptions{})
}

func noPolicy(t *testing.T) registry.TrustPolicy {
	p, err := registry.NewTrustPolicy(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPushLayoutAnonymousImageIndex(t *testing.T) {
	reg := registrytest.New(registrytest.Anonymous)
	defer reg.Close()
	s, desc := buildImage(t)

	prog := &registry.Progress{}
	c, err := registry.NewClient(reg.Host(), "team/orders", nil, noPolicy(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := registry.PushLayout(context.Background(), s, localRef, c, "1.0.0", registry.PushOptions{Progress: prog})
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != desc.Digest || res.MediaType != artifact.OCIImageIndexMediaType {
		t.Fatalf("result = %+v, want root %s", res, desc.Digest)
	}
	if res.BlobsUploaded == 0 || res.BlobsSkipped != 0 || reg.Uploads() != int64(res.BlobsUploaded) {
		t.Fatalf("uploads: result=%+v registry=%d", res, reg.Uploads())
	}
	if prog.UploadedBytes.Load() != prog.TotalBytes.Load() || prog.TotalBytes.Load() == 0 {
		t.Fatalf("progress %d/%d", prog.UploadedBytes.Load(), prog.TotalBytes.Load())
	}

	// Children by digest first, then the tagged root index.
	puts := reg.ManifestPuts()
	if len(puts) < 2 || puts[len(puts)-1] != "1.0.0" {
		t.Fatalf("manifest put order = %v", puts)
	}
	for _, p := range puts[:len(puts)-1] {
		if !strings.HasPrefix(p, "sha256:") {
			t.Fatalf("child manifest pushed by tag: %v", puts)
		}
	}
	root, ok := reg.Manifest("team/orders", "1.0.0")
	if !ok || root.MediaType != artifact.OCIImageIndexMediaType || registrytest.Digest(root.Body) != desc.Digest {
		t.Fatalf("root manifest = %+v", root.MediaType)
	}
	var idx artifact.Index
	if err := json.Unmarshal(root.Body, &idx); err != nil {
		t.Fatal(err)
	}
	for _, m := range idx.Manifests {
		got, ok := reg.Manifest("team/orders", m.Digest)
		if !ok || got.MediaType != m.MediaType {
			t.Fatalf("child %s missing or wrong media type %q", m.Digest, got.MediaType)
		}
		var man artifact.Manifest
		if err := json.Unmarshal(got.Body, &man); err != nil {
			t.Fatal(err)
		}
		for _, l := range append([]artifact.Descriptor{man.Config}, man.Layers...) {
			if _, ok := reg.Blob(l.Digest); !ok {
				t.Fatalf("blob %s not uploaded", l.Digest)
			}
		}
	}

	// A second push skips every blob.
	res2, err := push(t, reg, nil, noPolicy(t), s)
	if err != nil {
		t.Fatal(err)
	}
	if res2.BlobsUploaded != 0 || res2.BlobsSkipped != res.BlobsUploaded || res2.Digest != res.Digest {
		t.Fatalf("re-push = %+v", res2)
	}
}

func TestPushLayoutArtifactManifest(t *testing.T) {
	reg := registrytest.New(registrytest.Anonymous)
	defer reg.Close()
	dir := t.TempDir()
	jar := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jar, []byte("PK\x03\x04 app"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := artifact.Store{Root: filepath.Join(dir, "oci")}
	desc, err := s.Push(localRef, artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}, jar)
	if err != nil {
		t.Fatal(err)
	}
	res, err := push(t, reg, nil, noPolicy(t), s)
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest != desc.Digest || len(reg.ManifestPuts()) != 1 {
		t.Fatalf("result=%+v puts=%v", res, reg.ManifestPuts())
	}
	m, _ := reg.Manifest("team/orders", "1.0.0")
	if m.MediaType != desc.MediaType {
		t.Fatalf("media type %q, want %q", m.MediaType, desc.MediaType)
	}
}

func TestPushBasicAuth(t *testing.T) {
	reg := registrytest.New(registrytest.Basic)
	reg.Username, reg.Password = "alice", "s3cret"
	defer reg.Close()
	s, _ := buildImage(t)

	if _, err := push(t, reg, nil, noPolicy(t), s); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("anonymous push to Basic registry: err = %v", err)
	}
	if _, err := push(t, reg, &registry.Credential{Username: "alice", Secret: "s3cret"}, noPolicy(t), s); err != nil {
		t.Fatal(err)
	}
}

func TestPushBearerSameOriginWithPassword(t *testing.T) {
	reg := registrytest.New(registrytest.Bearer)
	reg.Username, reg.Password = "alice", "s3cret"
	reg.SameOriginRealm = true
	defer reg.Close()
	s, _ := buildImage(t)

	if _, err := push(t, reg, &registry.Credential{Username: "alice", Secret: "s3cret"}, noPolicy(t), s); err != nil {
		t.Fatal(err)
	}
	toks := reg.TokenRequests()
	if len(toks) != 1 || toks[0].Method != http.MethodGet || toks[0].Form.Get("service") != "fake-registry" ||
		toks[0].Form.Get("scope") != "repository:team/orders:pull,push" {
		t.Fatalf("token requests = %+v", toks)
	}
}

func TestPushIdentityTokenUsesRefreshGrant(t *testing.T) {
	reg := registrytest.New(registrytest.Bearer)
	reg.RefreshToken = "refresh-123"
	reg.SameOriginRealm = true
	defer reg.Close()
	s, _ := buildImage(t)

	cred := &registry.Credential{Username: registry.IdentityTokenUsername, Secret: "refresh-123", IdentityToken: true}
	if _, err := push(t, reg, cred, noPolicy(t), s); err != nil {
		t.Fatal(err)
	}
	toks := reg.TokenRequests()
	if len(toks) != 1 || toks[0].Method != http.MethodPost || toks[0].Form.Get("grant_type") != "refresh_token" ||
		toks[0].Authorization != "" || toks[0].ContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("token requests = %+v", toks)
	}
	for _, r := range reg.Requests() {
		if strings.HasPrefix(r.Authorization, "Basic ") {
			t.Fatalf("identity token sent as Basic on %s %s", r.Method, r.Path)
		}
	}
}

func TestPushCrossOriginRealmWithholdsCredentials(t *testing.T) {
	reg := registrytest.New(registrytest.Bearer)
	reg.Username, reg.Password = "alice", "s3cret"
	defer reg.Close()
	s, _ := buildImage(t)
	cred := &registry.Credential{Username: "alice", Secret: "s3cret"}

	_, err := push(t, reg, cred, noPolicy(t), s)
	if err == nil || !strings.Contains(err.Error(), "--allowed-token-realm "+reg.TokenHost()) {
		t.Fatalf("err = %v", err)
	}
	if n := len(reg.TokenRequests()); n != 0 {
		t.Fatalf("token service contacted %d time(s) with untrusted realm", n)
	}

	policy, err := registry.NewTrustPolicy(nil, []string{reg.TokenHost()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := push(t, reg, cred, policy, s); err != nil {
		t.Fatalf("allowed realm: %v", err)
	}
}

func TestPushAnonymousBearerCrossOrigin(t *testing.T) {
	// Without credentials there is nothing to leak, so any valid realm is used.
	reg := registrytest.New(registrytest.Bearer)
	defer reg.Close()
	s, _ := buildImage(t)
	if _, err := push(t, reg, nil, noPolicy(t), s); err != nil {
		t.Fatal(err)
	}
	if toks := reg.TokenRequests(); len(toks) != 1 || toks[0].Authorization != "" {
		t.Fatalf("token requests = %+v", toks)
	}
}

func TestPushForeignUploadLocationGetsNoAuth(t *testing.T) {
	reg := registrytest.New(registrytest.Basic)
	reg.Username, reg.Password = "alice", "s3cret"
	defer reg.Close()
	var mu sync.Mutex
	var seen []registrytest.Request
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, registrytest.Request{Method: r.Method, Authorization: r.Header.Get("Authorization")})
		mu.Unlock()
		reg.FinishUpload(w, r)
	}))
	defer storage.Close()
	reg.UploadLocation = storage.URL + "/upload/abc?sig=xyz"
	s, _ := buildImage(t)

	res, err := push(t, reg, &registry.Credential{Username: "alice", Secret: "s3cret"}, noPolicy(t), s)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != res.BlobsUploaded || len(seen) == 0 {
		t.Fatalf("storage saw %d uploads, want %d", len(seen), res.BlobsUploaded)
	}
	for _, r := range seen {
		if r.Authorization != "" {
			t.Fatalf("credentials forwarded to foreign upload location: %q", r.Authorization)
		}
	}
}

func TestPushMissingRef(t *testing.T) {
	reg := registrytest.New(registrytest.Anonymous)
	defer reg.Close()
	s, _ := buildImage(t)
	c, _ := registry.NewClient(reg.Host(), "team/orders", nil, noPolicy(t), nil)
	if _, err := registry.PushLayout(context.Background(), s, "example.test/other:1", c, "1", registry.PushOptions{}); err == nil {
		t.Fatal("want error for unknown local ref")
	}
}
