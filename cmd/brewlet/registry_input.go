package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/microsoft/brewlet/pkg/artifact"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type registryCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type registryOptions struct {
	Insecure    bool
	HTTPS       bool
	Credentials map[string]registryCredential
}

func (o registryOptions) registry(ref string) (*artifact.Registry, error) {
	registry, err := artifact.NewRegistry(ref, o.Insecure)
	if err != nil {
		return nil, err
	}
	if o.HTTPS {
		registry.Origin.Scheme = "https"
	}
	registry.Client.Timeout = 5 * time.Minute
	credential := o.Credentials[registry.Origin.String()]
	registry.Username, registry.Password = credential.Username, credential.Password
	return registry, nil
}

func pinnedInput(ctx context.Context, store artifact.Store, ref string, options registryOptions) (v1.Descriptor, error) {
	if !strings.Contains(ref, "@sha256:") {
		return v1.Descriptor{}, fmt.Errorf("digest-pinned input required: %s", ref)
	}
	local, target, isLocal, err := localLayout(ref)
	if err != nil {
		return target, err
	}
	if isLocal {
		err = artifact.CopyGraph(ctx, local, store, target)
		return target, err
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return target, err
	}
	if named.String() != ref {
		return target, fmt.Errorf("canonical digest reference required: %s", ref)
	}
	if _, ok := named.(reference.Digested); !ok {
		return target, fmt.Errorf("digest reference required")
	}
	if _, ok := named.(reference.Tagged); ok {
		return target, fmt.Errorf("tag plus digest is not a canonical pinned input")
	}
	registry, err := options.registry(ref)
	if err != nil {
		return target, err
	}
	return registry.Pull(ctx, store)
}

func atomicResult(file string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(file), ".brewlet-result-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), file)
}

func loadRegistries(authFile string, insecure, https bool) (registryOptions, error) {
	registries := registryOptions{Insecure: insecure, HTTPS: https, Credentials: map[string]registryCredential{}}
	if authFile != "" {
		raw, err := os.ReadFile(authFile)
		if err != nil {
			return registries, err
		}
		if err = artifact.StrictJSON(raw, &registries.Credentials); err != nil {
			return registries, err
		}
		if registries.Credentials == nil {
			return registries, fmt.Errorf("registry-auth must be a nonnull object")
		}
	}
	if username, password := os.Getenv("BREWLET_REGISTRY_USERNAME"), os.Getenv("BREWLET_REGISTRY_PASSWORD"); username != "" || password != "" {
		origin := os.Getenv("BREWLET_REGISTRY_ORIGIN")
		if origin == "" {
			return registries, fmt.Errorf("verify and inventory require BREWLET_REGISTRY_ORIGIN for environment credentials; alternatively use --registry-auth")
		}
		if _, exists := registries.Credentials[origin]; exists {
			return registries, fmt.Errorf("duplicate registry credential origin")
		}
		registries.Credentials[origin] = registryCredential{username, password}
	}
	for origin := range registries.Credentials {
		probe, err := artifact.NewRegistry(strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")+"/origin-check", insecure)
		if err != nil {
			return registries, err
		}
		if strings.HasPrefix(origin, "https://") {
			probe.Origin.Scheme = "https"
		}
		if probe.Origin.String() != origin {
			return registries, fmt.Errorf("credential key must be an exact registry origin")
		}
	}
	return registries, nil
}
