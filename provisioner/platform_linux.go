//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/errdefs"
	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type metadataReader struct {
	io.Reader
	io.Closer
}

// runtimePlatforms never calls Pull/Unpack or reads layer payloads.
func (i *ContainerdInstaller) runtimePlatforms(ctx context.Context, r *api.RuntimeImage) (*RuntimePlatforms, error) {
	if i.Client == nil {
		return nil, fmt.Errorf("runtime metadata content client unavailable")
	}
	if err := i.fence(ctx); err != nil {
		return nil, err
	}
	if err := policy.Runtime(r, nil); err != nil {
		return nil, err
	}
	if !strings.HasSuffix(r.Spec.Source.Image, "@"+r.Status.Digest) {
		return nil, fmt.Errorf("approved runtime metadata target differs from source")
	}
	ref := r.Spec.Source.Image
	if i.Rewrite != nil {
		var err error
		ref, err = i.Rewrite(ref)
		if err != nil {
			return nil, err
		}
	}
	var fetcher remotes.Fetcher
	var remoteTarget ocispec.Descriptor
	remote := func() error {
		if fetcher != nil {
			return nil
		}
		resolver, err := registryResolver(ctx, i.RegistryHostsDir)
		if err != nil {
			return err
		}
		_, target, err := resolver.Resolve(ctx, ref)
		if err != nil {
			return err
		}
		if target.Digest.String() != r.Status.Digest {
			return fmt.Errorf("runtime registry resolved a different digest")
		}
		fetcher, err = resolver.Fetcher(ctx, ref)
		remoteTarget = target
		return err
	}
	open := func(descriptor ocispec.Descriptor) (io.ReadCloser, error) {
		reader, err := i.Client.ContentStore().ReaderAt(ctx, descriptor)
		if err == nil {
			return metadataReader{io.NewSectionReader(reader, 0, reader.Size()), reader}, nil
		}
		if !errdefs.IsNotFound(err) {
			return nil, err
		}
		if err := remote(); err != nil {
			return nil, err
		}
		return fetcher.Fetch(ctx, descriptor)
	}
	var target ocispec.Descriptor
	image, err := i.Client.GetImage(ctx, ref)
	if errdefs.IsNotFound(err) && ref != r.Spec.Source.Image {
		image, err = i.Client.GetImage(ctx, r.Spec.Source.Image)
	}
	if err == nil {
		target = image.Target()
	} else if !errdefs.IsNotFound(err) {
		return nil, err
	}
	if target.Digest == "" {
		info, err := i.Client.ContentStore().Info(ctx, digest.Digest(r.Status.Digest))
		if err == nil && info.Size >= 0 && info.Size <= 16<<20 {
			target = ocispec.Descriptor{Digest: info.Digest, Size: info.Size}
			reader, err := open(target)
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(reader, info.Size+1))
			reader.Close()
			if err != nil {
				return nil, err
			}
			if int64(len(data)) != info.Size || digest.FromBytes(data) != target.Digest {
				return nil, fmt.Errorf("cached runtime metadata identity mismatch")
			}
			var header struct {
				MediaType string `json:"mediaType"`
			}
			if err := json.Unmarshal(data, &header); err != nil {
				return nil, err
			}
			target.MediaType = header.MediaType
		} else if err != nil && !errdefs.IsNotFound(err) {
			return nil, err
		}
	}
	if target.Digest == "" || target.MediaType == "" {
		if err := remote(); err != nil {
			return nil, err
		}
		target = remoteTarget
	}
	if target.Digest.String() != r.Status.Digest {
		return nil, fmt.Errorf("runtime metadata target does not match approved source digest")
	}
	return InspectRuntimePlatforms(ctx, target, open)
}
func (i *ContainerdInstaller) ValidatePlatforms(ctx context.Context, r *api.RuntimeImage, required []ocispec.Platform) error {
	declared, err := i.runtimePlatforms(ctx, r)
	if err != nil {
		return err
	}
	return declared.Covers(required)
}
