// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"
)

func TestValidateRuntimeImageIdentity(t *testing.T) {
	resolved := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		name        string
		annotations map[string]string
		wantError   string
	}{
		{name: "annotations absent"},
		{
			name: "matching digest",
			annotations: map[string]string{
				annArtifactDigest: resolved,
			},
		},
		{
			name: "matching digest reference",
			annotations: map[string]string{
				annArtifactRef: "registry.example.com/team/app@" + resolved,
			},
		},
		{
			name: "tag reference is informational",
			annotations: map[string]string{
				annArtifactRef: "registry.example.com/team/app:latest",
			},
		},
		{
			name: "conflicting digest",
			annotations: map[string]string{
				annArtifactDigest: other,
			},
			wantError: "artifact identity mismatch",
		},
		{
			name: "conflicting digest reference",
			annotations: map[string]string{
				annArtifactRef: "registry.example.com/team/app@" + other,
			},
			wantError: "artifact identity mismatch",
		},
		{
			name: "malformed digest",
			annotations: map[string]string{
				annArtifactDigest: "sha256:not-a-digest",
			},
			wantError: "must be a sha256 digest",
		},
		{
			name: "malformed digest reference",
			annotations: map[string]string{
				annArtifactRef: "registry.example.com/team/app@latest",
			},
			wantError: "must be a sha256 digest",
		},
		{
			name: "different selected container ignores shared hint",
			annotations: map[string]string{
				annArtifactContainer: "app",
				annCRIContainerName:  "sidecar",
				annArtifactDigest:    other,
			},
		},
		{
			name: "selected container validates shared hint",
			annotations: map[string]string{
				annArtifactContainer: "app",
				annCRIContainerName:  "app",
				annArtifactDigest:    other,
			},
			wantError: "artifact identity mismatch",
		},
		{
			name: "selected container requires protected container name",
			annotations: map[string]string{
				annArtifactContainer: "app",
				annArtifactDigest:    resolved,
			},
			wantError: "missing containerd container annotation",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRuntimeImageIdentity(tc.annotations, resolved)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("validateRuntimeImageIdentity: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantError)
			}
		})
	}
}

func TestValidateRuntimeImageIdentityRejectsInvalidResolvedDigest(t *testing.T) {
	err := validateRuntimeImageIdentity(nil, "sha512:"+strings.Repeat("a", 128))
	if err == nil || !strings.Contains(err.Error(), "resolved image target must be a sha256 digest") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateCRIImageIdentity(t *testing.T) {
	resolved := "sha256:" + strings.Repeat("a", 64)
	other := "sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		name      string
		imageName string
		wantError string
	}{
		{name: "matching digest", imageName: "registry.example.com/team/app@" + resolved},
		{name: "tag reference", imageName: "registry.example.com/team/app:latest", wantError: "must be digest-pinned"},
		{name: "missing", wantError: "missing containerd image annotation"},
		{name: "malformed digest", imageName: "registry.example.com/team/app@latest", wantError: "must be a sha256 digest"},
		{name: "conflicting digest", imageName: "registry.example.com/team/app@" + other, wantError: "artifact identity mismatch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCRIImageIdentity(map[string]string{annCRIImageName: tc.imageName}, resolved)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("validateCRIImageIdentity: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantError)
			}
		})
	}
}

func TestRequiredImageTargetDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name      string
		ref       string
		want      string
		wantError string
	}{
		{name: "digest pinned", ref: "registry.example.com/team/app@" + digest, want: digest},
		{name: "tag and digest", ref: "registry.example.com/team/app:1.0@" + digest, want: digest},
		{name: "tag only", ref: "registry.example.com/team/app:1.0", wantError: "must be digest-pinned"},
		{name: "missing repository", ref: "@" + digest, wantError: "must be digest-pinned"},
		{name: "malformed digest", ref: "registry.example.com/team/app@sha256:not-a-digest", wantError: "must be a sha256 digest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := requiredImageTargetDigest(tc.ref)
			if tc.wantError == "" {
				if err != nil {
					t.Fatalf("requiredImageTargetDigest: %v", err)
				}
				if got != tc.want {
					t.Fatalf("digest = %q, want %q", got, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantError)
			}
		})
	}
}
