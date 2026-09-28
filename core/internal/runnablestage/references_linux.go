// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package runnablestage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	contentapi "github.com/containerd/containerd/api/services/content/v1"
	imagesapi "github.com/containerd/containerd/api/services/images/v1"
	namespacesapi "github.com/containerd/containerd/api/services/namespaces/v1"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/proxy"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type metadata interface {
	namespaces(context.Context) ([]string, error)
	images(context.Context, string) ([]ocispec.Descriptor, error)
	contents(context.Context, string, func(content.Info) error) error
	read(context.Context, string, ocispec.Descriptor) ([]byte, error)
}

type containerMetadata struct {
	client *grpc.ClientConn
	store  content.Store
}

func (m containerMetadata) namespaces(ctx context.Context) ([]string, error) {
	response, err := namespacesapi.NewNamespacesClient(m.client).List(ctx, &namespacesapi.ListNamespacesRequest{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, namespace := range response.Namespaces {
		if namespace == nil || namespace.Name == "" {
			return nil, fmt.Errorf("invalid containerd namespace")
		}
		names = append(names, namespace.Name)
	}
	return names, nil
}

func (m containerMetadata) images(ctx context.Context, ns string) ([]ocispec.Descriptor, error) {
	response, err := imagesapi.NewImagesClient(m.client).List(namespaces.WithNamespace(ctx, ns), &imagesapi.ListImagesRequest{})
	if err != nil {
		return nil, err
	}
	targets := make([]ocispec.Descriptor, 0, len(response.Images))
	for _, image := range response.Images {
		if image == nil || image.Target == nil {
			return nil, fmt.Errorf("invalid containerd image")
		}
		targets = append(targets, ocispec.Descriptor{
			MediaType: image.Target.MediaType, Digest: digest.Digest(image.Target.Digest), Size: image.Target.Size,
		})
	}
	return targets, nil
}

func (m containerMetadata) contents(ctx context.Context, ns string, fn func(content.Info) error) error {
	return m.store.Walk(namespaces.WithNamespace(ctx, ns), fn)
}

func (m containerMetadata) read(ctx context.Context, ns string, d ocispec.Descriptor) ([]byte, error) {
	return content.ReadBlob(namespaces.WithNamespace(ctx, ns), m.store, d)
}

func containerReferences(ctx context.Context, address string) (map[string]bool, error) {
	if address == "" {
		return nil, fmt.Errorf("containerd address is required")
	}
	if filepath.IsAbs(address) {
		address = "unix://" + address
	}
	client, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer client.Close()
	store := proxy.NewContentStore(contentapi.NewContentClient(client))
	return references(ctx, containerMetadata{client, store})
}

func references(ctx context.Context, store metadata) (map[string]bool, error) {
	refs := make(map[string]bool)
	add := func(d digest.Digest) error {
		if err := d.Validate(); err != nil {
			return fmt.Errorf("invalid reference digest %q: %w", d, err)
		}
		if d.Algorithm() == digest.SHA256 {
			refs[d.Encoded()] = true
		}
		return nil
	}
	nss, err := store.namespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containerd namespaces: %w", err)
	}
	for _, ns := range nss {
		if err := store.contents(ctx, ns, func(info content.Info) error {
			if err := add(info.Digest); err != nil {
				return err
			}
			for key, value := range info.Labels {
				if strings.HasPrefix(key, "containerd.io/gc.ref.content.") {
					if err := add(digest.Digest(value)); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("list content in namespace %q: %w", ns, err)
		}
		targets, err := store.images(ctx, ns)
		if err != nil {
			return nil, fmt.Errorf("list images in namespace %q: %w", ns, err)
		}
		seen := make(map[digest.Digest]bool)
		var visit func(ocispec.Descriptor, int) error
		visit = func(d ocispec.Descriptor, depth int) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := add(d.Digest); err != nil {
				return err
			}
			if d.MediaType == ocispec.MediaTypeImageManifest || d.MediaType == "application/vnd.docker.distribution.manifest.v2+json" {
				// A target/child descriptor protects a manifest even if its
				// content has already disappeared.
				return nil
			}
			if d.MediaType != ocispec.MediaTypeImageIndex && d.MediaType != "application/vnd.docker.distribution.manifest.list.v2+json" {
				return fmt.Errorf("unknown image target media type %q", d.MediaType)
			}
			if seen[d.Digest] {
				return nil
			}
			if depth > 128 {
				return fmt.Errorf("image index nesting limit exceeded")
			}
			seen[d.Digest] = true
			raw, err := store.read(ctx, ns, d)
			if err != nil {
				return fmt.Errorf("read image index %s: %w", d.Digest, err)
			}
			if d.Digest.Algorithm().FromBytes(raw) != d.Digest {
				return fmt.Errorf("image index %s failed digest verification", d.Digest)
			}
			var index ocispec.Index
			if err := json.Unmarshal(raw, &index); err != nil {
				return fmt.Errorf("parse image index %s: %w", d.Digest, err)
			}
			if index.SchemaVersion != 2 || index.Manifests == nil {
				return fmt.Errorf("invalid image index %s", d.Digest)
			}
			for _, child := range index.Manifests {
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		for _, target := range targets {
			if err := visit(target, 0); err != nil {
				return nil, fmt.Errorf("image references in namespace %q: %w", ns, err)
			}
		}
	}
	return refs, nil
}
