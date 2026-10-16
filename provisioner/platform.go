package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"github.com/containerd/platforms"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
const dockerIndex = "application/vnd.docker.distribution.manifest.list.v2+json"
const dockerConfig = "application/vnd.docker.container.image.v1+json"

var platformComponent = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// NormalizeRuntimePlatform applies containerd's architecture/default-variant
// aliases, but never substitutes a different non-default CPU variant.
func NormalizeRuntimePlatform(p ocispec.Platform) (ocispec.Platform, error) {
	if p.OS == "" || p.Architecture == "" {
		return p, fmt.Errorf("NoCompatibleRuntime: explicit runtime OS and architecture required")
	}
	p = platforms.Normalize(p)
	if p.OS != "linux" || !platformComponent.MatchString(p.Architecture) || (p.Variant != "" && !platformComponent.MatchString(p.Variant)) || p.OSVersion != "" || len(p.OSFeatures) != 0 {
		return p, fmt.Errorf("NoCompatibleRuntime: unsupported or unknown Linux platform declaration")
	}
	return p, nil
}

func runtimePlatformKey(p ocispec.Platform) string {
	return p.OS + "/" + p.Architecture + "/" + p.Variant
}

type RuntimePlatform struct {
	Platform ocispec.Platform
	Manifest ocispec.Descriptor
	Config   ocispec.Image
}

type RuntimePlatforms struct {
	entries map[string]RuntimePlatform
}

func (p *RuntimePlatforms) Select(wanted ocispec.Platform) (RuntimePlatform, error) {
	wanted, err := NormalizeRuntimePlatform(wanted)
	if err != nil {
		return RuntimePlatform{}, err
	}
	entry, exists := p.entries[runtimePlatformKey(wanted)]
	if !exists {
		return RuntimePlatform{}, fmt.Errorf("NoCompatibleRuntime: pinned runtime lacks exact platform %s", platforms.Format(wanted))
	}
	return entry, nil
}

func (p *RuntimePlatforms) Covers(required []ocispec.Platform) error {
	if len(required) == 0 {
		return fmt.Errorf("NoCompatibleRuntime: offered pool platforms are unknown")
	}
	for _, platform := range required {
		if _, err := p.Select(platform); err != nil {
			return err
		}
	}
	return nil
}

// InspectRuntimePlatforms reads only digest-verified indexes, manifests and
// configs. Layer descriptors are checked structurally but never opened, so cold
// OnDemand validation cannot fetch or unpack a runtime root.
func InspectRuntimePlatforms(ctx context.Context, target ocispec.Descriptor, open func(ocispec.Descriptor) (io.ReadCloser, error)) (*RuntimePlatforms, error) {
	result := &RuntimePlatforms{entries: map[string]RuntimePlatform{}}
	read := func(descriptor ocispec.Descriptor) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if descriptor.Digest.Validate() != nil || descriptor.Digest.Algorithm() != digest.SHA256 || descriptor.Size < 0 || descriptor.Size > 16<<20 {
			return nil, fmt.Errorf("NoCompatibleRuntime: invalid or oversized runtime metadata descriptor")
		}
		reader, err := open(descriptor)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		data, err := io.ReadAll(io.LimitReader(reader, descriptor.Size+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != descriptor.Size || digest.FromBytes(data) != descriptor.Digest {
			return nil, fmt.Errorf("NoCompatibleRuntime: runtime metadata digest/size mismatch")
		}
		return data, nil
	}
	count := 0
	var visit func(ocispec.Descriptor, *ocispec.Platform, int) error
	visit = func(descriptor ocispec.Descriptor, declared *ocispec.Platform, depth int) error {
		count++
		if count > 2048 || depth > 8 {
			return fmt.Errorf("NoCompatibleRuntime: runtime index nesting/descriptor limit exceeded")
		}
		data, err := read(descriptor)
		if err != nil {
			return err
		}
		switch descriptor.MediaType {
		case ocispec.MediaTypeImageIndex, dockerIndex:
			var index ocispec.Index
			if err := json.Unmarshal(data, &index); err != nil {
				return err
			}
			if index.SchemaVersion != 2 || len(index.Manifests) == 0 {
				return fmt.Errorf("NoCompatibleRuntime: invalid runtime index")
			}
			for _, child := range index.Manifests {
				// OCI indexes may carry non-runnable attestation descriptors.
				if child.Platform != nil && child.Platform.OS != "linux" {
					continue
				}
				if child.MediaType != ocispec.MediaTypeImageManifest && child.MediaType != dockerManifest && child.MediaType != ocispec.MediaTypeImageIndex && child.MediaType != dockerIndex {
					return fmt.Errorf("NoCompatibleRuntime: unsupported runtime index descriptor")
				}
				childDeclaration := child.Platform
				if declared != nil && childDeclaration != nil {
					parentPlatform, err := NormalizeRuntimePlatform(*declared)
					if err != nil {
						return err
					}
					childPlatform, err := NormalizeRuntimePlatform(*childDeclaration)
					if err != nil {
						return err
					}
					if runtimePlatformKey(parentPlatform) != runtimePlatformKey(childPlatform) {
						return fmt.Errorf("NoCompatibleRuntime: nested index platform declarations differ")
					}
				}
				if childDeclaration == nil {
					childDeclaration = declared
				}
				if err := visit(child, childDeclaration, depth+1); err != nil {
					return err
				}
			}
		case ocispec.MediaTypeImageManifest, dockerManifest:
			var manifest ocispec.Manifest
			if err := json.Unmarshal(data, &manifest); err != nil {
				return err
			}
			if manifest.SchemaVersion != 2 || (manifest.Config.MediaType != ocispec.MediaTypeImageConfig && manifest.Config.MediaType != dockerConfig) {
				return fmt.Errorf("NoCompatibleRuntime: invalid runtime manifest/config type")
			}
			configData, err := read(manifest.Config)
			if err != nil {
				return err
			}
			var config ocispec.Image
			if err := json.Unmarshal(configData, &config); err != nil {
				return err
			}
			actual, err := NormalizeRuntimePlatform(config.Platform)
			if err != nil {
				return err
			}
			selected := actual
			if declared != nil {
				selected, err = NormalizeRuntimePlatform(*declared)
				if err != nil {
					return err
				}
				if actual.OS != selected.OS || actual.Architecture != selected.Architecture || (config.Variant != "" && actual.Variant != selected.Variant) {
					return fmt.Errorf("NoCompatibleRuntime: index and runtime config platform declarations differ")
				}
			}
			if config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != len(manifest.Layers) {
				return fmt.Errorf("NoCompatibleRuntime: runtime rootfs/layer declarations differ")
			}
			for index, layer := range manifest.Layers {
				if layer.Digest.Validate() != nil || layer.Digest.Algorithm() != digest.SHA256 || layer.Size < 0 || config.RootFS.DiffIDs[index].Validate() != nil || config.RootFS.DiffIDs[index].Algorithm() != digest.SHA256 {
					return fmt.Errorf("NoCompatibleRuntime: invalid runtime layer/diff-ID declaration")
				}
			}
			key := runtimePlatformKey(selected)
			if _, exists := result.entries[key]; exists {
				return fmt.Errorf("NoCompatibleRuntime: ambiguous normalized runtime platform %s", platforms.Format(selected))
			}
			result.entries[key] = RuntimePlatform{Platform: selected, Manifest: descriptor, Config: config}
		default:
			return fmt.Errorf("NoCompatibleRuntime: expected pinned runtime image/index metadata")
		}
		return nil
	}
	if err := visit(target, target.Platform, 0); err != nil {
		return nil, err
	}
	if len(result.entries) == 0 {
		return nil, fmt.Errorf("NoCompatibleRuntime: runtime has no declared Linux platform")
	}
	return result, nil
}
