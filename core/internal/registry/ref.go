// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package registry pushes OCI content from a local image layout to a remote
// OCI distribution registry. It mirrors the Maven plugin's credential chain
// and registry trust policy so `brewlet push` and `brewlet:push` behave alike.
package registry

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

// DockerHubRegistry is the API endpoint for references that name Docker Hub.
const DockerHubRegistry = "registry-1.docker.io"

// Reference is a parsed push target: registry authority, repository, and tag.
type Reference struct {
	// Name is the normalized repository name including the registry host,
	// e.g. myacr.azurecr.io/team/app.
	Name       string
	Registry   string
	Repository string
	Tag        string
}

// HasExplicitRegistry reports whether ref names its registry host explicitly
// (e.g. myacr.azurecr.io/app:1.0 or localhost:5000/app) rather than relying on
// the implicit Docker Hub default (app:1.0, team/app). It uses the same rule as
// the Maven plugin: the first path component must contain '.' or ':' or be
// "localhost".
func HasExplicitRegistry(ref string) bool {
	first, _, ok := strings.Cut(ref, "/")
	if !ok || first == "" {
		return false
	}
	return strings.ContainsAny(first, ".:") || first == "localhost"
}

// ParseReference parses a push target. It fails when the reference has no
// explicit registry host, so a push never silently defaults to Docker Hub,
// and rejects digest references because a push needs a tag to update.
func ParseReference(ref string) (Reference, error) {
	if !HasExplicitRegistry(ref) {
		return Reference{}, fmt.Errorf("image %q has no registry host; prefix it with your registry (e.g. myregistry.azurecr.io/%s), or use docker.io/<user>/... to target Docker Hub explicitly", ref, ref)
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return Reference{}, fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	if _, ok := named.(reference.Digested); ok {
		return Reference{}, fmt.Errorf("image %q is digest-pinned; push needs a tag (e.g. %s:1.0.0) and prints the pinned digest afterwards", ref, named.Name())
	}
	tag := "latest"
	if tagged, ok := named.(reference.Tagged); ok {
		tag = tagged.Tag()
	}
	host := reference.Domain(named)
	if host == "docker.io" || host == "index.docker.io" {
		host = DockerHubRegistry
	}
	return Reference{
		Name:       named.Name(),
		Registry:   host,
		Repository: reference.Path(named),
		Tag:        tag,
	}, nil
}

// String returns the tagged reference, e.g. myacr.azurecr.io/app:1.0.
func (r Reference) String() string { return r.Name + ":" + r.Tag }

// Pinned returns the digest-pinned deploy image, e.g.
// myacr.azurecr.io/app@sha256:....
func (r Reference) Pinned(digest string) string { return r.Name + "@" + digest }
