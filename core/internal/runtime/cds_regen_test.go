// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/brewlet/internal/artifact"
)

// fakeJDK writes a minimal JDK root with a `release` file advertising the given
// JAVA_VERSION, so readJDKIdentity can resolve the feature version without a real
// JVM.
func fakeJDK(t *testing.T, javaVersion string) string {
	t.Helper()
	root := t.TempDir()
	rel := "IMPLEMENTOR=\"Eclipse Adoptium\"\n" +
		"JAVA_VERSION=\"" + javaVersion + "\"\n" +
		"JAVA_RUNTIME_VERSION=\"" + javaVersion + "+11-LTS\"\n" +
		"OS_ARCH=\"aarch64\"\n"
	if err := os.WriteFile(filepath.Join(root, "release"), []byte(rel), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBuildJVMArgsSuppressesShippedArgsWhenRegenerate(t *testing.T) {
	cfg := artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar"}, CDS: &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"}}
	args, err := BuildJVMArgs(cfg, "/app/app.jar", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(args, " "); strings.Contains(got, "SharedArchiveFile") || strings.Contains(got, "AutoCreate") {
		t.Errorf("args = %q, want no CDS archive flags in regen mode (node injects them)", got)
	}
}

func TestParseFeature(t *testing.T) {
	cases := map[string]int{
		"21.0.5":    21,
		"17":        17,
		"1.8.0_412": 8,
		"25.0.3":    25,
		"":          0,
		"garbage":   0,
	}
	for in, want := range cases {
		if got := parseFeature(in); got != want {
			t.Errorf("parseFeature(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseFeatureFromInternal(t *testing.T) {
	s := "OpenJDK 64-Bit Server VM (21.0.5+11-LTS) for linux-aarch64 JRE (21.0.5+11-LTS)"
	if got := parseFeatureFromInternal(s); got != 21 {
		t.Errorf("parseFeatureFromInternal = %d, want 21", got)
	}
}

func TestReadJDKIdentityFromRelease(t *testing.T) {
	root := fakeJDK(t, "21.0.5")
	feature, buildID, err := readJDKIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if feature != 21 {
		t.Errorf("feature = %d, want 21", feature)
	}
	if !strings.Contains(buildID, "21.0.5+11-LTS") || !strings.Contains(buildID, "Eclipse Adoptium") {
		t.Errorf("buildID = %q, want it to include runtime version + implementor", buildID)
	}
}

func TestDecideCDSRegenGatesOldJDK(t *testing.T) {
	dec, err := DecideCDSRegen(RegenParams{
		CacheDir:       t.TempDir(),
		CacheScope:     "local",
		JDKRoot:        fakeJDK(t, "17.0.10"),
		ArtifactDigest: "sha256:abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Role != RegenSkip {
		t.Errorf("role = %q, want skip on JDK 17 (no AutoCreateSharedArchive)", dec.Role)
	}
	if len(dec.Args) != 0 {
		t.Errorf("args = %v, want none when skipped", dec.Args)
	}
}

func TestDecideCDSRegenElectsWriterThenConsumes(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	params := RegenParams{
		CacheDir:       cache,
		CacheScope:     "team-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:abc",
		ArchiveArgDir:  InSandboxCDSDir,
	}

	// First launch: empty cache -> elected writer with AutoCreateSharedArchive.
	dec, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Role != RegenWrite {
		t.Fatalf("role = %q, want write on empty cache", dec.Role)
	}
	if !dec.MountRW {
		t.Error("writer must mount the cache read-write")
	}
	joined := strings.Join(dec.Args, " ")
	if !strings.Contains(joined, "-XX:+AutoCreateSharedArchive") {
		t.Errorf("writer args = %q, want -XX:+AutoCreateSharedArchive", joined)
	}
	wantPath := InSandboxCDSDir + "/" + cacheArchiveName
	if !strings.Contains(joined, "-XX:SharedArchiveFile="+wantPath) {
		t.Errorf("writer args = %q, want -XX:SharedArchiveFile=%s", joined, wantPath)
	}
	if dec.HostMount == cache || filepath.Dir(dec.HostMount) != cache {
		t.Fatalf("host mount = %q, want a private child of %q", dec.HostMount, cache)
	}
	if dec.HostArchive != filepath.Join(dec.HostMount, cacheArchiveName) {
		t.Fatalf("host archive = %q, want archive inside private mount %q", dec.HostArchive, dec.HostMount)
	}
	if info, err := os.Stat(dec.HostMount); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o700 {
		t.Fatalf("private cache entry mode = %o, want 700", info.Mode().Perm())
	}

	// Simulate the writer having produced the archive at exit.
	if err := os.WriteFile(dec.HostArchive, []byte("jsa-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Next launch: valid archive present -> consume read-only, no AutoCreate.
	dec2, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if dec2.Role != RegenConsume {
		t.Fatalf("role = %q, want consume when a cached archive exists", dec2.Role)
	}
	if dec2.MountRW {
		t.Error("consumer must mount the cache read-only")
	}
	joined2 := strings.Join(dec2.Args, " ")
	if strings.Contains(joined2, "AutoCreate") {
		t.Errorf("consumer args = %q, must not recreate an existing archive", joined2)
	}
	if !strings.Contains(joined2, "-Xshare:auto") || !strings.Contains(joined2, "-XX:SharedArchiveFile="+wantPath) {
		t.Errorf("consumer args = %q, want -Xshare:auto -XX:SharedArchiveFile=%s", joined2, wantPath)
	}
	marker := filepath.Join(cache, dec.Key+writerMarkerSuffix)
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("consumer removed the active writer marker: %v", err)
	}
	dec.WriterLease.Release()
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("writer lease release left marker behind: %v", err)
	}
}

func TestDecideCDSRegenDefersSecondWriter(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	params := RegenParams{CacheDir: cache, CacheScope: "team-a", JDKRoot: jdk, ArtifactDigest: "sha256:abc"}

	first, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != RegenWrite {
		t.Fatalf("first role = %q, want write", first.Role)
	}
	// Archive not yet written; a concurrent launch must defer to base CDS.
	second, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if second.Role != RegenDefer {
		t.Errorf("second role = %q, want defer while a writer is in flight", second.Role)
	}
	if len(second.Args) != 0 {
		t.Errorf("defer args = %v, want none (base CDS)", second.Args)
	}
}

func TestWriterHeartbeatProtectsLongRunningEntry(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	lockTTL := 200 * time.Millisecond
	first, err := DecideCDSRegen(RegenParams{
		CacheDir:       cache,
		CacheScope:     "team-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:first",
		LockTTL:        lockTTL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != RegenWrite || first.WriterLease == nil {
		t.Fatalf("first decision = %#v, want writer lease", first)
	}
	stopHeartbeat := first.WriterLease.StartHeartbeat(lockTTL)
	defer stopHeartbeat()

	// Let the original marker age past its TTL. The heartbeat must keep it fresh
	// while a different key runs cache maintenance.
	marker := filepath.Join(cache, first.Key+writerMarkerSuffix)
	started := time.Now()
	deadline := started.Add(3 * time.Second)
	for {
		info, statErr := os.Lstat(marker)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if time.Since(started) > 2*lockTTL && time.Since(info.ModTime()) < lockTTL/2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("writer heartbeat did not refresh the marker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	second, err := DecideCDSRegen(RegenParams{
		CacheDir:       cache,
		CacheScope:     "team-b",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:second",
		LockTTL:        lockTTL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.WriterLease != nil {
		defer second.WriterLease.Release()
	}
	if _, err := os.Stat(first.HostMount); err != nil {
		t.Fatalf("cache maintenance removed the active writer entry: %v", err)
	}

	sameKey, err := DecideCDSRegen(RegenParams{
		CacheDir:       cache,
		CacheScope:     "team-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:first",
		LockTTL:        lockTTL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sameKey.Role != RegenDefer {
		t.Fatalf("same-key role = %q, want defer while writer heartbeat is active", sameKey.Role)
	}
}

func TestDecideCDSRegenStaleMarkerReelects(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	base := RegenParams{
		CacheDir:       cache,
		CacheScope:     "team-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:abc",
		LockTTL:        time.Minute,
	}

	first, err := DecideCDSRegen(base)
	if err != nil {
		t.Fatal(err)
	}
	if first.Role != RegenWrite {
		t.Fatalf("first role = %q, want write", first.Role)
	}
	// Age the writer marker past the TTL -> the next launch reclaims the key.
	marker := filepath.Join(cache, first.Key+writerMarkerSuffix)
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(marker, old, old); err != nil {
		t.Fatal(err)
	}
	reelect, err := DecideCDSRegen(base)
	if err != nil {
		t.Fatal(err)
	}
	if reelect.Role != RegenWrite {
		t.Errorf("role = %q, want write after reclaiming a stale marker", reelect.Role)
	}
	first.WriterLease.Release()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("old writer lease removed the replacement marker: %v", err)
	}
	reelect.WriterLease.Release()
}

func TestDecideCDSRegenSeeds(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	seed := filepath.Join(t.TempDir(), "app.jsa")
	if err := os.WriteFile(seed, []byte("seed-archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	dec, err := DecideCDSRegen(RegenParams{
		CacheDir:       cache,
		CacheScope:     "team-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:abc",
		SeedArchive:    seed,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Role != RegenWrite {
		t.Fatalf("role = %q, want write", dec.Role)
	}
	got, err := os.ReadFile(dec.HostArchive)
	if err != nil {
		t.Fatalf("seed not copied into cache: %v", err)
	}
	if string(got) != "seed-archive" {
		t.Errorf("cache archive = %q, want the seed bytes", got)
	}
}

func TestDecideCDSRegenEvictsStale(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	staleKey := strings.Repeat("a", 64)
	staleDir := filepath.Join(cache, staleKey)
	if err := os.Mkdir(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(staleDir, cacheArchiveName)
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(cache, strings.Repeat("b", 32)+".jsa")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := DecideCDSRegen(RegenParams{
		CacheDir:       cache,
		CacheScope:     "team-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:abc",
		EvictTTL:       14 * 24 * time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staleDir); !os.IsNotExist(err) {
		t.Errorf("stale entry should have been evicted, stat err = %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy flat archive should have been removed, stat err = %v", err)
	}
}

func TestDecideCDSRegenWritesMetric(t *testing.T) {
	cache := t.TempDir()
	metrics := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	if _, err := DecideCDSRegen(RegenParams{
		CacheDir:       cache,
		CacheScope:     "team-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:abc",
		MetricsDir:     metrics,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(metrics)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("expected a metric file to be written")
	}
	b, err := os.ReadFile(filepath.Join(metrics, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "brewlet_cds_archive_mapped") {
		t.Errorf("metric file = %q, want brewlet_cds_archive_mapped", b)
	}
}

func TestRegenKeyChangesWithBuild(t *testing.T) {
	a := regenKey("team-a", "sha256:abc", "temurin|21.0.5+11")
	b := regenKey("team-a", "sha256:abc", "temurin|21.0.6+7") // patch bump
	c := regenKey("team-a", "sha256:xyz", "temurin|21.0.5+11")
	d := regenKey("team-b", "sha256:abc", "temurin|21.0.5+11")
	if a == b {
		t.Error("key must change when the JDK build changes")
	}
	if a == c {
		t.Error("key must change when the artifact changes")
	}
	if a == d {
		t.Error("key must change when the cache scope changes")
	}
	if len(a) != 64 {
		t.Errorf("key length = %d, want 64", len(a))
	}
}

func TestGenerateBundleWithRegenMountsCache(t *testing.T) {
	dir := t.TempDir()
	jdkRoot := fakeJDK(t, "21.0.5")
	jarHost := filepath.Join(dir, "app.jar")
	if err := os.WriteFile(jarHost, []byte("jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	out := filepath.Join(dir, "bundle")
	cfg := artifact.JVMConfig{MainJar: "app.jar", Entry: artifact.Entry{Mode: "jar"}}
	if err := GenerateBundleWithRegen(cfg, jdkRoot, "", "", jarHost, nil, nil, "", out, Resources{}, nil,
		CDSRegenOptions{
			Regenerate:     true,
			CacheScope:     "local",
			ArtifactDigest: "sha256:abc",
			CacheDir:       cache,
		}); err != nil {
		t.Fatalf("GenerateBundleWithRegen: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfgJSON := string(b)
	if !strings.Contains(cfgJSON, "-XX:+AutoCreateSharedArchive") {
		t.Errorf("config.json missing regen argv:\n%s", cfgJSON)
	}
	if !strings.Contains(cfgJSON, InSandboxCDSDir) {
		t.Errorf("config.json missing cache mount at %s:\n%s", InSandboxCDSDir, cfgJSON)
	}
	// A regenerating artifact with no shipped archive must not mount /app/*.jsa.
	if strings.Contains(cfgJSON, "/app/app.jsa") {
		t.Errorf("config.json should not mount a /app archive in pure regen mode:\n%s", cfgJSON)
	}
	var spec ociSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	var cacheMount *ociMount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == InSandboxCDSDir {
			cacheMount = &spec.Mounts[i]
			break
		}
	}
	if cacheMount == nil {
		t.Fatal("config.json missing private AppCDS cache mount")
	}
	if cacheMount.Source == cache || filepath.Dir(cacheMount.Source) != cache {
		t.Fatalf("cache mount source = %q, want private child of %q", cacheMount.Source, cache)
	}
	for _, want := range []string{"rw", "nosuid", "nodev", "noexec"} {
		if !containsString(cacheMount.Options, want) {
			t.Errorf("cache mount options = %v, missing %q", cacheMount.Options, want)
		}
	}
}

func TestDecideCDSRegenSeparatesCacheScopes(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	base := RegenParams{
		CacheDir:       cache,
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:abc",
	}

	a := base
	a.CacheScope = "tenant-a"
	first, err := DecideCDSRegen(a)
	if err != nil {
		t.Fatal(err)
	}
	b := base
	b.CacheScope = "tenant-b"
	second, err := DecideCDSRegen(b)
	if err != nil {
		t.Fatal(err)
	}
	if first.Key == second.Key || first.HostMount == second.HostMount {
		t.Fatalf("different scopes shared a cache entry: first=%+v second=%+v", first, second)
	}
	if filepath.Dir(first.HostMount) != cache || filepath.Dir(second.HostMount) != cache {
		t.Fatalf("cache entries escaped root %q: %q %q", cache, first.HostMount, second.HostMount)
	}
}

func TestDecideCDSRegenRejectsUnsafeEntryTypes(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	params := RegenParams{
		CacheDir:       cache,
		CacheScope:     "tenant-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:abc",
	}
	first, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cache, first.Key+writerMarkerSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(first.HostArchive, 0o755); err != nil {
		t.Fatal(err)
	}

	next, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if next.Role != RegenWrite {
		t.Fatalf("unsafe archive role = %q, want a fresh writer", next.Role)
	}
	if _, err := os.Lstat(next.HostArchive); !os.IsNotExist(err) {
		t.Fatalf("unsafe archive was not removed before writer mount: %v", err)
	}
}

func TestDecideCDSRegenRejectsSymlinkedEntry(t *testing.T) {
	cache := t.TempDir()
	jdk := fakeJDK(t, "21.0.5")
	params := RegenParams{
		CacheDir:       cache,
		CacheScope:     "tenant-a",
		JDKRoot:        jdk,
		ArtifactDigest: "sha256:abc",
	}
	first, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cache, first.Key+writerMarkerSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(first.HostMount); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, cacheArchiveName), []byte("victim"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, first.HostMount); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	next, err := DecideCDSRegen(params)
	if err != nil {
		t.Fatal(err)
	}
	if next.Role != RegenWrite {
		t.Fatalf("symlinked entry role = %q, want a fresh writer", next.Role)
	}
	info, err := os.Lstat(next.HostMount)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("entry was not recreated as a real directory: mode=%v", info.Mode())
	}
	got, err := os.ReadFile(filepath.Join(target, cacheArchiveName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "victim" {
		t.Fatalf("reset followed the symlink and modified target: %q", got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
