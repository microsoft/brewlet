package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/artifact"
	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const runTestRef = "example.com/hello:1"

// buildRunTestImage writes a version 2 artifact layout directly, so tests do
// not depend on host source xattrs (macOS adds com.apple.provenance).
func buildRunTestImage(t *testing.T, launch string) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", ".test-run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	store := artifact.Store{Dir: filepath.Join(dir, "store")}
	l, err := artifact.ParseLaunch([]byte(launch))
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1700000000, 0)
	layer := func(role string, files map[string]string) (v1.Descriptor, digest.Digest) {
		var raw bytes.Buffer
		tw := tar.NewWriter(&raw)
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			h := &tar.Header{Name: n, Mode: 0644, Size: int64(len(files[n])), Typeflag: tar.TypeReg, ModTime: mtime}
			if strings.HasSuffix(n, "/") {
				h = &tar.Header{Name: n, Mode: 0755, Typeflag: tar.TypeDir, ModTime: mtime}
			}
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(files[n])); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		zw.Write(raw.Bytes())
		zw.Close()
		d, err := store.Put(v1.MediaTypeImageLayerGzip, &gz)
		if err != nil {
			t.Fatal(err)
		}
		d.Annotations = map[string]string{"brewlet.sh/layer": role}
		return d, digest.FromBytes(raw.Bytes())
	}
	app, appDiff := layer("app", map[string]string{"app/": "", "app/hello.txt": "hello from brewlet\n"})
	ln, lnDiff := layer("launch", map[string]string{"launch.json": launch})
	platform := v1.Platform{OS: "linux", Architecture: "amd64"}
	config, err := store.PutJSON(v1.MediaTypeImageConfig, v1.Image{Platform: platform, Config: l.ImageConfig(),
		RootFS: v1.RootFS{Type: "layers", DiffIDs: []digest.Digest{appDiff, lnDiff}}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.PutJSON(v1.MediaTypeImageManifest, v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageManifest, Config: config, Layers: []v1.Descriptor{app, ln},
		Annotations: map[string]string{"brewlet.sh/artifact-version": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	target.Platform = &platform
	if err = store.WriteIndex(target, runTestRef); err != nil {
		t.Fatal(err)
	}
	return store.Dir
}

func runDry(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runLocal(context.Background(), args, &out, &errOut)
	return strings.TrimSpace(out.String()), errOut.String(), err
}

func TestRunDryRunAssemblesContainer(t *testing.T) {
	store := buildRunTestImage(t, `{"schemaVersion":2,"process":{"entrypoint":["/bin/sh","-c"],"args":["cat /app/hello.txt"],"workingDir":"/app/bin","env":[{"name":"A","value":"1"},{"name":"B","value":"two words"}]},"runtimeImages":["shell-1"]}`)
	out, _, err := runDry(t, runTestRef, "--store", store, "--platform", "linux/amd64", "--engine", "podman",
		"--runtime", "shell-1=busybox:1.36", "--user", "1000:2000", "-e", "B=override", "--env", "C=3",
		"-p", "8080:80", "--tmpfs", "/app/cache", "--name", "hello", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	want := "podman run --rm -i --platform linux/amd64 --name hello --read-only --cap-drop ALL --security-opt no-new-privileges" +
		" --user 1000:2000 --mount 'type=bind,source=<app-dir>,target=/app,readonly' --tmpfs /app/cache --workdir /app/bin" +
		" --env A=1 --env B=override --env C=3 --publish 8080:80 --label brewlet.sh/runtime-image=shell-1" +
		" --entrypoint /bin/sh busybox:1.36 -c 'cat /app/hello.txt'"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

func TestRunDryRunDefaultsAndExtraArgs(t *testing.T) {
	store := buildRunTestImage(t, `{"schemaVersion":2,"process":{"entrypoint":["/bin/echo"],"args":["default"]}}`)
	t.Setenv("BREWLET_CONTAINER_ENGINE", "")
	out, stderr, err := runDry(t, "--store", store, "--platform", "linux/amd64", "--runtime", "any-id=alpine:3", "--dry-run", runTestRef, "--", "x", "--y")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "docker run ") || !strings.Contains(out, " --workdir /app ") ||
		!strings.HasSuffix(out, " --entrypoint /bin/echo alpine:3 x --y") || strings.Contains(out, "default") || strings.Contains(out, "--user") {
		t.Fatalf("unexpected command: %s", out)
	}
	if !strings.Contains(stderr, "warning: running as the runtime image's default user") {
		t.Fatalf("missing user warning: %q", stderr)
	}
}

func TestRunRuntimeSelection(t *testing.T) {
	allowList := buildRunTestImage(t, `{"schemaVersion":2,"process":{"entrypoint":["/bin/true"]},"runtimeImages":["shell-1"]}`)
	open := buildRunTestImage(t, `{"schemaVersion":2,"process":{"entrypoint":["/bin/true"]}}`)
	base := []string{runTestRef, "--platform", "linux/amd64", "--dry-run", "--user", "1:1"}
	for _, tc := range []struct {
		store string
		args  []string
		want  string
	}{
		{allowList, []string{"--runtime", "shell-1=busybox"}, ""},
		{allowList, []string{"--runtime", "busybox"}, ""},
		{allowList, []string{"--runtime", "shell-2=busybox"}, "RuntimeImageIncompatible"},
		{open, []string{"--runtime", "anything=busybox"}, ""},
		{open, nil, "--runtime required"},
		{open, []string{"--runtime", "Bad_ID=busybox"}, "InvalidRuntimeImageID"},
		{open, []string{"--runtime", "id="}, "empty image"},
		{open, []string{"--runtime", "id=busybox", "--from-cluster"}, "--from-cluster takes --runtime ID"},
		{open, []string{"--runtime", "busybox", "--context", "x"}, "require --from-cluster"},
		{open, []string{"--runtime", "busybox", "--engine", "nerdctl"}, "--engine must be docker or podman"},
		{open, []string{"--runtime", "busybox", "--user", "0:1"}, "RootNotSupported"},
		{open, []string{"--runtime", "busybox", "--user", "5"}, "needs a GID"},
		{open, []string{"--runtime", "busybox", "-e", "PATH=/x"}, "PATH is runtime-owned"},
		{open, []string{"--runtime", "busybox", "-e", "NOVALUE"}, "NAME=VALUE"},
		{open, []string{"--runtime", "busybox", "--tmpfs", "/app"}, "RuntimeMountNotAllowed"},
		{open, []string{"--runtime", "busybox", "--platform", "linux/s390x"}, "(linux/s390x)"},
	} {
		args := append(append(append([]string{}, base...), "--store", tc.store), tc.args...)
		_, _, err := runDry(t, args...)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestRunFromCluster(t *testing.T) {
	store := buildRunTestImage(t, `{"schemaVersion":2,"process":{"entrypoint":["/bin/true"]},"runtimeImages":["shell-1","shell-root"]}`)
	images := map[string]api.RuntimeImage{
		"shell-1": {Spec: api.RuntimeImageSpec{Source: api.Source{Image: "registry.example/busybox@sha256:" + strings.Repeat("a", 64)},
			User: api.User{UID: 65532, GID: 65533}, VolumeMountPaths: []string{"/var/data"}}},
		"shell-root": {Spec: api.RuntimeImageSpec{Source: api.Source{Image: "registry.example/busybox@sha256:" + strings.Repeat("b", 64)}}},
	}
	saved := lookupRuntimeImage
	t.Cleanup(func() { lookupRuntimeImage = saved })
	var gotKubeconfig, gotContext string
	lookupRuntimeImage = func(_ context.Context, kubeconfig, contextName, id string) (*api.RuntimeImage, error) {
		gotKubeconfig, gotContext = kubeconfig, contextName
		ri, ok := images[id]
		if !ok {
			return nil, errors.New("not found")
		}
		return &ri, nil
	}
	base := []string{runTestRef, "--store", store, "--platform", "linux/amd64", "--dry-run", "--from-cluster"}
	out, stderr, err := runDry(t, append(base, "--runtime", "shell-1", "--kubeconfig", "kc", "--context", "ctx", "--tmpfs", "/var/data/tmp")...)
	if err != nil {
		t.Fatal(err)
	}
	if gotKubeconfig != "kc" || gotContext != "ctx" || stderr != "" && strings.Contains(stderr, "warning") ||
		!strings.Contains(out, " --user 65532:65533 ") || !strings.Contains(out, " --tmpfs /var/data/tmp ") ||
		!strings.Contains(out, " registry.example/busybox@sha256:"+strings.Repeat("a", 64)) {
		t.Fatalf("unexpected: %s (%s)", out, stderr)
	}
	if out, _, err = runDry(t, append(base, "--runtime", "shell-1", "--user", "4242")...); err != nil || !strings.Contains(out, " --user 4242:65533 ") {
		t.Fatalf("UID override: %v %s", err, out)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--runtime", "shell-root"}, "RootNotSupported"},
		{[]string{"--runtime", "missing"}, "RuntimeImageIncompatible"},
		{[]string{"--runtime", "shell-1", "--tmpfs", "/tmp"}, "RuntimeMountNotAllowed"},
	} {
		if _, _, err := runDry(t, append(base, tc.args...)...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %s", tc.args, err, tc.want)
		}
	}
}

func TestRunLocalLayoutResolution(t *testing.T) {
	_, _, err := runDry(t, "example.invalid/never-built:0", "--runtime", "busybox", "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "no local build for example.invalid/never-built:0") || !strings.Contains(err.Error(), "brewlet build") {
		t.Fatalf("got %v", err)
	}
	store := buildRunTestImage(t, `{"schemaVersion":2,"process":{"entrypoint":["/bin/true"]}}`)
	if _, _, err = runDry(t, "other.example/name:2", "--store", store, "--platform", "linux/amd64", "--runtime", "busybox", "--user", "1:1", "--dry-run"); err != nil {
		t.Fatalf("unique root fallback: %v", err)
	}
	if _, _, err = runDry(t, "--store", store, "--runtime", "busybox", "--dry-run"); err == nil || !strings.Contains(err.Error(), "explicit reference required") {
		t.Fatalf("missing ref: %v", err)
	}
}

func TestRunHelp(t *testing.T) {
	out, _, err := runDry(t, "--help")
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(out, "Usage: brewlet run REF") || !strings.Contains(out, "-runtime") {
		t.Fatalf("got %v: %s", err, out)
	}
}

func TestShellJoin(t *testing.T) {
	if got := shellJoin([]string{"a", "b c", "it's", ""}); got != `a 'b c' 'it'\''s' ''` {
		t.Fatal(got)
	}
}
