// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifact

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func runnableLayersFixture(t *testing.T) (Store, Manifest, string, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	jar := filepath.Join(dir, "orders.jar")
	cds := filepath.Join(dir, "orders.jsa")
	cp := filepath.Join(dir, "deps.tar")
	mp := filepath.Join(dir, "mods.tar")
	jarBytes := bytes.Repeat([]byte("application"), 4096)
	cdsBytes := bytes.Repeat([]byte("archive"), 4096)
	if err := os.WriteFile(jar, jarBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cds, cdsBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	writeTar(t, cp, map[string]string{"dep.jar": "dependency"})
	writeTar(t, mp, map[string]string{"module.jar": "module"})
	cpBytes, err := os.ReadFile(cp)
	if err != nil {
		t.Fatal(err)
	}
	mpBytes, err := os.ReadFile(mp)
	if err != nil {
		t.Fatal(err)
	}
	cfg := JVMConfig{
		SchemaVersion: 1, MainJar: "orders.jar",
		Entry: Entry{Mode: "module", Module: "orders", ModulePath: []string{"orders.jar", "mods"}, ClassPath: []string{"lib/*"}},
		CDS:   &CDS{Archive: "orders.jsa"},
	}
	store := Store{Root: filepath.Join(dir, "oci")}
	if _, err := store.PushRunnableImage("orders:test", cfg, jar, []string{cp}, []string{mp}, cds); err != nil {
		t.Fatal(err)
	}
	man, digest, err := store.ResolveManifestByRef("orders:test")
	if err != nil {
		t.Fatal(err)
	}
	return store, man, digest, map[string][]byte{
		"app/orders.jar": jarBytes,
		"app/orders.jsa": cdsBytes,
		"cp-0.tar":       cpBytes,
		"mp-0.tar":       mpBytes,
	}
}

func checkStagedContents(t *testing.T, digest string, expected map[string][]byte) {
	t.Helper()
	root, err := runnableStageDir(digest)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range expected {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("staged %s: read error %v, content matches=%t", name, err, bytes.Equal(got, want))
		}
	}
}

func TestRunnableStageReusesOpenFilesAndLeavesLegacyUntouched(t *testing.T) {
	stage := t.TempDir()
	t.Setenv("BREWLET_RUNNABLE_STAGE", stage)
	store, man, digest, expected := runnableLayersFixture(t)
	legacy := filepath.Join(stage, strings.TrimPrefix(digest, "sha256:"), "app", "orders.jar")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy live container"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := ResolveRunnableBlobs(store, man, digest)
	if err != nil {
		t.Fatal(err)
	}
	root, err := runnableStageDir(digest)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{legacy}
	for name := range expected {
		paths = append(paths, filepath.Join(root, name))
	}
	type retainedFile struct {
		file *os.File
		info os.FileInfo
		raw  []byte
	}
	var retained []retainedFile
	for _, path := range paths {
		stamp := time.Unix(946684800, 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		info, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		retained = append(retained, retainedFile{f, info, raw})
	}
	for range 3 {
		got, err := ResolveRunnableBlobs(store, man, digest)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("resolution paths changed: %+v != %+v", got, first)
		}
	}
	for _, old := range retained {
		current, err := os.Stat(old.file.Name())
		if err != nil {
			t.Fatal(err)
		}
		openInfo, err := old.file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(old.info, current) || !old.info.ModTime().Equal(openInfo.ModTime()) {
			t.Errorf("live file replaced or rewritten: %s", old.file.Name())
		}
		if _, err := old.file.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(old.file)
		if err != nil || !bytes.Equal(raw, old.raw) {
			t.Errorf("open reader changed: %s (err=%v)", old.file.Name(), err)
		}
	}
	checkStagedContents(t, digest, expected)
}

type gatedStageSource struct {
	BlobSource
	digest  string
	entered chan<- struct{}
	release <-chan struct{}
}

func (s gatedStageSource) ReadBlob(digest string) ([]byte, error) {
	if digest == s.digest {
		s.entered <- struct{}{}
		<-s.release
	}
	return s.BlobSource.ReadBlob(digest)
}

func TestRunnableStageConcurrentPublication(t *testing.T) {
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
	store, man, digest, expected := runnableLayersFixture(t)
	app, err := man.RunnableAppLayer()
	if err != nil {
		t.Fatal(err)
	}
	const readers = 12
	entered := make(chan struct{}, 2*readers)
	release := make(chan struct{})
	source := gatedStageSource{store, app.Digest, entered, release}
	type result struct {
		blobs ResolvedBlobs
		err   error
	}
	results := make(chan result, readers)
	for range readers {
		go func() {
			blobs, err := ResolveRunnableBlobs(source, man, digest)
			results <- result{blobs, err}
		}()
	}
	for range readers {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatal("concurrent staging did not reach the publication barrier")
		}
	}
	root, err := runnableStageDir(digest)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	_, statErr := os.Lstat(root)
	close(release)
	if !os.IsNotExist(statErr) {
		t.Fatalf("incomplete stage became visible: %v", statErr)
	}
	for range readers {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.blobs.JarHostPath != filepath.Join(root, "app", "orders.jar") ||
			len(got.blobs.ClasspathHostPaths) != 1 || len(got.blobs.ModulepathHostPaths) != 1 ||
			got.blobs.CDSHostPath == "" {
			t.Fatalf("incomplete resolution: %+v", got.blobs)
		}
		checkStagedContents(t, digest, expected)
	}
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil || len(entries) != 1 {
		t.Fatalf("losing private stages not cleaned: entries=%v err=%v", entries, err)
	}
}

func TestRunnableStageHelperProcess(t *testing.T) {
	root := os.Getenv("BREWLET_STAGE_TEST_STORE")
	if root == "" {
		return
	}
	if _, err := (Store{Root: root}).ResolveBlobs("orders:test"); err != nil {
		t.Fatal(err)
	}
}

func TestRunnableStageIndependentProcesses(t *testing.T) {
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
	store, _, digest, expected := runnableLayersFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var commands []*exec.Cmd
	var outputs []*bytes.Buffer
	for range 6 {
		command := exec.Command(executable, "-test.run=^TestRunnableStageHelperProcess$")
		command.Env = append(os.Environ(), "BREWLET_STAGE_TEST_STORE="+store.Root)
		output := new(bytes.Buffer)
		command.Stdout, command.Stderr = output, output
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Process.Kill() })
		commands = append(commands, command)
		outputs = append(outputs, output)
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("resolver process: %v\n%s", err, outputs[i])
		}
		checkStagedContents(t, digest, expected)
	}
}

func TestRunnableStageFailureDoesNotPublish(t *testing.T) {
	for _, failure := range []string{"app", "classpath", "modulepath", "missing jar", "missing cds"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
			store, man, digest, expected := runnableLayersFixture(t)
			broken := man
			broken.Layers = append([]Descriptor(nil), man.Layers...)
			switch failure {
			case "missing jar", "missing cds":
				cfg, err := man.RunnableConfig()
				if err != nil {
					t.Fatal(err)
				}
				if failure == "missing jar" {
					cfg.MainJar = "missing.jar"
					cfg.Entry = Entry{Mode: "jar"}
				} else {
					cfg.CDS.Archive = "missing.jsa"
				}
				raw, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				broken.Annotations = map[string]string{JVMConfigAnnotation: string(raw)}
			default:
				for i, layer := range broken.Layers {
					if layer.Annotations[LayerRoleAnnotation] != failure {
						continue
					}
					raw, err := store.ReadBlob(layer.Digest)
					if err != nil {
						t.Fatal(err)
					}
					// Retain the layer bytes but remove its gzip trailer, so
					// failure occurs after files have begun to be staged.
					desc, err := store.writeBlob(raw[:len(raw)-8])
					if err != nil {
						t.Fatal(err)
					}
					desc.MediaType, desc.Annotations = layer.MediaType, layer.Annotations
					broken.Layers[i] = desc
				}
			}
			if _, err := ResolveRunnableBlobs(store, broken, digest); err == nil {
				t.Fatal("broken staging succeeded")
			}
			root, err := runnableStageDir(digest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				t.Fatalf("failed staging published a tree: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(root))
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed staging leaked files: %v, %v", entries, err)
			}
			if _, err := ResolveRunnableBlobs(store, man, digest); err != nil {
				t.Fatalf("retry after staging failure: %v", err)
			}
			checkStagedContents(t, digest, expected)
		})
	}
}

func TestRunnableStageCacheHitStillVerifiesDescriptors(t *testing.T) {
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
	store, man, digest, _ := runnableLayersFixture(t)
	if _, err := ResolveRunnableBlobs(store, man, digest); err != nil {
		t.Fatal(err)
	}
	layer := man.RunnableClasspathLayers()[0]
	path, err := store.BlobPath(layer.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRunnableBlobs(store, man, digest); err == nil {
		t.Fatal("cached stage bypassed descriptor verification")
	}
}

func TestRunnableStageRejectsIncompletePublishedTree(t *testing.T) {
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
	store, man, digest, _ := runnableLayersFixture(t)
	root, err := runnableStageDir(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "app", "orders.jar")
	if err := os.WriteFile(path, []byte("do not rewrite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveRunnableBlobs(store, man, digest); err == nil {
		t.Fatal("incomplete published tree accepted")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "do not rewrite" {
		t.Fatalf("incomplete published file was modified: %q, %v", raw, err)
	}
}
