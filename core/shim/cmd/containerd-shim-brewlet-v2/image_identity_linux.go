// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	"github.com/microsoft/brewlet/internal/artifact"
)

const criContainerMetadataExtension = "io.cri-containerd.container.metadata"

type containerIdentityRecord struct {
	ImageName  string
	Extensions map[string][]byte
}

type imageIdentityRecord struct {
	ConfigDigest   string
	TargetDigest   string
	ManifestDigest string
}

type imageIdentityMetadataClient interface {
	ContainerInfo(context.Context, string) (containerIdentityRecord, error)
	ImageTargetInfo(context.Context, string) (imageIdentityRecord, error)
}

type containerdImageIdentityResolver struct {
	client imageIdentityMetadataClient
}

func newContainerdImageIdentityResolver(containers containersapi.ContainersClient, contentRoot string) *containerdImageIdentityResolver {
	return &containerdImageIdentityResolver{
		client: containerdIdentityMetadataClient{
			containers:  containers,
			contentRoot: contentRoot,
		},
	}
}

func (r *containerdImageIdentityResolver) Resolve(ctx context.Context, containerID string) (resolvedImageIdentity, error) {
	if r == nil || r.client == nil {
		return resolvedImageIdentity{}, fmt.Errorf("containerd image identity resolver is not configured")
	}
	if _, err := namespaces.NamespaceRequired(ctx); err != nil {
		return resolvedImageIdentity{}, fmt.Errorf("resolve containerd namespace for task %q: %w", containerID, err)
	}
	namespace, _ := namespaces.Namespace(ctx)
	ctx = namespaces.WithNamespace(ctx, namespace)

	container, err := r.client.ContainerInfo(ctx, containerID)
	if err != nil {
		return resolvedImageIdentity{}, fmt.Errorf("load container %q: %w", containerID, err)
	}
	extension, ok := container.Extensions[criContainerMetadataExtension]
	if !ok || len(extension) == 0 {
		return resolvedImageIdentity{}, fmt.Errorf("container %q is missing CRI metadata extension %q", containerID, criContainerMetadataExtension)
	}
	metadata, err := decodeCRIImageMetadata(extension)
	if err != nil {
		return resolvedImageIdentity{}, fmt.Errorf("decode CRI metadata for container %q: %w", containerID, err)
	}
	configDigest, err := requireSHA256Digest("CRI image config identity", metadata.ConfigDigest)
	if err != nil {
		return resolvedImageIdentity{}, err
	}

	imageName := strings.TrimSpace(container.ImageName)
	if imageName == "" {
		return resolvedImageIdentity{}, fmt.Errorf("container %q has no containerd image reference", containerID)
	}
	target, err := requiredImageTargetDigest(metadata.RequestedImage)
	if err != nil {
		return resolvedImageIdentity{}, fmt.Errorf("CRI requested image %q: %w", metadata.RequestedImage, err)
	}
	// The CRI image config digest is not a safe lookup key because multiple
	// manifests may share one image config while carrying different launch
	// annotations. Resolve the exact digest-pinned request from the content store.
	image, err := r.client.ImageTargetInfo(ctx, target)
	if err != nil {
		return resolvedImageIdentity{}, fmt.Errorf("load digest-pinned containerd image target %q for container %q: %w", target, containerID, err)
	}
	actualConfig, err := requireSHA256Digest("containerd image config", image.ConfigDigest)
	if err != nil {
		return resolvedImageIdentity{}, err
	}
	if actualConfig != configDigest {
		return resolvedImageIdentity{}, fmt.Errorf("containerd image target config mismatch for container %q: CRI resolved config %s but target %s has config %s", containerID, configDigest, target, actualConfig)
	}
	actualTarget, err := requireSHA256Digest("containerd image target", image.TargetDigest)
	if err != nil {
		return resolvedImageIdentity{}, err
	}
	if actualTarget != target {
		return resolvedImageIdentity{}, fmt.Errorf("containerd image target mismatch for container %q: CRI requested %s but content resolution returned %s", containerID, target, actualTarget)
	}
	manifestDigest, err := requireSHA256Digest("containerd platform manifest", image.ManifestDigest)
	if err != nil {
		return resolvedImageIdentity{}, err
	}
	return resolvedImageIdentity{
		ImageName:      imageName,
		TargetDigest:   target,
		ManifestDigest: manifestDigest,
	}, nil
}

type criImageMetadata struct {
	ConfigDigest   string
	RequestedImage string
}

func decodeCRIImageMetadata(extension []byte) (criImageMetadata, error) {
	var envelope struct {
		Version  string
		Metadata struct {
			ImageRef string
			Config   struct {
				Image struct {
					Image              string `json:"image"`
					UserSpecifiedImage string `json:"user_specified_image"`
				} `json:"image"`
			} `json:"config"`
		}
	}
	if err := json.Unmarshal(extension, &envelope); err != nil {
		return criImageMetadata{}, err
	}
	if envelope.Version != "v1" {
		return criImageMetadata{}, fmt.Errorf("unsupported metadata version %q", envelope.Version)
	}
	if strings.TrimSpace(envelope.Metadata.ImageRef) == "" {
		return criImageMetadata{}, fmt.Errorf("CRI metadata has no image reference")
	}
	if strings.TrimSpace(envelope.Metadata.Config.Image.Image) == "" {
		return criImageMetadata{}, fmt.Errorf("CRI metadata has no resolved image identity")
	}
	if strings.TrimSpace(envelope.Metadata.Config.Image.Image) != strings.TrimSpace(envelope.Metadata.ImageRef) {
		return criImageMetadata{}, fmt.Errorf(
			"CRI metadata image identity mismatch: imageRef %q does not match resolved image %q",
			envelope.Metadata.ImageRef,
			envelope.Metadata.Config.Image.Image,
		)
	}
	if strings.TrimSpace(envelope.Metadata.Config.Image.UserSpecifiedImage) == "" {
		return criImageMetadata{}, fmt.Errorf("CRI metadata has no requested image")
	}
	return criImageMetadata{
		ConfigDigest:   envelope.Metadata.ImageRef,
		RequestedImage: envelope.Metadata.Config.Image.UserSpecifiedImage,
	}, nil
}

type containerdIdentityMetadataClient struct {
	containers  containersapi.ContainersClient
	contentRoot string
}

func (c containerdIdentityMetadataClient) ContainerInfo(ctx context.Context, containerID string) (containerIdentityRecord, error) {
	response, err := c.containers.Get(ctx, &containersapi.GetContainerRequest{ID: containerID})
	if err != nil {
		return containerIdentityRecord{}, err
	}
	container := response.GetContainer()
	if container == nil {
		return containerIdentityRecord{}, fmt.Errorf("containerd returned an empty container record")
	}
	extensions := make(map[string][]byte, len(container.GetExtensions()))
	for name, extension := range container.GetExtensions() {
		if extension != nil {
			extensions[name] = extension.GetValue()
		}
	}
	return containerIdentityRecord{
		ImageName:  container.GetImage(),
		Extensions: extensions,
	}, nil
}

func (c containerdIdentityMetadataClient) ImageTargetInfo(_ context.Context, targetDigest string) (imageIdentityRecord, error) {
	targetDigest, err := requireSHA256Digest("containerd image target", targetDigest)
	if err != nil {
		return imageIdentityRecord{}, err
	}
	manifest, manifestDigest, err := artifact.ResolveManifestFollowingIndex(contentStoreSource{root: c.contentRoot}, targetDigest)
	if err != nil {
		return imageIdentityRecord{}, err
	}
	return imageIdentityRecord{
		ConfigDigest:   manifest.Config.Digest,
		TargetDigest:   targetDigest,
		ManifestDigest: manifestDigest,
	}, nil
}
