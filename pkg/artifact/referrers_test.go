package artifact

import (
	"archive/tar"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func fixtureLayerSet(t *testing.T, store Store, withLock, withSBOM bool) (v1.Descriptor, LayerSetPredicate) {
	t.Helper()
	src := source(t, store, "gzip", []*tar.Header{fileHeader("app/lib/library", "bytes", 0644)}, []string{"bytes"})
	src.Descriptor.Annotations["brewlet.sh/layer"] = "dependency"
	config := LayerSetConfig{SchemaVersion: 1, Name: "approved", Version: "2026", Ecosystem: "explicit", Layers: []LayerBinding{{src.Descriptor.Digest, src.DiffID}}}
	cd, e := store.PutJSON(MediaTypeLayerSetConfig, config)
	must(t, e)
	m := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: MediaTypeLayerSet, Config: cd, Layers: []v1.Descriptor{src.Descriptor}}
	if withLock {
		lock := LayerSetLock{SchemaVersion: 1, Entries: []LockEntry{{Ecosystem: "explicit", Name: "library", Version: "1", Path: "/app/lib/library", SHA256: digest.FromString("bytes").Encoded(), Group: "author-supplied", PURL: "pkg:generic/author/library@1"}}}
		ld, e := store.PutJSON(MediaTypeLayerSetLock, lock)
		must(t, e)
		m.Layers = append(m.Layers, ld)
	}
	root, e := store.PutJSON(v1.MediaTypeImageManifest, m)
	must(t, e)
	layers, ev, e := consumeLayerSetContent(context.Background(), store, root)
	must(t, e)
	inventory, hasLock, e := layerSetInventory(context.Background(), store, root, layers)
	must(t, e)
	sbom, e := buildSBOM(inventory, hasLock)
	must(t, e)
	if withSBOM {
		_, e = publishReferrer(store, root, MediaTypeCycloneDX, MediaTypeCycloneDX, sbom)
		must(t, e)
	}
	return root, LayerSetPredicate{SchemaVersion: 1, LayerSetDigest: root.Digest, ConfigDigest: ev.ConfigDigest, Layers: ev.Layers, LockDigest: ev.LockDigest, SBOMDigest: digest.FromBytes(sbom)}
}
func testSigner(t *testing.T, identity string) *LayerSetSigner {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, e)
	return &LayerSetSigner{key, identity}
}
func addSignature(t *testing.T, store Store, root v1.Descriptor, predicate LayerSetPredicate, signer *LayerSetSigner) v1.Descriptor {
	t.Helper()
	b, e := signLayerSet(predicate, signer)
	must(t, e)
	d, e := publishReferrer(store, root, MediaTypeAttestation, MediaTypeDSSE, b)
	must(t, e)
	return d
}
func signerTrust(signer *LayerSetSigner) LayerSetTrust {
	return LayerSetTrust{PublicKeys: []*ecdsa.PublicKey{&signer.Key.PublicKey}, ExpectedIdentity: signer.Identity}
}
func TestLayerSetSBOMRequiredAndUnsigned(t *testing.T) {
	for _, withLock := range []bool{false, true} {
		t.Run(map[bool]string{false: "file-inventory", true: "lock-inventory"}[withLock], func(t *testing.T) {
			store := Store{Dir: filepath.Join(testDir(t), "store")}
			root, predicate := fixtureLayerSet(t, store, withLock, false)
			if _, _, e := ConsumeLayerSet(context.Background(), store, root); e == nil {
				t.Fatal("missing SBOM accepted")
			}
			layers, _, e := consumeLayerSetContent(context.Background(), store, root)
			must(t, e)
			inventory, hasLock, e := layerSetInventory(context.Background(), store, root, layers)
			must(t, e)
			sbom, e := buildSBOM(inventory, hasLock)
			must(t, e)
			_, e = publishReferrer(store, root, MediaTypeCycloneDX, MediaTypeCycloneDX, sbom)
			must(t, e)
			_, ev, e := ConsumeLayerSet(context.Background(), store, root)
			must(t, e)
			if ev.SBOMDigest != predicate.SBOMDigest || ev.AttestationDigest != "" {
				t.Fatal("unsigned SBOM evidence wrong")
			}
			if _, _, e = ConsumeLayerSet(context.Background(), store, root, LayerSetTrust{RequireProvenance: true}); e == nil {
				t.Fatal("required provenance absent")
			}
			var alternate map[string]any
			must(t, json.Unmarshal(sbom, &alternate))
			alternate["serialNumber"] = "urn:uuid:explicit-publisher-metadata"
			extra, e := json.Marshal(alternate)
			must(t, e)
			must(t, validateSBOM(extra, inventory, hasLock))
			_, e = publishReferrer(store, root, MediaTypeCycloneDX, MediaTypeCycloneDX, extra)
			must(t, e)
			if _, _, e = ConsumeLayerSet(context.Background(), store, root); e == nil || !strings.Contains(e.Error(), "discovered 2") {
				t.Fatalf("additional SBOM did not fail closed: %v", e)
			}
		})
	}
}
func TestLayerSetSignedConsumptionAndRotation(t *testing.T) {
	store := Store{Dir: filepath.Join(testDir(t), "store")}
	root, predicate := fixtureLayerSet(t, store, true, true)
	first := testSigner(t, "platform-builder")
	second := testSigner(t, "platform-builder")
	addSignature(t, store, root, predicate, first)
	if _, _, e := ConsumeLayerSet(context.Background(), store, root); e == nil {
		t.Fatal("signed provenance silently treated as unsigned")
	}
	if _, _, e := ConsumeLayerSet(context.Background(), store, root, signerTrust(second)); e == nil {
		t.Fatal("untrusted signer accepted")
	}
	wrongIdentity := signerTrust(first)
	wrongIdentity.ExpectedIdentity = "someone-else"
	if _, _, e := ConsumeLayerSet(context.Background(), store, root, wrongIdentity); e == nil {
		t.Fatal("wrong signer identity accepted")
	}
	secondRef := addSignature(t, store, root, predicate, second)
	_, e := publishReferrer(store, root, MediaTypeAttestation, MediaTypeDSSE, []byte(`{"payloadType":"invalid","payload":"","signatures":[]}`))
	must(t, e)
	_, ev, e := ConsumeLayerSet(context.Background(), store, root, signerTrust(second))
	must(t, e)
	if ev.AttestationDigest != secondRef.Digest {
		t.Fatal("trusted rotation candidate not selected")
	}
	_, _, e = ConsumeLayerSet(context.Background(), store, root, signerTrust(first))
	must(t, e)
}
func signTestStatement(t *testing.T, key *ecdsa.PrivateKey, statement layerSetStatement) []byte {
	t.Helper()
	payload, e := json.Marshal(statement)
	must(t, e)
	hash := sha256.Sum256(dssePAE(InTotoPayloadType, payload))
	sig, e := ecdsa.SignASN1(rand.Reader, key, hash[:])
	must(t, e)
	id, e := signerKeyID(&key.PublicKey)
	must(t, e)
	b, e := json.Marshal(dsseEnvelope{InTotoPayloadType, base64.StdEncoding.EncodeToString(payload), []dsseSignature{{id, base64.StdEncoding.EncodeToString(sig)}}})
	must(t, e)
	return b
}
func TestLayerSetPredicateBindings(t *testing.T) {
	signer := testSigner(t, "publisher")
	trust := signerTrust(signer)
	want := LayerSetPredicate{SchemaVersion: 1, LayerSetDigest: digest.FromString("set"), ConfigDigest: digest.FromString("config"), Layers: []LayerBinding{{digest.FromString("layer"), digest.FromString("diff")}}, LockDigest: digest.FromString("lock"), SBOMDigest: digest.FromString("sbom")}
	valid, e := signLayerSet(want, signer)
	must(t, e)
	must(t, verifyLayerSetSignature(valid, want, trust))
	var envelope dsseEnvelope
	must(t, StrictJSON(valid, &envelope))
	payload, e := base64.StdEncoding.DecodeString(envelope.Payload)
	must(t, e)
	for name, mutate := range map[string]func(*layerSetStatement){
		"statement-type":    func(s *layerSetStatement) { s.Type = "wrong" },
		"predicate-type":    func(s *layerSetStatement) { s.PredicateType = "wrong" },
		"subject":           func(s *layerSetStatement) { s.Subject[0].Digest["sha256"] = digest.FromString("other").Encoded() },
		"subject-count":     func(s *layerSetStatement) { s.Subject = append(s.Subject, s.Subject[0]) },
		"subject-algorithm": func(s *layerSetStatement) { s.Subject[0].Digest["sha512"] = "extra" },
		"schema":            func(s *layerSetStatement) { s.Predicate.SchemaVersion = 2 },
		"set":               func(s *layerSetStatement) { s.Predicate.LayerSetDigest = digest.FromString("other") },
		"config":            func(s *layerSetStatement) { s.Predicate.ConfigDigest = digest.FromString("other") },
		"layer":             func(s *layerSetStatement) { s.Predicate.Layers[0].Digest = digest.FromString("other") },
		"diff":              func(s *layerSetStatement) { s.Predicate.Layers[0].DiffID = digest.FromString("other") },
		"lock":              func(s *layerSetStatement) { s.Predicate.LockDigest = digest.FromString("other") },
		"sbom":              func(s *layerSetStatement) { s.Predicate.SBOMDigest = digest.FromString("other") },
		"identity":          func(s *layerSetStatement) { s.Predicate.BuilderIdentity = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			var statement layerSetStatement
			must(t, StrictJSON(payload, &statement))
			mutate(&statement)
			bad := signTestStatement(t, signer.Key, statement)
			if e := verifyLayerSetSignature(bad, want, trust); e == nil {
				t.Fatal("incomplete predicate accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*dsseEnvelope){
		"key-id":          func(e *dsseEnvelope) { e.Signatures[0].KeyID = "wrong" },
		"signature-count": func(e *dsseEnvelope) { e.Signatures = append(e.Signatures, e.Signatures[0]) },
		"payload-type":    func(e *dsseEnvelope) { e.PayloadType = "other" },
		"signature":       func(e *dsseEnvelope) { e.Signatures[0].Signature = "AAAA" },
		"payload":         func(e *dsseEnvelope) { e.Payload = "not base64" },
	} {
		t.Run(name, func(t *testing.T) {
			var envelope dsseEnvelope
			must(t, StrictJSON(valid, &envelope))
			mutate(&envelope)
			bad, e := json.Marshal(envelope)
			must(t, e)
			if e = verifyLayerSetSignature(bad, want, trust); e == nil {
				t.Fatal("bad DSSE accepted")
			}
		})
	}
}
func TestLayerSetTrustKeyFormats(t *testing.T) {
	signer := testSigner(t, "publisher")
	private, e := x509.MarshalPKCS8PrivateKey(signer.Key)
	must(t, e)
	public, e := x509.MarshalPKIXPublicKey(&signer.Key.PublicKey)
	must(t, e)
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public})
	_, e = ParseSigningKey(privatePEM)
	must(t, e)
	_, e = ParseTrustKey(publicPEM)
	must(t, e)
	if _, e = ParseTrustKey(append(publicPEM, publicPEM...)); e == nil {
		t.Fatal("multiple public PEM blocks accepted")
	}
	if _, e = ParseSigningKey(publicPEM); e == nil {
		t.Fatal("public key accepted as signer")
	}
	p384, e := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	must(t, e)
	der, e := x509.MarshalPKIXPublicKey(&p384.PublicKey)
	must(t, e)
	if _, e = ParseTrustKey(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})); e == nil {
		t.Fatal("non-P256 key accepted")
	}
}
func TestReferrerLocalLayoutCopyAndConcurrency(t *testing.T) {
	dir := testDir(t)
	sourceStore := Store{Dir: filepath.Join(dir, "source")}
	root, predicate := fixtureLayerSet(t, sourceStore, true, true)
	signer := testSigner(t, "builder")
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, e := signLayerSet(predicate, signer)
			if e == nil {
				_, e = publishReferrer(sourceStore, root, MediaTypeAttestation, MediaTypeDSSE, b)
			}
			errors <- e
		}()
	}
	wg.Wait()
	close(errors)
	for e := range errors {
		must(t, e)
	}
	refs, e := sourceStore.Referrers(root)
	must(t, e)
	if len(refs) != 9 {
		t.Fatalf("lost concurrent referrers: %d", len(refs))
	}
	must(t, sourceStore.WriteIndex(root, "approved"))
	// A portable OCI layout carries all discovery in index.json, not host-only sidecars.
	must(t, os.RemoveAll(filepath.Join(sourceStore.Dir, "referrers")))
	got, e := sourceStore.Target("")
	must(t, e)
	if got.Digest != root.Digest {
		t.Fatal("subject not selected from referrer-bearing layout")
	}
	destination := Store{Dir: filepath.Join(dir, "destination")}
	must(t, CopyGraph(context.Background(), sourceStore, destination, root))
	_, _, e = ConsumeLayerSet(context.Background(), destination, root, signerTrust(signer))
	must(t, e)
}

func TestReferrerDescriptorAndSubjectContracts(t *testing.T) {
	for name, mutate := range map[string]func(Store, *v1.Manifest, *v1.Descriptor) error{
		"schema":          func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.SchemaVersion = 1; return nil },
		"manifest-media":  func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.MediaType = "wrong"; return nil },
		"artifact-type":   func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.ArtifactType = "wrong"; return nil },
		"missing-subject": func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.Subject = nil; return nil },
		"subject-media":   func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.Subject.MediaType = "wrong"; return nil },
		"subject-size":    func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.Subject.Size++; return nil },
		"subject-digest": func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error {
			m.Subject.Digest = digest.FromString("other")
			return nil
		},
		"config-type": func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.Config.MediaType = "wrong"; return nil },
		"config-bytes": func(store Store, m *v1.Manifest, _ *v1.Descriptor) error {
			d, e := store.Put(v1.MediaTypeEmptyJSON, strings.NewReader("{ }"))
			m.Config = d
			return e
		},
		"layer-count": func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error {
			m.Layers = append(m.Layers, m.Layers[0])
			return nil
		},
		"layer-type": func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.Layers[0].MediaType = "wrong"; return nil },
		"layer-size": func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error { m.Layers[0].Size++; return nil },
		"layer-digest": func(_ Store, m *v1.Manifest, _ *v1.Descriptor) error {
			m.Layers[0].Digest = digest.FromString("other")
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := Store{Dir: filepath.Join(testDir(t), "store")}
			root, _ := fixtureLayerSet(t, store, true, true)
			refs, e := store.Referrers(root)
			must(t, e)
			b, e := store.Read(refs[0])
			must(t, e)
			var manifest v1.Manifest
			must(t, StrictJSON(b, &manifest))
			d := refs[0]
			must(t, mutate(store, &manifest, &d))
			d, e = store.PutJSON(v1.MediaTypeImageManifest, manifest)
			must(t, e)
			d.ArtifactType = MediaTypeCycloneDX
			must(t, os.RemoveAll(filepath.Join(store.Dir, "referrers")))
			must(t, store.RecordReferrer(root, d))
			if _, _, e = ConsumeLayerSet(context.Background(), store, root); e == nil {
				t.Fatal("invalid referrer accepted")
			}
		})
	}
}
func TestSBOMSemanticContract(t *testing.T) {
	inventory := []LockEntry{{Ecosystem: "asserted", Name: "name with spaces", Group: "group", Version: "1", Path: "/app/one", SHA256: digest.FromString("one").Encoded()}}
	base, e := buildSBOM(inventory, true)
	must(t, e)
	must(t, validateSBOM(base, inventory, true))
	for name, mutate := range map[string]func(*cycloneDX){
		"format":              func(d *cycloneDX) { d.BOMFormat = "other" },
		"schema":              func(d *cycloneDX) { d.SpecVersion = "1.6" },
		"version":             func(d *cycloneDX) { d.Version = 2 },
		"group":               func(d *cycloneDX) { d.Components[0].Group = "other" },
		"name":                func(d *cycloneDX) { d.Components[0].Name = "other" },
		"component-version":   func(d *cycloneDX) { d.Components[0].Version = "other" },
		"purl":                func(d *cycloneDX) { d.Components[0].PURL = "pkg:generic/other@1" },
		"hash":                func(d *cycloneDX) { d.Components[0].Hashes[0].Content = digest.FromString("wrong").Encoded() },
		"duplicate-component": func(d *cycloneDX) { d.Components = append(d.Components, d.Components[0]) },
		"duplicate-sha":       func(d *cycloneDX) { d.Components[0].Hashes = append(d.Components[0].Hashes, d.Components[0].Hashes[0]) },
		"path":                func(d *cycloneDX) { d.Components[0].Properties[1].Value = "/app/other" },
		"ecosystem":           func(d *cycloneDX) { d.Components[0].Properties[0].Value = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			var d cycloneDX
			must(t, json.Unmarshal(base, &d))
			mutate(&d)
			b, e := json.Marshal(d)
			must(t, e)
			if e = validateSBOM(b, inventory, true); e == nil {
				t.Fatal("SBOM mismatch accepted")
			}
		})
	}
}
