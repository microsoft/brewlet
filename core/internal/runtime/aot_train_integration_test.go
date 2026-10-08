// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/brewlet/internal/artifact"
)

// jdkFeature returns the feature version of the given java binary (0 if unknown).
func jdkFeature(t *testing.T, javaBin string) int {
	t.Helper()
	out, err := exec.Command(javaBin, "-version").CombinedOutput()
	if err != nil {
		return 0
	}
	m := javaVersionRE.FindStringSubmatch(string(out))
	if m == nil {
		return 0
	}
	f := 0
	for _, c := range m[1] {
		f = f*10 + int(c-'0')
	}
	return f
}

// runWithAOTCache runs the app in runDir against the cache and returns combined
// output and exit code. The mode is "required" on JDK >= 27 and "on" before.
func runWithAOTCache(t *testing.T, javaBin string, feature int, runDir string) (string, int) {
	t.Helper()
	mode := "on"
	if feature >= 27 {
		mode = "required"
	}
	cmd := exec.Command(javaBin, "-XX:AOTMode="+mode, "-XX:AOTCache=app.aot", "-jar", "app.jar")
	cmd.Dir = runDir
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running java: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

func TestAOTCacheTrainThenMapIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping AOT cache JDK integration test in -short mode")
	}
	javaBin, javacBin, jarBin := locateJDK(t)
	feature := jdkFeature(t, javaBin)
	if feature < 25 {
		t.Skipf("AOT cache training needs JDK >= 25, found feature %d; skipping", feature)
	}

	jar := buildTinyFatJar(t, javacBin, jarBin)
	cfg := artifact.JVMConfig{SchemaVersion: 1, MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}

	cache := filepath.Join(t.TempDir(), "out", "app.aot")
	if err := GenerateAOTCache(cfg, jar, javaBin, cache, 120*time.Second, nil); err != nil {
		t.Fatalf("GenerateAOTCache: %v", err)
	}
	if fi, err := os.Stat(cache); err != nil || fi.Size() == 0 {
		t.Fatalf("expected non-empty cache at %s (err=%v)", cache, err)
	}

	t.Run("maps with canonical mtime", func(t *testing.T) {
		runDir := t.TempDir()
		if _, err := StageCDSJar(jar, runDir, "app.jar"); err != nil {
			t.Fatalf("StageCDSJar: %v", err)
		}
		if err := copyFileContents(cache, filepath.Join(runDir, "app.aot")); err != nil {
			t.Fatal(err)
		}
		out, code := runWithAOTCache(t, javaBin, feature, runDir)
		if code != 0 {
			t.Fatalf("expected AOT cache to be usable (exit 0), got exit %d\n%s", code, out)
		}
		if !strings.Contains(out, "APPCDS_TRAINED") {
			t.Fatalf("app did not run to completion under the AOT cache:\n%s", out)
		}
	})

	t.Run("refuses with drifted mtime", func(t *testing.T) {
		runDir := t.TempDir()
		if err := copyFileContents(jar, filepath.Join(runDir, "app.jar")); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := os.Chtimes(filepath.Join(runDir, "app.jar"), now, now); err != nil {
			t.Fatal(err)
		}
		if err := copyFileContents(cache, filepath.Join(runDir, "app.aot")); err != nil {
			t.Fatal(err)
		}
		out, code := runWithAOTCache(t, javaBin, feature, runDir)
		if code == 0 {
			t.Fatalf("expected a drifted-mtime JAR to be refused, but it ran:\n%s", out)
		}
	})
}
