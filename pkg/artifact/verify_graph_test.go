package artifact

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestVerifyGraphDoesNotApplyBrewletPolicy(t *testing.T) {
	store := Store{Dir: filepath.Join(testDir(t), "store")}
	app := source(t, store, "gzip", []*tar.Header{fileHeader("app/file", "payload", 0644)}, []string{"payload"})
	data := `{"schemaVersion":0,"process":{"entrypoint":["runner"]}}`
	launch := source(t, store, "gzip", []*tar.Header{fileHeader("launch.json", data, 0644)}, []string{data})
	launch.Descriptor.Annotations["brewlet.sh/layer"] = "launch"
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	image := v1.Image{Platform: platform, Config: v1.ImageConfig{Entrypoint: []string{"runner"}, WorkingDir: "/app"}, RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{app.DiffID, launch.DiffID}}}
	config, e := store.PutJSON(v1.MediaTypeImageConfig, image)
	must(t, e)
	manifest := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{app.Descriptor, launch.Descriptor}, Annotations: map[string]string{"brewlet.sh/artifact-version": "1"}}
	target, e := store.PutJSON(v1.MediaTypeImageManifest, manifest)
	must(t, e)
	must(t, VerifyGraph(context.Background(), store, target))
	if _, e = Resolve(context.Background(), store.Open, target, platform); Reason(e) != "InvalidLaunchConfig" {
		t.Fatalf("fixture does not exercise launch rejection: %v", e)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if e = VerifyGraph(cancelled, store, target); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancelled graph verification continued: %v", e)
	}
	path, e := store.blobPath(app.Descriptor.Digest)
	must(t, e)
	must(t, os.WriteFile(path, []byte("different compressed bytes"), 0600))
	if e = VerifyGraph(context.Background(), store, target); Reason(e) != "DigestMismatch" {
		t.Fatalf("corrupt payload accepted: %v", e)
	}
}

func TestVerifyGraphIncludesReferrers(t *testing.T) {
	store := Store{Dir: filepath.Join(testDir(t), "store")}
	target, _ := fixtureLayerSet(t, store, true, true)
	must(t, VerifyGraph(context.Background(), store, target))
	refs, e := store.Referrers(target)
	must(t, e)
	b, e := store.Read(refs[0])
	must(t, e)
	var manifest v1.Manifest
	must(t, json.Unmarshal(b, &manifest))
	path, e := store.blobPath(manifest.Layers[0].Digest)
	must(t, e)
	must(t, os.WriteFile(path, []byte("corrupt SBOM document"), 0600))
	if e = VerifyGraph(context.Background(), store, target); Reason(e) != "DigestMismatch" {
		t.Fatalf("unverified referrer document accepted: %v", e)
	}
}
