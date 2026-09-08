// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"crypto/sha256"
	"fmt"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/util/validation"
)

// NodeProfilePolicy carries operator-level trust configuration that must not be
// delegated to a NodeProfile.
type NodeProfilePolicy struct {
	AllowedSourceMirrorHosts []string
}

// ParseAllowedSourceMirrorHosts parses the comma-separated manager/admission flag.
// Entries are exact registry hosts; whitespace, duplicates, paths, and schemes
// are rejected rather than normalized.
func ParseAllowedSourceMirrorHosts(value string) ([]string, error) {
	if value == "" {
		return nil, nil
	}
	if strings.HasPrefix(value, ",") || strings.HasSuffix(value, ",") || strings.Contains(value, ",,") {
		return nil, fmt.Errorf("allowed source mirror hosts must not contain empty entries")
	}
	hosts := strings.Split(value, ",")
	if _, err := allowedMirrorHostSet(hosts); err != nil {
		return nil, err
	}
	return hosts, nil
}

// ValidateNodeProfile applies the source policy with mirrors disabled.
func ValidateNodeProfile(profile *nodev1alpha1.NodeProfile) error {
	return (NodeProfilePolicy{}).Validate(profile)
}

// Validate enforces the NodeProfile invariants shared by admission and
// reconciliation: safe JDK tokens, digest-pinned explicit sources, approved
// mirrors, and valid rollout settings.
func (policy NodeProfilePolicy) Validate(profile *nodev1alpha1.NodeProfile) error {
	allowedMirrors, err := allowedMirrorHostSet(policy.AllowedSourceMirrorHosts)
	if err != nil {
		return fmt.Errorf("invalid allowed source mirror host policy: %w", err)
	}
	if len(profile.Spec.JDKs) == 0 {
		return fmt.Errorf("spec.jdks must list at least one JDK")
	}
	tokens := make(map[string]struct{}, len(profile.Spec.JDKs))
	for i, j := range profile.Spec.JDKs {
		if j.Feature <= 0 {
			return fmt.Errorf("spec.jdks[%d].feature must be a positive version, got %d", i, j.Feature)
		}
		if errs := validation.IsDNS1123Label(j.Distribution); len(errs) > 0 {
			return fmt.Errorf("spec.jdks[%d].distribution %q is invalid: %s", i, j.Distribution, strings.Join(errs, "; "))
		}
		const maxDistributionLength = 48
		if len(j.Distribution) > maxDistributionLength {
			return fmt.Errorf("spec.jdks[%d].distribution %q exceeds %d characters", i, j.Distribution, maxDistributionLength)
		}
		token := j.Token()
		const maxJDKTokenLength = validation.DNS1123LabelMaxLength - len("jdk.")
		if len(token) > maxJDKTokenLength {
			return fmt.Errorf("spec.jdks[%d] token %q exceeds %d characters", i, token, maxJDKTokenLength)
		}
		if _, exists := tokens[token]; exists {
			return fmt.Errorf("spec.jdks[%d] duplicates %q", i, token)
		}
		tokens[token] = struct{}{}
		if err := validateJDKSource(i, j); err != nil {
			return err
		}
	}
	launchers := make(map[string]struct{}, len(profile.Spec.Launchers))
	for i, launcher := range profile.Spec.Launchers {
		if errs := validation.IsDNS1123Label(launcher.Name); len(errs) > 0 {
			return fmt.Errorf("spec.launchers[%d].name %q is invalid: %s", i, launcher.Name, strings.Join(errs, "; "))
		}
		const maxLauncherNameLength = validation.DNS1123LabelMaxLength - len("launcher.")
		if len(launcher.Name) > maxLauncherNameLength {
			return fmt.Errorf("spec.launchers[%d].name %q exceeds %d characters", i, launcher.Name, maxLauncherNameLength)
		}
		if launcher.Name == "java" {
			return fmt.Errorf("spec.launchers[%d].name must not be %q because java is provided by every JDK", i, launcher.Name)
		}
		if _, exists := launchers[launcher.Name]; exists {
			return fmt.Errorf("spec.launchers[%d] duplicates %q", i, launcher.Name)
		}
		launchers[launcher.Name] = struct{}{}
		if err := validateLauncherSource(i, launcher); err != nil {
			return err
		}
	}
	if err := validateRegistryMirrors(profile.Spec.Registry, allowedMirrors); err != nil {
		return err
	}
	switch profile.Spec.Rollout.ContainerdRestart {
	case "", nodev1alpha1.ContainerdRestartValidated,
		nodev1alpha1.ContainerdRestartSIGHUP, nodev1alpha1.ContainerdRestartNone:
	default:
		return fmt.Errorf("spec.rollout.containerdRestart %q is invalid; want one of validated|sighup|none",
			profile.Spec.Rollout.ContainerdRestart)
	}
	return nil
}

func validateJDKSource(index int, j nodev1alpha1.JDKRef) error {
	if err := validateImageReference(j.Source.Image); err != nil {
		return fmt.Errorf("spec.jdks[%d].source.image: %w", index, err)
	}
	if err := validateAbsolutePath(j.Source.JavaHome); err != nil {
		return fmt.Errorf("spec.jdks[%d].source.javaHome %q must be a clean absolute path below /", index, j.Source.JavaHome)
	}
	return nil
}

func validateLauncherSource(index int, launcher nodev1alpha1.LauncherRef) error {
	if err := validateImageReference(launcher.Source.Image); err != nil {
		return fmt.Errorf("spec.launchers[%d].source.image: %w", index, err)
	}
	if err := validateAbsolutePath(launcher.Source.Path); err != nil {
		return fmt.Errorf("spec.launchers[%d].source.path %q must be a clean absolute path below /", index, launcher.Source.Path)
	}
	return nil
}

func validateAbsolutePath(value string) error {
	if strings.ContainsAny(value, " \t\r\n") ||
		!path.IsAbs(value) ||
		path.Clean(value) != value ||
		value == "/" {
		return fmt.Errorf("must be a clean absolute path below /")
	}
	return nil
}

func validateImageReference(ref string) error {
	if ref == "" || strings.ContainsAny(ref, " \t\r\n") || strings.Contains(ref, "://") {
		return fmt.Errorf("%q must be a non-empty digest-pinned OCI reference without whitespace or a URL scheme", ref)
	}
	name, digest, ok := strings.Cut(ref, "@")
	if !ok || strings.Contains(digest, "@") || !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("%q must be pinned as repository@sha256:<64 lowercase hex>", ref)
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	if len(hexDigest) != sha256.Size*2 {
		return fmt.Errorf("%q must use a 64-character SHA-256 digest", ref)
	}
	for _, c := range hexDigest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%q must use a lowercase hexadecimal SHA-256 digest", ref)
		}
	}
	named, err := reference.ParseNamed(ref)
	if err != nil {
		return fmt.Errorf("%q is not a valid OCI image reference: %w", ref, err)
	}
	if _, tagged := named.(reference.NamedTagged); tagged {
		return fmt.Errorf("%q must not include a mutable tag", ref)
	}
	if _, digested := named.(reference.Digested); !digested {
		return fmt.Errorf("%q must include a digest", ref)
	}
	host := reference.Domain(named)
	if err := validateRegistryHost(host); err != nil {
		return fmt.Errorf("%q has invalid registry host: %w", name, err)
	}
	return nil
}

func validateRegistryMirrors(registry *nodev1alpha1.RegistrySpec, allowed map[string]struct{}) error {
	if registry == nil || len(registry.Mirrors) == 0 {
		return nil
	}
	if len(allowed) == 0 {
		return fmt.Errorf("spec.registry.mirrors is disabled because no allowed source mirror hosts are configured")
	}
	sources := make([]string, 0, len(registry.Mirrors))
	for source := range registry.Mirrors {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		target := registry.Mirrors[source]
		if err := validateRegistryHost(source); err != nil {
			return fmt.Errorf("spec.registry.mirrors source %q is invalid: %w", source, err)
		}
		targetHost, err := validateMirrorTarget(target)
		if err != nil {
			return fmt.Errorf("spec.registry.mirrors[%q] %q is invalid: %w", source, target, err)
		}
		if source == targetHost {
			return fmt.Errorf("spec.registry.mirrors[%q] must use a different destination host", source)
		}
		if _, ok := allowed[targetHost]; !ok {
			return fmt.Errorf("spec.registry.mirrors[%q] destination host %q is not approved", source, targetHost)
		}
	}
	return nil
}

func allowedMirrorHostSet(hosts []string) (map[string]struct{}, error) {
	allowed := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		if err := validateRegistryHost(host); err != nil {
			return nil, fmt.Errorf("%q: %w", host, err)
		}
		if _, exists := allowed[host]; exists {
			return nil, fmt.Errorf("duplicate registry host %q", host)
		}
		allowed[host] = struct{}{}
	}
	return allowed, nil
}

func validateRegistryHost(host string) error {
	if host == "" || strings.ContainsAny(host, " \t\r\n/@") || strings.Contains(host, "://") {
		return fmt.Errorf("must be an exact registry host without whitespace, path, or scheme")
	}
	if host != strings.ToLower(host) {
		return fmt.Errorf("must be a canonical lowercase registry host")
	}
	if host != "localhost" && !strings.ContainsAny(host, ".:") {
		return fmt.Errorf("must be localhost or a fully qualified registry host")
	}
	probe := host + "/brewlet/host-validation"
	named, err := reference.ParseNamed(probe)
	if err != nil || reference.Domain(named) != host || reference.Path(named) != "brewlet/host-validation" {
		return fmt.Errorf("must be a canonical lowercase registry host")
	}
	if err := validateRegistryHostPort(host); err != nil {
		return err
	}
	if strings.HasPrefix(host, "[") {
		return nil
	}
	name := host
	if strings.Count(host, ":") == 1 {
		name, _, _ = strings.Cut(host, ":")
	}
	if name == "localhost" || net.ParseIP(name) != nil {
		return nil
	}
	if !strings.Contains(name, ".") {
		return fmt.Errorf("must be localhost, an IP address, or a fully qualified registry host")
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("must contain valid DNS labels")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("must contain valid DNS labels")
			}
		}
	}
	return nil
}

func validateRegistryHostPort(host string) error {
	port := ""
	if strings.HasPrefix(host, "[") {
		closeBracket := strings.IndexByte(host, ']')
		if closeBracket <= 1 || net.ParseIP(host[1:closeBracket]) == nil {
			return fmt.Errorf("must contain a valid bracketed IP address")
		}
		remainder := host[closeBracket+1:]
		if remainder == "" {
			return nil
		}
		if !strings.HasPrefix(remainder, ":") {
			return fmt.Errorf("must contain a valid registry port")
		}
		port = remainder[1:]
	} else if strings.Count(host, ":") == 1 {
		_, port, _ = strings.Cut(host, ":")
	} else if strings.Contains(host, ":") {
		return fmt.Errorf("IPv6 registry hosts must use brackets")
	}
	if port == "" {
		return nil
	}
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return fmt.Errorf("registry port must be between 1 and 65535")
	}
	return nil
}

func validateMirrorTarget(target string) (string, error) {
	if target == "" || strings.ContainsAny(target, " \t\r\n@") || strings.Contains(target, "://") ||
		strings.HasSuffix(target, "/") {
		return "", fmt.Errorf("must be a canonical mirror host with an optional repository prefix")
	}
	probe := target + "/brewlet/mirror-validation"
	named, err := reference.ParseNamed(probe)
	if err != nil || named.String() != probe {
		return "", fmt.Errorf("must be a canonical lowercase mirror host with an optional repository prefix")
	}
	host := reference.Domain(named)
	if err := validateRegistryHost(host); err != nil {
		return "", err
	}
	return host, nil
}

// ValidateNoPoolConflicts rejects ambiguous named selectors and multiple
// catch-alls, including overlaps possible on future nodes (§5.6/§8.3).
// Node claims still arbitrate simultaneous admissions.
func ValidateNoPoolConflicts(profile *nodev1alpha1.NodeProfile, others []nodev1alpha1.NodeProfile) error {
	for i := range others {
		o := &others[i]
		if o.Name == profile.Name {
			continue
		}
		if isDefaultProfile(profile) && isDefaultProfile(o) {
			return fmt.Errorf("pool ownership conflict: only one catch-all profile is allowed (already claimed by profile %q)", o.Name)
		}
		if isDefaultProfile(profile) || isDefaultProfile(o) {
			continue
		}
		// Different keys are independent constraints: a future node can carry
		// both. Auto-detection can also change when the fleet grows.
		if profile.Spec.NodePool.Key != o.Spec.NodePool.Key {
			return fmt.Errorf("pool ownership conflict: profile %q uses a different explicit/auto pool key; future nodes could match both selectors", o.Name)
		}
		for _, name := range profile.Spec.NodePool.Names {
			for _, other := range o.Spec.NodePool.Names {
				if name == other {
					return fmt.Errorf("pool ownership conflict: %q already claimed by profile %q", name, o.Name)
				}
			}
		}
	}
	return nil
}
