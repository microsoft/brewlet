// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

//go:embed testdata/conformance.json
var conformanceJSON []byte

type wireCase struct {
	ID, Operation                       string
	Foreign, Allowed, Identity, Success bool
}

type conformanceFixtures struct {
	References []struct {
		ID, Input, Registry, Repository, Reference string
		Explicit, Publish                          bool
	}
	Authorities []struct {
		ID, Authority    string
		Insecure         []string
		Valid, Plaintext bool
	}
	Realms []struct {
		ID, Origin, Realm              string
		Insecure, Allowed              []string
		Valid, Credentials, SameOrigin bool
	}
	Credentials []struct {
		ID, Registry, Username, Secret string
		Config                         json.RawMessage
		Env                            map[string]string
		Helpers                        map[string]json.RawMessage
		Calls                          []string
		Identity, HelperError          bool
	}
	Wire []wireCase
}

func loadConformance(t *testing.T) conformanceFixtures {
	t.Helper()
	var fixtures conformanceFixtures
	decoder := json.NewDecoder(bytes.NewReader(conformanceJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{len(fixtures.References), len(fixtures.Authorities), len(fixtures.Realms), len(fixtures.Credentials), len(fixtures.Wire)} {
		if count == 0 {
			t.Fatal("missing or empty conformance section")
		}
	}
	return fixtures
}

func TestConformanceReferences(t *testing.T) {
	for _, tc := range loadConformance(t).References {
		t.Run(tc.ID, func(t *testing.T) {
			if got := HasExplicitRegistry(tc.Input); got != tc.Explicit {
				t.Fatalf("explicit = %v, want %v", got, tc.Explicit)
			}
			ref, err := ParseReference(tc.Input)
			if !tc.Publish {
				if err == nil {
					t.Fatal("accepted invalid or non-publishing reference")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ref.Registry != tc.Registry || ref.Repository != tc.Repository || ref.Tag != tc.Reference {
				t.Fatalf("parsed reference = %+v", ref)
			}
		})
	}
}

func TestConformanceTrust(t *testing.T) {
	for _, tc := range loadConformance(t).Authorities {
		t.Run("authority/"+tc.ID, func(t *testing.T) {
			p, err := NewTrustPolicy(tc.Insecure, nil)
			if err != nil {
				t.Fatal(err)
			}
			if IsBareAuthority(tc.Authority) != tc.Valid || p.AllowsPlaintext(tc.Authority) != tc.Plaintext {
				t.Fatalf("authority %q: valid=%v plaintext=%v", tc.Authority, IsBareAuthority(tc.Authority), p.AllowsPlaintext(tc.Authority))
			}
			_, err = NewTrustPolicy([]string{tc.Authority}, nil)
			if (err == nil) != tc.Valid {
				t.Fatalf("insecure configuration: %v", err)
			}
			_, err = NewTrustPolicy(nil, []string{tc.Authority})
			if (err == nil) != tc.Valid {
				t.Fatalf("realm configuration: %v", err)
			}
		})
	}
	for _, tc := range loadConformance(t).Realms {
		t.Run("realm/"+tc.ID, func(t *testing.T) {
			p, err := NewTrustPolicy(tc.Insecure, tc.Allowed)
			if err != nil {
				t.Fatal(err)
			}
			realm, err := p.ValidateTokenRealm(tc.Realm)
			if (err == nil) != tc.Valid {
				t.Fatalf("validate realm: %v", err)
			}
			if !tc.Valid {
				return
			}
			origin, err := url.Parse(tc.Origin)
			if err != nil {
				t.Fatal(err)
			}
			if p.AllowsCredentials(origin, realm) != tc.Credentials || SameOrigin(origin, realm) != tc.SameOrigin {
				t.Fatal("credential or origin policy mismatch")
			}
		})
	}
}

func TestConformanceCredentials(t *testing.T) {
	for _, tc := range loadConformance(t).Credentials {
		t.Run(tc.ID, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), tc.Config, 0o600); err != nil {
				t.Fatal(err)
			}
			calls := []string{}
			resolver := CredentialResolver{
				Getenv: func(key string) string {
					if key == "DOCKER_CONFIG" {
						return dir
					}
					return tc.Env[key]
				},
				HomeDir: func() (string, error) {
					t.Fatal("must not inspect user home")
					return "", nil
				},
				RunHelper: func(helper, server string) (string, error) {
					calls = append(calls, helper+"|"+server)
					if tc.HelperError {
						return "", errors.New("fixture helper failure")
					}
					return string(tc.Helpers[helper]), nil
				},
			}
			got := resolver.Resolve(tc.Registry)
			if !reflect.DeepEqual(calls, tc.Calls) {
				t.Fatalf("helper calls = %v, want %v", calls, tc.Calls)
			}
			if tc.Username == "" {
				if got != nil {
					t.Fatal("expected anonymous access")
				}
			} else if got == nil || got.Username != tc.Username || got.Secret != tc.Secret || got.IdentityToken != tc.Identity {
				t.Fatalf("wrong credential selection: %v", got)
			}
		})
	}
}
