package artifact

import (
	"archive/tar"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type releaseFixture struct {
	store               Store
	app, set            v1.Descriptor
	publisher, composer *LayerSetSigner
	policy              ReleasePolicy
	platform            v1.Platform
}

func releaseAuthority(t *testing.T, signer *LayerSetSigner) ReleaseAuthority {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&signer.Key.PublicKey)
	must(t, err)
	return ReleaseAuthority{Identity: signer.Identity, Keys: []ReleaseKey{{
		PublicKey: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})),
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}}}
}

func releaseApplication(t *testing.T, store Store, platform v1.Platform, sources ...LayerSource) v1.Descriptor {
	t.Helper()
	launchJSON := []byte(`{"schemaVersion":2,"process":{"entrypoint":["/runtime"],"args":["/app/code"],"workingDir":"/app"},"runtimeImages":[]}`)
	launch, err := writeLayer(store, "launch", func(tw *tar.Writer) error {
		if err := tw.WriteHeader(fileHeader("launch.json", string(launchJSON), 0644)); err != nil {
			return err
		}
		_, err := tw.Write(launchJSON)
		return err
	})
	must(t, err)
	var descriptors []v1.Descriptor
	var diffs []digest.Digest
	for _, source := range sources {
		descriptors = append(descriptors, source.Descriptor)
		diffs = append(diffs, source.DiffID)
	}
	descriptors = append(descriptors, launch.Descriptor)
	diffs = append(diffs, launch.DiffID)
	parsed, err := ParseLaunch(launchJSON)
	must(t, err)
	config, err := store.PutJSON(v1.MediaTypeImageConfig, v1.Image{Platform: platform, Config: parsed.ImageConfig(), RootFS: v1.RootFS{Type: "layers", DiffIDs: diffs}})
	must(t, err)
	target, err := store.PutJSON(v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest, Config: config, Layers: descriptors, Annotations: map[string]string{"brewlet.sh/artifact-version": "2"}})
	must(t, err)
	target.Platform = &platform
	return target
}

func releaseSet(t *testing.T, store Store, version, payload string, signer *LayerSetSigner, locked bool) v1.Descriptor {
	t.Helper()
	layer := source(t, store, "gzip", []*tar.Header{fileHeader("app/lib/library", payload, 0644)}, []string{payload})
	layer.Descriptor.Annotations["brewlet.sh/layer"] = "dependency"
	config, err := store.PutJSON(MediaTypeLayerSetConfig, LayerSetConfig{SchemaVersion: 1, Name: "platform", Version: version,
		Ecosystem: "explicit", Layers: []LayerBinding{{layer.Descriptor.Digest, layer.DiffID}}})
	must(t, err)
	layers := []v1.Descriptor{layer.Descriptor}
	if locked {
		lock, err := store.PutJSON(MediaTypeLayerSetLock, LayerSetLock{SchemaVersion: 1, Entries: []LockEntry{{
			Ecosystem: "explicit", Name: "library", Version: version, Path: "/app/lib/library", SHA256: digest.FromString(payload).Encoded(),
			PURL: "pkg:generic/platform/library@" + version}}})
		must(t, err)
		layers = append(layers, lock)
	}
	target, err := store.PutJSON(v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest, ArtifactType: MediaTypeLayerSet, Config: config, Layers: layers})
	must(t, err)
	built, evidence, err := consumeLayerSetContent(context.Background(), store, target)
	must(t, err)
	inventory, hasLock, err := layerSetInventory(context.Background(), store, target, built)
	must(t, err)
	sbom, err := buildSBOM(inventory, hasLock)
	must(t, err)
	_, err = publishReferrer(store, target, MediaTypeCycloneDX, MediaTypeCycloneDX, sbom)
	must(t, err)
	if signer != nil {
		addSignature(t, store, target, LayerSetPredicate{SchemaVersion: 1, LayerSetDigest: target.Digest, ConfigDigest: evidence.ConfigDigest,
			LockDigest: evidence.LockDigest, SBOMDigest: digest.FromBytes(sbom), Layers: evidence.Layers}, signer)
	}
	return target
}

func newReleaseFixture(t *testing.T) releaseFixture {
	t.Helper()
	f := releaseFixture{store: Store{Dir: filepath.Join(testDir(t), "store")}, platform: v1.Platform{OS: "linux", Architecture: "arm64"},
		publisher: testSigner(t, "platform"), composer: testSigner(t, "release")}
	f.policy = ReleasePolicy{SchemaVersion: 1, Mode: "production", NotAfter: time.Now().Add(time.Hour),
		Publisher: releaseAuthority(t, f.publisher), Composer: releaseAuthority(t, f.composer)}
	app := source(t, f.store, "gzip", []*tar.Header{fileHeader("app/code", "unchanged application", 0644)}, []string{"unchanged application"})
	f.app = releaseApplication(t, f.store, f.platform, app)
	f.set = releaseSet(t, f.store, "1", "before", f.publisher, true)
	return f
}

func (f releaseFixture) options() ComposeOptions {
	return ComposeOptions{Application: f.app, DependencySet: f.set, Platform: f.platform, Policy: f.policy, Signer: f.composer}
}

func TestManagedCompositionPreservesBytesAndReplacesDependencies(t *testing.T) {
	ctx := context.Background()
	f := newReleaseFixture(t)
	first, evidence, err := Compose(ctx, f.store, f.options())
	must(t, err)
	if !evidence.Structural || !evidence.PublisherProvenance || !evidence.CompositionSignature || evidence.TestReleaseApproval != "not-evaluated" {
		t.Fatalf("ambiguous verification: %+v", evidence)
	}
	original, err := Resolve(ctx, f.store.Open, f.app, f.platform)
	must(t, err)
	resolved, err := Resolve(ctx, f.store.Open, first, f.platform)
	must(t, err)
	if !reflect.DeepEqual(original.LaunchJSON, resolved.LaunchJSON) || resolved.Launch.RuntimeImages == nil || len(resolved.Launch.RuntimeImages) != 0 {
		t.Fatal("launch bytes or explicit deny-all changed")
	}
	again, _, err := Compose(ctx, f.store, f.options())
	must(t, err)
	if again.Digest != first.Digest {
		t.Fatal("equivalent inputs changed output digest")
	}
	patch := releaseSet(t, f.store, "2", "after", f.publisher, true)
	options := f.options()
	options.Application, options.DependencySet = first, patch
	second, updated, err := Compose(ctx, f.store, options)
	must(t, err)
	if first.Digest == second.Digest || updated.Predicate.ParentComposition == "" ||
		updated.Predicate.ApplicationRoot.Digest != f.app.Digest ||
		!reflect.DeepEqual(evidence.Predicate.ApplicationLayers, updated.Predicate.ApplicationLayers) ||
		!reflect.DeepEqual(evidence.Predicate.EffectiveLaunch, updated.Predicate.EffectiveLaunch) ||
		evidence.Predicate.DependencyEvidence.Layers[0].Digest == updated.Predicate.DependencyEvidence.Layers[0].Digest {
		t.Fatal("replacement failed exact material preservation")
	}
	copy := Store{Dir: filepath.Join(testDir(t), "copy")}
	must(t, CopyGraph(ctx, f.store, copy, second))
	must(t, VerifyGraph(ctx, copy, second))
	verified, err := VerifyComposition(ctx, copy, second, f.platform, f.policy)
	must(t, err)
	if !verified.CompositionSignature {
		t.Fatal("copy lost verified history")
	}
	options.Launch = []byte(`{"schemaVersion":2,"process":{"entrypoint":["/runtime"],"args":["--explicit"]}}`)
	_, changed, err := Compose(ctx, f.store, options)
	must(t, err)
	if changed.Predicate.EffectiveLaunch.Descriptor.Digest == changed.Predicate.InputLaunch.Descriptor.Digest ||
		changed.Predicate.OriginalLaunch.Descriptor.Digest != launchLayer(original).Descriptor.Digest {
		t.Fatal("explicit launch replacement erased original launch provenance")
	}
}

func TestManagedCompositionRejectsUnsafeInputsAndPolicies(t *testing.T) {
	for name, mutate := range map[string]func(*releaseFixture, *ComposeOptions){
		"no-policy":      func(f *releaseFixture, o *ComposeOptions) { o.Policy = ReleasePolicy{} },
		"expired-policy": func(f *releaseFixture, o *ComposeOptions) { o.Policy.NotAfter = time.Now().Add(-time.Second) },
		"no-signer":      func(f *releaseFixture, o *ComposeOptions) { o.Signer = nil },
		"wrong-signer":   func(f *releaseFixture, o *ComposeOptions) { o.Signer = testSigner(t, "release") },
		"wrong-identity": func(f *releaseFixture, o *ComposeOptions) {
			o.Signer = &LayerSetSigner{Key: f.composer.Key, Identity: "intruder"}
		},
		"revoked-key": func(f *releaseFixture, o *ComposeOptions) { o.Policy.Composer.Keys[0].Revoked = true },
		"future-key": func(f *releaseFixture, o *ComposeOptions) {
			o.Policy.Publisher.Keys[0].NotBefore = time.Now().Add(time.Minute)
		},
		"expired-key": func(f *releaseFixture, o *ComposeOptions) {
			o.Policy.Publisher.Keys[0].NotAfter = time.Now().Add(-time.Minute)
		},
		"revoked-set": func(f *releaseFixture, o *ComposeOptions) {
			o.Policy.Publisher.RevokedSubjects = []digest.Digest{f.set.Digest}
		},
		"wrong-set-subject": func(f *releaseFixture, o *ComposeOptions) {
			o.Policy.Publisher.Subjects = []digest.Digest{digest.FromString("other")}
		},
		"unsigned-set": func(f *releaseFixture, o *ComposeOptions) {
			o.DependencySet = releaseSet(t, f.store, "unsigned", "before", nil, true)
		},
		"unlocked-set": func(f *releaseFixture, o *ComposeOptions) {
			o.DependencySet = releaseSet(t, f.store, "1", "before", f.publisher, false)
		},
		"wrong-platform": func(f *releaseFixture, o *ComposeOptions) { o.Platform.Architecture = "amd64" },
		"bad-launch":     func(f *releaseFixture, o *ComposeOptions) { o.Launch = []byte(`{"schemaVersion":2}`) },
		"unverified-dependency-stripping": func(f *releaseFixture, o *ComposeOptions) {
			dep := source(t, f.store, "gzip", []*tar.Header{fileHeader("app/other", "bundled", 0644)}, []string{"bundled"})
			dep.Descriptor.Annotations["brewlet.sh/layer"] = "dependency"
			app := source(t, f.store, "gzip", []*tar.Header{fileHeader("app/code", "app", 0644)}, []string{"app"})
			o.Application = releaseApplication(t, f.store, f.platform, dep, app)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReleaseFixture(t)
			options := f.options()
			mutate(&f, &options)
			target, _, err := Compose(context.Background(), f.store, options)
			if err == nil || target.Digest != "" {
				t.Fatalf("unsafe input accepted: %+v %v", target, err)
			}
		})
	}
}

func TestManagedCompositionProtectsPaths(t *testing.T) {
	for name, header := range map[string]*tar.Header{
		"same-bytes":         fileHeader("app/lib/library", "before", 0644),
		"shadow":             fileHeader("app/lib/library", "replacement", 0644),
		"ancestor-file":      fileHeader("app/lib", "replacement", 0644),
		"file-whiteout":      fileHeader("app/lib/.wh.library", "", 0),
		"directory-whiteout": fileHeader("app/.wh.lib", "", 0),
		"opaque-directory":   fileHeader("app/lib/.wh..wh..opq", "", 0),
		"ancestor-symlink":   {Name: "app/lib", Mode: 0777, Typeflag: tar.TypeSymlink, Linkname: "other"},
		"file-to-directory":  {Name: "app/lib/library", Mode: 0755, Typeflag: tar.TypeDir},
	} {
		t.Run(name, func(t *testing.T) {
			f := newReleaseFixture(t)
			payload := ""
			if header.Size > 0 {
				if name == "same-bytes" {
					payload = "before"
				} else {
					payload = "replacement"
				}
			}
			app := source(t, f.store, "gzip", []*tar.Header{header}, []string{payload})
			options := f.options()
			options.Application = releaseApplication(t, f.store, f.platform, app)
			_, _, err := Compose(context.Background(), f.store, options)
			if Reason(err) != "ManagedPathConflict" {
				t.Fatalf("managed collision did not fail precisely: %v", err)
			}
		})
	}
}

func TestManagedCompositionDevelopmentDoesNotDowngradeSignatures(t *testing.T) {
	f := newReleaseFixture(t)
	options := f.options()
	options.Policy.Mode = "development"
	options.Signer = nil
	options.DependencySet = releaseSet(t, f.store, "dev", "unsigned", nil, true)
	target, inspection, err := Compose(context.Background(), f.store, options)
	must(t, err)
	if !inspection.Structural || inspection.CompositionSignature || inspection.PublisherProvenance || inspection.PolicyMode != "development" {
		t.Fatal("unsigned development evidence mislabeled")
	}
	options.Application = target
	if _, _, err = Compose(context.Background(), f.store, options); err == nil {
		t.Fatal("unsigned composition authorized layer stripping")
	}
	bad := []byte(`{"payloadType":"bad","payload":"","signatures":[]}`)
	_, err = publishReferrer(f.store, target, MediaTypeComposition, MediaTypeDSSE, bad)
	must(t, err)
	if _, err = VerifyComposition(context.Background(), f.store, target, f.platform, options.Policy); err == nil {
		t.Fatal("present invalid signature downgraded to unsigned")
	}
}

func TestManagedCompositionPredicateBindings(t *testing.T) {
	f := newReleaseFixture(t)
	target, inspection, err := Compose(context.Background(), f.store, f.options())
	must(t, err)
	index, err := f.store.referrerIndex(target.Digest)
	must(t, err)
	original, err := os.ReadFile(index)
	must(t, err)
	for name, mutate := range map[string]func(*CompositionPredicate){
		"version":         func(p *CompositionPredicate) { p.SchemaVersion = 2 },
		"output":          func(p *CompositionPredicate) { p.Output = digest.FromString("wrong") },
		"platform":        func(p *CompositionPredicate) { p.Platform.Architecture = "amd64" },
		"source-root":     func(p *CompositionPredicate) { p.SourceRoot.Digest = digest.FromString("wrong") },
		"source-manifest": func(p *CompositionPredicate) { p.SourceManifest.Digest = digest.FromString("wrong") },
		"source-platform-descriptor": func(p *CompositionPredicate) {
			p.SourceManifest.Platform = &v1.Platform{OS: "linux", Architecture: "amd64"}
		},
		"app-root":          func(p *CompositionPredicate) { p.ApplicationRoot.Digest = digest.FromString("wrong") },
		"app-layer":         func(p *CompositionPredicate) { p.ApplicationLayers[0].DiffID = digest.FromString("wrong") },
		"dependency":        func(p *CompositionPredicate) { p.DependencySet.Digest = digest.FromString("wrong") },
		"sbom":              func(p *CompositionPredicate) { p.DependencyEvidence.SBOMDigest = digest.FromString("wrong") },
		"original-launch":   func(p *CompositionPredicate) { p.OriginalLaunch.DiffID = digest.FromString("wrong") },
		"input-launch":      func(p *CompositionPredicate) { p.InputLaunch.DiffID = digest.FromString("wrong") },
		"effective-launch":  func(p *CompositionPredicate) { p.EffectiveLaunch.DiffID = digest.FromString("wrong") },
		"config":            func(p *CompositionPredicate) { p.OutputConfig.Digest = digest.FromString("wrong") },
		"config-descriptor": func(p *CompositionPredicate) { p.OutputConfig.Annotations = map[string]string{"wrong": "binding"} },
		"parent":            func(p *CompositionPredicate) { p.ParentComposition = digest.FromString("wrong") },
		"identity":          func(p *CompositionPredicate) { p.BuilderIdentity = "intruder" },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() { must(t, os.WriteFile(index, original, 0644)) }()
			must(t, os.Remove(index))
			raw, err := json.Marshal(inspection.Predicate)
			must(t, err)
			var predicate CompositionPredicate
			must(t, StrictJSON(raw, &predicate))
			mutate(&predicate)
			payload, err := json.Marshal(compositionStatement{"https://in-toto.io/Statement/v1",
				[]statementSubject{{"application", map[string]string{"sha256": target.Digest.Encoded()}}}, CompositionPredicateType, predicate})
			must(t, err)
			signed, err := signDSSE(payload, f.composer)
			must(t, err)
			_, err = publishReferrer(f.store, target, MediaTypeComposition, MediaTypeDSSE, signed)
			must(t, err)
			if _, err = VerifyComposition(context.Background(), f.store, target, f.platform, f.policy); err == nil {
				t.Fatal("tampered composition accepted")
			}
		})
	}
}

func TestReleasePolicyStrictnessAndRotation(t *testing.T) {
	f := newReleaseFixture(t)
	raw, err := json.Marshal(f.policy)
	must(t, err)
	_, err = ParseReleasePolicy(raw)
	must(t, err)
	for _, bad := range []string{`{"schemaVersion":1,"SchemaVersion":1}`, `{"schemaVersion":1,"schemaVersion":1}`, `null`, `{"schemaVersion":1,"unknown":true}`} {
		if _, err := ParseReleasePolicy([]byte(bad)); err == nil {
			t.Fatalf("bad policy accepted: %s", bad)
		}
	}
	newSigner := testSigner(t, "release")
	f.policy.Composer.Keys = append(f.policy.Composer.Keys, releaseAuthority(t, newSigner).Keys...)
	f.policy.Composer.Keys[0].Revoked = true
	options := f.options()
	options.Signer = newSigner
	target, _, err := Compose(context.Background(), f.store, options)
	must(t, err)
	_, err = VerifyComposition(context.Background(), f.store, target, f.platform, f.policy)
	must(t, err)
	f.policy.Composer.RevokedSubjects = []digest.Digest{target.Digest}
	if _, err = VerifyComposition(context.Background(), f.store, target, f.platform, f.policy); err == nil {
		t.Fatal("revoked output accepted")
	}
}
