package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"github.com/microsoft/brewlet/pkg/artifact"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// newK8sRegistry opens a registry for ref. Tests replace it to point at an
// httptest server.
var newK8sRegistry = func(ref string, insecure bool) (*artifact.Registry, error) {
	r, err := artifact.NewRegistry(ref, insecure)
	if err != nil {
		return nil, err
	}
	r.Username = os.Getenv("BREWLET_REGISTRY_USERNAME")
	r.Password = os.Getenv("BREWLET_REGISTRY_PASSWORD")
	return r, nil
}

type k8sPlatform struct{ OS, Arch, Variant string }

func (p k8sPlatform) String() string {
	s := p.OS + "/" + p.Arch
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func parseK8sPlatform(s string) (k8sPlatform, error) {
	parts := strings.Split(s, "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		return k8sPlatform{OS: "linux", Arch: parts[0]}, nil
	case len(parts) == 2 && parts[0] != "" && parts[1] != "":
		return k8sPlatform{OS: parts[0], Arch: parts[1]}, nil
	case len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != "":
		return k8sPlatform{OS: parts[0], Arch: parts[1], Variant: parts[2]}, nil
	}
	return k8sPlatform{}, fmt.Errorf("invalid --platform %q (want os/arch[/variant])", s)
}

// k8sImageReport is the preflight result for a runtime image source.
type k8sImageReport struct {
	Input     string            `json:"input"`
	Canonical string            `json:"canonical"`
	Digest    string            `json:"digest"`
	MediaType string            `json:"mediaType,omitempty"`
	Platforms []string          `json:"platforms,omitempty"`
	Required  []string          `json:"required,omitempty"`
	Checked   map[string]string `json:"checked,omitempty"` // platform -> PATH
	User      string            `json:"user,omitempty"`    // first checked config's User
	Warnings  []string          `json:"warnings,omitempty"`
	Skipped   bool              `json:"skipped,omitempty"`
}

type ociDoc struct {
	MediaType string          `json:"mediaType"`
	Manifests []v1.Descriptor `json:"manifests"`
	Config    *v1.Descriptor  `json:"config"`
	Layers    []v1.Descriptor `json:"layers"`
}

const (
	dockerManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	dockerManifest     = "application/vnd.docker.distribution.manifest.v2+json"
	dockerConfig       = "application/vnd.docker.container.image.v1+json"
)

// k8sResolveImage resolves ref to a canonical repo@sha256 reference and runs
// the cheaply checkable golden-image requirements of SPEC §7.2 against the
// image index/manifest and config of every required platform.
func k8sResolveImage(ctx context.Context, ref string, required []k8sPlatform, insecure, skip bool) (*k8sImageReport, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return nil, fmt.Errorf("invalid image reference %q: %w", ref, err)
	}
	repo := reference.TrimNamed(named)
	rep := &k8sImageReport{Input: ref}
	for _, p := range required {
		rep.Required = append(rep.Required, p.String())
	}
	pinned, isDigest := named.(reference.Canonical)
	if skip {
		if !isDigest {
			return nil, fmt.Errorf("--skip-image-checks requires a digest reference (repo@sha256:...); tags cannot be resolved offline")
		}
		rep.Digest = pinned.Digest().String()
		rep.Canonical = repo.String() + "@" + rep.Digest
		rep.Skipped = true
		rep.Warnings = append(rep.Warnings, "registry checks skipped: platform coverage and image config were not verified")
		return rep, nil
	}
	key := "latest"
	if isDigest {
		key = pinned.Digest().String()
	} else if t, ok := named.(reference.Tagged); ok {
		key = t.Tag()
	}
	reg, err := newK8sRegistry(repo.String(), insecure)
	if err != nil {
		return nil, err
	}
	desc, body, err := reg.FetchManifest(ctx, key)
	if err != nil {
		if !isDigest {
			return nil, fmt.Errorf("cannot resolve tag %q to a digest: %w; RuntimeImage requires a canonical repo@sha256:<digest> source (SPEC §7.1) — pass a digest, or fix registry access (BREWLET_REGISTRY_USERNAME/PASSWORD, --insecure)", ref, err)
		}
		return nil, fmt.Errorf("fetch %s: %w (use --skip-image-checks to approve an unverifiable digest)", ref, err)
	}
	rep.Digest = desc.Digest.String()
	rep.Canonical = repo.String() + "@" + rep.Digest
	var doc ociDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%s: invalid manifest JSON: %w", ref, err)
	}
	mt := doc.MediaType
	if mt == "" {
		mt = desc.MediaType
	}
	rep.MediaType = mt
	type candidate struct {
		platform k8sPlatform
		desc     v1.Descriptor
	}
	var candidates []candidate
	switch {
	case doc.Manifests != nil || mt == v1.MediaTypeImageIndex || mt == dockerManifestList:
		for _, m := range doc.Manifests {
			if m.Platform == nil || m.Platform.OS == "unknown" || m.Platform.Architecture == "unknown" {
				continue // attestation/referrer entries
			}
			p := k8sPlatform{OS: m.Platform.OS, Arch: m.Platform.Architecture, Variant: m.Platform.Variant}
			candidates = append(candidates, candidate{p, m})
			rep.Platforms = append(rep.Platforms, p.String())
		}
	case doc.Config != nil:
		cfg, err := k8sFetchConfig(ctx, reg, *doc.Config)
		if err != nil {
			return nil, err
		}
		p := k8sPlatform{OS: cfg.OS, Arch: cfg.Architecture, Variant: cfg.Variant}
		rep.Platforms = append(rep.Platforms, p.String())
		candidates = append(candidates, candidate{p, desc})
	default:
		return nil, fmt.Errorf("%s: not an OCI image index or manifest (mediaType %q)", ref, mt)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%s: image declares no platforms", ref)
	}
	selected := map[string]candidate{}
	var missing []string
	for _, want := range required {
		i := slices.IndexFunc(candidates, func(c candidate) bool {
			return c.platform.OS == want.OS && c.platform.Arch == want.Arch && (want.Variant == "" || c.platform.Variant == want.Variant)
		})
		if i < 0 {
			missing = append(missing, want.String())
			continue
		}
		selected[candidates[i].platform.String()] = candidates[i]
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s does not cover required platform(s) %s (image provides %s); SPEC §7.2 requires every node architecture of the offering pools", ref, strings.Join(missing, ", "), strings.Join(rep.Platforms, ", "))
	}
	if len(required) == 0 {
		rep.Warnings = append(rep.Warnings, "no node architectures known; checked every linux platform in the image (use --platform to state requirements)")
		for _, c := range candidates {
			if c.platform.OS == "linux" {
				selected[c.platform.String()] = c
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("%s provides no linux platform", ref)
		}
	}
	rep.Checked = map[string]string{}
	keys := make([]string, 0, len(selected))
	for k := range selected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var problems []string
	for _, k := range keys {
		c := selected[k]
		if c.platform.OS != "linux" {
			problems = append(problems, k+": runtime images must be linux images")
			continue
		}
		cfg, err := k8sPlatformConfig(ctx, reg, c.desc)
		if err != nil {
			return nil, fmt.Errorf("%s (%s): %w", ref, k, err)
		}
		pathValue, warnings, errs := k8sGoldenConfig(cfg)
		for _, e := range errs {
			problems = append(problems, k+": "+e)
		}
		for _, w := range warnings {
			rep.Warnings = append(rep.Warnings, k+": "+w)
		}
		rep.Checked[k] = pathValue
		if rep.User == "" {
			rep.User = cfg.Config.User
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s violates golden image requirements (SPEC §7.2):\n  %s", ref, strings.Join(problems, "\n  "))
	}
	return rep, nil
}

func k8sPlatformConfig(ctx context.Context, reg *artifact.Registry, d v1.Descriptor) (*v1.Image, error) {
	_, b, err := reg.FetchManifest(ctx, d.Digest.String())
	if err != nil {
		return nil, err
	}
	var doc ociDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("invalid manifest JSON: %w", err)
	}
	if doc.Config == nil {
		return nil, fmt.Errorf("platform entry is not an image manifest")
	}
	if len(doc.Layers) == 0 {
		return nil, fmt.Errorf("image manifest has no layers")
	}
	return k8sFetchConfig(ctx, reg, *doc.Config)
}

func k8sFetchConfig(ctx context.Context, reg *artifact.Registry, d v1.Descriptor) (*v1.Image, error) {
	if d.MediaType != "" && d.MediaType != v1.MediaTypeImageConfig && d.MediaType != dockerConfig {
		return nil, fmt.Errorf("config media type %q is not an OCI image config", d.MediaType)
	}
	b, err := reg.FetchBlob(ctx, d.Digest, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("fetch image config: %w", err)
	}
	var cfg v1.Image
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("invalid image config: %w", err)
	}
	return &cfg, nil
}

// k8sGoldenConfig applies the config-level rules of SPEC §7.2: unique
// environment names and, when present, exactly one canonical absolute PATH
// outside /app with no empty or "." components. Fields the runtime ignores
// produce warnings.
func k8sGoldenConfig(cfg *v1.Image) (pathValue string, warnings, errs []string) {
	seen := map[string]bool{}
	for _, kv := range cfg.Config.Env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			errs = append(errs, fmt.Sprintf("malformed Env entry %q", kv))
			continue
		}
		if seen[name] {
			errs = append(errs, fmt.Sprintf("Env name %s is not unique", name))
			continue
		}
		seen[name] = true
		if name != "PATH" {
			continue
		}
		pathValue = value
		for _, dir := range strings.Split(value, ":") {
			switch {
			case dir == "" || dir == ".":
				errs = append(errs, fmt.Sprintf("PATH %q contains an empty or '.' component", value))
			case !path.IsAbs(dir) || path.Clean(dir) != dir:
				errs = append(errs, fmt.Sprintf("PATH entry %q is not a canonical absolute directory", dir))
			case dir == "/app" || strings.HasPrefix(dir, "/app/"):
				errs = append(errs, fmt.Sprintf("PATH entry %q lies under the reserved /app prefix", dir))
			default:
				continue
			}
			break
		}
	}
	if !seen["PATH"] {
		warnings = append(warnings, "no PATH in image config: only absolute executables can run (RuntimePATHRequired)")
	}
	var ignored []string
	if len(cfg.Config.Entrypoint) > 0 {
		ignored = append(ignored, "Entrypoint")
	}
	if len(cfg.Config.Cmd) > 0 {
		ignored = append(ignored, "Cmd")
	}
	if cfg.Config.WorkingDir != "" {
		ignored = append(ignored, "WorkingDir")
	}
	if len(cfg.Config.ExposedPorts) > 0 {
		ignored = append(ignored, "ExposedPorts")
	}
	if len(cfg.Config.Volumes) > 0 {
		ignored = append(ignored, "Volumes")
	}
	if len(ignored) > 0 {
		warnings = append(warnings, "config "+strings.Join(ignored, ", ")+" ignored: only Env is used (the application artifact and pod own the rest)")
	}
	return pathValue, warnings, errs
}

// k8sNumericUser returns uid/gid from an image config User of the form
// "uid[:gid]" when both are numeric and non-root.
func k8sNumericUser(user string) (uid, gid int64, ok bool) {
	u, g, hasGroup := strings.Cut(user, ":")
	uid, err := strconv.ParseInt(u, 10, 64)
	if err != nil || uid <= 0 {
		return 0, 0, false
	}
	gid = uid
	if hasGroup {
		if gid, err = strconv.ParseInt(g, 10, 64); err != nil || gid <= 0 {
			return 0, 0, false
		}
	}
	return uid, gid, true
}
