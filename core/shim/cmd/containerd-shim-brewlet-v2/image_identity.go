// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	annCRIContainerName = "io.kubernetes.cri.container-name"
	annCRIImageName     = "io.kubernetes.cri.image-name"
)

type resolvedImageIdentity struct {
	ImageName    string
	TargetDigest string
}

type imageIdentityResolver interface {
	Resolve(context.Context, string) (resolvedImageIdentity, error)
}

func validateCRIImageIdentity(annotations map[string]string, resolved string) error {
	resolved, err := requireSHA256Digest("resolved image target", resolved)
	if err != nil {
		return err
	}
	ref := strings.TrimSpace(annotations[annCRIImageName])
	if ref == "" {
		return fmt.Errorf("OCI runtime spec is missing containerd image annotation %q", annCRIImageName)
	}
	requested, hasDigest, err := imageReferenceDigest(ref)
	if err != nil {
		return fmt.Errorf("containerd image annotation %s: %w", annCRIImageName, err)
	}
	if hasDigest && requested != resolved {
		return fmt.Errorf("artifact identity mismatch: containerd image %s does not match resolved image target %s", ref, resolved)
	}
	return nil
}

func validateRuntimeImageIdentity(annotations map[string]string, resolved string) error {
	resolved, err := requireSHA256Digest("resolved image target", resolved)
	if err != nil {
		return err
	}
	if annotations == nil {
		return nil
	}
	if selected := strings.TrimSpace(annotations[annArtifactContainer]); selected != "" {
		current := strings.TrimSpace(annotations[annCRIContainerName])
		if current == "" {
			return fmt.Errorf("OCI runtime spec is missing containerd container annotation %q", annCRIContainerName)
		}
		if current != selected {
			return nil
		}
	}
	if annotated := strings.TrimSpace(annotations[annArtifactDigest]); annotated != "" {
		annotated, err = requireSHA256Digest("annotation "+annArtifactDigest, annotated)
		if err != nil {
			return err
		}
		if annotated != resolved {
			return fmt.Errorf("artifact identity mismatch: annotation %s=%s does not match resolved image target %s", annArtifactDigest, annotated, resolved)
		}
	}
	if ref := strings.TrimSpace(annotations[annArtifactRef]); ref != "" {
		annotated, hasDigest, err := imageReferenceDigest(ref)
		if err != nil {
			return fmt.Errorf("annotation %s: %w", annArtifactRef, err)
		}
		if hasDigest && annotated != resolved {
			return fmt.Errorf("artifact identity mismatch: annotation %s=%s does not match resolved image target %s", annArtifactRef, ref, resolved)
		}
	}
	return nil
}

func imageReferenceDigest(ref string) (string, bool, error) {
	at := strings.LastIndexByte(strings.TrimSpace(ref), '@')
	if at < 0 {
		return "", false, nil
	}
	value, err := requireSHA256Digest("digest-bearing image reference", ref[at+1:])
	if err != nil {
		return "", true, err
	}
	return value, true, nil
}

func requireSHA256Digest(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("%s must be a sha256 digest, got %q", field, value)
	}
	encoded := strings.TrimPrefix(value, prefix)
	if len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return "", fmt.Errorf("%s must be a sha256 digest, got %q", field, value)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return "", fmt.Errorf("%s must be a sha256 digest, got %q", field, value)
	}
	return value, nil
}
