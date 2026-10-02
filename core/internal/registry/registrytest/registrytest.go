// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package registrytest provides an in-memory OCI distribution registry for
// tests. It implements just enough of the push API (HEAD blob, monolithic
// POST+PUT upload, PUT manifest) plus optional Basic or Bearer auth.
package registrytest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
)

// Auth selects how the fake registry authenticates requests.
type Auth int

const (
	// Anonymous accepts every request.
	Anonymous Auth = iota
	// Basic requires HTTP Basic credentials.
	Basic
	// Bearer requires a token from the token service.
	Bearer
)

// Request records one request the registry or its token service received.
type Request struct {
	Method        string
	Path          string
	Authorization string
	ContentType   string
	Form          url.Values
}

// Manifest is a stored manifest.
type Manifest struct {
	MediaType string
	Body      []byte
}

// Registry is a fake OCI registry.
type Registry struct {
	Server *httptest.Server
	// Token is the token service; a separate origin from Server unless
	// SameOriginRealm is set.
	Token *httptest.Server

	Auth         Auth
	Username     string
	Password     string
	RefreshToken string
	// SameOriginRealm serves the token endpoint from the registry itself.
	SameOriginRealm bool
	// UploadLocation, when set, is returned as the upload Location instead
	// of a same-origin URL (e.g. a foreign "signed URL").
	UploadLocation string

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]Manifest // keyed by repo + ":" + reference
	requests  []Request
	tokenReqs []Request
	putOrder  []string
	uploads   atomic.Int64
}

const issuedToken = "fake-registry-token"

// New starts a fake registry. Call Close when done.
func New(auth Auth) *Registry {
	r := &Registry{Auth: auth, blobs: map[string][]byte{}, manifests: map[string]Manifest{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	r.Token = httptest.NewServer(http.HandlerFunc(r.serveToken))
	return r
}

// Close shuts the servers down.
func (r *Registry) Close() {
	r.Server.Close()
	r.Token.Close()
}

// Host returns the registry authority (127.0.0.1:port).
func (r *Registry) Host() string { return strings.TrimPrefix(r.Server.URL, "http://") }

// TokenHost returns the token service authority.
func (r *Registry) TokenHost() string { return strings.TrimPrefix(r.Token.URL, "http://") }

// Requests returns the registry requests received so far.
func (r *Registry) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.requests...)
}

// TokenRequests returns the token service requests received so far.
func (r *Registry) TokenRequests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.tokenReqs...)
}

// ManifestPuts returns the manifest references in the order they were put.
func (r *Registry) ManifestPuts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.putOrder...)
}

// Uploads returns how many blob uploads completed.
func (r *Registry) Uploads() int64 { return r.uploads.Load() }

// Manifest returns the manifest stored under repo and reference.
func (r *Registry) Manifest(repo, reference string) (Manifest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.manifests[repo+":"+reference]
	return m, ok
}

// Blob returns a stored blob.
func (r *Registry) Blob(digest string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.blobs[digest]
	return b, ok
}

// PutBlob preloads a blob and returns its digest.
func (r *Registry) PutBlob(content []byte) string {
	d := Digest(content)
	r.mu.Lock()
	r.blobs[d] = content
	r.mu.Unlock()
	return d
}

// Digest returns the sha256 digest of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r *Registry) record(list *[]Request, req *http.Request, form url.Values) {
	r.mu.Lock()
	*list = append(*list, Request{
		Method: req.Method, Path: req.URL.Path, Authorization: req.Header.Get("Authorization"),
		ContentType: req.Header.Get("Content-Type"), Form: form,
	})
	r.mu.Unlock()
}

func (r *Registry) realm() string {
	if r.SameOriginRealm {
		return r.Server.URL + "/token"
	}
	return r.Token.URL + "/token"
}

func (r *Registry) authorized(req *http.Request) bool {
	h := req.Header.Get("Authorization")
	switch r.Auth {
	case Basic:
		return h == "Basic "+base64.StdEncoding.EncodeToString([]byte(r.Username+":"+r.Password))
	case Bearer:
		return h == "Bearer "+issuedToken
	}
	return true
}

func (r *Registry) challenge(w http.ResponseWriter, repo string) {
	switch r.Auth {
	case Basic:
		w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
	case Bearer:
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fake-registry",scope="repository:%s:pull,push"`, r.realm(), repo))
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func (r *Registry) serveToken(w http.ResponseWriter, req *http.Request) {
	var form url.Values
	if req.Method == http.MethodPost {
		_ = req.ParseForm()
		form = req.PostForm
	} else {
		form = req.URL.Query()
	}
	r.record(&r.tokenReqs, req, form)
	ok := false
	switch {
	case req.Method == http.MethodPost:
		ok = r.RefreshToken != "" && form.Get("grant_type") == "refresh_token" && form.Get("refresh_token") == r.RefreshToken
	case r.Username == "":
		ok = req.Header.Get("Authorization") == ""
	default:
		ok = req.Header.Get("Authorization") == "Basic "+base64.StdEncoding.EncodeToString([]byte(r.Username+":"+r.Password))
	}
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"access_token": issuedToken})
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" && r.SameOriginRealm {
		r.serveToken(w, req)
		return
	}
	r.record(&r.requests, req, nil)
	rest, ok := strings.CutPrefix(req.URL.Path, "/v2/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	var repo, kind, ref string
	for _, k := range []string{"/blobs/uploads/", "/blobs/", "/manifests/"} {
		if i := strings.LastIndex(rest, k); i >= 0 {
			repo, kind, ref = rest[:i], k, rest[i+len(k):]
			break
		}
	}
	if repo == "" {
		http.NotFound(w, req)
		return
	}
	if !r.authorized(req) {
		r.challenge(w, repo)
		return
	}
	switch {
	case kind == "/blobs/" && req.Method == http.MethodHead:
		if _, ok := r.Blob(ref); ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case kind == "/blobs/uploads/" && req.Method == http.MethodPost:
		loc := r.UploadLocation
		if loc == "" {
			loc = "/v2/" + repo + "/blobs/uploads/session-1?state=abc"
		}
		w.Header().Set("Location", loc)
		w.WriteHeader(http.StatusAccepted)
	case kind == "/blobs/uploads/" && req.Method == http.MethodPut:
		r.FinishUpload(w, req)
	case kind == "/manifests/" && req.Method == http.MethodPut:
		body, _ := io.ReadAll(req.Body)
		m := Manifest{MediaType: req.Header.Get("Content-Type"), Body: body}
		d := Digest(body)
		r.mu.Lock()
		r.manifests[repo+":"+ref] = m
		r.manifests[repo+":"+d] = m
		r.putOrder = append(r.putOrder, ref)
		r.mu.Unlock()
		w.Header().Set("Docker-Content-Digest", d)
		w.WriteHeader(http.StatusCreated)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// FinishUpload completes a monolithic blob upload (PUT ?digest=), verifying
// the content digest. It is exported so tests can serve uploads from a
// foreign "blob storage" origin.
func (r *Registry) FinishUpload(w http.ResponseWriter, req *http.Request) {
	want := req.URL.Query().Get("digest")
	body, err := io.ReadAll(req.Body)
	if err != nil || Digest(body) != want {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.PutBlob(body)
	r.uploads.Add(1)
	w.WriteHeader(http.StatusCreated)
}
