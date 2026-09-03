// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package sourcepolicy validates administrator-provided runtime source fields.
package sourcepolicy

import (
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"

	"github.com/distribution/reference"
)

// ValidateDigestReference requires a canonical fully qualified OCI reference
// with no tag and exactly one lowercase sha256 digest.
func ValidateDigestReference(value string) error {
	if value == "" || strings.ContainsAny(value, " \t\r\n") || strings.Contains(value, "://") {
		return fmt.Errorf("%q must be a non-empty OCI reference without whitespace or a URL scheme", value)
	}
	if strings.Count(value, "@") != 1 {
		return fmt.Errorf("%q must contain exactly one digest separator", value)
	}
	name, digest, _ := strings.Cut(value, "@")
	if !isSHA256Digest(digest) {
		return fmt.Errorf("%q must end with @sha256:<64 lowercase hex>", value)
	}
	named, err := reference.ParseNamed(name)
	if err != nil || named.String() != name || !reference.IsNameOnly(named) {
		return fmt.Errorf("%q must contain a canonical tagless OCI repository", value)
	}
	host := reference.Domain(named)
	if err := ValidateRegistryHost(host); err != nil {
		return fmt.Errorf("%q has invalid registry host: %w", value, err)
	}
	return nil
}

// ValidateRegistryHost requires an exact lowercase registry host, optionally
// including a numeric port.
func ValidateRegistryHost(value string) error {
	if value == "" || value != strings.ToLower(value) {
		return fmt.Errorf("host must be non-empty and lowercase")
	}
	host := value
	if strings.HasPrefix(value, "[") {
		closeBracket := strings.IndexByte(value, ']')
		if closeBracket <= 1 || net.ParseIP(value[1:closeBracket]) == nil {
			return fmt.Errorf("invalid bracketed IP host")
		}
		remainder := value[closeBracket+1:]
		if remainder == "" {
			return nil
		}
		if !strings.HasPrefix(remainder, ":") || validateRegistryPort(remainder[1:]) != nil {
			return fmt.Errorf("invalid registry port")
		}
		return nil
	}
	if strings.Count(value, ":") == 1 {
		var port string
		host, port, _ = strings.Cut(value, ":")
		if validateRegistryPort(port) != nil {
			return fmt.Errorf("invalid registry port")
		}
	} else if strings.Contains(value, ":") {
		return fmt.Errorf("IPv6 hosts must use brackets")
	}
	if host == "localhost" || net.ParseIP(host) != nil {
		return nil
	}
	if !strings.Contains(host, ".") {
		return fmt.Errorf("host must be fully qualified")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("invalid DNS label")
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return fmt.Errorf("invalid DNS label")
			}
		}
	}
	return nil
}

// ValidateMirrorTarget validates a registry host with an optional repository
// prefix and returns its exact host for allowlist comparison.
func ValidateMirrorTarget(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, " \t\r\n") ||
		strings.Contains(value, "://") || strings.ContainsAny(value, "@,=") {
		return "", fmt.Errorf("%q must be a registry host with an optional repository path", value)
	}
	probe := value + "/brewlet/mirror-validation"
	named, err := reference.ParseNamed(probe)
	if err != nil || named.String() != probe || !reference.IsNameOnly(named) {
		return "", fmt.Errorf("%q must be a canonical registry host with an optional repository path", value)
	}
	host := reference.Domain(named)
	if err := ValidateRegistryHost(host); err != nil {
		return "", err
	}
	return host, nil
}

// ValidateAbsolutePath requires a clean absolute path below the filesystem root.
func ValidateAbsolutePath(value string) error {
	if strings.ContainsAny(value, " \t\r\n") ||
		!path.IsAbs(value) ||
		path.Clean(value) != value ||
		value == "/" {
		return fmt.Errorf("%q must be a clean absolute path below /", value)
	}
	return nil
}

func validateRegistryPort(port string) error {
	value, err := strconv.ParseUint(port, 10, 16)
	if err != nil || value == 0 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	return nil
}

func isSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	for _, r := range strings.TrimPrefix(value, "sha256:") {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}
