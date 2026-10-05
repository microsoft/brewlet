// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// IdentityTokenUsername is the username Docker credential helpers return when
// the secret is an OAuth2 identity (refresh) token rather than a password.
const IdentityTokenUsername = "<token>"

const dockerHubConfigKey = "https://index.docker.io/v1/"

var (
	dockerHubHosts = map[string]bool{"registry-1.docker.io": true, "docker.io": true, "index.docker.io": true}
	helperName     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	// HelperTimeout bounds how long a docker-credential-<name> helper may run.
	HelperTimeout = 30 * time.Second
)

// Credential is a registry username/password pair or an OAuth2 identity
// token. The secret must never be logged.
type Credential struct {
	Username string
	Secret   string
	// IdentityToken marks Secret as an OAuth2 refresh token that is exchanged
	// with grant_type=refresh_token and never sent as Basic credentials.
	IdentityToken bool
	// Source names where the credential came from (never the secret).
	Source string
}

// String never reveals the secret.
func (c Credential) String() string {
	return fmt.Sprintf("Credential{username=%q, source=%s, ******}", c.Username, c.Source)
}

// HelperRunner runs `docker-credential-<helper> get` for serverURL and returns
// its stdout, or ("", nil) when the helper has no credentials for it.
type HelperRunner func(helper, serverURL string) (string, error)

// CredentialResolver resolves registry credentials the way the Maven plugin
// does (minus settings.xml), including Docker config and credential helpers.
type CredentialResolver struct {
	// Getenv reads environment variables; defaults to os.Getenv.
	Getenv func(string) string
	// HomeDir returns the user's home directory; defaults to os.UserHomeDir.
	HomeDir func() (string, error)
	// RunHelper runs a credential helper; defaults to executing it.
	RunHelper HelperRunner
	// Diagnostics receives messages explaining why a configured source was
	// skipped. Messages never contain secrets.
	Diagnostics func(string)
}

// Resolve returns credentials for registry (host[:port]) from, in order:
//  1. the Docker config ($DOCKER_CONFIG/config.json or ~/.docker/config.json):
//     a matching credHelpers entry with a nonempty helper name is authoritative
//     within Docker config and suppresses inline auths and the default credsStore,
//     even if the helper fails or returns no usable credentials. Without such an
//     entry, try inline auths (identitytoken before auth), then credsStore if no
//     usable inline credentials are found;
//  2. BREWLET_REGISTRY_USERNAME / BREWLET_REGISTRY_PASSWORD.
//
// If Docker config yields no usable credentials, including after a per-registry
// helper failure, resolution continues with the environment variables.
// It returns nil for anonymous access if neither source supplies credentials.
func (r CredentialResolver) Resolve(registry string) *Credential {
	getenv := r.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if c := r.fromDockerConfig(registry, getenv); c != nil {
		return c
	}
	if user := getenv("BREWLET_REGISTRY_USERNAME"); user != "" {
		return &Credential{Username: user, Secret: getenv("BREWLET_REGISTRY_PASSWORD"), Source: "BREWLET_REGISTRY_USERNAME/BREWLET_REGISTRY_PASSWORD"}
	}
	return nil
}

func (r CredentialResolver) diag(format string, args ...any) {
	if r.Diagnostics != nil {
		r.Diagnostics(fmt.Sprintf(format, args...))
	}
}

type dockerConfig struct {
	Auths       map[string]dockerAuth `json:"auths"`
	CredHelpers map[string]string     `json:"credHelpers"`
	CredsStore  string                `json:"credsStore"`
}

type dockerAuth struct {
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
}

func (r CredentialResolver) configPath(getenv func(string) string) (string, bool) {
	if dir := getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json"), true
	}
	home := r.HomeDir
	if home == nil {
		home = os.UserHomeDir
	}
	h, err := home()
	if err != nil || h == "" {
		return "", false
	}
	return filepath.Join(h, ".docker", "config.json"), true
}

func (r CredentialResolver) fromDockerConfig(registry string, getenv func(string) string) *Credential {
	path, ok := r.configPath(getenv)
	if !ok {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.diag("ignoring unreadable Docker config %s: %v", path, err)
		}
		return nil
	}
	var cfg dockerConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		r.diag("ignoring malformed Docker config %s", path)
		return nil
	}
	registry = strings.ToLower(registry)
	dockerHub := dockerHubHosts[registry]
	serverURL := registry
	if dockerHub {
		serverURL = dockerHubConfigKey
	}

	if key, ok := matchingKey(cfg.CredHelpers, registry, dockerHub); ok && cfg.CredHelpers[key] != "" {
		return r.fromHelper(cfg.CredHelpers[key], serverURL)
	}
	if key, ok := matchingKey(cfg.Auths, registry, dockerHub); ok {
		entry := cfg.Auths[key]
		if entry.IdentityToken != "" {
			return &Credential{Username: IdentityTokenUsername, Secret: entry.IdentityToken, IdentityToken: true, Source: path + " (auths identitytoken)"}
		}
		if entry.Auth != "" {
			decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
			user, pass, found := strings.Cut(string(decoded), ":")
			if err != nil || !found {
				r.diag("ignoring malformed auth entry for %s in %s", registry, path)
			} else {
				return &Credential{Username: user, Secret: pass, Source: path + " (auths)"}
			}
		}
	}
	if cfg.CredsStore != "" {
		return r.fromHelper(cfg.CredsStore, serverURL)
	}
	return nil
}

// matchingKey finds the key naming registry. Keys may be bare authorities or
// URLs (https://host/v1/); only the authority is compared, exactly, so
// credentials for one registry are never offered to another whose name
// merely contains it.
func matchingKey[V any](m map[string]V, registry string, dockerHub bool) (string, bool) {
	for key := range m {
		a := keyAuthority(key)
		if a == "" {
			continue
		}
		if a == registry || (dockerHub && dockerHubHosts[a]) {
			return key, true
		}
	}
	return "", false
}

func keyAuthority(key string) string {
	v := strings.TrimSpace(key)
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.User != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

func (r CredentialResolver) fromHelper(helper, serverURL string) *Credential {
	if !helperName.MatchString(helper) {
		r.diag("ignoring Docker credential helper with an invalid name: %q", helper)
		return nil
	}
	run := r.RunHelper
	if run == nil {
		run = runHelper
	}
	out, err := run(helper, serverURL)
	if err != nil {
		r.diag("Docker credential helper docker-credential-%s could not be run for %s: %v", helper, serverURL, err)
		return nil
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var resp struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		r.diag("Docker credential helper docker-credential-%s returned malformed output for %s", helper, serverURL)
		return nil
	}
	if resp.Secret == "" {
		return nil
	}
	source := "docker-credential-" + helper
	if resp.Username == "" || resp.Username == IdentityTokenUsername {
		return &Credential{Username: IdentityTokenUsername, Secret: resp.Secret, IdentityToken: true, Source: source}
	}
	return &Credential{Username: resp.Username, Secret: resp.Secret, Source: source}
}

// runHelper executes `docker-credential-<helper> get`, writing serverURL to
// stdin as the helper protocol requires. A non-zero exit means the helper has
// no credentials for that server.
func runHelper(helper, serverURL string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), HelperTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(serverURL)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("timed out after %s", HelperTimeout)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return stdout.String(), nil
}
