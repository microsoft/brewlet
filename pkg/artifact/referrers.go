package artifact

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/url"
	"slices"
	"strings"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	MediaTypeCycloneDX        = "application/vnd.cyclonedx+json"
	MediaTypeAttestation      = "application/vnd.brewlet.attestation.v1+json"
	MediaTypeDSSE             = "application/vnd.dsse.envelope.v1+json"
	LayerSetPredicateType     = "https://brewlet.sh/attestations/layerset/v1"
	InTotoPayloadType         = "application/vnd.in-toto+json"
	AnnotationReferrerSubject = "brewlet.sh/referrer-subject"
)

type LayerSetSigner struct {
	Key      *ecdsa.PrivateKey
	Identity string
}
type LayerSetTrust struct {
	PublicKeys        []*ecdsa.PublicKey
	ExpectedIdentity  string
	RequireProvenance bool
}
type LayerSetPublishOptions struct {
	SBOM   []byte
	Signer *LayerSetSigner
	Trust  LayerSetTrust
}
type LayerSetPredicate struct {
	SchemaVersion   int            `json:"schemaVersion"`
	LayerSetDigest  digest.Digest  `json:"layerSetDigest"`
	ConfigDigest    digest.Digest  `json:"layerSetConfigDigest"`
	Layers          []LayerBinding `json:"layers"`
	LockDigest      digest.Digest  `json:"layerSetLockDigest,omitempty"`
	SBOMDigest      digest.Digest  `json:"sbomDigest"`
	BuilderIdentity string         `json:"builderIdentity"`
}
type cdxHash struct {
	Algorithm string `json:"alg"`
	Content   string `json:"content"`
}
type cdxProperty struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type cdxComponent struct {
	Type       string        `json:"type"`
	Group      string        `json:"group,omitempty"`
	Name       string        `json:"name"`
	Version    string        `json:"version,omitempty"`
	Hashes     []cdxHash     `json:"hashes"`
	PURL       string        `json:"purl,omitempty"`
	Properties []cdxProperty `json:"properties"`
}
type cycloneDX struct {
	BOMFormat   string         `json:"bomFormat"`
	SpecVersion string         `json:"specVersion"`
	Version     int            `json:"version"`
	Components  []cdxComponent `json:"components"`
}
type statementSubject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}
type layerSetStatement struct {
	Type          string             `json:"_type"`
	Subject       []statementSubject `json:"subject"`
	PredicateType string             `json:"predicateType"`
	Predicate     LayerSetPredicate  `json:"predicate"`
}
type dsseSignature struct {
	KeyID     string `json:"keyid"`
	Signature string `json:"sig"`
}
type dsseEnvelope struct {
	PayloadType string          `json:"payloadType"`
	Payload     string          `json:"payload"`
	Signatures  []dsseSignature `json:"signatures"`
}

func ParseSigningKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("one PKCS#8 private PEM required")
	}
	value, e := x509.ParsePKCS8PrivateKey(block.Bytes)
	if e != nil {
		return nil, e
	}
	key, ok := value.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("ECDSA P-256 key required")
	}
	return key, nil
}
func ParseTrustKey(data []byte) (*ecdsa.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("one SPKI public PEM required")
	}
	value, e := x509.ParsePKIXPublicKey(block.Bytes)
	if e != nil {
		return nil, e
	}
	key, ok := value.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("ECDSA P-256 key required")
	}
	return key, nil
}
func signerKeyID(key *ecdsa.PublicKey) (string, error) {
	if key == nil || key.Curve != elliptic.P256() {
		return "", fmt.Errorf("ECDSA P-256 public key required")
	}
	der, e := x509.MarshalPKIXPublicKey(key)
	if e != nil {
		return "", e
	}
	return digest.FromBytes(der).String(), nil
}
func dssePAE(payloadType string, payload []byte) []byte {
	prefix := fmt.Sprintf("DSSEv1 %d %s %d ", len(payloadType), payloadType, len(payload))
	return append([]byte(prefix), payload...)
}
func genericPURL(entry LockEntry) string {
	encode := func(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
	if entry.PURL != "" {
		return entry.PURL
	}
	return "pkg:generic/" + encode(entry.Ecosystem) + "/" + encode(entry.Name) + "@" + encode(entry.Version)
}
func expectedComponent(entry LockEntry, hasLock bool) cdxComponent {
	component := cdxComponent{Type: "library", Group: entry.Group, Name: entry.Name, Version: entry.Version, Hashes: []cdxHash{{"SHA-256", entry.SHA256}}, Properties: []cdxProperty{{"brewlet:ecosystem", entry.Ecosystem}, {"brewlet:path", entry.Path}}}
	if hasLock {
		component.PURL = genericPURL(entry)
	} else {
		component.Type = "file"
		component.Name = entry.Path
		component.Version = ""
	}
	return component
}
func buildSBOM(inventory []LockEntry, hasLock bool) ([]byte, error) {
	doc := cycloneDX{BOMFormat: "CycloneDX", SpecVersion: "1.5", Version: 1, Components: make([]cdxComponent, 0, len(inventory))}
	for _, entry := range inventory {
		doc.Components = append(doc.Components, expectedComponent(entry, hasLock))
	}
	return json.Marshal(doc)
}
func validateSBOM(data []byte, inventory []LockEntry, hasLock bool) error {
	var raw map[string]json.RawMessage
	if e := StrictJSON(data, &raw); e != nil {
		return e
	}
	var doc cycloneDX
	if e := json.Unmarshal(data, &doc); e != nil {
		return e
	}
	if doc.BOMFormat != "CycloneDX" || doc.SpecVersion != "1.5" || doc.Version != 1 || len(doc.Components) != len(inventory) {
		return fmt.Errorf("CycloneDX 1.5 inventory does not match layer set")
	}
	want := map[string]cdxComponent{}
	for _, entry := range inventory {
		want[entry.Path] = expectedComponent(entry, hasLock)
	}
	seen := map[string]bool{}
	for _, component := range doc.Components {
		properties := map[string]string{}
		for _, p := range component.Properties {
			if _, ok := properties[p.Name]; ok {
				return fmt.Errorf("duplicate SBOM property")
			}
			properties[p.Name] = p.Value
		}
		p := properties["brewlet:path"]
		expected, ok := want[p]
		if !ok || seen[p] || component.Type != expected.Type || component.Group != expected.Group || component.Name != expected.Name || component.Version != expected.Version || component.PURL != expected.PURL || properties["brewlet:ecosystem"] != expected.Properties[0].Value {
			return fmt.Errorf("SBOM component mismatch at %q", p)
		}
		seen[p] = true
		hashes := 0
		for _, hash := range component.Hashes {
			if hash.Algorithm == "SHA-256" {
				hashes++
				if strings.ToLower(hash.Content) != expected.Hashes[0].Content {
					return fmt.Errorf("SBOM SHA-256 mismatch")
				}
			}
		}
		if hashes != 1 {
			return fmt.Errorf("exactly one matching SHA-256 hash required")
		}
	}
	return nil
}
func sameSubject(a, b v1.Descriptor) bool {
	return a.MediaType == b.MediaType && a.Digest == b.Digest && a.Size == b.Size
}
func publishReferrer(store Store, subject v1.Descriptor, artifactType, documentType string, data []byte) (v1.Descriptor, error) {
	config, e := store.Put(v1.MediaTypeEmptyJSON, bytes.NewReader([]byte("{}")))
	if e != nil {
		return v1.Descriptor{}, e
	}
	layer, e := store.Put(documentType, bytes.NewReader(data))
	if e != nil {
		return v1.Descriptor{}, e
	}
	m := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifactType, Subject: &subject, Config: config, Layers: []v1.Descriptor{layer}}
	if artifactType == MediaTypeAttestation {
		m.Annotations = map[string]string{"brewlet.sh/predicate-type": LayerSetPredicateType}
	}
	d, e := store.PutJSON(v1.MediaTypeImageManifest, m)
	if e != nil {
		return d, e
	}
	d.ArtifactType = artifactType
	e = store.RecordReferrer(subject, d)
	return d, e
}
func readReferrer(store Store, subject, descriptor v1.Descriptor, artifactType, documentType string) ([]byte, v1.Descriptor, error) {
	if descriptor.MediaType != v1.MediaTypeImageManifest {
		return nil, v1.Descriptor{}, fmt.Errorf("OCI referrer manifest required")
	}
	b, e := store.Read(descriptor)
	if e != nil {
		return nil, v1.Descriptor{}, e
	}
	var m v1.Manifest
	if e = StrictJSON(b, &m); e != nil {
		return nil, v1.Descriptor{}, e
	}
	if m.SchemaVersion != 2 || m.MediaType != v1.MediaTypeImageManifest || m.ArtifactType != artifactType || m.Subject == nil || !sameSubject(*m.Subject, subject) || m.Config.MediaType != v1.MediaTypeEmptyJSON || len(m.Layers) != 1 || m.Layers[0].MediaType != documentType {
		return nil, v1.Descriptor{}, fmt.Errorf("referrer subject/config/document mismatch")
	}
	if descriptor.ArtifactType != "" && descriptor.ArtifactType != artifactType {
		return nil, v1.Descriptor{}, fmt.Errorf("referrer artifact type mismatch")
	}
	b, e = store.Read(m.Config)
	if e != nil {
		return nil, v1.Descriptor{}, e
	}
	if !bytes.Equal(b, []byte("{}")) {
		return nil, v1.Descriptor{}, fmt.Errorf("referrer config must be exact empty object")
	}
	if artifactType == MediaTypeAttestation && m.Annotations["brewlet.sh/predicate-type"] != LayerSetPredicateType {
		return nil, v1.Descriptor{}, fmt.Errorf("predicate discovery annotation mismatch")
	}
	b, e = store.Read(m.Layers[0])
	return b, m.Layers[0], e
}
func signLayerSet(predicate LayerSetPredicate, signer *LayerSetSigner) ([]byte, error) {
	if signer == nil || signer.Key == nil || signer.Key.Curve != elliptic.P256() || signer.Identity == "" {
		return nil, fmt.Errorf("P-256 signer and explicit builder identity required")
	}
	predicate.BuilderIdentity = signer.Identity
	statement := layerSetStatement{Type: "https://in-toto.io/Statement/v1", Subject: []statementSubject{{"layerset", map[string]string{"sha256": predicate.LayerSetDigest.Encoded()}}}, PredicateType: LayerSetPredicateType, Predicate: predicate}
	payload, e := json.Marshal(statement)
	if e != nil {
		return nil, e
	}
	hash := sha256.Sum256(dssePAE(InTotoPayloadType, payload))
	signature, e := ecdsa.SignASN1(rand.Reader, signer.Key, hash[:])
	if e != nil {
		return nil, e
	}
	keyID, e := signerKeyID(&signer.Key.PublicKey)
	if e != nil {
		return nil, e
	}
	return json.Marshal(dsseEnvelope{InTotoPayloadType, base64.StdEncoding.EncodeToString(payload), []dsseSignature{{keyID, base64.StdEncoding.EncodeToString(signature)}}})
}
func verifyLayerSetSignature(data []byte, want LayerSetPredicate, trust LayerSetTrust) error {
	if len(trust.PublicKeys) == 0 || trust.ExpectedIdentity == "" {
		return fmt.Errorf("signed layer set requires public key and expected signer identity")
	}
	var envelope dsseEnvelope
	if e := StrictJSON(data, &envelope); e != nil {
		return e
	}
	if envelope.PayloadType != InTotoPayloadType || len(envelope.Signatures) != 1 {
		return fmt.Errorf("invalid DSSE payload type/signature count")
	}
	payload, e := base64.StdEncoding.Strict().DecodeString(envelope.Payload)
	if e != nil {
		return e
	}
	signature, e := base64.StdEncoding.Strict().DecodeString(envelope.Signatures[0].Signature)
	if e != nil {
		return e
	}
	hash := sha256.Sum256(dssePAE(envelope.PayloadType, payload))
	valid := false
	for _, key := range trust.PublicKeys {
		keyID, e := signerKeyID(key)
		if e != nil {
			return e
		}
		if keyID == envelope.Signatures[0].KeyID && ecdsa.VerifyASN1(key, hash[:], signature) {
			valid = true
		}
	}
	if !valid {
		return fmt.Errorf("untrusted DSSE signature or key ID")
	}
	var statement layerSetStatement
	if e = StrictJSON(payload, &statement); e != nil {
		return e
	}
	if statement.Type != "https://in-toto.io/Statement/v1" || statement.PredicateType != LayerSetPredicateType || len(statement.Subject) != 1 || len(statement.Subject[0].Digest) != 1 || statement.Subject[0].Digest["sha256"] != want.LayerSetDigest.Encoded() {
		return fmt.Errorf("in-toto subject or predicate type mismatch")
	}
	got := statement.Predicate
	if got.SchemaVersion != 1 || got.LayerSetDigest != want.LayerSetDigest || got.ConfigDigest != want.ConfigDigest || got.LockDigest != want.LockDigest || got.SBOMDigest != want.SBOMDigest || !slices.Equal(got.Layers, want.Layers) || got.BuilderIdentity != trust.ExpectedIdentity {
		return fmt.Errorf("layer-set predicate binding/identity mismatch")
	}
	return nil
}

func verifyLayerSetReferrers(ctx context.Context, store Store, target v1.Descriptor, inventory []LockEntry, hasLock bool, evidence *LayerSetEvidence, trust LayerSetTrust) error {
	candidates, e := store.Referrers(target)
	if e != nil {
		return e
	}
	var sboms, attestations []v1.Descriptor
	for _, d := range candidates {
		if e = ctx.Err(); e != nil {
			return e
		}
		typ := ""
		if b, e := store.Read(d); e == nil {
			var m v1.Manifest
			if json.Unmarshal(b, &m) == nil {
				typ = m.ArtifactType
			}
		}
		if typ == MediaTypeCycloneDX || d.ArtifactType == MediaTypeCycloneDX {
			sboms = append(sboms, d)
		}
		if typ == MediaTypeAttestation || d.ArtifactType == MediaTypeAttestation {
			attestations = append(attestations, d)
		}
	}
	if len(sboms) != 1 {
		return fmt.Errorf("exactly one SBOM referrer required; discovered %d", len(sboms))
	}
	data, document, e := readReferrer(store, target, sboms[0], MediaTypeCycloneDX, MediaTypeCycloneDX)
	if e != nil {
		return e
	}
	if e = validateSBOM(data, inventory, hasLock); e != nil {
		return e
	}
	evidence.SBOMDigest = document.Digest
	if len(attestations) == 0 {
		if trust.RequireProvenance {
			return fmt.Errorf("trusted layer-set provenance required")
		}
		return nil
	}
	if len(trust.PublicKeys) == 0 || trust.ExpectedIdentity == "" {
		return fmt.Errorf("signed layer set requires trust credentials")
	}
	want := LayerSetPredicate{SchemaVersion: 1, LayerSetDigest: target.Digest, ConfigDigest: evidence.ConfigDigest, Layers: evidence.Layers, LockDigest: evidence.LockDigest, SBOMDigest: evidence.SBOMDigest}
	for _, candidate := range attestations {
		data, _, e := readReferrer(store, target, candidate, MediaTypeAttestation, MediaTypeDSSE)
		if e != nil {
			continue
		}
		if verifyLayerSetSignature(data, want, trust) == nil {
			evidence.AttestationDigest = candidate.Digest
			return nil
		}
	}
	return fmt.Errorf("no complete trusted layer-set provenance candidate")
}
