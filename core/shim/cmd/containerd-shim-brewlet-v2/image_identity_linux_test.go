// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/containerd/containerd/namespaces"
)

type fakeIdentityMetadataClient struct {
	container    containerIdentityRecord
	containerErr error
	image        imageIdentityRecord
	imageErr     error
	namespace    string
	containerID  string
	imageName    string
}

func (f *fakeIdentityMetadataClient) ContainerInfo(ctx context.Context, containerID string) (containerIdentityRecord, error) {
	f.namespace, _ = namespaces.Namespace(ctx)
	f.containerID = containerID
	return f.container, f.containerErr
}

func (f *fakeIdentityMetadataClient) ImageInfo(_ context.Context, imageName string) (imageIdentityRecord, error) {
	f.imageName = imageName
	return f.image, f.imageErr
}

func TestContainerdImageIdentityResolver(t *testing.T) {
	configDigest := "sha256:" + strings.Repeat("a", 64)
	targetDigest := "sha256:" + strings.Repeat("b", 64)
	requested := "registry.example.com/team/app@" + targetDigest
	client := &fakeIdentityMetadataClient{
		container: containerIdentityRecord{
			ImageName: "registry.example.com/team/app:1.0",
			Extensions: map[string][]byte{
				criContainerMetadataExtension: criMetadataAny(configDigest, requested),
			},
		},
		image: imageIdentityRecord{
			ConfigDigest: configDigest,
			TargetDigest: targetDigest,
		},
	}
	resolver := &containerdImageIdentityResolver{client: client}

	got, err := resolver.Resolve(namespaces.WithNamespace(context.Background(), "k8s.io"), "container-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ImageName != client.container.ImageName || got.TargetDigest != targetDigest {
		t.Fatalf("identity = %+v", got)
	}
	if client.namespace != "k8s.io" || client.containerID != "container-1" || client.imageName != configDigest {
		t.Fatalf("resolver calls: namespace=%q container=%q image=%q", client.namespace, client.containerID, client.imageName)
	}
}

func TestContainerdImageIdentityResolverFailsClosed(t *testing.T) {
	configDigest := "sha256:" + strings.Repeat("a", 64)
	targetDigest := "sha256:" + strings.Repeat("b", 64)
	otherDigest := "sha256:" + strings.Repeat("c", 64)
	goodContainer := func(requested string) containerIdentityRecord {
		return containerIdentityRecord{
			ImageName: "demo/app:1",
			Extensions: map[string][]byte{
				criContainerMetadataExtension: criMetadataAny(configDigest, requested),
			},
		}
	}
	tests := []struct {
		name    string
		context context.Context
		client  *fakeIdentityMetadataClient
		want    string
	}{
		{
			name:    "missing namespace",
			context: context.Background(),
			client:  &fakeIdentityMetadataClient{},
			want:    "resolve containerd namespace",
		},
		{
			name:    "container lookup",
			context: namespacedContext(),
			client:  &fakeIdentityMetadataClient{containerErr: errors.New("not found")},
			want:    "load container",
		},
		{
			name:    "missing metadata",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: containerIdentityRecord{ImageName: "demo/app:1"},
			},
			want: "missing CRI metadata",
		},
		{
			name:    "malformed metadata",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: containerIdentityRecord{
					ImageName: "demo/app:1",
					Extensions: map[string][]byte{
						criContainerMetadataExtension: []byte("{"),
					},
				},
			},
			want: "decode CRI metadata",
		},
		{
			name:    "missing requested image",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: containerIdentityRecord{
					ImageName: "demo/app:1",
					Extensions: map[string][]byte{
						criContainerMetadataExtension: criMetadataAny(configDigest, ""),
					},
				},
			},
			want: "no requested image",
		},
		{
			name:    "resolved metadata mismatch",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: containerIdentityRecord{
					ImageName: "demo/app:1",
					Extensions: map[string][]byte{
						criContainerMetadataExtension: []byte(
							`{"Version":"v1","Metadata":{"ImageRef":"` + configDigest +
								`","Config":{"image":{"image":"` + otherDigest +
								`","user_specified_image":"demo/app:1"}}}}`,
						),
					},
				},
			},
			want: "CRI metadata image identity mismatch",
		},
		{
			name:    "missing image name",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: func() containerIdentityRecord {
					record := goodContainer("demo/app:1")
					record.ImageName = ""
					return record
				}(),
			},
			want: "no containerd image reference",
		},
		{
			name:    "image lookup",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app:1"),
				imageErr:  errors.New("not found"),
			},
			want: "load containerd image",
		},
		{
			name:    "config identity changed",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app:1"),
				image: imageIdentityRecord{
					ConfigDigest: targetDigest,
					TargetDigest: targetDigest,
				},
			},
			want: "image identity changed",
		},
		{
			name:    "invalid target",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app:1"),
				image: imageIdentityRecord{
					ConfigDigest: configDigest,
					TargetDigest: "not-a-digest",
				},
			},
			want: "containerd image target must be a sha256 digest",
		},
		{
			name:    "requested digest mismatch",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app@" + otherDigest),
				image: imageIdentityRecord{
					ConfigDigest: configDigest,
					TargetDigest: targetDigest,
				},
			},
			want: "image target mismatch",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolver := &containerdImageIdentityResolver{client: tc.client}
			_, err := resolver.Resolve(tc.context, "container-1")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func namespacedContext() context.Context {
	return namespaces.WithNamespace(context.Background(), "k8s.io")
}

func criMetadataAny(imageRef, requestedImage string) []byte {
	return []byte(`{"Version":"v1","Metadata":{"ImageRef":"` + imageRef +
		`","Config":{"image":{"image":"` + imageRef +
		`","user_specified_image":"` + requestedImage + `"}}}}`)
}
