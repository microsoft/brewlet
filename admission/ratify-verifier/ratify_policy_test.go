// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/microsoft/brewlet/pkg/attest"
	"github.com/opencontainers/go-digest"
	"github.com/ratify-project/ratify/config"
	"github.com/ratify-project/ratify/pkg/common"
	"github.com/ratify-project/ratify/pkg/executor"
	executorcore "github.com/ratify-project/ratify/pkg/executor/core"
	executortypes "github.com/ratify-project/ratify/pkg/executor/types"
	"github.com/ratify-project/ratify/pkg/ocispecs"
	"github.com/ratify-project/ratify/pkg/policyprovider"
	policyconfig "github.com/ratify-project/ratify/pkg/policyprovider/config"
	_ "github.com/ratify-project/ratify/pkg/policyprovider/configpolicy"
	policyfactory "github.com/ratify-project/ratify/pkg/policyprovider/factory"
	_ "github.com/ratify-project/ratify/pkg/policyprovider/regopolicy"
	"github.com/ratify-project/ratify/pkg/referrerstore"
	"github.com/ratify-project/ratify/pkg/verifier"
	verifierconfig "github.com/ratify-project/ratify/pkg/verifier/config"
	"github.com/ratify-project/ratify/pkg/verifier/plugin"
	"github.com/ratify-project/ratify/pkg/verifier/plugin/skel"
	verifiertypes "github.com/ratify-project/ratify/pkg/verifier/types"
	"sigs.k8s.io/yaml"
)

func cliConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Load("../deploy/config.json")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func deployedPolicies(t *testing.T) map[string]policyprovider.PolicyProvider {
	t.Helper()
	raw, err := os.ReadFile("../deploy/30-ratify-policy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cluster struct {
		Spec struct {
			Type       string
			Parameters policyconfig.PolicyPluginConfig
		}
	}
	if err := yaml.Unmarshal(raw, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster.Spec.Parameters["name"] = cluster.Spec.Type
	configs := map[string]policyconfig.PoliciesConfig{
		"CLI":     cliConfig(t).PoliciesConfig,
		"cluster": {PolicyPlugin: cluster.Spec.Parameters},
	}
	policies := make(map[string]policyprovider.PolicyProvider)
	for name, cfg := range configs {
		policies[name], err = policyfactory.CreatePolicyProviderFromConfig(cfg)
		if err != nil {
			t.Fatalf("%s policy: %v", name, err)
		}
	}
	return policies
}

// Only the subprocess transport is replaced: selection uses Ratify's plugin
// implementation and verification uses our entrypoint and shared DSSE verifier.
type localManagedVerifier struct {
	verifier.ReferenceVerifier
	stdin []byte
}

func (v localManagedVerifier) Verify(_ context.Context, subject common.Reference, ref ocispecs.ReferenceDescriptor, store referrerstore.ReferrerStore) (verifier.VerifierResult, error) {
	result, err := VerifyReference(&skel.CmdArgs{StdinData: v.stdin}, subject, ref, store)
	if err != nil {
		return verifier.VerifierResult{}, err
	}
	return *result, nil
}

func managedVerifier(t *testing.T, publicKey string) verifier.ReferenceVerifier {
	t.Helper()
	cfg := cliConfig(t)
	if len(cfg.VerifiersConfig.Verifiers) != 1 {
		t.Fatal("shipped config must register the Brewlet verifier only")
	}
	vc := cfg.VerifiersConfig.Verifiers[0]
	if vc["name"] != pluginName || vc["artifactTypes"] != attest.AttestationArtifactType ||
		vc["expectedBuilderIdentity"] != identity || vc["trustedPublicKeyPath"] != "/etc/brewlet/trusted-builder.pub" {
		t.Fatalf("unexpected deployed verifier contract: %v", vc)
	}
	if typ, exists := vc["type"]; exists && typ != pluginName {
		t.Fatalf("configured verifier must invoke the Brewlet binary, not %v", typ)
	}
	// Supply the test trust anchor instead of reading the operator's mounted key.
	delete(vc, "trustedPublicKeyPath")
	vc["trustedPublicKey"] = publicKey
	selection, err := plugin.NewVerifier(cfg.VerifiersConfig.Version, vc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !selection.CanVerify(context.Background(), managedRefDesc()) ||
		selection.CanVerify(context.Background(), ocispecs.ReferenceDescriptor{ArtifactType: "unrelated"}) {
		t.Fatal("Brewlet verifier must select exactly its attestation artifact type")
	}
	return localManagedVerifier{selection, mustStdin(t, map[string]any{"config": vc})}
}

type candidateStore struct {
	*fakeStore
	refs      []ocispecs.ReferenceDescriptor
	manifests map[digest.Digest]ocispecs.ReferenceManifest
}

func candidates(envelopes ...[]byte) *candidateStore {
	store := &candidateStore{
		fakeStore: &fakeStore{blobs: make(map[digest.Digest][]byte)},
		manifests: make(map[digest.Digest]ocispecs.ReferenceManifest),
	}
	for i, envelope := range envelopes {
		one := storeFor(envelope)
		ref := managedRefDesc()
		ref.Digest = digest.FromString(fmt.Sprintf("candidate-%d-%s", i, digest.FromBytes(envelope)))
		store.refs = append(store.refs, ref)
		store.manifests[ref.Digest] = one.manifest
		for d, blob := range one.blobs {
			store.blobs[d] = blob
		}
	}
	return store
}

func (s *candidateStore) addUnrelatedReferrer() {
	ref := ocispecs.ReferenceDescriptor{ArtifactType: "unrelated"}
	ref.Digest = digest.FromString("unrelated")
	s.refs = append(s.refs, ref)
}

func (s *candidateStore) ListReferrers(_ context.Context, subject common.Reference, _ []string, _ string, _ *ocispecs.SubjectDescriptor) (referrerstore.ListReferrersResult, error) {
	if subject.Digest.String() == subjectDigest {
		return referrerstore.ListReferrersResult{Referrers: s.refs}, nil
	}
	// Rego's executor also discovers nested referrers; none exist in this fixture.
	return referrerstore.ListReferrersResult{}, nil
}

func (s *candidateStore) GetSubjectDescriptor(_ context.Context, subject common.Reference) (*ocispecs.SubjectDescriptor, error) {
	desc := &ocispecs.SubjectDescriptor{}
	desc.Digest = subject.Digest
	return desc, nil
}

func (s *candidateStore) GetReferenceManifest(_ context.Context, _ common.Reference, ref ocispecs.ReferenceDescriptor) (ocispecs.ReferenceManifest, error) {
	manifest, ok := s.manifests[ref.Digest]
	if !ok {
		return ocispecs.ReferenceManifest{}, fmt.Errorf("unknown referrer %s", ref.Digest)
	}
	return manifest, nil
}

func runRatify(t *testing.T, policy policyprovider.PolicyProvider, store referrerstore.ReferrerStore, verifiers ...verifier.ReferenceVerifier) executortypes.VerifyResult {
	t.Helper()
	engine := executorcore.Executor{
		ReferrerStores: []referrerstore.ReferrerStore{store},
		Verifiers:      verifiers,
		PolicyEnforcer: policy,
	}
	result, err := engine.VerifySubject(context.Background(), executor.VerifyParameters{
		Subject: "registry.example.com/application@" + subjectDigest,
	})
	if err != nil {
		t.Fatalf("Ratify executor: %v", err)
	}
	return result
}

func TestRatifySignerRotation(t *testing.T) {
	key, oldKey := testKey(t), testKey(t)
	current := sign(t, key, validPredicate(), subjectDigest)
	old := sign(t, oldKey, validPredicate(), subjectDigest)
	wrongBuilder := validPredicate()
	wrongBuilder.BuilderIdentity = "untrusted-builder"
	partial := validPredicate()
	partial.DependencyBundleDigest = ""
	differentSubject := "sha256:" + strings.Repeat("9", 64)
	wrongSubject := validPredicate()
	wrongSubject.FinalImageDigest = differentSubject
	// Change a well-formed signed bundle claim without updating its signature.
	var tampered map[string]any
	if err := json.Unmarshal(current, &tampered); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.StdEncoding.DecodeString(tampered["payload"].(string))
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(strings.Replace(string(payload), `"dependencyBundleDigest":"sha256:`+strings.Repeat("2", 64)+`"`,
		`"dependencyBundleDigest":"sha256:`+strings.Repeat("3", 64)+`"`, 1))
	tampered["payload"] = base64.StdEncoding.EncodeToString(payload)
	tamperedEnvelope := mustStdin(t, tampered)
	cases := []struct {
		name      string
		envelopes [][]byte
		want      bool
	}{
		{"current", [][]byte{current}, true},
		{"old then current", [][]byte{old, current}, true},
		{"current then old", [][]byte{current, old}, true},
		{"malformed then current", [][]byte{[]byte("{"), current}, true},
		{"current then malformed", [][]byte{current, []byte("{")}, true},
		{"tampered then current", [][]byte{tamperedEnvelope, current}, true},
		{"current then tampered", [][]byte{current, tamperedEnvelope}, true},
		{"no evidence", nil, false},
		{"old key only", [][]byte{old}, false},
		{"wrong builder", [][]byte{sign(t, key, wrongBuilder, subjectDigest)}, false},
		{"wrong subject", [][]byte{sign(t, key, wrongSubject, differentSubject)}, false},
		{"tampered bundle claim", [][]byte{tamperedEnvelope}, false},
		{"all untrusted", [][]byte{old, tamperedEnvelope, sign(t, key, wrongBuilder, subjectDigest)}, false},
		// One has the correct identity but the wrong key; another has the right
		// key but the wrong identity; a third lacks a required bundle claim.
		{"no merging partial trust", [][]byte{old, sign(t, key, wrongBuilder, subjectDigest), sign(t, key, partial, subjectDigest)}, false},
	}
	managed := managedVerifier(t, pubPEM(t, key))
	for policyName, policy := range deployedPolicies(t) {
		t.Run(policyName, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					got := runRatify(t, policy, candidates(tc.envelopes...), managed)
					if got.IsSuccess != tc.want {
						t.Fatalf("isSuccess=%v, want %v; reports=%+v", got.IsSuccess, tc.want, got.VerifierReports)
					}
				})
			}
		})
	}
}

type unrelatedVerifier struct {
	verifier.ReferenceVerifier
	calls   atomic.Int32
	success bool
}

func (v *unrelatedVerifier) Verify(context.Context, common.Reference, ocispecs.ReferenceDescriptor, referrerstore.ReferrerStore) (verifier.VerifierResult, error) {
	v.calls.Add(1)
	return verifier.VerifierResult{Name: v.Name(), VerifierName: v.Name(), IsSuccess: v.success}, nil
}

func TestRatifyVerifierSelection(t *testing.T) {
	key := testKey(t)
	managed := managedVerifier(t, pubPEM(t, key))
	for policyName, policy := range deployedPolicies(t) {
		for _, otherType := range []string{"*", attest.AttestationArtifactType, "unrelated"} {
			for _, otherFirst := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/other-first=%t", policyName, otherType, otherFirst), func(t *testing.T) {
					selection, err := plugin.NewVerifier("1.0.0", verifierconfig.VerifierConfig{
						"name": "unrelated-verifier", "artifactTypes": otherType,
					}, nil)
					if err != nil {
						t.Fatal(err)
					}
					other := &unrelatedVerifier{ReferenceVerifier: selection, success: true}
					verifiers := []verifier.ReferenceVerifier{managed, other}
					if otherFirst {
						verifiers[0], verifiers[1] = verifiers[1], verifiers[0]
					}
					for _, trusted := range []bool{false, true} {
						signer := key
						if !trusted {
							signer = testKey(t)
						}
						store := candidates(sign(t, signer, validPredicate(), subjectDigest))
						if otherType == "unrelated" {
							store.addUnrelatedReferrer()
						}
						got := runRatify(t, policy, store, verifiers...)
						if got.IsSuccess != trusted {
							t.Fatalf("unrelated success changed decision: trusted=%t, result=%+v", trusted, got)
						}
					}
					if other.calls.Load() != 2 {
						t.Fatalf("expected both verifiers to run; unrelated calls=%d", other.calls.Load())
					}
					if got := runRatify(t, policy, candidates([]byte("{")), other); got.IsSuccess {
						t.Fatal("missing Brewlet verifier must not admit on another verifier's behalf")
					}
					if otherType == "unrelated" {
						store := candidates()
						store.addUnrelatedReferrer()
						if got := runRatify(t, policy, store, verifiers...); got.IsSuccess {
							t.Fatal("unrelated evidence alone must not substitute for a Brewlet attestation")
						}
					}
					other.success = false
					store := candidates(sign(t, key, validPredicate(), subjectDigest))
					store.addUnrelatedReferrer()
					if got := runRatify(t, policy, store, verifiers...); !got.IsSuccess {
						t.Fatal("another verifier's failure must not block a valid Brewlet candidate")
					}
				})
			}
		}
	}
}

func TestRatifyPolicyReportBinding(t *testing.T) {
	report := func(artifactType, verifierName string, success bool) executortypes.NestedVerifierReport {
		return executortypes.NestedVerifierReport{
			ArtifactType: artifactType,
			VerifierReports: []verifiertypes.VerifierResult{{
				VerifierName: verifierName, IsSuccess: success,
			}},
		}
	}
	valid := report(attest.AttestationArtifactType, pluginName, true)
	bad := report(attest.AttestationArtifactType, pluginName, false)
	wrongType := report("unrelated", pluginName, true)
	wrongName := report(attest.AttestationArtifactType, "unrelated", true)
	nestedOnly := bad
	nestedOnly.NestedReports = []executortypes.NestedVerifierReport{valid}
	for policyName, policy := range deployedPolicies(t) {
		for _, tc := range []struct {
			name    string
			reports []any
			want    bool
		}{
			{"valid", []any{valid}, true},
			{"bad then valid", []any{bad, valid}, true},
			{"valid then bad", []any{valid, bad}, true},
			{"empty", nil, false},
			{"wrong verifier", []any{wrongName}, false},
			{"wrong artifact", []any{wrongType}, false},
			{"no cross-report binding", []any{wrongType, wrongName}, false},
			{"nested success only", []any{nestedOnly}, false},
		} {
			t.Run(policyName+"/"+tc.name, func(t *testing.T) {
				if got := policy.OverallVerifyResult(context.Background(), tc.reports); got != tc.want {
					t.Fatalf("isSuccess=%t, want %t", got, tc.want)
				}
			})
		}
	}
}
