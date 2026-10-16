package artifact

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type registryObject struct {
	Media string
	Data  []byte
}
type referrerRegistryFixture struct {
	mu                    sync.Mutex
	native                bool
	objects               map[string]registryObject
	referrers             map[string][]v1.Descriptor
	tags                  map[string]bool
	nativePages, tagPages int
}

func (f *referrerRegistryFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimPrefix(r.URL.Path, "/v2/team/layers/")
	if key == "blobs/uploads/" && r.Method == "POST" {
		w.Header().Set("Location", "/v2/team/layers/blobs/uploads/id")
		w.WriteHeader(202)
		return
	}
	if key == "blobs/uploads/id" && r.Method == "PUT" {
		b, _ := io.ReadAll(r.Body)
		dg := r.URL.Query().Get("digest")
		if digest.FromBytes(b).String() != dg {
			w.WriteHeader(400)
			return
		}
		f.objects["blobs/"+dg] = registryObject{"application/octet-stream", b}
		w.WriteHeader(201)
		return
	}
	if strings.HasPrefix(key, "manifests/") && r.Method == "PUT" {
		b, _ := io.ReadAll(r.Body)
		object := registryObject{r.Header.Get("Content-Type"), b}
		f.objects[key] = object
		dg := digest.FromBytes(b)
		f.objects["manifests/"+dg.String()] = object
		if tag := strings.TrimPrefix(key, "manifests/"); !strings.HasPrefix(tag, "sha256:") {
			f.tags[tag] = true
		}
		var m v1.Manifest
		if json.Unmarshal(b, &m) == nil && m.Subject != nil && f.native {
			w.Header().Set("OCI-Subject", m.Subject.Digest.String())
			d := v1.Descriptor{MediaType: v1.MediaTypeImageManifest, Digest: dg, Size: int64(len(b)), ArtifactType: m.ArtifactType}
			duplicate := false
			for _, prior := range f.referrers[m.Subject.Digest.String()] {
				if prior.Digest == d.Digest {
					duplicate = true
				}
			}
			if !duplicate {
				f.referrers[m.Subject.Digest.String()] = append(f.referrers[m.Subject.Digest.String()], d)
			}
		}
		w.WriteHeader(201)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("last"))
	if strings.HasPrefix(key, "referrers/") {
		if !f.native {
			w.WriteHeader(404)
			return
		}
		f.nativePages++
		list := f.referrers[strings.TrimPrefix(key, "referrers/")]
		sort.Slice(list, func(i, j int) bool { return list[i].Digest < list[j].Digest })
		var page []v1.Descriptor
		if offset < len(list) {
			page = list[offset : offset+1]
		}
		if offset+1 < len(list) {
			w.Header().Set("Link", "<"+r.URL.Path+"?last="+strconv.Itoa(offset+1)+">; type=\"application/vnd.oci.image.index.v1+json\"; rel=\"next\"")
		}
		w.Header().Set("Content-Type", v1.MediaTypeImageIndex)
		json.NewEncoder(w).Encode(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: page})
		return
	}
	if key == "tags/list" {
		f.tagPages++
		var names []string
		for name := range f.tags {
			names = append(names, name)
		}
		sort.Strings(names)
		var page []string
		if offset < len(names) {
			page = names[offset : offset+1]
		}
		if offset+1 < len(names) {
			w.Header().Set("Link", "<"+r.URL.Path+"?last="+strconv.Itoa(offset+1)+">; rel=\"next\"")
		}
		json.NewEncoder(w).Encode(struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}{"team/layers", page})
		return
	}
	if object, ok := f.objects[key]; ok {
		w.Header().Set("Content-Type", object.Media)
		if r.Method != "HEAD" {
			w.Write(object.Data)
		}
		return
	}
	w.WriteHeader(404)
}
func TestRegistryLayerSetReferrersNativeAndFallback(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(map[bool]string{true: "native", false: "fallback"}[native], func(t *testing.T) {
			dir := testDir(t)
			store := Store{Dir: filepath.Join(dir, "source")}
			root, predicate := fixtureLayerSet(t, store, true, true)
			old := testSigner(t, "platform")
			current := testSigner(t, "platform")
			addSignature(t, store, root, predicate, old)
			addSignature(t, store, root, predicate, current)
			fixture := &referrerRegistryFixture{native: native, objects: map[string]registryObject{}, referrers: map[string][]v1.Descriptor{}, tags: map[string]bool{}}
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			reg, e := NewRegistry(strings.TrimPrefix(server.URL, "http://")+"/team/layers:v1", false)
			must(t, e)
			must(t, reg.Push(context.Background(), store, root))
			must(t, reg.Push(context.Background(), store, root))
			remote := Store{Dir: filepath.Join(dir, "remote")}
			got, e := reg.Pull(context.Background(), remote)
			must(t, e)
			if got.Digest != root.Digest {
				t.Fatal("layer-set digest changed")
			}
			_, ev, e := ConsumeLayerSet(context.Background(), remote, got, signerTrust(current))
			must(t, e)
			if ev.SBOMDigest != predicate.SBOMDigest {
				t.Fatal("registry SBOM binding changed")
			}
			_, _, e = ConsumeLayerSet(context.Background(), remote, got, signerTrust(old))
			must(t, e)
			if _, _, e = ConsumeLayerSet(context.Background(), remote, got); e == nil {
				t.Fatal("remote signed set accepted without credentials")
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if native {
				if fixture.nativePages < 3 || len(fixture.tags) != 1 {
					t.Fatalf("native discovery or publication incorrect: pages=%d tags=%d", fixture.nativePages, len(fixture.tags))
				}
			} else if fixture.tagPages < 4 || len(fixture.tags) != 4 {
				t.Fatalf("fallback signer-rotation tags not preserved: pages=%d tags=%d", fixture.tagPages, len(fixture.tags))
			}
		})
	}
}
func TestReferrerPaginationCycleFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "<"+r.URL.Path+">; rel=\"next\"")
		json.NewEncoder(w).Encode(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex})
	}))
	defer server.Close()
	reg, e := NewRegistry(strings.TrimPrefix(server.URL, "http://")+"/team/layers:v1", false)
	must(t, e)
	if _, e = reg.Referrers(context.Background(), v1.Descriptor{Digest: digest.FromString("subject")}); e == nil {
		t.Fatal("pagination loop accepted")
	}
}
func TestReferrerPaginationDoesNotLeakCredentials(t *testing.T) {
	var leaked string
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex})
	}))
	defer foreign.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "builder" || p != "secret" {
			t.Error("origin credentials missing")
		}
		w.Header().Set("Link", "<"+foreign.URL+"/page2>; rel=\"next\"")
		json.NewEncoder(w).Encode(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex})
	}))
	defer origin.Close()
	reg, e := NewRegistry(strings.TrimPrefix(origin.URL, "https://")+"/team/layers:v1", false)
	must(t, e)
	reg.Origin, e = url.Parse(origin.URL)
	must(t, e)
	reg.Client = origin.Client()
	reg.Username = "builder"
	reg.Password = "secret"
	_, e = reg.Referrers(context.Background(), v1.Descriptor{Digest: digest.FromString("subject")})
	must(t, e)
	if leaked != "" {
		t.Fatal("registry credentials reached foreign pagination origin")
	}
}
