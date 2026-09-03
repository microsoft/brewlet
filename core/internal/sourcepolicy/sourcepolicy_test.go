// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sourcepolicy

import "testing"

func TestValidateDigestReference(t *testing.T) {
	for _, valid := range []string{
		"docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
		"[2001:db8::1]:5000/java/runtime@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
	} {
		if err := ValidateDigestReference(valid); err != nil {
			t.Fatalf("valid reference %q rejected: %v", valid, err)
		}
	}
	for _, value := range []string{
		"",
		"eclipse-temurin:21",
		"docker.io/library/eclipse-temurin:21",
		"docker.io/library/eclipse-temurin:21@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
		"docker.io/library/eclipse___temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
		"docker.io/library/eclipse..temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
		"docker.io/library/eclipse.-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
		"docker.io/library/eclipse-temurin@sha256:85F00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
		"https://docker.io/library/eclipse-temurin@sha256:85f00967bcc624fc19fa9c2cf124ea426a5363898e267141726f31f358c2e14b",
	} {
		if err := ValidateDigestReference(value); err == nil {
			t.Errorf("expected %q to fail", value)
		}
	}
}

func TestValidateRegistryHost(t *testing.T) {
	for _, value := range []string{"docker.io", "registry.internal:5000", "localhost", "[2001:db8::1]:443"} {
		if err := ValidateRegistryHost(value); err != nil {
			t.Errorf("valid host %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{
		"",
		"registry",
		"HTTPS://registry.example.com",
		"registry.example.com/path",
		"registry.example.com:0",
		"registry.example.com:65536",
	} {
		if err := ValidateRegistryHost(value); err == nil {
			t.Errorf("expected %q to fail", value)
		}
	}
}

func TestValidateMirrorTarget(t *testing.T) {
	valid := map[string]string{
		"registry.internal.example.com:5000/java": "registry.internal.example.com:5000",
		"[2001:db8::2]:5000/java":                 "[2001:db8::2]:5000",
	}
	for target, wantHost := range valid {
		host, err := ValidateMirrorTarget(target)
		if err != nil {
			t.Fatalf("valid mirror %q rejected: %v", target, err)
		}
		if host != wantHost {
			t.Fatalf("host = %q, want %q", host, wantHost)
		}
	}
	for _, value := range []string{
		"",
		"https://registry.example.com/java",
		"registry example.com/java",
		"registry.example.com/java@sha256:abcd",
		"registry/java",
		"registry.example.com/java___runtimes",
		"registry.example.com/java..runtimes",
		"registry.example.com:0/java",
		"registry.example.com:65536/java",
	} {
		if _, err := ValidateMirrorTarget(value); err == nil {
			t.Errorf("expected %q to fail", value)
		}
	}
}

func TestValidateAbsolutePath(t *testing.T) {
	for _, value := range []string{"/opt/java/openjdk", "/usr/bin/jaz"} {
		if err := ValidateAbsolutePath(value); err != nil {
			t.Errorf("valid path %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"", "/", "opt/jdk", "/opt/../jdk", "/path with spaces"} {
		if err := ValidateAbsolutePath(value); err == nil {
			t.Errorf("expected %q to fail", value)
		}
	}
}
