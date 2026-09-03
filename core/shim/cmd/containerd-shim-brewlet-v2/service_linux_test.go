// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v2"
	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	runtimeoptions "github.com/containerd/containerd/pkg/runtimeoptions/v1"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/microsoft/brewlet/internal/artifact"
	kcruntime "github.com/microsoft/brewlet/internal/runtime"
)

type deleteTaskService struct {
	taskAPI.TaskService
	err error
}

type staticImageIdentityResolver struct {
	identity resolvedImageIdentity
	err      error
}

func (r staticImageIdentityResolver) Resolve(context.Context, string) (resolvedImageIdentity, error) {
	return r.identity, r.err
}

func (s *deleteTaskService) Delete(context.Context, *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	return &taskAPI.DeleteResponse{}, s.err
}

func TestDeleteCleansPendingLaunch(t *testing.T) {
	released := false
	service := &brewletTaskService{
		TaskService: &deleteTaskService{},
		pending:     map[string]launchInfo{"task-1": {entryMode: "jar"}},
		writers:     map[string]func(){"task-1": func() { released = true }},
	}

	if _, err := service.Delete(context.Background(), &taskAPI.DeleteRequest{ID: "task-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := service.pending["task-1"]; ok {
		t.Fatal("successful task deletion left stale pending launch state")
	}
	if !released {
		t.Fatal("successful task deletion did not release the AppCDS writer lease")
	}
	if _, ok := service.writers["task-1"]; ok {
		t.Fatal("successful task deletion left stale writer state")
	}
}

func TestDeleteKeepsPendingLaunchWhenDeleteFails(t *testing.T) {
	released := false
	service := &brewletTaskService{
		TaskService: &deleteTaskService{err: errors.New("delete failed")},
		pending:     map[string]launchInfo{"task-1": {entryMode: "jar"}},
		writers:     map[string]func(){"task-1": func() { released = true }},
	}

	if _, err := service.Delete(context.Background(), &taskAPI.DeleteRequest{ID: "task-1"}); err == nil {
		t.Fatal("Delete succeeded unexpectedly")
	}
	if _, ok := service.pending["task-1"]; !ok {
		t.Fatal("failed task deletion removed pending launch state")
	}
	if released {
		t.Fatal("failed task deletion released the active AppCDS writer lease")
	}
	if _, ok := service.writers["task-1"]; !ok {
		t.Fatal("failed task deletion removed active writer state")
	}
}

func testResolved() resolvedArtifact {
	return resolvedArtifact{
		Config: artifact.JVMConfig{
			MainJar: "app.jar",
			Entry:   artifact.Entry{Mode: "jar"},
			Env:     []artifact.EnvVar{{Name: "FOO", Value: "bar"}},
		},
		JarHostPath: "/var/lib/containerd/.../blobs/sha256/deadbeef",
		JDKRoot:     "/opt/brewlet/jdks/temurin-21",
		JDKHome:     "/opt/brewlet/jdks/temurin-21/opt/java/openjdk",
	}
}

func TestAssembleBrewletBundleUsesResolvedImageWithoutHints(t *testing.T) {
	contentRoot, _, targetDigest := buildRunnableStore(t)
	jdkRoots := t.TempDir()
	mkJDK(t, jdkRoots, "temurin-21")
	t.Setenv("BREWLET_CONTENT_ROOT", contentRoot)
	t.Setenv("BREWLET_JDK_ROOTS", jdkRoots)
	t.Setenv("BREWLET_LAUNCHER_ROOTS", t.TempDir())
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())

	bundle := t.TempDir()
	writeSpecJSON(t, bundle, specs.Spec{
		Annotations: map[string]string{
			annCRIImageName: "demo/orders@" + targetDigest,
			annRequestedJDK: "temurin-21",
		},
		Process: &specs.Process{},
	})
	request := &taskAPI.CreateTaskRequest{ID: "task-1", Bundle: bundle}
	info, err := assembleBrewletBundle(context.Background(), request, staticImageIdentityResolver{
		identity: resolvedImageIdentity{
			ImageName:    "demo/orders@" + targetDigest,
			TargetDigest: targetDigest,
		},
	})
	if err != nil {
		t.Fatalf("assembleBrewletBundle: %v", err)
	}
	if info.format != "image" || info.entryMode != "classpath" {
		t.Fatalf("launch info = %+v", info)
	}
	if len(request.Rootfs) != 1 || request.Rootfs[0].Type != "overlay" {
		t.Fatalf("rootfs = %+v, want Brewlet overlay", request.Rootfs)
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got specs.Spec
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if args := strings.Join(got.Process.Args, " "); !strings.Contains(args, "com.acme.Main") {
		t.Fatalf("process args do not come from the resolved runnable image: %q", args)
	}
}

func TestAssembleBrewletBundleRejectsConflictingHint(t *testing.T) {
	resolved := "sha256:" + strings.Repeat("a", 64)
	bundle := t.TempDir()
	writeSpecJSON(t, bundle, specs.Spec{
		Annotations: map[string]string{
			annArtifactDigest: "sha256:" + strings.Repeat("b", 64),
			annCRIImageName:   "demo/app@" + resolved,
		},
	})
	_, err := assembleBrewletBundle(context.Background(), &taskAPI.CreateTaskRequest{
		ID: "task-1", Bundle: bundle,
	}, staticImageIdentityResolver{
		identity: resolvedImageIdentity{ImageName: "demo/app:1", TargetDigest: resolved},
	})
	if err == nil || !strings.Contains(err.Error(), "artifact identity mismatch") {
		t.Fatalf("error = %v, want artifact identity mismatch", err)
	}
}

func TestAssembleBrewletBundleRejectsNativeArtifact(t *testing.T) {
	contentRoot := t.TempDir()
	jarPath := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jarPath, []byte("PK\x03\x04 app"), 0o644); err != nil {
		t.Fatal(err)
	}
	desc, err := (artifact.Store{Root: contentRoot}).Push("demo/app:1", artifact.JVMConfig{
		SchemaVersion: 1,
		MainJar:       "app.jar",
		Entry:         artifact.Entry{Mode: "jar"},
	}, jarPath)
	if err != nil {
		t.Fatal(err)
	}
	jdkRoots := t.TempDir()
	mkJDK(t, jdkRoots, "temurin-21")
	t.Setenv("BREWLET_CONTENT_ROOT", contentRoot)
	t.Setenv("BREWLET_JDK_ROOTS", jdkRoots)
	t.Setenv("BREWLET_LAUNCHER_ROOTS", t.TempDir())

	bundle := t.TempDir()
	writeSpecJSON(t, bundle, specs.Spec{
		Annotations: map[string]string{annCRIImageName: "demo/app@" + desc.Digest},
		Process:     &specs.Process{},
	})
	_, err = assembleBrewletBundle(context.Background(), &taskAPI.CreateTaskRequest{
		ID: "task-1", Bundle: bundle,
	}, staticImageIdentityResolver{
		identity: resolvedImageIdentity{ImageName: "demo/app:1", TargetDigest: desc.Digest},
	})
	if err == nil || !strings.Contains(err.Error(), "require a runnable OCI image") {
		t.Fatalf("error = %v, want native artifact rejection", err)
	}
}

func TestSetupOverlayRootfs(t *testing.T) {
	bundle := t.TempDir()
	r := &taskAPI.CreateTaskRequest{ID: "task-1", Bundle: bundle}

	if err := setupOverlayRootfs(r, testResolved()); err != nil {
		t.Fatalf("setupOverlayRootfs: %v", err)
	}
	if len(r.Rootfs) != 1 {
		t.Fatalf("want 1 rootfs mount, got %d", len(r.Rootfs))
	}
	m := r.Rootfs[0]
	if m.Type != "overlay" {
		t.Errorf("type = %q, want overlay", m.Type)
	}
	joined := strings.Join(m.Options, ",")
	if !strings.Contains(joined, "lowerdir=/opt/brewlet/jdks/temurin-21") {
		t.Errorf("missing JDK lowerdir in %q", joined)
	}
	for _, want := range []string{"upperdir=" + filepath.Join(bundle, "brewlet", "upper"), "workdir=" + filepath.Join(bundle, "brewlet", "work")} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
	if _, err := os.Stat(filepath.Join(bundle, "brewlet", "upper")); err != nil {
		t.Errorf("upper dir not created: %v", err)
	}
}

func TestSetupOverlayRootfsLauncherLower(t *testing.T) {
	bundle := t.TempDir()
	r := &taskAPI.CreateTaskRequest{ID: "task-2", Bundle: bundle}
	ra := testResolved()
	ra.LauncherName = "jaz"
	ra.LauncherRoot = "/opt/brewlet/launchers/jaz"

	if err := setupOverlayRootfs(r, ra); err != nil {
		t.Fatal(err)
	}
	opts := strings.Join(r.Rootfs[0].Options, ",")
	// launcher layer must be higher priority (listed before the JDK) in lowerdir.
	if !strings.Contains(opts, "lowerdir=/opt/brewlet/launchers/jaz:/opt/brewlet/jdks/temurin-21") {
		t.Errorf("launcher not layered above JDK: %q", opts)
	}
}

func TestApplyBrewletLaunch(t *testing.T) {
	spec := &specs.Spec{Process: &specs.Process{Env: []string{"POD_VAR=keepme", "JAVA_HOME=/wrong"}}}
	if err := applyBrewletLaunch(spec, testResolved(), t.TempDir()); err != nil {
		t.Fatalf("applyBrewletLaunch: %v", err)
	}

	if got := spec.Process.Args[0]; got != "java" {
		t.Errorf("args[0] = %q, want java", got)
	}
	if spec.Process.Args[len(spec.Process.Args)-1] != "/app/app.jar" {
		t.Errorf("last arg = %q, want /app/app.jar", spec.Process.Args[len(spec.Process.Args)-1])
	}
	if spec.Process.Cwd != "/app" {
		t.Errorf("cwd = %q", spec.Process.Cwd)
	}

	env := map[string]string{}
	for _, kv := range spec.Process.Env {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["JAVA_HOME"] != "/opt/jdk" {
		t.Errorf("JAVA_HOME = %q, want /opt/jdk (Brewlet must win over pod default)", env["JAVA_HOME"])
	}
	if env["POD_VAR"] != "keepme" {
		t.Errorf("pod env not preserved: %q", env["POD_VAR"])
	}
	if env["FOO"] != "bar" {
		t.Errorf("artifact env FOO = %q, want bar", env["FOO"])
	}

	// JDK + JAR must be mounted.
	var haveJDK, haveJar bool
	for _, m := range spec.Mounts {
		if m.Destination == "/opt/jdk" {
			haveJDK = true
			if m.Source != testResolved().JDKHome {
				t.Errorf("/opt/jdk source = %q, want resolved Java home %q", m.Source, testResolved().JDKHome)
			}
		}
		if m.Destination == "/app/app.jar" {
			haveJar = true
		}
	}
	if !haveJDK || !haveJar {
		t.Errorf("missing mounts: jdk=%v jar=%v", haveJDK, haveJar)
	}
}

func TestApplyBrewletLaunchWithCDS(t *testing.T) {
	// StageCDSJar copies the real JAR bytes, so back JarHostPath with a file.
	jarHost := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(jarHost, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	ra := testResolved()
	ra.JarHostPath = jarHost
	ra.Config.CDS = &artifact.CDS{Archive: "app.jsa", Mode: "dynamic"}
	ra.CDSHostPath = "/var/lib/containerd/.../blobs/sha256/cafe"
	bundleDir := t.TempDir()
	spec := &specs.Spec{Process: &specs.Process{}}
	if err := applyBrewletLaunch(spec, ra, bundleDir); err != nil {
		t.Fatalf("applyBrewletLaunch: %v", err)
	}

	// The archive must be bind-mounted at /app/app.jsa...
	var haveCDS bool
	var jarSource string
	for _, m := range spec.Mounts {
		if m.Destination == "/app/app.jsa" && m.Source == ra.CDSHostPath {
			haveCDS = true
		}
		if m.Destination == "/app/app.jar" {
			jarSource = m.Source
		}
	}
	if !haveCDS {
		t.Errorf("missing /app/app.jsa mount: %+v", spec.Mounts)
	}
	// The JAR must be a canonical-mtime staged copy, NOT the raw blob, so the
	// archive's recorded JAR timestamp matches at load time.
	if jarSource == ra.JarHostPath {
		t.Errorf("jar mount source = raw blob %q, want a staged CDS copy", jarSource)
	}
	if fi, err := os.Stat(jarSource); err != nil {
		t.Errorf("stat staged jar: %v", err)
	} else if !fi.ModTime().Equal(kcruntime.CDSModTime) {
		t.Errorf("staged jar mtime = %v, want canonical %v", fi.ModTime(), kcruntime.CDSModTime)
	}
	// ...and the launch argv must reference it with the safe -Xshare:auto.
	argv := strings.Join(spec.Process.Args, " ")
	if !strings.Contains(argv, "-Xshare:auto") || !strings.Contains(argv, "-XX:SharedArchiveFile=/app/app.jsa") {
		t.Errorf("argv missing CDS flags: %q", argv)
	}
}

func TestApplyBrewletLaunchNoCDS(t *testing.T) {
	spec := &specs.Spec{Process: &specs.Process{}}
	ra := testResolved()
	if err := applyBrewletLaunch(spec, ra, t.TempDir()); err != nil {
		t.Fatalf("applyBrewletLaunch: %v", err)
	}

	for _, m := range spec.Mounts {
		if strings.HasSuffix(m.Destination, ".jsa") {
			t.Errorf("unexpected CDS mount for artifact with no archive: %q", m.Destination)
		}
		// Without CDS the JAR is bind-mounted straight from the blob (no copy).
		if m.Destination == "/app/app.jar" && m.Source != ra.JarHostPath {
			t.Errorf("jar mount source = %q, want raw blob %q (no copy without CDS)", m.Source, ra.JarHostPath)
		}
	}
	if argv := strings.Join(spec.Process.Args, " "); strings.Contains(argv, "SharedArchiveFile") {
		t.Errorf("unexpected CDS flag in argv: %q", argv)
	}
}

func TestApplyBrewletLaunchRegenUsesPrivateScopedMount(t *testing.T) {
	enableTestCDSRegenPolicy(t)
	jdk := t.TempDir()
	if err := os.WriteFile(filepath.Join(jdk, "release"), []byte(
		"IMPLEMENTOR=\"Eclipse Adoptium\"\n"+
			"JAVA_VERSION=\"21.0.5\"\n"+
			"JAVA_RUNTIME_VERSION=\"21.0.5+11-LTS\"\n"+
			"OS_ARCH=\"amd64\"\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	jar := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(jar, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	ra := testResolved()
	ra.JDKHome = jdk
	ra.JarHostPath = jar
	ra.ManifestDigest = "sha256:" + strings.Repeat("a", 64)
	// Use the test process's own UID/GID as the writer owner: os.Chown only
	// succeeds without CAP_CHOWN when the target UID/GID matches the caller,
	// which unprivileged CI runners are. This still exercises the real
	// resetCacheEntry -> os.Chown production path.
	writerUID, writerGID := os.Getuid(), os.Getgid()
	ra.Config.User = &artifact.User{UID: writerUID, GID: writerGID}

	apply := func(namespace, annotatedDigest, cache string) (specs.Mount, string) {
		t.Helper()
		if err := os.Setenv("BREWLET_CDS_CACHE", cache); err != nil {
			t.Fatal(err)
		}
		spec := &specs.Spec{
			Process: &specs.Process{},
			Annotations: map[string]string{
				annCDSRegenerate:       "TrUe",
				annCRISandboxNamespace: namespace,
				annArtifactDigest:      annotatedDigest,
			},
		}
		if err := applyBrewletLaunch(spec, ra, t.TempDir()); err != nil {
			t.Fatalf("applyBrewletLaunch: %v", err)
		}
		for _, mount := range spec.Mounts {
			if mount.Destination == kcruntime.InSandboxCDSDir {
				return mount, strings.Join(spec.Process.Args, " ")
			}
		}
		t.Fatalf("missing %s mount: %+v", kcruntime.InSandboxCDSDir, spec.Mounts)
		return specs.Mount{}, ""
	}
	oldCache, hadCache := os.LookupEnv("BREWLET_CDS_CACHE")
	t.Cleanup(func() {
		if hadCache {
			_ = os.Setenv("BREWLET_CDS_CACHE", oldCache)
		} else {
			_ = os.Unsetenv("BREWLET_CDS_CACHE")
		}
	})

	cacheA := t.TempDir()
	first, firstArgs := apply("tenant-a", "sha256:"+strings.Repeat("1", 64), cacheA)
	if first.Source == cacheA || filepath.Dir(first.Source) != cacheA {
		t.Fatalf("writer source = %q, want private child of %q", first.Source, cacheA)
	}
	info, err := os.Stat(first.Source)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(writerUID) || stat.Gid != uint32(writerGID) {
		t.Fatalf("writer entry owner = %#v, want uid=%d gid=%d", info.Sys(), writerUID, writerGID)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("writer entry mode = %o, want 700", info.Mode().Perm())
	}
	for _, want := range []string{"rw", "nosuid", "nodev", "noexec"} {
		if !hasMountOption(first.Options, want) {
			t.Errorf("writer options = %v, missing %q", first.Options, want)
		}
	}
	if !strings.Contains(firstArgs, "-XX:SharedArchiveFile="+kcruntime.InSandboxCDSDir+"/archive.jsa") {
		t.Fatalf("writer args target the wrong archive: %q", firstArgs)
	}

	cacheSameScope := t.TempDir()
	sameScope, _ := apply("tenant-a", "sha256:"+strings.Repeat("2", 64), cacheSameScope)
	if filepath.Base(first.Source) != filepath.Base(sameScope.Source) {
		t.Fatalf("pod annotation changed cache identity: %q vs %q", first.Source, sameScope.Source)
	}

	cacheOtherScope := t.TempDir()
	otherScope, _ := apply("tenant-b", "sha256:"+strings.Repeat("1", 64), cacheOtherScope)
	if filepath.Base(first.Source) == filepath.Base(otherScope.Source) {
		t.Fatalf("different namespaces shared cache identity: %q", first.Source)
	}
}

func TestApplyBrewletLaunchRegenRequiresPolicyNamespaceAndDigest(t *testing.T) {
	ra := testResolved()
	spec := &specs.Spec{
		Process: &specs.Process{},
		Annotations: map[string]string{
			annCDSRegenerate:       "true",
			annCRISandboxNamespace: "tenant-a",
		},
	}

	oldPath, oldUID := cdsRegenPolicyPath, cdsRegenPolicyOwnerUID
	cdsRegenPolicyPath = filepath.Join(t.TempDir(), "missing")
	cdsRegenPolicyOwnerUID = uint32(os.Getuid())
	t.Cleanup(func() {
		cdsRegenPolicyPath = oldPath
		cdsRegenPolicyOwnerUID = oldUID
	})
	if err := applyBrewletLaunch(spec, ra, t.TempDir()); err == nil || !strings.Contains(err.Error(), "disabled by node policy") {
		t.Fatalf("missing policy error = %v", err)
	}

	enableTestCDSRegenPolicy(t)
	delete(spec.Annotations, annCRISandboxNamespace)
	if err := applyBrewletLaunch(spec, ra, t.TempDir()); err == nil || !strings.Contains(err.Error(), "sandbox namespace") {
		t.Fatalf("missing namespace error = %v", err)
	}

	spec.Annotations[annCRISandboxNamespace] = "tenant-a"
	ra.ManifestDigest = ""
	if err := applyBrewletLaunch(spec, ra, t.TempDir()); err == nil || !strings.Contains(err.Error(), "verified resolved manifest digest") {
		t.Fatalf("missing manifest digest error = %v", err)
	}
}

func enableTestCDSRegenPolicy(t *testing.T) {
	t.Helper()
	oldPath, oldUID := cdsRegenPolicyPath, cdsRegenPolicyOwnerUID
	path := filepath.Join(t.TempDir(), "appcds-regeneration-enabled")
	if err := os.WriteFile(path, []byte("enabled\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	cdsRegenPolicyPath = path
	cdsRegenPolicyOwnerUID = uint32(os.Getuid())
	t.Cleanup(func() {
		cdsRegenPolicyPath = oldPath
		cdsRegenPolicyOwnerUID = oldUID
	})
}

func TestRequireCDSRegenPolicyRejectsUnsafeSentinels(t *testing.T) {
	oldPath, oldUID := cdsRegenPolicyPath, cdsRegenPolicyOwnerUID
	cdsRegenPolicyOwnerUID = uint32(os.Getuid())
	t.Cleanup(func() {
		cdsRegenPolicyPath = oldPath
		cdsRegenPolicyOwnerUID = oldUID
	})

	t.Run("group writable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "policy")
		if err := os.WriteFile(path, nil, 0o664); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o664); err != nil {
			t.Fatal(err)
		}
		cdsRegenPolicyPath = path
		if err := requireCDSRegenPolicy(); err == nil || !strings.Contains(err.Error(), "non-group-writable") {
			t.Fatalf("error = %v, want unsafe-mode rejection", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, nil, 0o444); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "policy")
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		cdsRegenPolicyPath = path
		if err := requireCDSRegenPolicy(); err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("error = %v, want symlink rejection", err)
		}
	})

	t.Run("wrong owner", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "policy")
		if err := os.WriteFile(path, nil, 0o444); err != nil {
			t.Fatal(err)
		}
		cdsRegenPolicyPath = path
		cdsRegenPolicyOwnerUID = uint32(os.Getuid()) + 1
		if err := requireCDSRegenPolicy(); err == nil || !strings.Contains(err.Error(), "owned by uid") {
			t.Fatalf("error = %v, want owner rejection", err)
		}
		cdsRegenPolicyOwnerUID = uint32(os.Getuid())
	})
}

func hasMountOption(options []string, want string) bool {
	for _, option := range options {
		if option == want {
			return true
		}
	}
	return false
}

func TestMountClasspathLayers(t *testing.T) {
	// A classpath.layer.v1+tar staged on the host must be unpacked and
	// bind-mounted read-only at /app/lib so `-cp /app/app.jar:/app/lib/*`
	// resolves the dependency JARs in the production shim path (parity with the
	// CLI/harness bundle assembly).
	src := t.TempDir()
	tarPath := filepath.Join(src, "deps.tar")
	writeTar(t, tarPath, map[string][]byte{"dep.jar": []byte("JAR")})

	bundle := t.TempDir()
	spec := &specs.Spec{Process: &specs.Process{}}
	ra := testResolved()
	ra.ClasspathHostPaths = []string{tarPath}

	if err := mountClasspathLayers(spec, ra, bundle); err != nil {
		t.Fatalf("mountClasspathLayers: %v", err)
	}

	libHost := filepath.Join(bundle, "brewlet", "lib")
	var libMount *specs.Mount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == "/app/lib" {
			libMount = &spec.Mounts[i]
		}
	}
	if libMount == nil {
		t.Fatal("no /app/lib mount added")
	}
	if libMount.Source != libHost {
		t.Errorf("lib mount source = %q, want %q", libMount.Source, libHost)
	}
	if strings.Join(libMount.Options, ",") != "rbind,ro" {
		t.Errorf("lib mount options = %v, want [rbind ro]", libMount.Options)
	}
	if _, err := os.Stat(filepath.Join(libHost, "dep.jar")); err != nil {
		t.Errorf("dependency JAR not staged: %v", err)
	}
}

func TestMountClasspathLayersNoop(t *testing.T) {
	// No classpath layers => no /app/lib mount.
	spec := &specs.Spec{Process: &specs.Process{}}
	if err := mountClasspathLayers(spec, testResolved(), t.TempDir()); err != nil {
		t.Fatalf("mountClasspathLayers: %v", err)
	}
	for _, m := range spec.Mounts {
		if m.Destination == "/app/lib" {
			t.Errorf("unexpected /app/lib mount for artifact with no classpath layers")
		}
	}
}

func TestMountModulepathLayers(t *testing.T) {
	// A modulepath.layer.v1+tar staged on the host must be unpacked and
	// bind-mounted read-only at /app/mods so `-p /app/orders.jar:/app/mods`
	// resolves the library modules in the production shim path.
	src := t.TempDir()
	tarPath := filepath.Join(src, "mods.tar")
	writeTar(t, tarPath, map[string][]byte{"guava.jar": []byte("JAR")})

	bundle := t.TempDir()
	spec := &specs.Spec{Process: &specs.Process{}}
	ra := testResolved()
	ra.ModulepathHostPaths = []string{tarPath}

	if err := mountModulepathLayers(spec, ra, bundle); err != nil {
		t.Fatalf("mountModulepathLayers: %v", err)
	}

	modsHost := filepath.Join(bundle, "brewlet", "mods")
	var modsMount *specs.Mount
	for i := range spec.Mounts {
		if spec.Mounts[i].Destination == "/app/mods" {
			modsMount = &spec.Mounts[i]
		}
	}
	if modsMount == nil {
		t.Fatal("no /app/mods mount added")
	}
	if modsMount.Source != modsHost {
		t.Errorf("mods mount source = %q, want %q", modsMount.Source, modsHost)
	}
	if strings.Join(modsMount.Options, ",") != "rbind,ro" {
		t.Errorf("mods mount options = %v, want [rbind ro]", modsMount.Options)
	}
	if _, err := os.Stat(filepath.Join(modsHost, "guava.jar")); err != nil {
		t.Errorf("module JAR not staged: %v", err)
	}
}

func TestMountModulepathLayersNoop(t *testing.T) {
	// No module layers => no /app/mods mount.
	spec := &specs.Spec{Process: &specs.Process{}}
	if err := mountModulepathLayers(spec, testResolved(), t.TempDir()); err != nil {
		t.Fatalf("mountModulepathLayers: %v", err)
	}
	for _, m := range spec.Mounts {
		if m.Destination == "/app/mods" {
			t.Errorf("unexpected /app/mods mount for artifact with no module layers")
		}
	}
}

func writeTar(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeSpecJSON(t *testing.T, bundle string, spec specs.Spec) {
	t.Helper()
	raw, err := json.Marshal(&spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), raw, 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
}

func TestIsSandboxBundle(t *testing.T) {
	cases := []struct {
		name string
		anns map[string]string
		want bool
	}{
		{"sandbox", map[string]string{annContainerType: containerTypeSandbox}, true},
		{"workload", map[string]string{annContainerType: "container"}, false},
		{"unset (non-CRI/harness path)", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := t.TempDir()
			writeSpecJSON(t, bundle, specs.Spec{Annotations: tc.anns})
			got, err := isSandboxBundle(bundle)
			if err != nil {
				t.Fatalf("isSandboxBundle: %v", err)
			}
			if got != tc.want {
				t.Errorf("isSandboxBundle = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNormalizeRuncOptionsGeneric proves the fix for the CRI panic: containerd's
// CRI plugin wraps a custom runtime handler's config in a generic
// runtimeoptions.Options, but the embedded runc task service hard-asserts
// options.Options. We must translate, preserving SystemdCgroup.
func TestNormalizeRuncOptionsGeneric(t *testing.T) {
	generic := &runtimeoptions.Options{
		ConfigBody: []byte("BinaryName = \"runc\"\nSystemdCgroup = true\n"),
	}
	anyGeneric, err := anypb.New(generic)
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	r := &taskAPI.CreateTaskRequest{Options: anyGeneric}

	if err := normalizeRuncOptions(r); err != nil {
		t.Fatalf("normalizeRuncOptions: %v", err)
	}
	v, err := typeurl.UnmarshalAny(r.Options)
	if err != nil {
		t.Fatalf("unmarshal normalized options: %v", err)
	}
	opts, ok := v.(*runcoptions.Options)
	if !ok {
		t.Fatalf("normalized options type = %T, want *runcoptions.Options", v)
	}
	if !opts.SystemdCgroup {
		t.Errorf("SystemdCgroup not preserved through translation")
	}
	if opts.BinaryName != "runc" {
		t.Errorf("BinaryName = %q, want runc", opts.BinaryName)
	}
}

func TestNormalizeRuncOptionsPassthrough(t *testing.T) {
	// Already runc-native options must be left untouched.
	runcAny, err := anypb.New(&runcoptions.Options{SystemdCgroup: true, BinaryName: "runc"})
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}
	r := &taskAPI.CreateTaskRequest{Options: runcAny}
	before := r.Options
	if err := normalizeRuncOptions(r); err != nil {
		t.Fatalf("normalizeRuncOptions: %v", err)
	}
	if r.Options != before {
		t.Errorf("runc-native options were rewritten")
	}

	// A request without options must not panic and must stay nil.
	empty := &taskAPI.CreateTaskRequest{}
	if err := normalizeRuncOptions(empty); err != nil {
		t.Fatalf("normalizeRuncOptions(nil options): %v", err)
	}
	if empty.Options != nil {
		t.Errorf("nil options became %v", empty.Options)
	}
}
