package artifact

import (
	"archive/tar"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	MediaTypeLayerSet       = "application/vnd.brewlet.layerset.v1+json"
	MediaTypeLayerSetConfig = "application/vnd.brewlet.layerset.config.v1+json"
	MediaTypeLayerSetLock   = "application/vnd.brewlet.layerset.lock.v1+json"
)

type LayerBinding struct {
	Digest digest.Digest `json:"digest"`
	DiffID digest.Digest `json:"diffId"`
}
type LayerSetConfig struct {
	SchemaVersion           int            `json:"schemaVersion"`
	Name                    string         `json:"name"`
	Version                 string         `json:"version"`
	Ecosystem               string         `json:"ecosystem"`
	Layers                  []LayerBinding `json:"layers"`
	CompatibleRuntimeImages []string       `json:"compatibleRuntimeImages,omitempty"`
}
type LockEntry struct {
	Ecosystem string `json:"ecosystem"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Group     string `json:"group,omitempty"`
	PURL      string `json:"purl,omitempty"`
}
type LayerSetLock struct {
	SchemaVersion int         `json:"schemaVersion"`
	Entries       []LockEntry `json:"entries"`
}
type LayerSetEvidence struct {
	ManifestDigest    digest.Digest  `json:"manifestDigest"`
	ConfigDigest      digest.Digest  `json:"configDigest"`
	LockDigest        digest.Digest  `json:"lockDigest,omitempty"`
	SBOMDigest        digest.Digest  `json:"sbomDigest"`
	AttestationDigest digest.Digest  `json:"attestationDigest,omitempty"`
	Layers            []LayerBinding `json:"layers"`
}

// ConsumeLayerSet validates dependency layers without rewriting a single byte.
// Registry import copies the entire graph into store before calling this API.
func consumeLayerSetContent(ctx context.Context, store Store, target v1.Descriptor) ([]BuiltLayer, LayerSetEvidence, error) {
	ev := LayerSetEvidence{ManifestDigest: target.Digest}
	b, e := store.Read(target)
	if e != nil {
		return nil, ev, e
	}
	var m v1.Manifest
	if e = StrictJSON(b, &m); e != nil {
		return nil, ev, e
	}
	if m.SchemaVersion != 2 || m.MediaType != v1.MediaTypeImageManifest || m.ArtifactType != MediaTypeLayerSet || m.Config.MediaType != MediaTypeLayerSetConfig {
		return nil, ev, fail("UnsupportedArtifact", "invalid layer-set manifest")
	}
	b, e = store.Read(m.Config)
	if e != nil {
		return nil, ev, e
	}
	var config LayerSetConfig
	if e = StrictJSON(b, &config); e != nil {
		return nil, ev, e
	}
	if config.SchemaVersion != 1 || config.Name == "" || config.Version == "" || config.Ecosystem == "" || len(config.Layers) == 0 {
		return nil, ev, fmt.Errorf("invalid layer-set config")
	}
	ids := map[string]bool{}
	for _, id := range config.CompatibleRuntimeImages {
		if e = ValidateRuntimeImageID(id); e != nil {
			return nil, ev, e
		}
		if ids[id] {
			return nil, ev, fmt.Errorf("duplicate compatible runtime")
		}
		ids[id] = true
	}
	ev.ConfigDigest = m.Config.Digest
	ev.Layers = config.Layers
	var result []BuiltLayer
	var lock *LayerSetLock
	for _, d := range m.Layers {
		if d.MediaType == MediaTypeLayerSetLock {
			if lock != nil {
				return nil, ev, fmt.Errorf("duplicate layer-set lock")
			}
			b, e = store.Read(d)
			if e != nil {
				return nil, ev, e
			}
			lock = &LayerSetLock{}
			if e = StrictJSON(b, lock); e != nil {
				return nil, ev, e
			}
			ev.LockDigest = d.Digest
			continue
		}
		i := len(result)
		if d.MediaType != v1.MediaTypeImageLayerGzip || i >= len(config.Layers) || d.Digest != config.Layers[i].Digest || d.Annotations["brewlet.sh/layer"] != "dependency" {
			return nil, ev, fmt.Errorf("layer-set binding mismatch")
		}
		desc := d
		src := LayerSource{Descriptor: d, DiffID: config.Layers[i].DiffID, Open: func() (io.ReadCloser, error) { return store.Open(desc) }}
		_, e = WalkLayer(ctx, src, nil)
		if e != nil {
			return nil, ev, e
		}
		result = append(result, BuiltLayer{d, src.DiffID})
	}
	if len(result) != len(config.Layers) {
		return nil, ev, fmt.Errorf("missing layer-set layers")
	}
	contents, e := layerSetFiles(ctx, store, result)
	if e != nil {
		return nil, ev, e
	}
	if lock != nil {
		if lock.SchemaVersion != 1 {
			return nil, ev, fmt.Errorf("invalid layer-set lock version")
		}
		seen := map[string]bool{}
		for _, entry := range lock.Entries {
			p := strings.TrimPrefix(entry.Path, "/")
			if _, e = archivePath(p); e != nil {
				return nil, ev, e
			}
			sha, e := hex.DecodeString(entry.SHA256)
			if e != nil || len(sha) != 32 || entry.SHA256 != strings.ToLower(entry.SHA256) || entry.Ecosystem == "" || entry.Name == "" || entry.Version == "" || strings.ContainsAny(entry.Group+entry.PURL, "\x00") || entry.PURL != "" && !strings.HasPrefix(entry.PURL, "pkg:") || seen[p] || contents[p] != entry.SHA256 {
				return nil, ev, fmt.Errorf("layer-set lock mismatch: %s", entry.Path)
			}
			seen[p] = true
		}
		// Lock inventories are complete, not an unauthenticated partial hint.
		for p := range contents {
			if _, wh, _ := whiteout(p); !wh && !seen[p] {
				return nil, ev, fmt.Errorf("unlocked layer-set file %s", p)
			}
		}
	}
	return result, ev, nil
}
func ConsumeLayerSet(ctx context.Context, store Store, target v1.Descriptor, trusts ...LayerSetTrust) ([]BuiltLayer, LayerSetEvidence, error) {
	layers, ev, e := consumeLayerSetContent(ctx, store, target)
	if e != nil {
		return nil, ev, e
	}
	inventory, hasLock, e := layerSetInventory(ctx, store, target, layers)
	if e != nil {
		return nil, ev, e
	}
	var trust LayerSetTrust
	if len(trusts) > 1 {
		return nil, ev, fmt.Errorf("one layer-set trust configuration required")
	}
	if len(trusts) == 1 {
		trust = trusts[0]
	}
	e = verifyLayerSetReferrers(ctx, store, target, inventory, hasLock, &ev, trust)
	if e != nil {
		return nil, ev, e
	}
	return layers, ev, nil
}
func BuildLayerSet(ctx context.Context, store Store, config LayerSetConfig, layers []LayerSpec, lock *LayerSetLock, options ...LayerSetPublishOptions) (v1.Descriptor, error) {
	var option LayerSetPublishOptions
	if len(options) > 1 {
		return v1.Descriptor{}, fmt.Errorf("one layer-set publication configuration required")
	}
	if len(options) == 1 {
		option = options[0]
	}
	config.SchemaVersion = 1
	config.Layers = nil
	var descs []v1.Descriptor
	for _, spec := range layers {
		if spec.Platform != nil {
			return v1.Descriptor{}, fail("InvalidPlatform", "layer sets do not support platform-scoped layers")
		}
		spec.Role = "dependency"
		b, e := BuildLayer(ctx, store, spec)
		if e != nil {
			return v1.Descriptor{}, e
		}
		descs = append(descs, b.Descriptor)
		config.Layers = append(config.Layers, LayerBinding{b.Descriptor.Digest, b.DiffID})
	}
	config.CompatibleRuntimeImages = slices.Clone(config.CompatibleRuntimeImages)
	sort.Strings(config.CompatibleRuntimeImages)
	if lock != nil {
		copy := *lock
		copy.Entries = slices.Clone(lock.Entries)
		lock = &copy
		lock.SchemaVersion = 1
		sort.Slice(lock.Entries, func(i, j int) bool { return lock.Entries[i].Path < lock.Entries[j].Path })
		d, e := store.PutJSON(MediaTypeLayerSetLock, lock)
		if e != nil {
			return v1.Descriptor{}, e
		}
		descs = append(descs, d)
	}
	cd, e := store.PutJSON(MediaTypeLayerSetConfig, config)
	if e != nil {
		return v1.Descriptor{}, e
	}
	m := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: MediaTypeLayerSet, Config: cd, Layers: descs}
	d, e := store.PutJSON(v1.MediaTypeImageManifest, m)
	if e != nil {
		return v1.Descriptor{}, e
	}
	built, ev, e := consumeLayerSetContent(ctx, store, d)
	if e != nil {
		return d, e
	}
	inventory, hasLock, e := layerSetInventory(ctx, store, d, built)
	if e != nil {
		return d, e
	}
	sbom := option.SBOM
	if len(sbom) == 0 {
		sbom, e = buildSBOM(inventory, hasLock)
		if e != nil {
			return d, e
		}
	}
	if e = validateSBOM(sbom, inventory, hasLock); e != nil {
		return d, e
	}
	if _, e = publishReferrer(store, d, MediaTypeCycloneDX, MediaTypeCycloneDX, sbom); e != nil {
		return d, e
	}
	trust := option.Trust
	if option.Signer != nil {
		predicate := LayerSetPredicate{SchemaVersion: 1, LayerSetDigest: d.Digest, ConfigDigest: ev.ConfigDigest, Layers: ev.Layers, LockDigest: ev.LockDigest, SBOMDigest: digest.FromBytes(sbom)}
		envelope, e := signLayerSet(predicate, option.Signer)
		if e != nil {
			return d, e
		}
		if _, e = publishReferrer(store, d, MediaTypeAttestation, MediaTypeDSSE, envelope); e != nil {
			return d, e
		}
		trust = LayerSetTrust{PublicKeys: []*ecdsa.PublicKey{&option.Signer.Key.PublicKey}, ExpectedIdentity: option.Signer.Identity}
	}
	_, _, e = ConsumeLayerSet(ctx, store, d, trust)
	return d, e
}

func layerSetFiles(ctx context.Context, store Store, layers []BuiltLayer) (map[string]string, error) {
	var sources []LayerSource
	for _, layer := range layers {
		d := layer.Descriptor
		sources = append(sources, LayerSource{Descriptor: d, DiffID: layer.DiffID, Open: func() (io.ReadCloser, error) { return store.Open(d) }})
	}
	if e := ValidateComposition(ctx, sources); e != nil {
		return nil, e
	}
	files := map[string]string{}
	for _, layer := range layers {
		d := layer.Descriptor
		src := LayerSource{Descriptor: d, DiffID: layer.DiffID, Open: func() (io.ReadCloser, error) { return store.Open(d) }}
		info, e := WalkLayer(ctx, src, nil)
		if e != nil {
			return nil, e
		}
		compiled, e := compileLayer(info)
		if e != nil {
			return nil, e
		}
		hashes := map[string]string{}
		i := 0
		_, e = WalkLayer(ctx, src, func(h *tar.Header, r io.Reader) error {
			entry := compiled.entries[i]
			i++
			if _, wh, _ := whiteout(entry.Path); wh {
				return nil
			}
			if h.Typeflag == tar.TypeReg {
				dg := digest.SHA256.Digester()
				if _, e := io.Copy(dg.Hash(), r); e != nil {
					return e
				}
				hashes[entry.Path] = dg.Digest().Encoded()
			}
			return nil
		})
		if e != nil {
			return nil, e
		}
		for _, entry := range compiled.entries {
			if entry.Type == tar.TypeLink {
				hashes[entry.Path] = hashes[entry.Link]
			}
		}
		remove := func(target string, childrenOnly bool) {
			for p := range files {
				if p == target && !childrenOnly || strings.HasPrefix(p, target+"/") {
					delete(files, p)
				}
			}
		}
		for _, entry := range compiled.entries {
			target, wh, opaque := whiteout(entry.Path)
			if wh {
				remove(target, opaque)
			}
		}
		names := make([]string, 0, len(compiled.nodes))
		for p := range compiled.nodes {
			names = append(names, p)
		}
		sort.Strings(names)
		for _, p := range names {
			node := compiled.nodes[p]
			if node.Type == tar.TypeDir {
				delete(files, p)
				continue
			}
			remove(p, false)
			if node.Type == tar.TypeReg {
				hash, ok := hashes[p]
				if !ok || hash == "" {
					return nil, fmt.Errorf("missing layer-set file hash")
				}
				files[p] = hash
			}
		}
	}
	return files, nil
}
func layerSetInventory(ctx context.Context, store Store, target v1.Descriptor, layers []BuiltLayer) ([]LockEntry, bool, error) {
	b, e := store.Read(target)
	if e != nil {
		return nil, false, e
	}
	var manifest v1.Manifest
	if e = StrictJSON(b, &manifest); e != nil {
		return nil, false, e
	}
	for _, d := range manifest.Layers {
		if d.MediaType == MediaTypeLayerSetLock {
			b, e := store.Read(d)
			if e != nil {
				return nil, false, e
			}
			var lock LayerSetLock
			if e = StrictJSON(b, &lock); e != nil {
				return nil, false, e
			}
			entries := slices.Clone(lock.Entries)
			for i := range entries {
				entries[i].Path = "/" + strings.TrimPrefix(entries[i].Path, "/")
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
			return entries, true, nil
		}
	}
	b, e = store.Read(manifest.Config)
	if e != nil {
		return nil, false, e
	}
	var config LayerSetConfig
	if e = StrictJSON(b, &config); e != nil {
		return nil, false, e
	}
	files, e := layerSetFiles(ctx, store, layers)
	if e != nil {
		return nil, false, e
	}
	entries := make([]LockEntry, 0, len(files))
	for p, hash := range files {
		entries = append(entries, LockEntry{Ecosystem: config.Ecosystem, Name: p, Path: "/" + p, SHA256: hash})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, false, nil
}

// ValidateEvidence checks that evidence references only dependency descriptors
// actually present in the application manifest.
func ValidateEvidence(m v1.Manifest) error {
	raw := m.Annotations["brewlet.sh/layerset-evidence"]
	if raw == "" {
		return nil
	}
	var evidence []LayerSetEvidence
	if e := StrictJSON([]byte(raw), &evidence); e != nil {
		return e
	}
	for _, ev := range evidence {
		if ev.ManifestDigest.Validate() != nil || ev.ConfigDigest.Validate() != nil || ev.SBOMDigest.Validate() != nil || ev.AttestationDigest != "" && ev.AttestationDigest.Validate() != nil || ev.LockDigest != "" && ev.LockDigest.Validate() != nil || len(ev.Layers) == 0 {
			return fmt.Errorf("invalid layer-set evidence")
		}
		for _, binding := range ev.Layers {
			if binding.Digest.Validate() != nil || binding.DiffID.Validate() != nil {
				return fmt.Errorf("invalid layer-set layer binding")
			}
			found := false
			for _, d := range m.Layers {
				if d.Digest == binding.Digest && d.Annotations["brewlet.sh/layer"] == "dependency" {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("unbound layer-set evidence")
			}
		}
	}
	return nil
}

func validateEvidenceDiffIDs(m v1.Manifest, image v1.Image) error {
	raw := m.Annotations["brewlet.sh/layerset-evidence"]
	if raw == "" {
		return nil
	}
	var evidence []LayerSetEvidence
	if e := StrictJSON([]byte(raw), &evidence); e != nil {
		return e
	}
	for _, ev := range evidence {
		for _, binding := range ev.Layers {
			found := false
			for i, d := range m.Layers {
				if d.Digest == binding.Digest && d.Annotations["brewlet.sh/layer"] == "dependency" && i < len(image.RootFS.DiffIDs) && image.RootFS.DiffIDs[i] == binding.DiffID {
					found = true
				}
			}
			if !found {
				return fail("UnsupportedArtifact", "layer-set evidence diff ID mismatch")
			}
		}
	}
	return nil
}
