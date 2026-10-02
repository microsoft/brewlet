// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// builtInTokenRealms lists token services that well-known registries delegate
// to on a different origin. Credentials may be sent to them over HTTPS without
// an explicit --allowed-token-realm.
var builtInTokenRealms = map[string][]string{
	"registry-1.docker.io":    {"auth.docker.io"},
	"index.docker.io":         {"auth.docker.io"},
	"docker.io":               {"auth.docker.io"},
	"registry.hub.docker.com": {"auth.docker.io"},
}

// TrustPolicy decides which transport a registry uses and where registry
// credentials may be sent. It is a port of the Maven plugin's
// RegistryTrustPolicy: HTTPS by default, plain HTTP only for exact loopback
// authorities and explicitly configured insecure registries, and credentials
// only for same-origin token realms unless a realm is explicitly allowed.
type TrustPolicy struct {
	insecure      map[string]bool
	allowedRealms map[string]bool
}

// NewTrustPolicy validates and normalizes insecure registries and allowed
// token realms, each a bare host[:port] authority.
func NewTrustPolicy(insecureRegistries, allowedTokenRealms []string) (TrustPolicy, error) {
	insecure, err := normalizeAuthorities(insecureRegistries, "--insecure-registry")
	if err != nil {
		return TrustPolicy{}, err
	}
	realms, err := normalizeAuthorities(allowedTokenRealms, "--allowed-token-realm")
	if err != nil {
		return TrustPolicy{}, err
	}
	return TrustPolicy{insecure: insecure, allowedRealms: realms}, nil
}

func normalizeAuthorities(values []string, option string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		norm, ok := normalizeAuthority(v)
		if !ok {
			return nil, fmt.Errorf("%s %q must be a bare registry authority such as registry.internal or registry.internal:5000 (no scheme, path, credentials, or wildcards)", option, v)
		}
		out[norm] = true
	}
	return out, nil
}

// IsBareAuthority reports whether s is exactly host[:port], with no scheme,
// userinfo, path, query or whitespace, so a value such as
// evil.example@real.example cannot silently retarget requests.
func IsBareAuthority(s string) bool {
	_, ok := normalizeAuthority(s)
	return ok
}

func normalizeAuthority(s string) (string, bool) {
	if s == "" || strings.ContainsAny(s, " \t\r\n/@?#\\*") {
		return "", false
	}
	u, err := url.Parse("//" + s)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" {
		return "", false
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return "", false
	}
	auth := authority(host, port)
	if !strings.EqualFold(auth, s) {
		return "", false
	}
	return auth, true
}

func authority(host, port string) string {
	host = strings.ToLower(host)
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// AllowsPlaintext reports whether registry (host[:port]) may be contacted
// over plain HTTP.
func (p TrustPolicy) AllowsPlaintext(registry string) bool {
	norm, ok := normalizeAuthority(registry)
	if !ok {
		return false
	}
	u, _ := url.Parse("//" + norm)
	if IsLoopbackHost(u.Hostname()) || p.insecure[norm] {
		return true
	}
	// host and host:80 are the same plain-HTTP origin; any other port must
	// be listed explicitly.
	return u.Port() == "80" && p.insecure[authority(u.Hostname(), "")]
}

// Scheme returns the URL scheme to use for registry.
func (p TrustPolicy) Scheme(registry string) string {
	if p.AllowsPlaintext(registry) {
		return "http"
	}
	return "https"
}

// IsLoopbackHost reports whether host is exactly a loopback host. Unlike a
// prefix check, localhost.attacker.example and 127.example.com are not.
func IsLoopbackHost(host string) bool {
	h := strings.Trim(strings.ToLower(host), "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// SameOrigin reports whether two URLs share a scheme, host and effective port.
func SameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || a.Scheme == "" || b.Scheme == "" || a.Hostname() == "" || b.Hostname() == "" {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

// ValidateTokenRealm checks an authentication challenge realm before it is
// contacted at all: it must be an absolute http(s) URL without userinfo, and
// may use plain HTTP only for an insecure-eligible authority.
func (p TrustPolicy) ValidateTokenRealm(realm string) (*url.URL, error) {
	realm = strings.TrimSpace(realm)
	if realm == "" {
		return nil, fmt.Errorf("registry authentication challenge did not provide a token realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return nil, fmt.Errorf("registry authentication realm is not a valid URL: %s", realm)
	}
	if !u.IsAbs() || u.Hostname() == "" {
		return nil, fmt.Errorf("registry authentication realm must be an absolute URL: %s", realm)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return nil, fmt.Errorf("registry authentication realm must use http or https: %s", realm)
	}
	if u.User != nil {
		return nil, fmt.Errorf("registry authentication realm must not embed credentials: %s", realm)
	}
	if scheme == "http" && !p.AllowsPlaintext(authority(u.Hostname(), u.Port())) {
		return nil, fmt.Errorf("registry authentication realm %s uses plaintext HTTP; use an https realm, or pass --insecure-registry %s to accept it", realm, authority(u.Hostname(), u.Port()))
	}
	return u, nil
}

// AllowsCredentials reports whether registry credentials may be sent to realm
// while authenticating to the registry at registryOrigin.
func (p TrustPolicy) AllowsCredentials(registryOrigin, realm *url.URL) bool {
	if registryOrigin == nil || realm == nil || realm.Hostname() == "" {
		return false
	}
	if SameOrigin(registryOrigin, realm) {
		return true
	}
	realmHost := strings.ToLower(realm.Hostname())
	if p.allowedRealms[authority(realm.Hostname(), realm.Port())] || p.allowedRealms[authority(realm.Hostname(), "")] {
		return true
	}
	if !strings.EqualFold(realm.Scheme, "https") || effectivePort(realm) != "443" {
		return false
	}
	for _, h := range builtInTokenRealms[strings.ToLower(registryOrigin.Hostname())] {
		if h == realmHost {
			return true
		}
	}
	return false
}
