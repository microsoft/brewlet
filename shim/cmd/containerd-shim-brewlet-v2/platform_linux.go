//go:build linux

package main

import (
	"context"
	"fmt"

	"github.com/microsoft/brewlet/pkg/artifact"
	brewletplatform "github.com/microsoft/brewlet/pkg/platform"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func resolveNativeArtifact(ctx context.Context, open artifact.BlobOpener, target ocispec.Descriptor, host ocispec.Platform) (*artifact.Resolved, error) {
	if host.OS != "linux" || host.Architecture == "" {
		return nil, fmt.Errorf("native Linux host platform unavailable")
	}
	return artifact.Resolve(ctx, open, target, host)
}

func validateRuntimeHostPlatform(runtime, host ocispec.Platform) error {
	if host.OS != "linux" || !brewletplatform.Exact(runtime, host) {
		return fmt.Errorf("runtime platform does not exactly match native host OS/architecture/variant")
	}
	return nil
}
