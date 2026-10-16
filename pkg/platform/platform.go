// Package platform provides exact OCI platform identity after containerd's
// architecture and default-variant normalization. It never detects workloads.
package platform

import (
	"slices"

	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func Native() ocispec.Platform { return platforms.Normalize(platforms.DefaultSpec()) }

func Exact(a, b ocispec.Platform) bool {
	if a.OS == "" || a.Architecture == "" || b.OS == "" || b.Architecture == "" {
		return false
	}
	a = platforms.Normalize(a)
	b = platforms.Normalize(b)
	return a.OS == b.OS && a.Architecture == b.Architecture && a.Variant == b.Variant && a.OSVersion == b.OSVersion && slices.Equal(a.OSFeatures, b.OSFeatures)
}
