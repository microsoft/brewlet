// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/brewlet/internal/artifact"
)

// Verify real HotSpot consumption, including relocation and mtime validation,
// using an archive built externally with the JDK rather than a Brewlet API.
func TestPrebuiltAppCDSConsumptionIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a full JDK")
	}
	tools := map[string]string{}
	for _, tool := range []string{"java", "javac", "jar"} {
		if home := os.Getenv("JAVA_HOME"); home != "" {
			candidate := filepath.Join(home, "bin", tool)
			if _, err := os.Stat(candidate); err == nil {
				tools[tool] = candidate
			}
		}
		if tools[tool] == "" {
			path, err := exec.LookPath(tool)
			if err != nil {
				t.Skipf("requires %s from a full JDK: %v", tool, err)
			}
			tools[tool] = path
		}
	}
	version, err := exec.Command(tools["java"], "-version").CombinedOutput()
	if err != nil {
		t.Skipf("JDK unavailable: %v\n%s", err, version)
	}
	match := regexp.MustCompile(`version "(\d+)`).FindStringSubmatch(string(version))
	if len(match) != 2 {
		t.Skipf("cannot identify JDK version: %s", version)
	}
	feature, _ := strconv.Atoi(match[1])
	if feature < 17 {
		t.Skip("requires JDK 17 or newer")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runTool := func(tool, dir string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, tools[tool], args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %q: %v\n%s", tool, args, err, out)
		}
		return out
	}
	build := t.TempDir()
	source := `public class Main {
  public static void main(String[] args) {
    System.out.println("PREBUILT_CDS_OK");
  }
}`
	if err := os.WriteFile(filepath.Join(build, "Main.java"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	runTool("javac", build, "Main.java")
	runTool("jar", build, "--create", "--file", "app.jar", "--main-class", "Main", "Main.class")
	jar := filepath.Join(build, "app.jar")
	if err := os.Chtimes(jar, CDSModTime, CDSModTime); err != nil {
		t.Fatal(err)
	}
	runTool("java", build, "-XX:ArchiveClassesAtExit=app.jsa", "-jar", "app.jar")
	archive := filepath.Join(build, "app.jsa")
	if info, err := os.Stat(archive); err != nil || info.Size() == 0 {
		t.Fatalf("expected nonempty externally built archive: %v", err)
	}
	cfg := artifact.JVMConfig{
		SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"},
		CDS: &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"},
	}
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
	for _, format := range []string{"artifact", "image"} {
		t.Run(format, func(t *testing.T) {
			store := artifact.Store{Root: filepath.Join(t.TempDir(), "oci")}
			const ref = "test/prebuilt:1"
			var err error
			if format == "artifact" {
				_, err = store.PushWithCDS(ref, cfg, jar, nil, nil, archive)
			} else {
				_, err = store.PushRunnableImage(ref, cfg, jar, nil, nil, archive)
			}
			if err != nil {
				t.Fatal(err)
			}
			blobs, err := store.ResolveBlobs(ref)
			if err != nil {
				t.Fatal(err)
			}
			sandbox, stagedJar, err := AssembleSandboxWithCDS(blobs.Config, blobs.JarHostPath, nil, nil, blobs.CDSHostPath)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(sandbox)
			args, err := BuildJVMArgs(blobs.Config, stagedJar, []string{"-Xshare:on", "-Xlog:cds=info,class+load=info"})
			if err != nil {
				t.Fatal(err)
			}
			out := string(runTool("java", filepath.Dir(stagedJar), args...))
			if !strings.Contains(out, "PREBUILT_CDS_OK") || !strings.Contains(out, "Main source: shared objects file (top)") {
				t.Fatalf("application class did not map from the prebuilt archive:\n%s", out)
			}

			drift := CDSModTime.Add(24 * time.Hour)
			if err := os.Chtimes(stagedJar, drift, drift); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, tools["java"], args...)
			cmd.Dir = filepath.Dir(stagedJar)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("-Xshare:on accepted a drifted JAR mtime:\n%s", out)
			}
		})
	}
}
