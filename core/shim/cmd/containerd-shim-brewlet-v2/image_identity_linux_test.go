// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/containerd/containerd/namespaces"

	"github.com/microsoft/brewlet/internal/artifact"
)

type fakeIdentityMetadataClient struct {
	container    containerIdentityRecord
	containerErr error
	image        imageIdentityRecord
	imageErr     error
	namespace    string
	containerID  string
	targetDigest string
}

func (f *fakeIdentityMetadataClient) ContainerInfo(ctx context.Context, containerID string) (containerIdentityRecord, error) {
	f.namespace, _ = namespaces.Namespace(ctx)
	f.containerID = containerID
	return f.container, f.containerErr
}

func (f *fakeIdentityMetadataClient) ImageTargetInfo(_ context.Context, targetDigest string) (imageIdentityRecord, error) {
	f.targetDigest = targetDigest
	return f.image, f.imageErr
}

func TestContainerdImageIdentityResolver(t *testing.T) {
	configDigest := "sha256:" + strings.Repeat("a", 64)
	targetDigest := "sha256:" + strings.Repeat("b", 64)
	manifestDigest := "sha256:" + strings.Repeat("c", 64)
	requested := "registry.example.com/team/app@" + targetDigest
	client := &fakeIdentityMetadataClient{
		container: containerIdentityRecord{
			ImageName: "registry.example.com/team/app:1.0",
			Extensions: map[string][]byte{
				criContainerMetadataExtension: criMetadataAny(configDigest, requested),
			},
		},
		image: imageIdentityRecord{
			ConfigDigest:   configDigest,
			TargetDigest:   targetDigest,
			ManifestDigest: manifestDigest,
		},
	}
	resolver := &containerdImageIdentityResolver{client: client}

	got, err := resolver.Resolve(namespaces.WithNamespace(context.Background(), "k8s.io"), "container-1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.ImageName != client.container.ImageName ||
		got.TargetDigest != targetDigest ||
		got.ManifestDigest != manifestDigest {
		t.Fatalf("identity = %+v", got)
	}
	if client.namespace != "k8s.io" || client.containerID != "container-1" || client.targetDigest != targetDigest {
		t.Fatalf("resolver calls: namespace=%q container=%q target=%q", client.namespace, client.containerID, client.targetDigest)
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
								`","user_specified_image":"demo/app@` + targetDigest + `"}}}}`,
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
			name:    "tag-only request",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app:1"),
			},
			want: "must be digest-pinned",
		},
		{
			name:    "target lookup",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app@" + targetDigest),
				imageErr:  errors.New("not found"),
			},
			want: "load digest-pinned containerd image target",
		},
		{
			name:    "config identity mismatch",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app@" + targetDigest),
				image: imageIdentityRecord{
					ConfigDigest: targetDigest,
					TargetDigest: targetDigest,
				},
			},
			want: "image target config mismatch",
		},
		{
			name:    "invalid target",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app@" + targetDigest),
				image: imageIdentityRecord{
					ConfigDigest: configDigest,
					TargetDigest: "not-a-digest",
				},
			},
			want: "containerd image target must be a sha256 digest",
		},
		{
			name:    "resolved target mismatch",
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
		{
			name:    "invalid platform manifest",
			context: namespacedContext(),
			client: &fakeIdentityMetadataClient{
				container: goodContainer("demo/app@" + targetDigest),
				image: imageIdentityRecord{
					ConfigDigest:   configDigest,
					TargetDigest:   targetDigest,
					ManifestDigest: "not-a-digest",
				},
			},
			want: "containerd platform manifest must be a sha256 digest",
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

func TestContainerdIdentityMetadataClientKeepsTargetsWithSharedConfigDistinct(t *testing.T) {
	contentRoot := t.TempDir()
	configDigest := "sha256:" + strings.Repeat("a", 64)
	safeTarget := writeIdentityManifest(t, contentRoot, configDigest, `{"mainJar":"safe.jar"}`)
	maliciousTarget := writeIdentityManifest(t, contentRoot, configDigest, `{"mainJar":"safe.jar","user":{"uid":0,"gid":0}}`)
	if safeTarget == maliciousTarget {
		t.Fatal("targets with different launch metadata unexpectedly share a digest")
	}

	client := containerdIdentityMetadataClient{contentRoot: contentRoot}
	safe, err := client.ImageTargetInfo(context.Background(), safeTarget)
	if err != nil {
		t.Fatalf("resolve safe target: %v", err)
	}
	malicious, err := client.ImageTargetInfo(context.Background(), maliciousTarget)
	if err != nil {
		t.Fatalf("resolve malicious target: %v", err)
	}
	if safe.ConfigDigest != configDigest || malicious.ConfigDigest != configDigest {
		t.Fatalf("shared config identity was not preserved: safe=%+v malicious=%+v", safe, malicious)
	}
	if safe.TargetDigest != safeTarget || malicious.TargetDigest != maliciousTarget {
		t.Fatalf("target identity was not preserved: safe=%+v malicious=%+v", safe, malicious)
	}
	if safe.ManifestDigest != safeTarget || malicious.ManifestDigest != maliciousTarget {
		t.Fatalf("resolved manifest identity was not preserved: safe=%+v malicious=%+v", safe, malicious)
	}
}

func TestContainerdIdentityMetadataClientReturnsResolvedPlatformManifestDigest(t *testing.T) {
	contentRoot := t.TempDir()
	configDigest := "sha256:" + strings.Repeat("a", 64)
	manifestDigest := writeIdentityManifest(t, contentRoot, configDigest, `{"mainJar":"app.jar"}`)
	target := artifact.Platform{OS: "linux", Architecture: runtime.GOARCH}
	indexRaw, err := json.Marshal(artifact.Index{
		SchemaVersion: 2,
		MediaType:     artifact.OCIImageIndexMediaType,
		Manifests: []artifact.Descriptor{{
			Digest:   manifestDigest,
			Platform: &target,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexDigest := writeContentBlob(t, contentRoot, indexRaw)

	got, err := (containerdIdentityMetadataClient{contentRoot: contentRoot}).ImageTargetInfo(context.Background(), indexDigest)
	if err != nil {
		t.Fatalf("ImageTargetInfo: %v", err)
	}
	if got.TargetDigest != indexDigest {
		t.Fatalf("target digest = %q, want requested index %q", got.TargetDigest, indexDigest)
	}
	if got.ManifestDigest != manifestDigest {
		t.Fatalf("manifest digest = %q, want selected platform manifest %q", got.ManifestDigest, manifestDigest)
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

func writeIdentityManifest(t *testing.T, contentRoot, configDigest, launchConfig string) string {
	t.Helper()
	raw, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		Config: artifact.Descriptor{
			MediaType: artifact.OCIImageConfigMediaType,
			Digest:    configDigest,
			Size:      2,
		},
		Layers: []artifact.Descriptor{},
		Annotations: map[string]string{
			artifact.JVMConfigAnnotation: launchConfig,
		},
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	sum := sha256.Sum256(raw)
	digest := fmt.Sprintf("sha256:%x", sum)
	path := filepath.Join(contentRoot, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create content directory: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return digest
}
