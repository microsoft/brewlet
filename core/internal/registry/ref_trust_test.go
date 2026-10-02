// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"net/url"
	"strings"
	"testing"
)

func TestHasExplicitRegistry(t *testing.T) {
	cases := map[string]bool{
		"myacr.azurecr.io/app:1.0":     true,
		"localhost:5000/app":           true,
		"localhost/app":                true,
		"127.0.0.1:5000/team/app:v1":   true,
		"docker.io/me/app:1":           true,
		"app:1.0":                      false,
		"team/app:1.0":                 false,
		"demo/hello:1":                 false,
		"localhostx/app":               false,
		"":                             false,
		"/app":                         false,
		"registry.example.com":         false,
		"registry.example.com/team/a":  true,
		"ghcr.io/org/repo@sha256:abcd": true,
	}
	for ref, want := range cases {
		if got := HasExplicitRegistry(ref); got != want {
			t.Errorf("HasExplicitRegistry(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestParseReference(t *testing.T) {
	cases := []struct {
		in                         string
		registry, repo, tag, named string
	}{
		{"myacr.azurecr.io/team/app:1.4.2", "myacr.azurecr.io", "team/app", "1.4.2", "myacr.azurecr.io/team/app"},
		{"localhost:5000/app", "localhost:5000", "app", "latest", "localhost:5000/app"},
		{"docker.io/me/app:2", DockerHubRegistry, "me/app", "2", "docker.io/me/app"},
		{"index.docker.io/me/app:2", DockerHubRegistry, "me/app", "2", "docker.io/me/app"},
	}
	for _, c := range cases {
		r, err := ParseReference(c.in)
		if err != nil {
			t.Fatalf("ParseReference(%q): %v", c.in, err)
		}
		if r.Registry != c.registry || r.Repository != c.repo || r.Tag != c.tag || r.Name != c.named {
			t.Errorf("ParseReference(%q) = %+v", c.in, r)
		}
	}
	r, _ := ParseReference("myacr.azurecr.io/app:1")
	if got := r.String(); got != "myacr.azurecr.io/app:1" {
		t.Errorf("String() = %q", got)
	}
	if got := r.Pinned("sha256:abc"); got != "myacr.azurecr.io/app@sha256:abc" {
		t.Errorf("Pinned() = %q", got)
	}
}

func TestParseReferenceRejects(t *testing.T) {
	for in, want := range map[string]string{
		"app:1.0":      "no registry host",
		"team/app:1":   "no registry host",
		"myacr.io/App": "invalid image reference",
		"myacr.io/app@sha256:0123456789012345678901234567890123456789012345678901234567890123": "digest-pinned",
	} {
		_, err := ParseReference(in)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseReference(%q) err = %v, want containing %q", in, err, want)
		}
	}
}

func TestTrustPolicyPlaintext(t *testing.T) {
	p, err := NewTrustPolicy([]string{"registry.internal:5000", "plain.internal"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"localhost:5000":             true,
		"127.0.0.1:5000":             true,
		"[::1]:5000":                 true,
		"registry.internal:5000":     true,
		"REGISTRY.internal:5000":     true,
		"registry.internal:5001":     false,
		"localhost.attacker.example": false,
		"127.example.com":            false,
		"127.1":                      false,
		"myacr.azurecr.io":           false,
	} {
		if got := p.AllowsPlaintext(host); got != want {
			t.Errorf("AllowsPlaintext(%q) = %v, want %v", host, got, want)
		}
	}
	if p.Scheme("myacr.azurecr.io") != "https" || p.Scheme("localhost:5000") != "http" {
		t.Error("Scheme mismatch")
	}
}

func TestNewTrustPolicyRejectsNonAuthorities(t *testing.T) {
	for _, bad := range []string{"https://registry.internal", "registry.internal/path", "user@host", "*.example.com"} {
		if _, err := NewTrustPolicy([]string{bad}, nil); err == nil {
			t.Errorf("NewTrustPolicy insecure %q: want error", bad)
		}
		if _, err := NewTrustPolicy(nil, []string{bad}); err == nil {
			t.Errorf("NewTrustPolicy realm %q: want error", bad)
		}
	}
}

func TestValidateTokenRealm(t *testing.T) {
	p, _ := NewTrustPolicy(nil, nil)
	for _, ok := range []string{"https://auth.example.com/token", "http://127.0.0.1:9000/token", "http://localhost/token"} {
		if _, err := p.ValidateTokenRealm(ok); err != nil {
			t.Errorf("ValidateTokenRealm(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/token", "ftp://auth.example.com/token", "https://u:p@auth.example.com/token", "http://auth.example.com/token"} {
		if _, err := p.ValidateTokenRealm(bad); err == nil {
			t.Errorf("ValidateTokenRealm(%q): want error", bad)
		}
	}
}

func TestAllowsCredentials(t *testing.T) {
	mustURL := func(s string) *url.URL {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	p, _ := NewTrustPolicy(nil, []string{"login.example.com"})
	cases := []struct {
		registry, realm string
		want            bool
	}{
		{"https://myacr.azurecr.io", "https://myacr.azurecr.io/oauth2/token", true},
		{"https://myacr.azurecr.io", "https://myacr.azurecr.io:443/oauth2/token", true},
		{"https://myacr.azurecr.io", "https://evil.example/token", false},
		{"https://myacr.azurecr.io", "http://myacr.azurecr.io/token", false},
		{"https://myacr.azurecr.io", "https://login.example.com/token", true},
		{"https://registry-1.docker.io", "https://auth.docker.io/token", true},
		{"https://registry-1.docker.io", "https://auth.docker.io:8443/token", false},
		{"https://myacr.azurecr.io", "https://auth.docker.io/token", false},
		{"http://127.0.0.1:5000", "http://127.0.0.1:5001/token", false},
	}
	for _, c := range cases {
		if got := p.AllowsCredentials(mustURL(c.registry), mustURL(c.realm)); got != c.want {
			t.Errorf("AllowsCredentials(%s, %s) = %v, want %v", c.registry, c.realm, got, c.want)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.example.com/token",service="reg",scope="repository:a/b:pull,push"`)
	if scheme != "Bearer" || params["realm"] != "https://auth.example.com/token" || params["service"] != "reg" || params["scope"] != "repository:a/b:pull,push" {
		t.Fatalf("parseChallenge = %q %v", scheme, params)
	}
}
