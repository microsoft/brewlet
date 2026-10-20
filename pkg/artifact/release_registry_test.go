package artifact

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRegistryCompositionAndReplacementNativeAndFallback(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(map[bool]string{true: "native", false: "fallback"}[native], func(t *testing.T) {
			f := newReleaseFixture(t)
			ctx := context.Background()
			target, original, err := Compose(ctx, f.store, f.options())
			must(t, err)
			fixture := &referrerRegistryFixture{native: native, objects: map[string]registryObject{}, referrers: map[string][]v1.Descriptor{}, tags: map[string]bool{}}
			server := httptest.NewServer(http.HandlerFunc(fixture.serve))
			defer server.Close()
			registry, err := NewRegistry(strings.TrimPrefix(server.URL, "http://")+"/team/layers:application", false)
			must(t, err)
			must(t, registry.Push(ctx, f.store, target))
			download := Store{Dir: filepath.Join(testDir(t), "download")}
			pulled, err := registry.Pull(ctx, download)
			must(t, err)
			inspection, err := VerifyComposition(ctx, download, pulled, f.platform, f.policy)
			must(t, err)
			if !reflect.DeepEqual(original.Predicate, inspection.Predicate) {
				t.Fatal("registry transport changed composition bindings")
			}
			patch := releaseSet(t, f.store, "2", "patched", f.publisher, true)
			must(t, CopyGraph(ctx, f.store, download, patch))
			options := f.options()
			options.Application, options.DependencySet = pulled, patch
			updated, _, err := Compose(ctx, download, options)
			must(t, err)
			registry.Reference = "patched"
			must(t, registry.Push(ctx, download, updated))
			fresh := Store{Dir: filepath.Join(testDir(t), "fresh")}
			selected, err := registry.Pull(ctx, fresh)
			must(t, err)
			facts, err := InspectDependencies(ctx, fresh, selected, f.platform, f.policy)
			must(t, err)
			if facts.Verification.Predicate.ParentComposition == "" || facts.Packages[0].Version != "2" || len(facts.SBOM) == 0 {
				t.Fatal("replacement history/SBOM did not survive independent registry pull")
			}
		})
	}
}

func TestRegistryRequiredReferrerFailureDoesNotPublishOutputTag(t *testing.T) {
	f := newReleaseFixture(t)
	target, _, err := Compose(context.Background(), f.store, f.options())
	must(t, err)
	fixture := &referrerRegistryFixture{native: true, objects: map[string]registryObject{}, referrers: map[string][]v1.Descriptor{}, tags: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/manifests/") {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			var manifest v1.Manifest
			if json.Unmarshal(data, &manifest) == nil && manifest.ArtifactType == MediaTypeComposition {
				w.WriteHeader(503)
				return
			}
		}
		fixture.serve(w, r)
	}))
	defer server.Close()
	registry, err := NewRegistry(strings.TrimPrefix(server.URL, "http://")+"/team/layers:accepted", false)
	must(t, err)
	if err = registry.Push(context.Background(), f.store, target); err == nil {
		t.Fatal("failed evidence publication reported success")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.tags["accepted"] {
		t.Fatal("accepted output tag published before required evidence")
	}
}
