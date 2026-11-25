package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func fixtureApplication(t *testing.T, store Store) v1.Descriptor {
	t.Helper()
	app := filepath.Join(testDir(t), "app")
	must(t, os.Mkdir(app, 0755))
	must(t, os.WriteFile(filepath.Join(app, "main"), []byte("application"), 0644))
	cleanSource(t, app)
	root, err := Build(context.Background(), store, BuildOptions{
		Launch:    []byte(`{"schemaVersion":1,"process":{"command":["runner"]}}`),
		Platforms: []v1.Platform{{OS: "linux", Architecture: "amd64"}},
		Layers:    []LayerSpec{{Role: "app", Source: app, Destination: "app"}},
	})
	must(t, err)
	return root
}

func TestReferrerLocalLayoutCopy(t *testing.T) {
	dir := testDir(t)
	source := Store{Dir: filepath.Join(dir, "source")}
	target := fixtureApplication(t, source)
	ref, err := publishReferrer(source, target, MediaTypeCycloneDX, MediaTypeCycloneDX, []byte(`{"bomFormat":"CycloneDX"}`))
	must(t, err)
	destination := Store{Dir: filepath.Join(dir, "destination")}
	must(t, CopyGraph(context.Background(), source, destination, target))
	refs, err := destination.Referrers(target)
	must(t, err)
	if len(refs) != 1 || !sameSubject(refs[0], ref) {
		t.Fatal("OCI referrer identity changed")
	}
	must(t, VerifyGraph(context.Background(), destination, target))
}
