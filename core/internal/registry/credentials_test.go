// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type helperCall struct{ helper, server string }

func resolverFor(t *testing.T, config string, env map[string]string, helpers map[string]string) (CredentialResolver, *[]helperCall, *[]string) {
	t.Helper()
	dir := t.TempDir()
	if config != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	calls := &[]helperCall{}
	diags := &[]string{}
	return CredentialResolver{
		Getenv: func(k string) string {
			if k == "DOCKER_CONFIG" {
				return dir
			}
			return env[k]
		},
		HomeDir: func() (string, error) { return "", errors.New("no home") },
		RunHelper: func(helper, server string) (string, error) {
			*calls = append(*calls, helperCall{helper, server})
			out, ok := helpers[helper]
			if !ok {
				return "", errors.New("not found")
			}
			return out, nil
		},
		Diagnostics: func(s string) { *diags = append(*diags, s) },
	}, calls, diags
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestResolveOrder(t *testing.T) {
	env := map[string]string{"BREWLET_REGISTRY_USERNAME": "envuser", "BREWLET_REGISTRY_PASSWORD": "envpass"}

	// credHelpers wins over auths, credsStore and env.
	r, calls, _ := resolverFor(t, `{"credHelpers":{"myacr.azurecr.io":"acr"},"auths":{"myacr.azurecr.io":{"auth":"`+b64("a:b")+`"}},"credsStore":"desktop"}`,
		env, map[string]string{"acr": `{"Username":"helperuser","Secret":"helpersecret"}`})
	c := r.Resolve("myacr.azurecr.io")
	if c == nil || c.Username != "helperuser" || c.Secret != "helpersecret" || c.Source != "docker-credential-acr" {
		t.Fatalf("credHelpers: %+v", c)
	}
	if len(*calls) != 1 || (*calls)[0].server != "myacr.azurecr.io" {
		t.Fatalf("helper calls: %+v", *calls)
	}

	// A matching credHelper with no credentials skips the rest of the Docker
	// config (as the Docker CLI does) and falls back to the env vars.
	r, _, _ = resolverFor(t, `{"credHelpers":{"myacr.azurecr.io":"acr"},"auths":{"myacr.azurecr.io":{"auth":"`+b64("a:b")+`"}}}`, env, nil)
	if c := r.Resolve("myacr.azurecr.io"); c == nil || c.Username != "envuser" {
		t.Fatalf("credHelper miss should skip auths: %+v", c)
	}

	// auths identitytoken beats auth.
	r, _, _ = resolverFor(t, `{"auths":{"https://myacr.azurecr.io":{"auth":"`+b64("a:b")+`","identitytoken":"refresh"}}}`, env, nil)
	c = r.Resolve("myacr.azurecr.io")
	if c == nil || !c.IdentityToken || c.Secret != "refresh" || c.Username != IdentityTokenUsername {
		t.Fatalf("identitytoken: %+v", c)
	}

	// auths basic.
	r, _, _ = resolverFor(t, `{"auths":{"myacr.azurecr.io":{"auth":"`+b64("user:pa:ss")+`"}},"credsStore":"desktop"}`, env, nil)
	c = r.Resolve("myacr.azurecr.io")
	if c == nil || c.Username != "user" || c.Secret != "pa:ss" || c.IdentityToken {
		t.Fatalf("auths: %+v", c)
	}

	// credsStore when no auths entry.
	r, calls, _ = resolverFor(t, `{"credsStore":"desktop"}`, env, map[string]string{"desktop": `{"Username":"<token>","Secret":"tok"}`})
	c = r.Resolve("myacr.azurecr.io")
	if c == nil || !c.IdentityToken || c.Secret != "tok" {
		t.Fatalf("credsStore: %+v", c)
	}

	// env fallback.
	r, _, _ = resolverFor(t, `{}`, env, nil)
	c = r.Resolve("myacr.azurecr.io")
	if c == nil || c.Username != "envuser" || c.Secret != "envpass" {
		t.Fatalf("env: %+v", c)
	}

	// anonymous.
	r, _, _ = resolverFor(t, "", nil, nil)
	if c := r.Resolve("myacr.azurecr.io"); c != nil {
		t.Fatalf("anonymous: %+v", c)
	}
}

func TestResolveExactAuthority(t *testing.T) {
	cfg := `{"auths":{"myacr.azurecr.io":{"auth":"` + b64("a:b") + `"},"localhost:5000":{"auth":"` + b64("c:d") + `"}}}`
	r, _, _ := resolverFor(t, cfg, nil, nil)
	for _, host := range []string{"evilmyacr.azurecr.io", "myacr.azurecr.io.evil.example", "myacr.azurecr.io:8443", "localhost:5001", "localhost"} {
		if c := r.Resolve(host); c != nil {
			t.Errorf("Resolve(%q) leaked %+v", host, c)
		}
	}
	if c := r.Resolve("LOCALHOST:5000"); c == nil || c.Username != "c" {
		t.Errorf("case-insensitive match: %+v", c)
	}
}

func TestResolveDockerHub(t *testing.T) {
	r, calls, _ := resolverFor(t, `{"credsStore":"desktop","auths":{"https://index.docker.io/v1/":{"auth":"`+b64("hub:pw")+`"}}}`, nil, nil)
	c := r.Resolve(DockerHubRegistry)
	if c == nil || c.Username != "hub" {
		t.Fatalf("docker hub auths: %+v", c)
	}
	r, calls, _ = resolverFor(t, `{"credsStore":"desktop"}`, nil, map[string]string{"desktop": `{"Username":"hub","Secret":"pw"}`})
	if c := r.Resolve(DockerHubRegistry); c == nil || c.Username != "hub" {
		t.Fatalf("docker hub credsStore: %+v", c)
	}
	if len(*calls) != 1 || (*calls)[0].server != "https://index.docker.io/v1/" {
		t.Fatalf("docker hub helper server URL: %+v", *calls)
	}
}

func TestResolveDiagnostics(t *testing.T) {
	r, calls, diags := resolverFor(t, `{"credHelpers":{"myacr.azurecr.io":"../evil"}}`, nil, nil)
	if c := r.Resolve("myacr.azurecr.io"); c != nil {
		t.Fatalf("invalid helper name: %+v", c)
	}
	if len(*calls) != 0 || len(*diags) != 1 || !strings.Contains((*diags)[0], "invalid name") {
		t.Fatalf("calls=%v diags=%v", *calls, *diags)
	}

	r, _, diags = resolverFor(t, `{"auths":{"myacr.azurecr.io":{"auth":"`+b64("nocolon")+`"}}}`, nil, nil)
	if c := r.Resolve("myacr.azurecr.io"); c != nil {
		t.Fatalf("malformed auth: %+v", c)
	}
	if len(*diags) != 1 || strings.Contains((*diags)[0], "nocolon") {
		t.Fatalf("diags: %v", *diags)
	}

	r, _, diags = resolverFor(t, `{not json`, nil, nil)
	if c := r.Resolve("myacr.azurecr.io"); c != nil || len(*diags) != 1 {
		t.Fatalf("malformed config: %+v %v", c, *diags)
	}
}

func TestCredentialStringMasksSecret(t *testing.T) {
	c := Credential{Username: "u", Secret: "supersecret", Source: "x"}
	if strings.Contains(c.String(), "supersecret") {
		t.Fatalf("String leaked secret: %s", c.String())
	}
}
