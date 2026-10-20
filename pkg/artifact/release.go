package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"time"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	MediaTypeComposition     = "application/vnd.brewlet.composition.v1+json"
	CompositionPredicateType = "https://brewlet.sh/attestations/composition/v1"
)

type CompositionPredicate struct {
	SchemaVersion       int              `json:"schemaVersion"`
	Output              digest.Digest    `json:"output"`
	Platform            v1.Platform      `json:"platform"`
	SourceRoot          v1.Descriptor    `json:"sourceRoot"`
	SourceManifest      v1.Descriptor    `json:"sourceManifest"`
	ApplicationRoot     v1.Descriptor    `json:"applicationRoot"`
	ApplicationManifest v1.Descriptor    `json:"applicationManifest"`
	ApplicationLayers   []BuiltLayer     `json:"applicationLayers"`
	DependencySet       v1.Descriptor    `json:"dependencySet"`
	DependencyEvidence  LayerSetEvidence `json:"dependencyEvidence"`
	OriginalLaunch      BuiltLayer       `json:"originalLaunch"`
	InputLaunch         BuiltLayer       `json:"inputLaunch"`
	EffectiveLaunch     BuiltLayer       `json:"effectiveLaunch"`
	OutputConfig        v1.Descriptor    `json:"outputConfig"`
	ParentComposition   digest.Digest    `json:"parentComposition,omitempty"`
	BuilderIdentity     string           `json:"builderIdentity,omitempty"`
}

type CompositionInspection struct {
	SchemaVersion        int                  `json:"schemaVersion"`
	Structural           bool                 `json:"structural"`
	PublisherProvenance  bool                 `json:"publisherProvenance"`
	CompositionSignature bool                 `json:"compositionSignature"`
	TestReleaseApproval  string               `json:"testReleaseApproval"`
	PolicyMode           string               `json:"policyMode"`
	Attestation          v1.Descriptor        `json:"attestation"`
	Predicate            CompositionPredicate `json:"predicate"`
}

type ComposeOptions struct {
	Application   v1.Descriptor
	DependencySet v1.Descriptor
	Platform      v1.Platform
	Policy        ReleasePolicy
	Signer        *LayerSetSigner
	Launch        []byte
}

type compositionStatement struct {
	Type          string               `json:"_type"`
	Subject       []statementSubject   `json:"subject"`
	PredicateType string               `json:"predicateType"`
	Predicate     CompositionPredicate `json:"predicate"`
}

func signDSSE(payload []byte, signer *LayerSetSigner) ([]byte, error) {
	if signer == nil || signer.Key == nil || signer.Identity == "" {
		return nil, fmt.Errorf("signer and explicit identity required")
	}
	id, err := signerKeyID(&signer.Key.PublicKey)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(dssePAE(InTotoPayloadType, payload))
	signature, err := ecdsa.SignASN1(rand.Reader, signer.Key, hash[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(dsseEnvelope{InTotoPayloadType, base64.StdEncoding.EncodeToString(payload),
		[]dsseSignature{{id, base64.StdEncoding.EncodeToString(signature)}}})
}

func verifyDSSE(data []byte, trust LayerSetTrust) ([]byte, error) {
	var envelope dsseEnvelope
	if err := StrictJSON(data, &envelope); err != nil {
		return nil, err
	}
	if envelope.PayloadType != InTotoPayloadType || len(envelope.Signatures) != 1 {
		return nil, fmt.Errorf("invalid DSSE envelope")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil {
		return nil, err
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(envelope.Signatures[0].Signature)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(dssePAE(envelope.PayloadType, payload))
	for _, key := range trust.PublicKeys {
		id, err := signerKeyID(key)
		if err != nil {
			return nil, err
		}
		if id == envelope.Signatures[0].KeyID && ecdsa.VerifyASN1(key, hash[:], signature) {
			return payload, nil
		}
	}
	return nil, fmt.Errorf("untrusted DSSE signature or key ID")
}

func compositionDocument(store Store, subject, ref v1.Descriptor, trust *LayerSetTrust) (CompositionPredicate, bool, error) {
	var predicate CompositionPredicate
	raw, err := store.Read(ref)
	if err != nil {
		return predicate, false, err
	}
	var manifest v1.Manifest
	if err = StrictJSON(raw, &manifest); err != nil {
		return predicate, false, err
	}
	if len(manifest.Layers) != 1 {
		return predicate, false, fmt.Errorf("one composition document required")
	}
	media := manifest.Layers[0].MediaType
	if media != MediaTypeDSSE && media != MediaTypeComposition {
		return predicate, false, fmt.Errorf("unsupported composition document")
	}
	data, _, err := readReferrer(store, subject, ref, MediaTypeComposition, media)
	if err != nil {
		return predicate, false, err
	}
	signed := media == MediaTypeDSSE
	if signed {
		if trust != nil {
			data, err = verifyDSSE(data, *trust)
		} else {
			var envelope dsseEnvelope
			err = StrictJSON(data, &envelope)
			if err == nil {
				data, err = base64.StdEncoding.Strict().DecodeString(envelope.Payload)
			}
		}
		if err != nil {
			return predicate, false, err
		}
		var statement compositionStatement
		if err = StrictJSON(data, &statement); err != nil {
			return predicate, false, err
		}
		if statement.Type != "https://in-toto.io/Statement/v1" || statement.PredicateType != CompositionPredicateType ||
			len(statement.Subject) != 1 || len(statement.Subject[0].Digest) != 1 ||
			statement.Subject[0].Digest["sha256"] != subject.Digest.Encoded() {
			return predicate, false, fmt.Errorf("composition subject/predicate mismatch")
		}
		predicate = statement.Predicate
		if trust != nil && predicate.BuilderIdentity != trust.ExpectedIdentity {
			return predicate, false, fmt.Errorf("composition builder identity mismatch")
		}
	} else if err = StrictJSON(data, &predicate); err != nil {
		return predicate, false, err
	}
	if predicate.SchemaVersion != 1 || predicate.Output != subject.Digest {
		return predicate, false, fmt.Errorf("composition output/version mismatch")
	}
	return predicate, signed, nil
}

// CompositionMaterials returns the immutable supporting graphs to transport
// with a composed image. It parses bindings only; it does not authorize them.
func CompositionMaterials(store Store, subject v1.Descriptor) ([]v1.Descriptor, error) {
	refs, err := compositionReferrers(store, subject)
	if err != nil {
		return nil, err
	}

	var materials []v1.Descriptor
	for _, ref := range refs {
		if ref.ArtifactType != MediaTypeComposition {
			continue
		}
		predicate, _, err := compositionDocument(store, subject, ref, nil)
		if err != nil {
			return nil, err
		}
		materials = append(materials, predicate.SourceRoot, predicate.ApplicationRoot, predicate.DependencySet)
	}
	return materials, nil
}

func compositionReferrers(store Store, subject v1.Descriptor) ([]v1.Descriptor, error) {
	refs, err := store.Referrers(subject)
	if err != nil {
		return nil, err
	}
	var result []v1.Descriptor
	for _, ref := range refs {
		raw, err := store.Read(ref)
		if err != nil {
			return nil, err
		}
		var manifest v1.Manifest
		if err = StrictJSON(raw, &manifest); err != nil {
			return nil, err
		}
		if ref.ArtifactType == MediaTypeComposition || manifest.ArtifactType == MediaTypeComposition {
			if ref.ArtifactType != "" && ref.ArtifactType != manifest.ArtifactType {
				return nil, fmt.Errorf("composition referrer type mismatch")
			}
			ref.ArtifactType = MediaTypeComposition
			result = append(result, ref)
		}
	}
	return result, nil
}

func resolvedLayer(r *Resolved, i int) BuiltLayer {
	return BuiltLayer{r.Manifest.Layers[i], r.Image.RootFS.DiffIDs[i]}
}

func appLayers(r *Resolved) []BuiltLayer {
	var layers []BuiltLayer
	for i, descriptor := range r.Manifest.Layers {
		if descriptor.Annotations["brewlet.sh/layer"] == "app" {
			layers = append(layers, resolvedLayer(r, i))
		}
	}
	return layers
}

func launchLayer(r *Resolved) BuiltLayer { return resolvedLayer(r, len(r.Manifest.Layers)-1) }

func dependencyCount(r *Resolved) int {
	n := 0
	for _, descriptor := range r.Manifest.Layers {
		if descriptor.Annotations["brewlet.sh/layer"] == "dependency" {
			n++
		}
	}
	return n
}

func validateManagedPaths(ctx context.Context, store Store, dependencies, application []BuiltLayer) error {
	protected := layerTree{}
	for _, layer := range dependencies {
		compiled, err := compileStoredLayer(ctx, store, layer)
		if err != nil {
			return err
		}
		mergeLayerTree(protected, compiled)
	}
	for _, layer := range application {
		compiled, err := compileStoredLayer(ctx, store, layer)
		if err != nil {
			return err
		}
		for _, entry := range compiled.entries {
			target, wh, _ := whiteout(entry.Path)
			if wh {
				if _, exists := protected[target]; exists {
					return fail("ManagedPathConflict", "application whiteout affects %s", target)
				}
			}
		}
		for name, node := range compiled.nodes {
			if lower, exists := protected[name]; exists && (node.Type != tar.TypeDir || lower.Type != tar.TypeDir) {
				return fail("ManagedPathConflict", "application shadows managed path %s", name)
			}
		}
	}
	return nil
}

func compileStoredLayer(ctx context.Context, store Store, layer BuiltLayer) (*compiledLayer, error) {
	info, err := WalkLayer(ctx, storedLayer(store, layer), nil)
	if err != nil {
		return nil, err
	}
	return compileLayer(info)
}

func storedLayer(store Store, layer BuiltLayer) LayerSource {
	return LayerSource{Descriptor: layer.Descriptor, DiffID: layer.DiffID,
		Open: func() (io.ReadCloser, error) { return store.Open(layer.Descriptor) }}
}

func Compose(ctx context.Context, store Store, options ComposeOptions) (v1.Descriptor, CompositionInspection, error) {
	var zero v1.Descriptor
	var inspection CompositionInspection
	policy := options.Policy
	now := time.Now()
	if err := policy.Validate(now); err != nil {
		return zero, inspection, err
	}
	if _, err := ParsePlatform(options.Platform.OS + "/" + options.Platform.Architecture + variantSuffix(options.Platform.Variant)); err != nil {
		return zero, inspection, err
	}
	if err := policy.Publisher.authorize(options.DependencySet.Digest); err != nil {
		return zero, inspection, err
	}
	trust, err := policy.Publisher.trust(now, policy.Mode == "production")
	if err != nil {
		return zero, inspection, err
	}
	composerTrust, err := policy.Composer.trust(now, policy.Mode == "production")
	if err != nil {
		return zero, inspection, err
	}
	if options.Signer != nil {
		if err = authorizeSigner(options.Signer, composerTrust); err != nil {
			return zero, inspection, err
		}
	} else if policy.Mode == "production" {
		return zero, inspection, fmt.Errorf("production composition requires an authorized signer")
	}
	for _, target := range []v1.Descriptor{options.Application, options.DependencySet} {
		if err = VerifyGraph(ctx, store, target); err != nil {
			return zero, inspection, err
		}
	}
	input, err := Resolve(ctx, store.Open, options.Application, options.Platform)
	if err != nil {
		return zero, inspection, err
	}
	predicate := CompositionPredicate{SchemaVersion: 1, Platform: options.Platform,
		SourceRoot: options.Application, SourceManifest: input.ManifestDescriptor,
		ApplicationRoot: options.Application, ApplicationManifest: input.ManifestDescriptor,
		ApplicationLayers: appLayers(input), OriginalLaunch: launchLayer(input),
		InputLaunch: launchLayer(input), EffectiveLaunch: launchLayer(input), DependencySet: options.DependencySet}
	refs, err := compositionReferrers(store, input.ManifestDescriptor)
	if err != nil {
		return zero, inspection, err
	}
	hasComposition := false
	for _, ref := range refs {
		hasComposition = hasComposition || ref.ArtifactType == MediaTypeComposition
	}
	if dependencyCount(input) > 0 || hasComposition {
		prior, err := VerifyComposition(ctx, store, options.Application, options.Platform, policy)
		if err != nil {
			return zero, inspection, err
		}
		if !prior.CompositionSignature {
			return zero, inspection, fmt.Errorf("replacement requires verified signed prior composition")
		}
		predicate.ParentComposition = prior.Attestation.Digest
		predicate.ApplicationRoot = prior.Predicate.ApplicationRoot
		predicate.ApplicationManifest = prior.Predicate.ApplicationManifest
		predicate.OriginalLaunch = prior.Predicate.OriginalLaunch
	}
	dependencies, evidence, err := ConsumeLayerSet(ctx, store, options.DependencySet, trust)
	if err != nil {
		return zero, inspection, err
	}
	if evidence.LockDigest == "" {
		return zero, inspection, fmt.Errorf("managed composition requires a complete dependency lock")
	}
	predicate.DependencyEvidence = evidence
	if err = validateManagedPaths(ctx, store, dependencies, predicate.ApplicationLayers); err != nil {
		return zero, inspection, err
	}
	effective := input.Launch
	if options.Launch != nil {
		effective, err = ParseLaunch(options.Launch)
		if err != nil {
			return zero, inspection, err
		}
		predicate.EffectiveLaunch, err = writeLayer(store, "launch", func(tw *tar.Writer) error {
			if err := tw.WriteHeader(&tar.Header{Name: "launch.json", Mode: 0644, Size: int64(len(options.Launch)), Typeflag: tar.TypeReg}); err != nil {
				return err
			}
			_, err := tw.Write(options.Launch)
			return err
		})
		if err != nil {
			return zero, inspection, err
		}
	}
	var descriptors []v1.Descriptor
	var diffs []digest.Digest
	for _, layer := range append(append(dependencies, predicate.ApplicationLayers...), predicate.EffectiveLaunch) {
		descriptors = append(descriptors, layer.Descriptor)
		diffs = append(diffs, layer.DiffID)
	}
	image := v1.Image{Platform: options.Platform, Config: effective.ImageConfig(), RootFS: v1.RootFS{Type: "layers", DiffIDs: diffs}}
	config, err := store.PutJSON(v1.MediaTypeImageConfig, image)
	if err != nil {
		return zero, inspection, err
	}
	predicate.OutputConfig = config
	encodedEvidence, err := json.Marshal([]LayerSetEvidence{evidence})
	if err != nil {
		return zero, inspection, err
	}
	manifest := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest,
		Config: config, Layers: descriptors, Annotations: map[string]string{"brewlet.sh/artifact-version": "2", "brewlet.sh/layerset-evidence": string(encodedEvidence)}}
	target, err := store.PutJSON(v1.MediaTypeImageManifest, manifest)
	if err != nil {
		return zero, inspection, err
	}
	target.Platform = &options.Platform
	if _, err = Resolve(ctx, store.Open, target, options.Platform); err != nil {
		return zero, inspection, err
	}
	if err = policy.Composer.authorize(target.Digest); err != nil {
		return zero, inspection, err
	}
	predicate.Output = target.Digest
	media := MediaTypeComposition
	var document []byte
	if options.Signer != nil {
		predicate.BuilderIdentity = options.Signer.Identity
		statement := compositionStatement{"https://in-toto.io/Statement/v1",
			[]statementSubject{{"application", map[string]string{"sha256": target.Digest.Encoded()}}}, CompositionPredicateType, predicate}
		payload, err := json.Marshal(statement)
		if err != nil {
			return zero, inspection, err
		}
		document, err = signDSSE(payload, options.Signer)
		if err != nil {
			return zero, inspection, err
		}
		media = MediaTypeDSSE
	} else {
		document, err = json.Marshal(predicate)
		if err != nil {
			return zero, inspection, err
		}
	}
	attestation, err := publishReferrer(store, target, MediaTypeComposition, media, document)
	if err != nil {
		return zero, inspection, err
	}
	resolved, err := Resolve(ctx, store.Open, target, options.Platform)
	if err != nil {
		return zero, inspection, err
	}
	inspection, err = inspectCompositionCandidate(ctx, store, resolved, attestation, policy)
	if err != nil {
		return zero, inspection, err
	}
	return target, inspection, nil
}

func variantSuffix(variant string) string {
	if variant == "" {
		return ""
	}
	return "/" + variant
}

func VerifyComposition(ctx context.Context, store Store, target v1.Descriptor, platform v1.Platform, policy ReleasePolicy) (CompositionInspection, error) {
	var result CompositionInspection
	if err := policy.Validate(time.Now()); err != nil {
		return result, err
	}
	resolved, err := Resolve(ctx, store.Open, target, platform)
	if err != nil {
		return result, err
	}
	if err = policy.Composer.authorize(resolved.ManifestDescriptor.Digest); err != nil {
		return result, err
	}
	refs, err := compositionReferrers(store, resolved.ManifestDescriptor)
	if err != nil {
		return result, err
	}
	var candidates []v1.Descriptor
	for _, ref := range refs {
		if ref.ArtifactType == MediaTypeComposition {
			candidates = append(candidates, ref)
		}
	}
	if len(candidates) == 0 {
		return result, fmt.Errorf("composition evidence required")
	}
	var last error
	for _, candidate := range candidates {
		inspection, err := inspectCompositionCandidate(ctx, store, resolved, candidate, policy)
		if err != nil {
			last = err
			continue
		}
		// Unsigned evidence cannot hide an invalid present signature.
		if !inspection.CompositionSignature && len(candidates) != 1 {
			last = fmt.Errorf("ambiguous unsigned composition evidence")
			continue
		}
		return inspection, nil
	}
	return result, fmt.Errorf("no complete authorized composition: %w", last)
}

func inspectCompositionCandidate(ctx context.Context, store Store, resolved *Resolved, candidate v1.Descriptor, policy ReleasePolicy) (CompositionInspection, error) {
	var result CompositionInspection
	if err := policy.Validate(time.Now()); err != nil {
		return result, err
	}
	trust, err := policy.Composer.trust(time.Now(), policy.Mode == "production")
	if err != nil {
		return result, err
	}
	predicate, signed, err := compositionDocument(store, resolved.ManifestDescriptor, candidate, &trust)
	if err != nil {
		return result, err
	}
	if !signed && policy.Mode == "production" {
		return result, fmt.Errorf("signed composition required")
	}
	if err = verifyCompositionBindings(ctx, store, resolved, predicate, policy); err != nil {
		return result, err
	}
	return CompositionInspection{1, true, predicate.DependencyEvidence.AttestationDigest != "", signed,
		"not-evaluated", policy.Mode, candidate, predicate}, nil
}

func verifyCompositionBindings(ctx context.Context, store Store, output *Resolved, p CompositionPredicate, policy ReleasePolicy) error {
	if !sameDeclaredPlatform(output.Image.Platform, p.Platform) || !reflect.DeepEqual(output.Manifest.Config, p.OutputConfig) {
		return fmt.Errorf("composition output config/platform mismatch")
	}
	source, err := Resolve(ctx, store.Open, p.SourceRoot, p.Platform)
	if err != nil {
		return err
	}
	application, err := Resolve(ctx, store.Open, p.ApplicationRoot, p.Platform)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(source.ManifestDescriptor, p.SourceManifest) || !reflect.DeepEqual(application.ManifestDescriptor, p.ApplicationManifest) ||
		dependencyCount(application) != 0 || !reflect.DeepEqual(appLayers(application), p.ApplicationLayers) ||
		!reflect.DeepEqual(appLayers(source), p.ApplicationLayers) || !reflect.DeepEqual(launchLayer(source), p.InputLaunch) ||
		!reflect.DeepEqual(launchLayer(application), p.OriginalLaunch) || !reflect.DeepEqual(launchLayer(output), p.EffectiveLaunch) {
		return fmt.Errorf("composition source/application/launch binding mismatch")
	}
	if dependencyCount(source) > 0 && p.ParentComposition.Validate() != nil ||
		dependencyCount(source) == 0 && p.ParentComposition != "" {
		return fmt.Errorf("composition parent binding mismatch")
	}
	if p.ParentComposition != "" {
		refs, err := compositionReferrers(store, source.ManifestDescriptor)
		if err != nil {
			return err
		}
		found := false
		for _, ref := range refs {
			found = found || ref.ArtifactType == MediaTypeComposition && ref.Digest == p.ParentComposition
		}
		if !found {
			return fmt.Errorf("parent composition referrer missing")
		}
	}
	if err = policy.Publisher.authorize(p.DependencySet.Digest); err != nil {
		return err
	}
	trust, err := policy.Publisher.trust(time.Now(), policy.Mode == "production")
	if err != nil {
		return err
	}
	dependencies, evidence, err := ConsumeLayerSet(ctx, store, p.DependencySet, trust)
	if err != nil {
		return err
	}
	bound := p.DependencyEvidence.AttestationDigest
	if bound != "" {
		candidates, err := store.Referrers(p.DependencySet)
		if err != nil {
			return err
		}
		found := false
		for _, candidate := range candidates {
			if candidate.Digest != bound {
				continue
			}
			data, _, err := readReferrer(store, p.DependencySet, candidate, MediaTypeAttestation, MediaTypeDSSE)
			if err != nil {
				return err
			}
			want := LayerSetPredicate{SchemaVersion: 1, LayerSetDigest: p.DependencySet.Digest, ConfigDigest: evidence.ConfigDigest,
				Layers: evidence.Layers, LockDigest: evidence.LockDigest, SBOMDigest: evidence.SBOMDigest}
			if err = verifyLayerSetSignature(data, want, trust); err != nil {
				return err
			}
			found = true
		}
		if !found {
			return fmt.Errorf("bound publisher attestation unavailable")
		}
		evidence.AttestationDigest = bound
	}
	if evidence.LockDigest == "" || !reflect.DeepEqual(evidence, p.DependencyEvidence) {
		return fmt.Errorf("composition dependency evidence mismatch")
	}
	want := append(append(dependencies, p.ApplicationLayers...), p.EffectiveLaunch)
	if len(want) != len(output.Manifest.Layers) {
		return fmt.Errorf("composition layer count mismatch")
	}
	for i, layer := range want {
		if !reflect.DeepEqual(layer, resolvedLayer(output, i)) {
			return fmt.Errorf("composition layer ordering/descriptor/diff-ID mismatch")
		}
	}
	raw, err := json.Marshal([]LayerSetEvidence{p.DependencyEvidence})
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, []byte(output.Manifest.Annotations["brewlet.sh/layerset-evidence"])) {
		return fmt.Errorf("composition output evidence mismatch")
	}
	return validateManagedPaths(ctx, store, dependencies, p.ApplicationLayers)
}
