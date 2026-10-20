package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/distribution/reference"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type Registry struct {
	Origin                *url.URL
	Repository, Reference string
	Username, Password    string
	Client                *http.Client
	token                 string
	mu                    sync.RWMutex
}

func (r *Registry) client() *http.Client {
	base := r.Client
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	previous := base.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if e := r.safeURL(req.URL); e != nil {
			return e
		}
		if previous != nil {
			if e := previous(req, via); e != nil {
				return e
			}
		}
		if !r.sameOrigin(req.URL) {
			req.Header.Del("Authorization")
		}
		return nil
	}
	return &client
}

func NewRegistry(ref string, insecure bool) (*Registry, error) {
	named, e := reference.ParseNormalizedNamed(ref)
	if e != nil {
		return nil, e
	}
	host := reference.Domain(named)
	repo := reference.Path(named)
	tag := "latest"
	if d, ok := named.(reference.Digested); ok {
		tag = d.Digest().String()
	} else if t, ok := named.(reference.Tagged); ok {
		tag = t.Tag()
	}
	scheme := "https"
	hostname := host
	if h, _, e := net.SplitHostPort(host); e == nil {
		hostname = h
	}
	ip := net.ParseIP(strings.Trim(hostname, "[]"))
	if insecure || hostname == "localhost" || ip != nil && ip.IsLoopback() {
		scheme = "http"
	}
	origin, e := url.Parse(scheme + "://" + host)
	if e != nil {
		return nil, e
	}
	r := &Registry{Origin: origin, Repository: repo, Reference: tag}
	r.Client = &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		if e := r.safeURL(req.URL); e != nil {
			return e
		}
		if !r.sameOrigin(req.URL) {
			req.Header.Del("Authorization")
		}
		return nil
	}}
	transport, e := registryTransportFromEnvironment()
	if e != nil {
		return nil, e
	}
	if transport != nil {
		r.Client.Transport = transport
	}
	return r, nil
}
func (r *Registry) sameOrigin(u *url.URL) bool {
	return u.Scheme == r.Origin.Scheme && u.Host == r.Origin.Host
}
func (r *Registry) safeURL(u *url.URL) error {
	if u.User != nil || u.Host == "" {
		return fmt.Errorf("invalid registry URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && r.sameOrigin(u) && r.Origin.Scheme == "http" {
		return nil
	}
	return fmt.Errorf("unsafe registry URL %s", u.Redacted())
}
func (r *Registry) endpoint(kind, key string) *url.URL {
	u := *r.Origin
	u.Path = "/v2/" + r.Repository + "/" + kind + "/" + key
	return &u
}
func (r *Registry) request(ctx context.Context, method string, u *url.URL, body io.Reader, media string) (*http.Response, error) {
	if e := r.safeURL(u); e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, method, u.String(), body)
	if e != nil {
		return nil, e
	}
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	req.Header.Set("Accept", v1.MediaTypeImageIndex+", "+v1.MediaTypeImageManifest)
	if r.sameOrigin(u) {
		r.mu.RLock()
		token := r.token
		r.mu.RUnlock()
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else if r.Username != "" {
			req.SetBasicAuth(r.Username, r.Password)
		}
	}
	return r.client().Do(req)
}

var authParameter = regexp.MustCompile(`([a-zA-Z_]+)="([^"]*)"`)

func (r *Registry) authenticate(ctx context.Context, challenge string) error {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return fmt.Errorf("registry authentication failed")
	}
	params := map[string]string{}
	for _, m := range authParameter.FindAllStringSubmatch(challenge, -1) {
		params[strings.ToLower(m[1])] = m[2]
	}
	u, e := url.Parse(params["realm"])
	if e != nil {
		return e
	}
	if e = r.safeURL(u); e != nil {
		return e
	}
	q := u.Query()
	q.Set("service", params["service"])
	q.Set("scope", params["scope"])
	u.RawQuery = q.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return e
	}
	if r.sameOrigin(u) && r.Username != "" {
		req.SetBasicAuth(r.Username, r.Password)
	}
	resp, e := r.client().Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); e != nil {
		return e
	}
	token := t.Token
	if token == "" {
		token = t.AccessToken
	}
	if token == "" {
		return fmt.Errorf("empty registry token")
	}
	r.mu.Lock()
	r.token = token
	r.mu.Unlock()
	return nil
}
func (r *Registry) do(ctx context.Context, method string, u *url.URL, open func() (io.ReadCloser, error), media string) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var body io.ReadCloser
		var e error
		if open != nil {
			body, e = open()
			if e != nil {
				return nil, e
			}
		}
		resp, e := r.request(ctx, method, u, body, media)
		if body != nil {
			body.Close()
		}
		if e != nil {
			return nil, e
		}
		if resp.StatusCode != 401 || attempt == 1 {
			return resp, nil
		}
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		if e = r.authenticate(ctx, challenge); e != nil {
			return nil, e
		}
	}
	return nil, fmt.Errorf("authentication failed")
}
func status(resp *http.Response, want ...int) error {
	for _, s := range want {
		if resp.StatusCode == s {
			return nil
		}
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("registry returned %d: %s", resp.StatusCode, b)
}
func (r *Registry) fetchManifest(ctx context.Context, key string) (v1.Descriptor, []byte, error) {
	resp, e := r.do(ctx, "GET", r.endpoint("manifests", key), nil, "")
	if e != nil {
		return v1.Descriptor{}, nil, e
	}
	defer resp.Body.Close()
	if e = status(resp, 200); e != nil {
		return v1.Descriptor{}, nil, e
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
	if e != nil {
		return v1.Descriptor{}, nil, e
	}
	if len(b) > 16<<20 {
		return v1.Descriptor{}, nil, fmt.Errorf("manifest too large")
	}
	d := v1.Descriptor{MediaType: strings.Split(resp.Header.Get("Content-Type"), ";")[0], Digest: digest.FromBytes(b), Size: int64(len(b))}
	if strings.HasPrefix(key, "sha256:") && key != d.Digest.String() {
		return d, nil, fail("DigestMismatch", "manifest digest mismatch")
	}
	return d, b, nil
}

// Pull imports a manifest/index and its full graph into a local layout CAS.
func (r *Registry) Pull(ctx context.Context, store Store) (v1.Descriptor, error) {
	d, b, e := r.fetchManifest(ctx, r.Reference)
	if e != nil {
		return d, e
	}
	var importManifest func(v1.Descriptor, []byte) error
	imported := map[digest.Digest]bool{}
	importBlob := func(desc v1.Descriptor) error {
		resp, e := r.do(ctx, "GET", r.endpoint("blobs", desc.Digest.String()), nil, "")
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if e = status(resp, 200); e != nil {
			return e
		}
		got, e := store.Put(desc.MediaType, io.LimitReader(resp.Body, desc.Size+1))
		if e != nil {
			return e
		}
		if got.Digest != desc.Digest || got.Size != desc.Size {
			return fail("DigestMismatch", "downloaded blob identity mismatch")
		}
		return nil
	}
	importManifest = func(desc v1.Descriptor, b []byte) error {
		if imported[desc.Digest] {
			return nil
		}
		if len(imported) >= 10000 {
			return fmt.Errorf("OCI graph manifest limit exceeded")
		}
		got, e := store.Put(desc.MediaType, bytes.NewReader(b))
		if e != nil {
			return e
		}
		if got.Digest != desc.Digest || got.Size != desc.Size {
			return fail("DigestMismatch", "manifest mismatch")
		}
		imported[desc.Digest] = true
		switch desc.MediaType {
		case v1.MediaTypeImageIndex:
			var idx v1.Index
			if e = json.Unmarshal(b, &idx); e != nil {
				return e
			}
			for _, child := range idx.Manifests {
				got, b, e := r.fetchManifest(ctx, child.Digest.String())
				if e != nil {
					return e
				}
				if got.Digest != child.Digest || got.Size != child.Size || got.MediaType != child.MediaType {
					return fail("DigestMismatch", "index child mismatch")
				}
				if e = importManifest(child, b); e != nil {
					return e
				}
			}
		case v1.MediaTypeImageManifest:
			var m v1.Manifest
			if e = json.Unmarshal(b, &m); e != nil {
				return e
			}
			if e = importBlob(m.Config); e != nil {
				return e
			}
			for _, blob := range m.Layers {
				if e = importBlob(blob); e != nil {
					return e
				}
				if m.ArtifactType == MediaTypeLayerSet || m.Annotations["brewlet.sh/artifact-version"] == "2" {
					refs, err := r.Referrers(ctx, desc)
					if err != nil {
						return err
					}
					for _, ref := range refs {
						got, data, err := r.fetchManifest(ctx, ref.Digest.String())
						if err == nil && !sameSubject(got, ref) {
							err = fail("DigestMismatch", "referrer manifest identity mismatch")
						}
						if err == nil {
							err = importManifest(ref, data)
						}
						if err != nil && ref.ArtifactType != MediaTypeAttestation {
							return err
						}
						if err = store.RecordReferrer(desc, ref); err != nil {
							return err
						}
					}
					materials, err := CompositionMaterials(store, desc)
					if err != nil {
						return err
					}
					for _, material := range materials {
						if imported[material.Digest] {
							continue
						}
						got, data, err := r.fetchManifest(ctx, material.Digest.String())
						if err != nil {
							return err
						}
						if !sameSubject(got, material) {
							return fail("DigestMismatch", "composition material identity mismatch")
						}
						if err = importManifest(material, data); err != nil {
							return err
						}
					}
				}
			}
		default:
			return fail("UnsupportedArtifact", "registry media type %s", desc.MediaType)
		}
		return nil
	}
	e = importManifest(d, b)
	if e != nil {
		return d, e
	}
	return d, e
}
func (r *Registry) Push(ctx context.Context, store Store, target v1.Descriptor) error {
	var pushManifest func(v1.Descriptor, string) error
	nativeSubjects := map[digest.Digest]bool{}
	pushed := map[digest.Digest]bool{}
	pushBlob := func(d v1.Descriptor) error {
		resp, e := r.do(ctx, "HEAD", r.endpoint("blobs", d.Digest.String()), nil, "")
		if e != nil {
			return e
		}
		code := resp.StatusCode
		resp.Body.Close()
		if code == 200 {
			return nil
		}
		if code != 404 {
			return fmt.Errorf("blob HEAD returned %d", code)
		}
		resp, e = r.do(ctx, "POST", r.endpoint("blobs", "uploads/"), nil, "")
		if e != nil {
			return e
		}
		if e = status(resp, 202); e != nil {
			resp.Body.Close()
			return e
		}
		location := resp.Header.Get("Location")
		resp.Body.Close()
		if location == "" {
			return fmt.Errorf("upload omitted Location")
		}
		u, e := url.Parse(location)
		if e != nil {
			return e
		}
		u = r.Origin.ResolveReference(u)
		q := u.Query()
		q.Set("digest", d.Digest.String())
		u.RawQuery = q.Encode()
		resp, e = r.do(ctx, "PUT", u, func() (io.ReadCloser, error) { return store.Open(d) }, "application/octet-stream")
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		return status(resp, 201)
	}
	pushManifest = func(d v1.Descriptor, key string) error {
		if pushed[d.Digest] {
			return nil
		}
		if len(pushed) >= 10000 {
			return fmt.Errorf("OCI graph manifest limit exceeded")
		}
		pushed[d.Digest] = true
		b, e := store.Read(d)
		if e != nil {
			return e
		}
		var subject *v1.Descriptor
		if d.MediaType == v1.MediaTypeImageIndex {
			var idx v1.Index
			if e = json.Unmarshal(b, &idx); e != nil {
				return e
			}
			for _, child := range idx.Manifests {
				if e = pushManifest(child, child.Digest.String()); e != nil {
					return e
				}
			}
		} else {
			var m v1.Manifest
			if e = json.Unmarshal(b, &m); e != nil {
				return e
			}
			subject = m.Subject
			if e = pushBlob(m.Config); e != nil {
				return e
			}
			for _, blob := range m.Layers {
				if e = pushBlob(blob); e != nil {
					return e
				}
			}
		}
		resp, e := r.do(ctx, "PUT", r.endpoint("manifests", key), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }, d.MediaType)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if e = status(resp, 201, 202); e != nil {
			return e
		}
		if subject != nil {
			nativeSubjects[d.Digest] = resp.Header.Get("OCI-Subject") == subject.Digest.String()
		}
		refs, e := store.Referrers(d)
		if e != nil {
			return e
		}
		for _, ref := range refs {
			if e = pushManifest(ref, ref.Digest.String()); e != nil {
				return e
			}
			if !nativeSubjects[ref.Digest] {
				data, err := store.Read(ref)
				if err != nil {
					return err
				}
				response, err := r.do(ctx, "PUT", r.endpoint("manifests", ReferrerTag(d, ref)),
					func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }, ref.MediaType)
				if err != nil {
					return err
				}
				err = status(response, 201, 202)
				response.Body.Close()
				if err != nil {
					return err
				}
			}
		}
		materials, e := CompositionMaterials(store, d)
		if e != nil {
			return e
		}
		for _, material := range materials {
			if e = pushManifest(material, material.Digest.String()); e != nil {
				return e
			}
		}
		return nil
	}
	if e := pushManifest(target, target.Digest.String()); e != nil {
		return e
	}
	if r.Reference == target.Digest.String() {
		return nil
	}
	b, e := store.Read(target)
	if e != nil {
		return e
	}
	resp, e := r.do(ctx, "PUT", r.endpoint("manifests", r.Reference),
		func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil }, target.MediaType)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	return status(resp, 201, 202)
}
