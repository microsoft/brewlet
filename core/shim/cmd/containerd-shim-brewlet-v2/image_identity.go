// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/microsoft/brewlet/internal/artifact"
)

const (
	annCRIContainerName = "io.kubernetes.cri.container-name"
	annCRIImageName     = "io.kubernetes.cri.image-name"
)

type resolvedImageIdentity struct {
	ImageName      string
	TargetDigest   string // exact manifest/index digest from the CRI request
	ManifestDigest string // verified platform manifest selected from that target
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
	if !hasDigest {
		return fmt.Errorf("containerd image annotation %s=%q must be digest-pinned", annCRIImageName, ref)
	}
	if requested != resolved {
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
	ref = strings.TrimSpace(ref)
	at := strings.LastIndexByte(ref, '@')
	if at < 0 {
		return "", false, nil
	}
	value, err := requireSHA256Digest("digest-bearing image reference", ref[at+1:])
	if err != nil {
		return "", true, err
	}
	return value, true, nil
}

func requiredImageTargetDigest(ref string) (string, error) {
	if strings.LastIndexByte(strings.TrimSpace(ref), '@') <= 0 {
		return "", fmt.Errorf("image reference %q must be digest-pinned", strings.TrimSpace(ref))
	}
	value, hasDigest, err := imageReferenceDigest(ref)
	if err != nil {
		return "", err
	}
	if !hasDigest {
		return "", fmt.Errorf("image reference %q must be digest-pinned", strings.TrimSpace(ref))
	}
	return value, nil
}

// requireSHA256Digest validates a digest carried on textual identity metadata
// (a CRI field or an OCI annotation), where surrounding whitespace is a
// formatting artifact rather than an attack. It trims that whitespace once and
// then defers to artifact.ValidateDigest so there is a single definition of the
// canonical digest grammar: a second copy here could drift from the one that
// guards path construction.
func requireSHA256Digest(field, value string) (string, error) {
	value = strings.TrimSpace(value)
	if err := artifact.ValidateDigest(value); err != nil {
		return "", fmt.Errorf("%s must be a sha256 digest, got %q", field, value)
	}
	return value, nil
}
