// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestConformanceWire(t *testing.T) {
	for _, tc := range loadConformance(t).Wire {
		t.Run(tc.ID, func(t *testing.T) {
			const body = `{"schemaVersion":2}`
			const scope = "repository:team/app:pull,push"
			const secret = "refresh/+=="
			basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:"+secret))
			cred := &Credential{Username: "user", Secret: secret}
			if tc.Identity {
				cred = &Credential{Username: IdentityTokenUsername, Secret: secret, IdentityToken: true}
			}
			var mu sync.Mutex
			var realmCalls, landingCalls, manifestCalls int
			var origin, foreign string
			handler := func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				auth := r.Header.Get("Authorization")
				switch r.URL.Path {
				case "/token":
					realmCalls++
					if err := r.ParseForm(); err != nil {
						t.Error(err)
					}
					if r.Form.Get("scope") != scope || r.Form.Get("service") != "registry" {
						t.Errorf("token parameters = %v", r.Form)
					}
					if tc.Identity {
						if r.Method != "POST" || auth != "" || r.Form.Get("refresh_token") != secret || r.Form.Get("grant_type") != "refresh_token" {
							t.Error("identity token not exchanged as refresh grant")
						}
					} else if r.Method != "GET" || auth != basic {
						t.Error("password not exchanged as Basic")
					}
					if tc.Operation == "tokenRedirect" {
						w.Header().Set("Location", foreign+"/landing")
						w.WriteHeader(307)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"token":"issued"}`)
				case "/landing":
					landingCalls++
					wantAuth := basic
					if tc.Foreign {
						wantAuth = ""
					}
					got, _ := io.ReadAll(r.Body)
					if auth != wantAuth || r.Method != "PUT" || string(got) != body || r.Header.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" {
						t.Error("redirect body, method, media type or credential scope mismatch")
					}
					w.WriteHeader(201)
				default:
					manifestCalls++
					if tc.Operation == "redirect" {
						target := origin
						if tc.Foreign {
							target = foreign
						}
						w.Header().Set("Location", target+"/landing")
						w.WriteHeader(307)
						return
					}
					if auth == "Bearer issued" {
						w.WriteHeader(201)
						return
					}
					if tc.Identity && auth != "" {
						t.Error("identity token leaked into initial registry auth")
					}
					realm := origin
					if tc.Operation == "realm" && tc.Foreign {
						realm = foreign
					}
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry",scope="%s"`, realm, scope))
					w.WriteHeader(401)
				}
			}
			reg := httptest.NewServer(http.HandlerFunc(handler))
			defer reg.Close()
			other := httptest.NewServer(http.HandlerFunc(handler))
			defer other.Close()
			origin, foreign = reg.URL, other.URL
			var allowed []string
			if tc.Allowed {
				allowed = []string{strings.TrimPrefix(foreign, "http://")}
			}
			policy, err := NewTrustPolicy(nil, allowed)
			if err != nil {
				t.Fatal(err)
			}
			u, _ := url.Parse(origin)
			client, err := NewClient(u.Host, "team/app", cred, policy, reg.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = client.PutManifest(context.Background(), "latest", []byte(body), "application/vnd.oci.image.index.v1+json")
			if (err == nil) != tc.Success {
				t.Fatalf("success=%v, error=%v", tc.Success, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if tc.Operation == "redirect" {
				if landingCalls != 1 || manifestCalls != 1 || realmCalls != 0 {
					t.Fatalf("redirect requests: landing=%d manifest=%d token=%d", landingCalls, manifestCalls, realmCalls)
				}
			} else {
				wantRealm, wantManifest := 1, 1
				if tc.Success {
					wantManifest = 2
				} else if tc.Operation == "realm" {
					wantRealm = 0
				}
				if realmCalls != wantRealm || manifestCalls != wantManifest || landingCalls != 0 {
					t.Fatalf("auth requests: landing=%d manifest=%d token=%d", landingCalls, manifestCalls, realmCalls)
				}
			}
		})
	}
}
