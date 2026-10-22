package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/node"
	brewletplatform "github.com/microsoft/brewlet/pkg/platform"
	digest "github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

func inspect(ctx context.Context, id string) (map[string]string, error) {
	client, err := containerd.New("/run/containerd/containerd.sock")
	if err != nil {
		return nil, err
	}
	defer client.Close()
	guard, err := node.AcquireCacheGuard(ctx, "/var/lib/brewlet/layers", false)
	if err != nil {
		return nil, err
	}
	defer guard.Close()
	container, err := client.LoadContainer(ctx, id)
	if err != nil {
		return nil, err
	}
	info, err := container.Info(ctx)
	if err != nil {
		return nil, err
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return nil, err
	}
	extension, ok := info.Extensions["io.cri-containerd.container.metadata"]
	if !ok || info.Image == "" {
		return nil, fmt.Errorf("actual protected CRI metadata missing")
	}
	var envelope struct {
		Version  string
		Metadata struct {
			ImageRef string
			Config   struct {
				Image struct {
					Image     string `json:"image"`
					Requested string `json:"user_specified_image"`
				} `json:"image"`
			} `json:"config"`
		}
	}
	if err := json.Unmarshal(extension.GetValue(), &envelope); err != nil {
		return nil, err
	}
	cri := envelope.Metadata
	ref, err := reference.ParseNormalizedNamed(cri.Config.Image.Requested)
	if err != nil {
		return nil, err
	}
	canonical, ok := ref.(reference.Canonical)
	if !ok {
		return nil, fmt.Errorf("actual CRI requested image is not pinned")
	}
	_, tagged := ref.(reference.Tagged)
	if tagged || canonical.Digest().Algorithm() != digest.SHA256 || canonical.Digest().Validate() != nil ||
		envelope.Version != "v1" || cri.ImageRef == "" || cri.Config.Image.Image != cri.ImageRef ||
		spec.Annotations["io.kubernetes.cri.image-name"] != canonical.String() {
		return nil, fmt.Errorf("actual protected CRI config, requested image and stamp disagree")
	}
	cache := &artifact.ContentCache{Dir: filepath.Join("/var/lib/brewlet/layers", ".content"),
		IsMissing: func(err error) bool { return errdefs.IsNotFound(err) || os.IsNotExist(err) }}
	store := client.ContentStore()
	content, err := store.Info(ctx, canonical.Digest())
	target := oci.Descriptor{Digest: canonical.Digest(), Size: content.Size}
	if errdefs.IsNotFound(err) {
		target, err = cache.Target(ctx, canonical.Digest())
	}
	if err != nil {
		return nil, err
	}
	source := func(value oci.Descriptor) (io.ReadCloser, error) {
		reader, err := store.ReaderAt(ctx, value)
		if err != nil {
			return nil, err
		}
		return struct {
			io.Reader
			io.Closer
		}{io.NewSectionReader(reader, 0, reader.Size()), reader}, nil
	}
	open := func(descriptor oci.Descriptor) (io.ReadCloser, error) {
		return cache.Open(ctx, source, descriptor)
	}
	if target.Size <= 0 || target.Size > 16<<20 {
		return nil, fmt.Errorf("actual target metadata size is invalid")
	}
	reader, err := source(target)
	if errdefs.IsNotFound(err) {
		retained, retainedErr := cache.Target(ctx, canonical.Digest())
		if retainedErr != nil {
			return nil, retainedErr
		}
		if retained.Size != target.Size {
			return nil, fmt.Errorf("retained target differs from actual protected content size")
		}
		target = retained
		reader, err = open(target)
	}
	if err != nil {
		return nil, err
	}
	var header struct{ MediaType string }
	err = json.NewDecoder(io.LimitReader(reader, 16<<20)).Decode(&header)
	reader.Close()
	if err != nil {
		return nil, err
	}
	target.MediaType = header.MediaType
	resolved, err := artifact.Resolve(ctx, open, target, brewletplatform.Native())
	if err != nil {
		return nil, err
	}
	if resolved.Manifest.Config.Digest.String() != cri.ImageRef {
		return nil, fmt.Errorf("native selected config differs from actual protected CRI identity")
	}
	return map[string]string{"containerID": id, "reference": canonical.String(),
		"targetDigest": canonical.Digest().String(), "configDigest": cri.ImageRef,
		"architecture": brewletplatform.Native().Architecture}, nil
}

func main() {
	if len(os.Args) != 2 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(os.Args[1]) {
		fmt.Fprintln(os.Stderr, "exact owned container ID required")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(namespaces.WithNamespace(context.Background(), "k8s.io"), time.Minute)
	defer cancel()
	value, err := inspect(ctx, os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
