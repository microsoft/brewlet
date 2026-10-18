package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	p, err := os.MkdirTemp(".", ".node-test-")
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(abs) })
	return abs
}
func reason(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}
func testProcess() specs.Process {
	return specs.Process{Args: []string{"tool", "two words", ""}, Cwd: "/app", Env: []string{"PATH=/untrusted", "X=cri"}, User: specs.User{UID: 1000, GID: 1000}, NoNewPrivileges: true}
}
func testMetadata() Metadata {
	return Metadata{ID: "python-3.12", InventoryUID: "epoch-one", Generation: 1, SourceDigest: "sha256:" + strings.Repeat("a", 64), ManifestDigest: "sha256:" + strings.Repeat("b", 64), Env: []string{"PATH=/usr/bin:/bin", "X=runtime", "BASE=yes"}, UID: 1000, GID: 1000, Home: "/home/app", Homes: map[uint32]string{1000: "/home/app"}}
}
func TestProcessPrecedenceAndExec(t *testing.T) {
	p := testProcess()
	originalArgs := append([]string(nil), p.Args...)
	if err := MergeProcess(&p, testMetadata()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Args, originalArgs) || p.Cwd != "/app" {
		t.Fatal("argv/cwd changed")
	}
	want := []string{"PATH=/usr/bin:/bin", "X=cri", "BASE=yes", "HOME=/home/app"}
	if !reflect.DeepEqual(p.Env, want) {
		t.Fatalf("environment %v", p.Env)
	}
	exec := testProcess()
	exec.Args = []string{"probe"}
	exec.Cwd = "/"
	exec.Env = []string{"PATH=/wrong"}
	if err := RewriteExec(&exec, InitialState{Process: p}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exec.Env, want) || exec.Args[0] != "probe" || exec.Cwd != "/" {
		t.Fatal("exec parity")
	}
}
func TestHomeAndRestrictions(t *testing.T) {
	p := testProcess()
	p.User.UID = 42
	if err := MergeProcess(&p, testMetadata()); err != nil {
		t.Fatal(err)
	}
	if p.Env[len(p.Env)-1] != "HOME=/tmp" {
		t.Fatal(p.Env)
	}
	p = testProcess()
	p.Env = append(p.Env, "HOME=/custom")
	if err := MergeProcess(&p, testMetadata()); err != nil {
		t.Fatal(err)
	}
	if p.Env[len(p.Env)-1] != "HOME=/custom" {
		t.Fatal(p.Env)
	}
	for _, mutate := range []func(*specs.Process){
		func(p *specs.Process) { p.User.UID = 0 }, func(p *specs.Process) { p.User.GID = 0 }, func(p *specs.Process) { p.User.AdditionalGids = []uint32{0} },
	} {
		p := testProcess()
		mutate(&p)
		if reason(MergeProcess(&p, testMetadata())) != "RootNotSupported" {
			t.Fatal("accepted root")
		}
	}
	for _, capability := range []*specs.LinuxCapabilities{
		{Effective: []string{"CAP_CHOWN"}}, {Bounding: []string{"CAP_CHOWN"}},
		{Permitted: []string{"CAP_CHOWN"}}, {Inheritable: []string{"CAP_CHOWN"}},
		{Ambient: []string{"CAP_CHOWN"}},
	} {
		p = testProcess()
		p.Capabilities = capability
		if reason(MergeProcess(&p, testMetadata())) != "PrivilegeEscalationNotSupported" {
			t.Fatal("accepted capability set", capability)
		}
	}
	p = testProcess()
	p.NoNewPrivileges = false
	if reason(MergeProcess(&p, testMetadata())) != "PrivilegeEscalationNotSupported" {
		t.Fatal("accepted escalation")
	}
}
func TestRuntimePATH(t *testing.T) {
	for _, p := range []string{"", ".", "relative", "/bin:", "/app", "/app/bin", "/bin:relative", "/bin/../app", "/bin::/usr/bin", "/bin/.", "/bin//tools", "/bin\x00bad", "/bin:."} {
		if _, err := runtimeEnvironment([]string{"PATH=" + p}); err == nil {
			t.Fatalf("accepted PATH %q", p)
		}
	}
	if _, err := runtimeEnvironment([]string{"PATH=/bin", "PATH=/usr/bin"}); err == nil {
		t.Fatal("duplicate PATH")
	}
}
func TestBarrier(t *testing.T) {
	for _, raw := range []string{`{}`, `{"id":"other","inventoryUID":"x","generation":1}`, `{"id":"x","inventoryUID":"x","generation":0}`, `{"id":"x","inventoryUID":"x","generation":1,"unknown":true}`, `{"id":"x","inventoryUID":"x","generation":1} {}`} {
		if _, err := ParseBarrier(raw, "x"); reason(err) != "InvalidRuntimeGeneration" {
			t.Fatalf("accepted %s", raw)
		}
	}
	b, err := ParseBarrier(`{"id":"x","inventoryUID":"epoch-one","generation":2}`, "x")
	if err != nil || b.Generation != 2 {
		t.Fatal(b, err)
	}
}
func TestBarrierRejectsAmbiguousOrNonintegerGeneration(t *testing.T) {
	for _, raw := range []string{
		`{"id":"x","inventoryUID":"epoch","generation":99,"generation":1}`,
		`{"id":"foreign","id":"x","inventoryUID":"epoch","generation":1}`,
		`{"id":"x","inventoryUID":"old","inventoryUID":"epoch","generation":1}`,
		`{"id":"x","inventoryUID":"epoch","generation":99,"generat\u0069on":1}`,
		`{"id":"x","inventoryUID":"epoch","generation":-1}`,
		`{"id":"x","inventoryUID":"epoch","generation":1.0}`,
		`{"id":"x","inventoryUID":"epoch","generation":1e0}`,
		`{"id":"x","inventoryUID":"epoch","generation":"1"}`,
		`{"id":"x","inventoryUID":"epoch","generation":true}`,
		`{"id":"x","inventoryUID":"epoch","generation":null}`,
		`{"id":"x","inventoryUID":"epoch","generation":9223372036854775808}`,
	} {
		if _, err := ParseBarrier(raw, "x"); reason(err) != "InvalidRuntimeGeneration" {
			t.Fatalf("accepted ambiguous or non-integer barrier %s: %v", raw, err)
		}
	}
	for _, item := range []struct {
		raw        string
		generation int64
	}{
		{`{"id":"x","inventoryUID":"epoch","generation":9007199254740993}`, 9007199254740993},
		{`{"id":"x","inventoryUID":"epoch","generation":9223372036854775807}`, 9223372036854775807},
	} {
		barrier, err := ParseBarrier(item.raw, "x")
		if err != nil {
			t.Fatal(err)
		}
		if barrier.Generation != item.generation {
			t.Fatal("strict parser rounded an exact int64 generation", barrier)
		}
	}
}

func TestProtectedNodeRecordsRejectDuplicateMembers(t *testing.T) {
	for _, raw := range []string{
		`{"id":"x","profileUID":"old","profileUID":"new"}`,
		`{"state":"ready","state":"failed"}`,
		`{"homes":{"1000":"/home/app","1000":"/app"}}`,
	} {
		var record map[string]any
		if err := strictJSON([]byte(raw), &record); err == nil {
			t.Fatal("accepted ambiguous protected node JSON", raw)
		}
	}
}

func TestRootIdentityAndSymlinks(t *testing.T) {
	root := testDirectory(t)
	for _, p := range []string{"usr/bin", "etc", "home/app", "app"} {
		if err := os.MkdirAll(filepath.Join(root, p), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("usr/bin", filepath.Join(root, "bin")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("app:x:1000:1000:app:/home/app:/bin/sh\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:1000:\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m := testMetadata()
	m.Platform.OS = "linux"
	m.Platform.Architecture = "amd64"
	if err := ValidateRoot(root, &m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app/bad"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRoot(root, &m); err == nil {
		t.Fatal("accepted nonempty app")
	}
	if err := os.Remove(filepath.Join(root, "app/bad")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/app", filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}
	m.Env = []string{"PATH=/evil"}
	if err := ValidateRoot(root, &m); err == nil {
		t.Fatal("accepted PATH symlink into app")
	}
}
func testStore(t *testing.T) *Store {
	s := NewStore(testDirectory(t))
	s.ownerUID = uint32(os.Getuid())
	s.testRootOnly = true
	if err := s.Initialize(); err != nil {
		t.Fatal(err)
	}
	return s
}
func testAuthorization() Authorization {
	m := testMetadata()
	return Authorization{ID: m.ID, InventoryUID: m.InventoryUID, Generation: 1, Digest: m.SourceDigest, ProfileUID: "profile-one", ProfileGeneration: 1, ExpiresAt: time.Now().Add(time.Minute), Install: "OnDemand", State: "available"}
}
func TestOnDemandAndAuthorization(t *testing.T) {
	s := testStore(t)
	a := testAuthorization()
	ctx := context.Background()
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Resolve(ctx, a.ID, "k8s.io", "task-one", nil, time.Millisecond)
	if reason(err) != "RuntimeImageInstalling" {
		t.Fatal(err)
	}
	markers, err := s.Requests()
	if err != nil || len(markers) != 1 || markers[0] != a.ID {
		t.Fatal(markers, err)
	}
	marker := filepath.Join(s.Root, ".requests", a.ID)
	first, err := os.Stat(marker)
	if err != nil || first.Size() != 0 || first.Mode().Perm() != 0600 {
		t.Fatalf("request is not an empty private regular marker: info=%v error=%v", first, err)
	}
	_, _, err = s.Resolve(ctx, a.ID, "k8s.io", "task-concurrent", nil, time.Millisecond)
	if reason(err) != "RuntimeImageInstalling" {
		t.Fatal(err)
	}
	again, err := os.Stat(marker)
	if err != nil || !os.SameFile(first, again) {
		t.Fatal("repeated first use replaced the idempotent marker", err)
	}
	if err := s.RemoveRequest(a.ID); err != nil {
		t.Fatal(err)
	}
	a.State = "failed"
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Resolve(ctx, a.ID, "k8s.io", "task-two", nil, time.Millisecond)
	if reason(err) != "RuntimeImageInstallFailed" {
		t.Fatal(err)
	}
	if markers, err := s.Requests(); err != nil || len(markers) != 0 {
		t.Fatal("failed Create issued another install request", markers, err)
	}
	a.State = "available"
	a.Revoked = true
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Resolve(ctx, a.ID, "k8s.io", "task-two", nil, time.Millisecond)
	if reason(err) != "RuntimeImageRevoked" {
		t.Fatal(err)
	}
	a.Revoked = false
	a.ExpiresAt = time.Now().Add(-time.Second)
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Resolve(ctx, a.ID, "k8s.io", "task-two", nil, time.Millisecond)
	if reason(err) != "RuntimeImageAuthorizationExpired" {
		t.Fatal(err)
	}
}
func TestUntrustedInventoryAndMissingExecState(t *testing.T) {
	s := testStore(t)
	if reason(func() error { _, e := s.LoadInitial("unknown"); return e }()) != "ExecStateUnavailable" {
		t.Fatal("missing state did not fail closed")
	}
	if err := os.Chmod(s.Root, 0777); err != nil {
		t.Fatal(err)
	}
	if err := s.Check(); err == nil {
		t.Fatal("accepted writable inventory")
	}
}
func TestUninstalledBarrierReportsInstallingAndRequestsFirstUse(t *testing.T) {
	s := testStore(t)
	a := testAuthorization()
	ctx := context.Background()
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Resolve(ctx, a.ID, "k8s.io", "cold-barrier",
		&Barrier{ID: a.ID, InventoryUID: a.InventoryUID, Generation: a.Generation},
		100*time.Millisecond)
	if reason(err) != "RuntimeImageInstalling" {
		t.Fatalf("cold generation barrier was confused with installed rotation: %v", err)
	}
	requests, err := s.Requests()
	if err != nil || len(requests) != 1 || requests[0] != a.ID {
		t.Fatalf("cold barrier did not request its authorized first-use install: %v, %v", requests, err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, ".tasks", "cold-barrier", "lease.json")); !os.IsNotExist(err) {
		t.Fatalf("cold waiting Create published a task lease: %v", err)
	}
}

func TestEpochMismatchBeforeInstall(t *testing.T) {
	s := testStore(t)
	a := testAuthorization()
	if err := s.WriteAuthorization(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Resolve(context.Background(), a.ID, "k8s.io", "task-one", &Barrier{ID: a.ID, InventoryUID: "old-epoch", Generation: 1}, time.Millisecond)
	if reason(err) != "RuntimeImageEpochMismatch" {
		t.Fatal(err)
	}
	requests, err := s.Requests()
	if err != nil || len(requests) != 0 {
		t.Fatal(requests, err)
	}
}
func fixtureGeneration(t *testing.T, s *Store, gen int64) Metadata {
	t.Helper()
	m := testMetadata()
	m.Generation = gen
	m.Platform.OS = "linux"
	m.Platform.Architecture = "amd64"
	m.ValidatedAt = time.Now().Add(-time.Hour)
	base := filepath.Join(s.Root, m.ID, m.InventoryUID, strconv.FormatInt(gen, 10))
	if err := os.MkdirAll(filepath.Join(base, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(base, "metadata.json"), m); err != nil {
		t.Fatal(err)
	}
	if err := atomicFile(filepath.Join(base, "rootfs", ".brewlet-source"), []byte(m.SourceDigest+"\n")); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestInstalledBarrierLeaseAndDurableExec(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testAuthorization()
	a.Generation = 2
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	m := fixtureGeneration(t, s, 1)
	if err := os.Symlink(filepath.Join(m.InventoryUID, "1"), filepath.Join(s.Root, m.ID, "current")); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Resolve(ctx, m.ID, "k8s.io", "task-one", &Barrier{ID: m.ID, InventoryUID: m.InventoryUID, Generation: 2}, time.Millisecond)
	if reason(err) != "RuntimeImageUpdating" {
		t.Fatal(err)
	}
	markers, err := s.Requests()
	if err != nil || len(markers) != 0 {
		t.Fatal("installed barrier must not request first-use installation", err)
	}
	lease, _, err := s.Resolve(ctx, m.ID, "k8s.io", "task-one", nil, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	p := testProcess()
	if err := MergeProcess(&p, m); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInitial(InitialState{Lease: lease, Process: p}); err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(s.Root)
	restarted.ownerUID = s.ownerUID
	restarted.testRootOnly = true
	state, err := restarted.LoadInitial("task-one")
	if err != nil {
		t.Fatal(err)
	}
	if state.Lease.Metadata.Generation != 1 || !reflect.DeepEqual(state.Process.Env, p.Env) {
		t.Fatal("restart lost execution authority")
	}
	_ = fixtureGeneration(t, s, 2)
	if err := os.Symlink(filepath.Join(m.InventoryUID, "2"), filepath.Join(s.Root, m.ID, "next")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(s.Root, m.ID, "next"), filepath.Join(s.Root, m.ID, "current")); err != nil {
		t.Fatal(err)
	}
	state, err = restarted.LoadInitial("task-one")
	if err != nil || state.Lease.Metadata.Generation != 1 {
		t.Fatal("rotation changed running generation", err)
	}
	newLease, _, err := s.Resolve(ctx, m.ID, "k8s.io", "task-two", &Barrier{ID: m.ID, InventoryUID: m.InventoryUID, Generation: 2}, time.Second)
	if err != nil || newLease.Metadata.Generation != 2 {
		t.Fatal(newLease, err)
	}
}

type fixedEvidence struct {
	gone bool
	err  error
}

func (e fixedEvidence) Gone(context.Context, Lease) (bool, error)        { return e.gone, e.err }
func (e fixedEvidence) MountsGone(context.Context, string) (bool, error) { return e.gone, e.err }
func TestRecoveryAndCollectionFailClosed(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testAuthorization()
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	m := fixtureGeneration(t, s, 1)
	if err := os.Symlink(filepath.Join(m.InventoryUID, "1"), filepath.Join(s.Root, m.ID, "current")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Resolve(ctx, m.ID, "k8s.io", "task-one", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, "task-one", fixedEvidence{gone: false}); err == nil {
		t.Fatal("released live lease")
	}
	if err := s.Recover(ctx, fixedEvidence{err: errors.New("uncertain mounts")}); err == nil {
		t.Fatal("accepted uncertain recovery")
	}
	if err := s.Recover(ctx, fixedEvidence{gone: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, ".tasks/task-one")); !os.IsNotExist(err) {
		t.Fatal("dead lease retained", err)
	}
	if err := s.Collect(ctx, m.ID, time.Millisecond, nil, nil); err == nil {
		t.Fatal("collected without evidence")
	}
}
func TestIDLockBoundsWait(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := testAuthorization()
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	lock, err := s.lock(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(lock)
	start := time.Now()
	_, _, err = s.Resolve(ctx, a.ID, "k8s.io", "task-one", nil, 10*time.Millisecond)
	if reason(err) != "RuntimeImageInstalling" || time.Since(start) > time.Second {
		t.Fatal("lock wait not bounded", err)
	}
}

func TestColdResolveReturnsInstallingBeforeUpstreamRequestDeadline(t *testing.T) {
	s := testStore(t)
	if err := s.WriteAuthorization(context.Background(), testAuthorization()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := s.Resolve(ctx, testAuthorization().ID, "k8s.io", "cold-request", nil, 0)
	if reason(err) != "RuntimeImageInstalling" || ctx.Err() != nil {
		t.Fatal("cold install consumed the upstream runtime request deadline", err, ctx.Err())
	}
}

func TestMissingAndUninstalledEagerRuntimeHaveExactReasonWithoutRequest(t *testing.T) {
	for _, offered := range []bool{false, true} {
		t.Run(strconv.FormatBool(offered), func(t *testing.T) {
			s := testStore(t)
			a := testAuthorization()
			if offered {
				a.Install = "Eager"
				if err := s.WriteAuthorization(context.Background(), a); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := s.Resolve(context.Background(), a.ID, "k8s.io", "not-installed", nil,
				time.Millisecond); reason(err) != "RuntimeImageNotInstalled" {
				t.Fatal("missing/Eager runtime did not retain the specified failure reason", err)
			}
			if requests, err := s.Requests(); err != nil || len(requests) != 0 {
				t.Fatal("missing/Eager runtime issued an OnDemand request", requests, err)
			}
		})
	}
}

func TestIdleTTLRequiresNoLeasesAndCompleteEvidence(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	a := testAuthorization()
	if err := s.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	m := fixtureGeneration(t, s, 1)
	if err := os.Symlink(filepath.Join(m.InventoryUID, "1"), filepath.Join(s.Root, m.ID, "current")); err != nil {
		t.Fatal(err)
	}
	fence := func(context.Context) error { return nil }
	if _, _, err := s.Resolve(ctx, m.ID, "k8s.io", "task-one", nil, time.Second); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := s.IdleUninstall(ctx, m.ID, time.Millisecond, fixedEvidence{gone: true}, fence); err != nil || reclaimed {
		t.Fatal("reclaimed leased runtime", err)
	}
	if err := s.Release(ctx, "task-one", fixedEvidence{gone: true}); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(filepath.Join(s.Root, m.ID, "usage.json"), usage{IdleSince: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if reclaimed, err := s.IdleUninstall(ctx, m.ID, time.Millisecond, fixedEvidence{err: errors.New("unknown mounts")}, fence); err == nil || reclaimed {
		t.Fatal("reclaimed without evidence", err)
	}
	if reclaimed, err := s.IdleUninstall(ctx, m.ID, time.Millisecond, fixedEvidence{gone: true}, func(context.Context) error { return errors.New("lost fence") }); err == nil || reclaimed {
		t.Fatal("reclaimed without authority", err)
	}
	reclaimed, err := s.IdleUninstall(ctx, m.ID, time.Millisecond, fixedEvidence{gone: true}, fence)
	if err != nil || !reclaimed {
		t.Fatal(reclaimed, err)
	}
	if _, err := os.Lstat(filepath.Join(s.Root, m.ID, "current")); !os.IsNotExist(err) {
		t.Fatal("idle current retained", err)
	}
	auth, err := s.auth(m.ID, time.Now())
	if err != nil || auth.State != "available" {
		t.Fatal(auth, err)
	}
}
