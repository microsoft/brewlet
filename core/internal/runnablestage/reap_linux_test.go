// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package runnablestage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func createStage(t *testing.T, root, name string, age time.Duration) string {
	t.Helper()
	path := filepath.Join(root, "immutable-v2", name)
	writeFile(t, filepath.Join(path, "app", "app.jar"), "jar")
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func fakeDependencies(t *testing.T, root string) dependencies {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(root, &stat); err != nil {
		t.Fatal(err)
	}
	base := mount{device: fmt.Sprintf("%d:%d", unix.Major(uint64(stat.Dev)), unix.Minor(uint64(stat.Dev))), root: "/", point: "/"}
	return dependencies{
		references: func(context.Context, string) (map[string]bool, error) { return map[string]bool{}, nil },
		mounts: func(context.Context, bool) (mountSnapshot, error) {
			return mountSnapshot{current: []mount{base}, all: []mount{base}}, nil
		},
		now: time.Now,
	}
}

func TestReapFiltersAndDryRun(t *testing.T) {
	for _, dry := range []bool{true, false} {
		t.Run(fmt.Sprintf("dry=%t", dry), func(t *testing.T) {
			root := t.TempDir()
			deps := fakeDependencies(t, root)
			names := make([]string, 7)
			for i := range names {
				names[i] = strings.Repeat(fmt.Sprintf("%x", i), 64)
			}
			old := createStage(t, root, names[0], 48*time.Hour)
			image := createStage(t, root, names[1], 48*time.Hour)
			content := createStage(t, root, names[2], 48*time.Hour)
			mounted := createStage(t, root, names[3], 48*time.Hour)
			young := createStage(t, root, names[4], time.Hour)
			inside := createStage(t, root, names[5], 48*time.Hour)
			unknown := createStage(t, root, "unknown", 48*time.Hour)
			pending := createStage(t, root, "."+names[0]+"-pending", 48*time.Hour)
			uppercase := createStage(t, root, strings.Repeat("A", 64), 48*time.Hour)
			writeFile(t, filepath.Join(root, "legacy", "app.jar"), "jar")
			writeFile(t, filepath.Join(root, "immutable-v1", names[0], "app.jar"), "jar")
			outside := t.TempDir()
			writeFile(t, filepath.Join(outside, "app.jar"), "untouched")
			link := filepath.Join(root, "immutable-v2", names[6])
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			deps.references = func(context.Context, string) (map[string]bool, error) {
				return map[string]bool{names[1]: true, names[2]: true}, nil
			}
			snapshot, _ := deps.mounts(context.Background(), false)
			snapshot.all = append(snapshot.all, mount{device: snapshot.current[0].device, root: filepath.Join(mounted, "app", "app.jar"), point: "/app/app.jar"})
			snapshot.all = append(snapshot.all, mount{device: snapshot.current[0].device, root: root, point: "/exporter-stage"})
			snapshot.current = append(snapshot.current, mount{device: "99:9", root: "/", point: filepath.Join(inside, "nested")})
			deps.mounts = func(context.Context, bool) (mountSnapshot, error) { return snapshot, nil }
			result, err := reap(context.Background(), "fake", Options{Root: root, MinAge: DefaultMinAge, DryRun: dry}, deps)
			if err != nil {
				t.Fatal(err)
			}
			wantRemaining := int64(33)
			if !dry {
				wantRemaining -= 3
			}
			if result.Removed != 1 || result.ReclaimedBytes != 3 || result.RemainingBytes != wantRemaining {
				t.Fatalf("unexpected result: %+v, want remaining %d", result, wantRemaining)
			}
			if _, err := os.Lstat(old); dry && err != nil || !dry && !os.IsNotExist(err) {
				t.Fatalf("old stage: %v", err)
			}
			for _, path := range []string{image, content, mounted, young, inside, unknown, pending, uppercase, link, outside, filepath.Join(root, "legacy"), filepath.Join(root, "immutable-v1")} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("removed protected path %s: %v", path, err)
				}
			}
		})
	}
}

func TestReapFailClosed(t *testing.T) {
	for _, failure := range []string{"references", "mounts", "mapping", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			root := t.TempDir()
			path := createStage(t, root, strings.Repeat("a", 64), 48*time.Hour)
			second := createStage(t, root, strings.Repeat("b", 64), 48*time.Hour)
			deps := fakeDependencies(t, root)
			ctx := context.Background()
			switch failure {
			case "references":
				deps.references = func(context.Context, string) (map[string]bool, error) {
					return nil, errors.New("containerd unavailable")
				}
			case "mounts":
				deps.mounts = func(context.Context, bool) (mountSnapshot, error) {
					return mountSnapshot{}, os.ErrPermission
				}
			case "mapping":
				deps.mounts = func(context.Context, bool) (mountSnapshot, error) {
					return mountSnapshot{}, nil
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := reap(ctx, "", Options{Root: root, MinAge: DefaultMinAge}, deps); err == nil {
				t.Fatal("expected failure")
			}
			for _, stage := range []string{path, second} {
				if data, err := os.ReadFile(filepath.Join(stage, "app", "app.jar")); err != nil || string(data) != "jar" {
					t.Fatalf("changed stage before complete validation: %q, %v", data, err)
				}
			}
		})
	}
}

func TestReapSymlinksAndMissing(t *testing.T) {
	root := t.TempDir()
	deps := fakeDependencies(t, root)
	missing := filepath.Join(root, "missing")
	if got, err := reap(context.Background(), "", Options{Root: missing, MinAge: DefaultMinAge}, dependencies{}); got != (Result{}) || err != nil {
		t.Fatalf("missing root: %+v, %v", got, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("reaping created missing root")
	}
	target := t.TempDir()
	createStage(t, target, strings.Repeat("a", 64), 48*time.Hour)
	for _, name := range []string{"link", "immutable-v2"} {
		path := filepath.Join(root, name)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		reapRoot := root
		if name == "link" {
			reapRoot = path
		}
		if _, err := reap(context.Background(), "", Options{Root: reapRoot, MinAge: DefaultMinAge}, deps); err == nil {
			t.Fatalf("accepted symlink %s", name)
		}
	}
	for _, age := range []time.Duration{-1, 0} {
		if _, err := reap(context.Background(), "", Options{Root: root, MinAge: age}, deps); err == nil {
			t.Fatalf("accepted nonpositive age %s", age)
		}
	}
}

func TestAgeBoundary(t *testing.T) {
	root := t.TempDir()
	deps := fakeDependencies(t, root)
	now := time.Now().Truncate(time.Second)
	deps.now = func() time.Time { return now }
	path := createStage(t, root, strings.Repeat("a", 64), time.Hour)
	boundary := now.Add(-time.Hour)
	if err := os.Chtimes(path, boundary, boundary); err != nil {
		t.Fatal(err)
	}
	result, err := reap(context.Background(), "", Options{Root: root, MinAge: time.Hour}, deps)
	if err != nil || result.Removed != 0 {
		t.Fatalf("age boundary: %+v, %v", result, err)
	}
	deps.now = func() time.Time { return now.Add(time.Second) }
	result, err = reap(context.Background(), "", Options{Root: root, MinAge: time.Hour}, deps)
	if err != nil || result.Removed != 1 {
		t.Fatalf("past age boundary: %+v, %v", result, err)
	}
}

func TestLocks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "created")
	a, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := Reap(context.Background(), "", Options{Root: root, MinAge: DefaultMinAge}); !errors.Is(err, ErrBusy) {
		t.Fatalf("reaper with active readers: %v", err)
	}
	a.Close()
	if _, err := lock(root, true); !errors.Is(err, ErrBusy) {
		t.Fatalf("second reader failed to retain lock: %v", err)
	}
	b.Close()
	exclusive, err := lock(root, true)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan io.Closer, 1)
	failures := make(chan error, 1)
	go func() {
		reader, err := Acquire(root)
		if err != nil {
			failures <- err
			return
		}
		acquired <- reader
	}()
	select {
	case reader := <-acquired:
		reader.Close()
		t.Fatal("reader bypassed exclusive lock")
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(25 * time.Millisecond):
	}
	exclusive.Close()
	select {
	case reader := <-acquired:
		reader.Close()
	case err := <-failures:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not unblock")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("locking added staging entries: %v, %v", entries, err)
	}
}

func TestLockInterprocess(t *testing.T) {
	if root := os.Getenv("BREWLET_STAGE_LOCK_TEST_CHILD"); root != "" {
		guard, err := Acquire(root)
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Close()
		fmt.Println("locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestLockInterprocess$")
	cmd.Env = append(os.Environ(), "BREWLET_STAGE_LOCK_TEST_CHILD="+root)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		input.Close()
		if err := cmd.Wait(); err != nil {
			t.Error(err)
		}
	}()
	line, err := bufio.NewReader(output).ReadString('\n')
	if err != nil || line != "locked\n" {
		t.Fatalf("child readiness: %q %v", line, err)
	}
	if _, err := Reap(context.Background(), "", Options{Root: root, MinAge: DefaultMinAge}); !errors.Is(err, ErrBusy) {
		t.Fatalf("cross-process reaper lock: %v", err)
	}
}

func TestLockedStageSurvivesUntilGuardReleased(t *testing.T) {
	root := t.TempDir()
	path := createStage(t, root, strings.Repeat("a", 64), 48*time.Hour)
	deps := fakeDependencies(t, root)
	guard, err := Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	opts := Options{Root: root, MinAge: DefaultMinAge}
	if _, err := reap(context.Background(), "", opts, deps); !errors.Is(err, ErrBusy) {
		t.Fatalf("locked stage must block reaping: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(path, "app", "app.jar")); err != nil || string(data) != "jar" {
		t.Fatalf("locked payload changed: %q, %v", data, err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := reap(context.Background(), "", opts, deps)
	if err != nil || result.Removed != 1 || result.ReclaimedBytes != 3 || result.RemainingBytes != 0 {
		t.Fatalf("unlocked eligible stage not reclaimed: %+v, %v", result, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reclaimed stage still exists: %v", err)
	}
}

func TestReapPassesNestedPIDOptIn(t *testing.T) {
	for _, allow := range []bool{false, true} {
		root := t.TempDir()
		createStage(t, root, strings.Repeat("a", 64), 48*time.Hour)
		deps := fakeDependencies(t, root)
		inner := deps.mounts
		var got *bool
		deps.mounts = func(ctx context.Context, nested bool) (mountSnapshot, error) {
			got = &nested
			return inner(ctx, nested)
		}
		if _, err := reap(context.Background(), "", Options{Root: root, MinAge: time.Hour, DryRun: true, AllowNestedPIDNamespace: allow}, deps); err != nil {
			t.Fatal(err)
		}
		if got == nil || *got != allow {
			t.Fatalf("mount reader received nested=%v, want %t", got, allow)
		}
	}
}
