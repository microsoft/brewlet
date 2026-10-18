//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/typeurl/v2"
	brewletplatform "github.com/microsoft/brewlet/pkg/platform"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestResolveUsesProtectedRequestedTargetNotHintsOrConfigIdentity(t *testing.T) {
	for _, name := range []string{
		"valid", "conflicting hints", "empty container image", "missing metadata",
		"malformed metadata", "old metadata version", "requested tag",
		"tag and digest", "protected stamp mismatch", "CRI config mismatch",
		"selected config mismatch",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := &platformFixture{blobs: map[digest.Digest][]byte{}}
			native := brewletplatform.Native()
			target := f.manifest(t, native, native)
			var manifest ocispec.Manifest
			if err := json.Unmarshal(f.blobs[target.Digest], &manifest); err != nil {
				t.Fatal(err)
			}
			var hinted ocispec.Descriptor
			if name == "conflicting hints" {
				alternate := manifest
				alternate.Annotations = map[string]string{"brewlet.sh/artifact-version": "1"}
				hinted = f.json(t, ocispec.MediaTypeImageManifest, alternate)
			}
			store, err := local.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			for identity, data := range f.blobs {
				descriptor := ocispec.Descriptor{Digest: identity, Size: int64(len(data))}
				if err := content.WriteBlob(ctx, store, identity.String(), bytes.NewReader(data), descriptor); err != nil {
					t.Fatal(err)
				}
			}
			requested := "registry.example.test/app@" + target.Digest.String()
			stamped, configID, imageID, version := requested, manifest.Config.Digest.String(), manifest.Config.Digest.String(), "v1"
			switch name {
			case "empty container image":
				imageID = ""
			case "old metadata version":
				version = "v0"
			case "requested tag":
				requested = "registry.example.test/app:latest"
			case "tag and digest":
				requested = "registry.example.test/app:latest@" + target.Digest.String()
			case "protected stamp mismatch":
				stamped = "registry.example.test/other@" + target.Digest.String()
			case "selected config mismatch":
				configID = digest.FromString("unrelated config").String()
			}
			configImage := configID
			if name == "CRI config mismatch" {
				configImage = digest.FromString("conflicting CRI config").String()
			}
			envelope := map[string]any{"Version": version, "Metadata": map[string]any{
				"ImageRef": configID, "config": map[string]any{"image": map[string]string{
					"image": configImage, "user_specified_image": requested,
				}},
			}}
			data, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if name == "malformed metadata" {
				data = []byte("{")
			}
			value := containers.Container{ID: "owned-task", Image: imageID}
			metadata := &anypb.Any{Value: data}
			value.Extensions = map[string]typeurl.Any{"io.cri-containerd.container.metadata": metadata}
			if name == "missing metadata" {
				value.Extensions = nil
			}
			client, err := containerd.New("", containerd.WithServices(
				containerd.WithContainerStore(startMetadataStore{value: value}),
				containerd.WithContentStore(store)))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			annotations := map[string]string{"io.kubernetes.cri.image-name": stamped}
			if name == "conflicting hints" {
				annotations["brewlet.sh/artifact-ref"] = "registry.example.test/foreign@" + hinted.Digest.String()
				annotations["brewlet.sh/artifact-digest"] = hinted.Digest.String()
				annotations["brewlet.sh/artifact-container"] = "foreign-container"
			}
			resolved, err := (&service{client: client}).resolve(ctx, value.ID, annotations)
			if name != "valid" && name != "conflicting hints" {
				if err == nil {
					t.Fatal("conflicting protected CRI identity selected executable content")
				}
				return
			}
			if err != nil || resolved.Target.Digest != target.Digest || resolved.Manifest.Config.Digest != manifest.Config.Digest {
				t.Fatal("protected target/config identity was not preserved", resolved, err)
			}
			if _, err := os.Stat(cacheRoot); err != nil {
				t.Fatal("native content cache was not exercised", err)
			}
		})
	}
}
