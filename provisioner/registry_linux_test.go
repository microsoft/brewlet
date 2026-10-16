//go:build linux

package provisioner

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
)

type registryTestTransport func(*http.Request) (*http.Response, error)

func (f registryTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRegistryOriginTransportNeverForwardsCredentialBodiesOrHeaders(t *testing.T) {
	sent := 0
	transport := registryOriginTransport{
		authority: "registry.test:5443",
		base: registryTestTransport(func(r *http.Request) (*http.Response, error) {
			sent++
			if r.Header.Get("Authorization") != "" && r.URL.String() != "https://registry.test:5443/token" {
				t.Error("authorization crossed HTTPS origin")
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
		}),
	}
	for _, destination := range []string{
		"https://token.test:5443/token", "https://registry.test:5444/token",
		"http://registry.test:5443/token",
	} {
		r, err := http.NewRequest(http.MethodPost, destination,
			strings.NewReader(url.Values{"password": {"private-test-value"}}.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		r.SetBasicAuth("reader", "private-test-value")
		if _, err := transport.RoundTrip(r); err == nil {
			t.Fatal("credential POST permitted outside exact HTTPS origin")
		}
		r.Method = http.MethodGet
		if _, err := transport.RoundTrip(r); err == nil {
			t.Fatal("cross-origin credential body permitted on GET")
		}
		r.Body = nil
		if _, err := transport.RoundTrip(r); err != nil {
			t.Fatal(err)
		}
		if r.Header.Get("Authorization") == "" {
			t.Fatal("caller request was mutated instead of cloned")
		}
	}
	if sent != 3 {
		t.Fatal("cross-origin credential body reached transport")
	}
	r, _ := http.NewRequest(http.MethodPost, "https://registry.test:5443/token", nil)
	r.SetBasicAuth("reader", "private-test-value")
	if _, err := transport.RoundTrip(r); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryResolverPrivateCARelativeHostMappingAndAuth(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1},"layers":[]}`)
	target := digest.FromBytes(manifest)
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		u, p, ok := r.BasicAuth()
		if !ok || u != "reader" || p != "private-test-value" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Header().Set("Docker-Content-Digest", target.String())
		w.Header().Set("Content-Length", fmt.Sprint(len(manifest)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(manifest)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	host := strings.TrimPrefix(server.URL, "https://")
	dir := filepath.Join(root, host)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("hosts.toml", []byte(fmt.Sprintf("server = %q\n[host.%q]\ncapabilities = [\"pull\", \"resolve\"]\nca = \"ca.crt\"\n", server.URL, server.URL)))
	write("ca.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	write("credentials.json", []byte(`{"username":"reader","password":"private-test-value"}`))
	resolver, err := registryResolver(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := resolver.Resolve(context.Background(), host+"/runtime@"+target.String())
	if err != nil || got.Digest != target || requests < 1 {
		t.Fatalf("verified relative-CA/authenticated resolve failed: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "credentials.json")); err != nil {
		t.Fatal(err)
	}
	resolver, err = registryResolver(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.Resolve(context.Background(), host+"/runtime@"+target.String()); err == nil {
		t.Fatal("registry allowed missing credentials")
	}
	if err := os.Remove(filepath.Join(dir, "ca.crt")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolver.Resolve(context.Background(), host+"/runtime@"+target.String()); err == nil {
		t.Fatal("registry allowed untrusted TLS")
	}
}

func TestRegistryCredentialsAuthorityAndPermissions(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "registry.test:5443")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "credentials.json")
	write := func(data string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(name, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(name, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"username":"reader","password":"private-test-value"}`, 0600)
	creds := registryCredentials(root)
	user, password, err := creds("registry.test:5443")
	if err != nil || user != "reader" || password != "private-test-value" {
		t.Fatalf("origin credentials unavailable: %v", err)
	}
	for _, other := range []string{"registry.test", "registry.test:5444", "token.test:5443"} {
		u, p, err := creds(other)
		if err != nil || u != "" || p != "" {
			t.Fatalf("credentials crossed origin: %s", other)
		}
	}
	for _, bad := range []string{"../registry.test:5443", "user@registry.test:5443", ""} {
		if _, _, err := creds(bad); err == nil {
			t.Fatalf("accepted authority %q", bad)
		}
	}
	for _, data := range []string{
		`{"username":"reader","password":"private-test-value","extra":true}`,
		`{"username":"reader","password":"private-test-value"} {}`,
		`{"username":"reader"}`,
		`private-test-value`,
	} {
		write(data, 0600)
		if _, _, err := creds("registry.test:5443"); err == nil || strings.Contains(err.Error(), "private-test-value") {
			t.Fatal("invalid credentials accepted or disclosed")
		}
	}
	write(`{"username":"reader","password":"private-test-value"}`, 0644)
	if _, _, err := creds("registry.test:5443"); err == nil {
		t.Fatal("world-readable credentials accepted")
	}
	if err := os.Rename(name, name+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name+".real", name); err != nil {
		t.Fatal(err)
	}
	if _, _, err := creds("registry.test:5443"); err == nil {
		t.Fatal("symlink credentials accepted")
	}
}

func TestRegistryCredentialsDefaultNeverAuthorizesOtherHosts(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "_default")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"),
		[]byte(`{"username":"reader","password":"private-test-value"}`), 0600); err != nil {
		t.Fatal(err)
	}
	u, p, err := registryCredentials(root)("unrelated.test:5443")
	if err != nil || u != "" || p != "" {
		t.Fatal("default credentials leaked to unrelated authority")
	}
	if _, err := registryResolver(context.Background(), "relative"); err == nil {
		t.Fatal("relative trust directory accepted")
	}
}
