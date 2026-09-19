// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ResolvedBlobs is an artifact after separating the JVM launch config from its
// payload layers, normalized to on-disk paths. It is independent of which store
// the blobs were read from (the Brewlet-local OCI layout or containerd's content
// store) and of the delivery format (native artifact vs runnable image): both
// the local CLI (run/bundle) and the node shim resolve down to this shape before
// assembling a sandbox.
type ResolvedBlobs struct {
	Config              JVMConfig
	JarHostPath         string   // on-disk path of the JAR payload
	ClasspathHostPaths  []string // on-disk paths of the optional classpath layer tars
	ModulepathHostPaths []string // on-disk paths of the optional modulepath layer tars
	CDSHostPath         string   // on-disk path of the optional AppCDS archive, or ""
	ManifestDigest      string   // verified digest of the resolved platform manifest
	Format              string   // "native" or "image"
}

// BlobSource abstracts a content-addressed blob store so format resolution
// (index following + runnable-image layer staging) can be shared by both the
// Brewlet-local OCI layout and containerd's content store. Store already
// satisfies it via its ReadBlob/BlobPath methods.
type BlobSource interface {
	// ReadBlob returns the raw bytes of a blob by digest ("sha256:…").
	ReadBlob(digest string) ([]byte, error)
	// BlobPath returns the on-disk path of a blob by digest. It returns an
	// error rather than a path for any non-canonical digest, so an untrusted
	// descriptor cannot name a host path outside the store.
	BlobPath(digest string) (string, error)
}

// ResolveBlobs resolves a tagged ref in this local OCI layout to normalized
// on-disk blobs, transparently following an image index and handling BOTH a
// native Brewlet artifact and a runnable OCI image. It is the local (run/bundle)
// counterpart of the shim's content-store resolution; the manifest tells the two
// formats apart via Manifest.IsRunnableImage.
func (s Store) ResolveBlobs(ref string) (ResolvedBlobs, error) {
	man, digest, err := s.ResolveManifestByRef(ref)
	if err != nil {
		return ResolvedBlobs{}, fmt.Errorf("resolve artifact: %w", err)
	}
	if man.IsRunnableImage() {
		return ResolveRunnableBlobs(s, man, digest)
	}
	return ResolveNativeBlobs(s, man, digest)
}

// ResolveManifestFollowingIndex reads the blob at digest from src; when it is an
// OCI image index it selects the entry for the current Linux node's complete OCI
// platform and reads that platform manifest. Returns the resolved manifest and
// its digest. Works for both native artifacts and runnable images.
func ResolveManifestFollowingIndex(src BlobSource, digest string) (Manifest, string, error) {
	raw, err := ReadVerifiedBlob(src, Descriptor{Digest: digest})
	if err != nil {
		return Manifest{}, "", fmt.Errorf("read manifest blob: %w", err)
	}
	if IsIndexBlob(raw) {
		var idx Index
		if err := json.Unmarshal(raw, &idx); err != nil {
			return Manifest{}, "", fmt.Errorf("parse image index %s: %w", digest, err)
		}
		target := currentRunnablePlatform()
		sel, ok := idx.SelectPlatformManifest(target)
		if !ok {
			return Manifest{}, "", fmt.Errorf("image index %s has no manifest for %s", digest, platformName(target))
		}
		digest = sel.Digest
		if raw, err = ReadVerifiedBlob(src, sel); err != nil {
			return Manifest{}, "", fmt.Errorf("read platform manifest %s: %w", digest, err)
		}
	}
	var man Manifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return Manifest{}, "", fmt.Errorf("parse manifest %s: %w", digest, err)
	}
	return man, digest, nil
}

// ReadVerifiedBlob reads the blob a descriptor names and verifies it before
// returning it: the digest must be canonical (so it cannot name a path outside
// the store) and the bytes must hash to exactly that digest. Every descriptor
// read — manifest, index entry, config, or layer — goes through this function,
// because all of them come from attacker-controlled OCI metadata.
func ReadVerifiedBlob(src BlobSource, desc Descriptor) ([]byte, error) {
	if err := ValidateDigest(desc.Digest); err != nil {
		return nil, err
	}
	raw, err := src.ReadBlob(desc.Digest)
	if err != nil {
		return nil, err
	}
	if err := VerifyBytes(desc, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// ResolveNativeBlobs resolves a native Brewlet artifact (custom media-type
// layers) to on-disk blob paths, mounting each layer blob directly from src with
// no copy — the historical production/PoC path.
func ResolveNativeBlobs(src BlobSource, man Manifest, manifestDigest string) (ResolvedBlobs, error) {
	cb, err := ReadVerifiedBlob(src, man.Config)
	if err != nil {
		return ResolvedBlobs{}, fmt.Errorf("read config blob: %w", err)
	}
	cfg, err := DecodeConfig(cb)
	if err != nil {
		return ResolvedBlobs{}, fmt.Errorf("parse jvm config: %w", err)
	}
	layer, err := man.JarLayer()
	if err != nil {
		return ResolvedBlobs{}, err
	}
	jarPath, err := verifiedBlobPath(src, layer, "jar")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	cpPaths, err := verifiedBlobPaths(src, man.ClasspathLayers(), "classpath")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	mpPaths, err := verifiedBlobPaths(src, man.ModulepathLayers(), "modulepath")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	var cdsPath string
	if l, ok := man.CDSLayer(); ok {
		if cdsPath, err = verifiedBlobPath(src, l, "cds"); err != nil {
			return ResolvedBlobs{}, err
		}
	}
	return ResolvedBlobs{Config: cfg, JarHostPath: jarPath, ClasspathHostPaths: cpPaths, ModulepathHostPaths: mpPaths, CDSHostPath: cdsPath, ManifestDigest: manifestDigest, Format: "native"}, nil
}

// verifiedBlobPath resolves one layer descriptor to the host path that will be
// bind-mounted into the sandbox. The digest must be canonical (so the path
// cannot escape the store) and the bytes on disk must hash to it: these blobs
// are mounted rather than read, so this is the only point at which the content
// the workload executes is checked against the descriptor that named it.
func verifiedBlobPath(src BlobSource, layer Descriptor, kind string) (string, error) {
	// Validate before calling into BlobSource rather than relying on the
	// implementation to do it: the interface cannot force an implementation to
	// check, and a future backend that builds a path before validating would
	// silently reopen this hole.
	if err := ValidateDigest(layer.Digest); err != nil {
		return "", fmt.Errorf("%s layer: %w", kind, err)
	}
	p, err := src.BlobPath(layer.Digest)
	if err != nil {
		return "", fmt.Errorf("%s layer: %w", kind, err)
	}
	if err := VerifyFile(layer, p); err != nil {
		return "", fmt.Errorf("%s blob %s is not usable: %w", kind, layer.Digest, err)
	}
	return p, nil
}

// verifiedBlobPaths resolves and verifies each layer, in manifest order.
func verifiedBlobPaths(src BlobSource, layers []Descriptor, kind string) ([]string, error) {
	var out []string
	for _, l := range layers {
		p, err := verifiedBlobPath(src, l, kind)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ResolveRunnableBlobs resolves a runnable OCI image: the launch config comes
// from the manifest's brewlet.sh/jvm-config annotation, and the standard
// tar+gzip layers are staged (gunzipped) into an immutable per-image tree the sandbox
// reads from — the JAR (and optional CDS archive) as files, and the
// classpath/module layers as uncompressed tars the existing staging path
// consumes unchanged. Staging is idempotent (keyed on manifestDigest) so
// repeated resolutions of the same image reuse it.
func ResolveRunnableBlobs(src BlobSource, man Manifest, manifestDigest string) (ResolvedBlobs, error) {
	cfg, err := man.RunnableConfig()
	if err != nil {
		return ResolvedBlobs{}, err
	}
	stageDir, err := runnableStageDir(manifestDigest)
	if err != nil {
		return ResolvedBlobs{}, err
	}

	appLayer, err := man.RunnableAppLayer()
	if err != nil {
		return ResolvedBlobs{}, err
	}
	if _, err := os.Lstat(stageDir); err == nil {
		return reuseRunnableStage(src, cfg, man, manifestDigest, stageDir)
	} else if !os.IsNotExist(err) {
		return ResolvedBlobs{}, fmt.Errorf("read runnable image staging: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(stageDir), 0o755); err != nil {
		return ResolvedBlobs{}, err
	}
	pending, err := os.MkdirTemp(filepath.Dir(stageDir), "."+filepath.Base(stageDir)+"-")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	defer os.RemoveAll(pending)
	// CRI may discard packed layers after unpacking. Retain verified bytes in
	// the atomically published stage so later replicas can verify them again.
	retained := Store{Root: filepath.Join(pending, "content")}
	for _, layer := range runnableLayers(man, appLayer) {
		raw, err := ReadVerifiedBlob(src, layer)
		if err != nil {
			if os.IsNotExist(err) {
				return ResolvedBlobs{}, fmt.Errorf("runnable image layer %s is unavailable before verified staging; re-pull the digest-pinned image with packed-layer retention enabled: %w", layer.Digest, err)
			}
			return ResolvedBlobs{}, fmt.Errorf("retain runnable image layer: %w", err)
		}
		if _, err := retained.writeBlob(raw); err != nil {
			return ResolvedBlobs{}, fmt.Errorf("retain runnable image layer: %w", err)
		}
	}
	if err := extractGzTar(retained, appLayer, filepath.Join(pending, "app")); err != nil {
		return ResolvedBlobs{}, fmt.Errorf("stage app layer: %w", err)
	}
	if _, err := stageLayerTars(retained, man.RunnableClasspathLayers(), pending, "cp"); err != nil {
		return ResolvedBlobs{}, err
	}
	if _, err := stageLayerTars(retained, man.RunnableModulepathLayers(), pending, "mp"); err != nil {
		return ResolvedBlobs{}, err
	}
	if _, err := runnableStagedBlobs(cfg, man, manifestDigest, pending); err != nil {
		return ResolvedBlobs{}, err
	}
	if err := os.Chmod(pending, 0o755); err != nil {
		return ResolvedBlobs{}, err
	}
	// A complete, non-empty directory is published in one rename. Competing
	// processes cannot replace a published non-empty directory; losers discard
	// their private staging tree and use the winner without modifying its files.
	if err := os.Rename(pending, stageDir); err != nil {
		if _, statErr := os.Lstat(stageDir); statErr != nil {
			return ResolvedBlobs{}, fmt.Errorf("publish runnable image staging: %w", err)
		}
		return reuseRunnableStage(src, cfg, man, manifestDigest, stageDir)
	}
	return runnableStagedBlobs(cfg, man, manifestDigest, stageDir)
}

func reuseRunnableStage(src BlobSource, cfg JVMConfig, man Manifest, manifestDigest, stageDir string) (ResolvedBlobs, error) {
	appLayer, err := man.RunnableAppLayer()
	if err != nil {
		return ResolvedBlobs{}, err
	}
	retained := Store{Root: filepath.Join(stageDir, "content")}
	for _, layer := range runnableLayers(man, appLayer) {
		if _, err := ReadVerifiedBlob(retained, layer); err != nil {
			return ResolvedBlobs{}, fmt.Errorf("verify retained runnable image layer: %w", err)
		}
		if _, err := ReadVerifiedBlob(src, layer); err != nil && !os.IsNotExist(err) {
			return ResolvedBlobs{}, fmt.Errorf("verify cached runnable image layer: %w", err)
		}
	}
	return runnableStagedBlobs(cfg, man, manifestDigest, stageDir)
}

func runnableLayers(man Manifest, appLayer Descriptor) []Descriptor {
	layers := append([]Descriptor{appLayer}, man.RunnableClasspathLayers()...)
	return append(layers, man.RunnableModulepathLayers()...)
}

func runnableStagedBlobs(cfg JVMConfig, man Manifest, manifestDigest, stageDir string) (ResolvedBlobs, error) {
	appDir := filepath.Join(stageDir, "app")
	for _, dir := range []string{stageDir, appDir} {
		info, err := os.Lstat(dir)
		if err != nil {
			return ResolvedBlobs{}, fmt.Errorf("read runnable image staging: %w", err)
		}
		if !info.IsDir() {
			return ResolvedBlobs{}, fmt.Errorf("runnable image staging %q must be a directory, not a link or file", dir)
		}
	}
	jarName, err := MainJarName(cfg)
	if err != nil {
		return ResolvedBlobs{}, err
	}
	jarPath, err := stagedPath(appDir, jarName)
	if err != nil {
		return ResolvedBlobs{}, fmt.Errorf("runnable image jar: %w", err)
	}
	if err := requireStagedFile(jarPath); err != nil {
		return ResolvedBlobs{}, fmt.Errorf("runnable image app layer missing jar %q: %w", jarName, err)
	}
	var cdsPath string
	if cfg.CDS != nil && cfg.CDS.Archive != "" {
		cdsPath, err = stagedPath(appDir, cfg.CDS.Archive)
		if err != nil {
			return ResolvedBlobs{}, fmt.Errorf("runnable image cds archive: %w", err)
		}
		if err := requireStagedFile(cdsPath); err != nil {
			return ResolvedBlobs{}, fmt.Errorf("runnable image app layer missing cds archive %q: %w", cfg.CDS.Archive, err)
		}
	}

	cpPaths, err := stagedLayerPaths(man.RunnableClasspathLayers(), stageDir, "cp")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	mpPaths, err := stagedLayerPaths(man.RunnableModulepathLayers(), stageDir, "mp")
	if err != nil {
		return ResolvedBlobs{}, err
	}
	return ResolvedBlobs{Config: cfg, JarHostPath: jarPath, ClasspathHostPaths: cpPaths, ModulepathHostPaths: mpPaths, CDSHostPath: cdsPath, ManifestDigest: manifestDigest, Format: "image"}, nil
}

func requireStagedFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%q must be a regular staged file", path)
	}
	return nil
}

func stagedLayerPaths(layers []Descriptor, stageDir, prefix string) ([]string, error) {
	var paths []string
	for i := range layers {
		path := filepath.Join(stageDir, fmt.Sprintf("%s-%d.tar", prefix, i))
		if err := requireStagedFile(path); err != nil {
			return nil, fmt.Errorf("read staged %s layer: %w", prefix, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// stagedPath joins name onto dir and verifies the result is contained by dir.
// name is expected to have already been rejected by Validate if it is not a
// bare filename; the containment check is the independent second line of
// defense that makes the guarantee structural — every path this function
// returns is under the per-image staging directory, so a launch config that
// reached resolution without validation (or a future caller that forgets it)
// still cannot turn a bind-mount source into an arbitrary host path.
func stagedPath(dir, name string) (string, error) {
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("%q escapes the staging directory", name)
	}
	target := filepath.Join(dir, filepath.Clean(name))
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%q escapes the staging directory", name)
	}
	return target, nil
}

// runnableStageDir is the per-image staging directory a runnable image is
// gunzipped into. It is derived from the verified manifest digest so
// concurrent/repeated resolutions of the same image share one tree, and the
// digest is validated first so the directory name cannot escape the staging
// root. Overridable via BREWLET_RUNNABLE_STAGE for tests/harnesses.
func runnableStageDir(manifestDigest string) (string, error) {
	hex, err := DigestHex(manifestDigest)
	if err != nil {
		return "", fmt.Errorf("runnable image staging: %w", err)
	}
	base := os.Getenv("BREWLET_RUNNABLE_STAGE")
	if base == "" {
		base = filepath.Join(os.TempDir(), "brewlet-runnable")
	}
	// Previous stages may still be mounted and lack retained verification blobs.
	// Never rewrite or migrate a live stage during an upgrade.
	return filepath.Join(base, "immutable-v2", hex), nil
}

// stageLayerTars gunzips each layer blob to an uncompressed <prefix>-<i>.tar
// under stageDir and returns those paths (which the existing classpath/modulepath
// extraction consumes unchanged).
func stageLayerTars(src BlobSource, layers []Descriptor, stageDir, prefix string) ([]string, error) {
	if len(layers) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(layers))
	for i, l := range layers {
		dst := filepath.Join(stageDir, fmt.Sprintf("%s-%d.tar", prefix, i))
		if err := gunzipToFile(src, l, dst); err != nil {
			return nil, fmt.Errorf("stage %s layer %s: %w", prefix, l.Digest, err)
		}
		out = append(out, dst)
	}
	return out, nil
}

// gunzipToFile decompresses the verified gzip blob the descriptor names into dst.
func gunzipToFile(src BlobSource, desc Descriptor, dst string) error {
	raw, err := ReadVerifiedBlob(src, desc)
	if err != nil {
		return err
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer gr.Close()
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, gr); err != nil { //nolint:gosec // trusted layer content
		return err
	}
	return f.Close()
}

// extractGzTar gunzips the verified tar+gzip blob the descriptor names and
// unpacks its (flat) entries into destDir, rejecting any entry whose path would
// escape destDir.
func extractGzTar(src BlobSource, desc Descriptor, destDir string) error {
	raw, err := ReadVerifiedBlob(src, desc)
	if err != nil {
		return err
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer gr.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			_, err := io.Copy(io.Discard, gr)
			return err
		}
		if err != nil {
			return err
		}
		if !filepath.IsLocal(hdr.Name) {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}
		name := filepath.Clean(hdr.Name)
		if name == "." {
			continue
		}
		target := filepath.Join(destDir, name)
		rel, err := filepath.Rel(destDir, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // trusted layer content
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}
