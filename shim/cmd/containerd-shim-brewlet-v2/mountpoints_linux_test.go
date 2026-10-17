//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestAbsentAppVolumeMountpointUsesManagedUpper(t *testing.T) {
	base, err := os.MkdirTemp(".", ".mountpoint-test-")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	for _, name := range []string{"lower/app", "upper", "work", "bundle", "volume", "final"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "lower/app/program"), []byte("immutable"), 0555); err != nil {
		t.Fatal(err)
	}
	mounts := []specs.Mount{{Destination: "/app/absent/deep", Type: "bind", Source: filepath.Join(base, "volume")}}
	err = prepareAppMountpoints(filepath.Join(base, "bundle"), "rootfs", []string{filepath.Join(base, "lower")}, filepath.Join(base, "upper"), filepath.Join(base, "work"), mounts)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("requires isolated Linux SYS_ADMIN overlay mount capability: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "upper/app/absent/deep")); err != nil {
		t.Fatal("mountpoint not in managed upper", err)
	}
	if _, err := os.Stat(filepath.Join(base, "lower/app/absent")); !os.IsNotExist(err) {
		t.Fatal("immutable lower mutated", err)
	}
	// A read-only root overlay can still include the prepared upper. The mount
	// placeholders therefore remain available before runc binds /app read-only.
	options := []string{"ro", "lowerdir=" + filepath.Join(base, "lower"), "upperdir=" + filepath.Join(base, "upper"), "workdir=" + filepath.Join(base, "work")}
	if err := mount.All([]mount.Mount{{Type: "overlay", Source: "overlay", Options: options}}, filepath.Join(base, "final")); err != nil {
		t.Fatal(err)
	}
	defer mount.UnmountAll(filepath.Join(base, "final"), 0)
	if _, err := os.Stat(filepath.Join(base, "final/app/absent/deep")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "final/app/should-fail"), []byte("x"), 0644); err == nil {
		t.Fatal("read-only root became writable")
	}
}

func TestAppVolumeEscapingSymlinkIsRejected(t *testing.T) {
	base, err := os.MkdirTemp(".", ".mountpoint-link-test-")
	if err != nil {
		t.Fatal(err)
	}
	base, err = filepath.Abs(base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	for _, name := range []string{"lower/app", "lower/outside", "upper", "work", "bundle", "volume"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../outside", filepath.Join(base, "lower/app/escape")); err != nil {
		t.Fatal(err)
	}
	mounts := []specs.Mount{{Destination: "/app/escape/new", Type: "bind", Source: filepath.Join(base, "volume")}}
	err = prepareAppMountpoints(filepath.Join(base, "bundle"), "rootfs", []string{filepath.Join(base, "lower")}, filepath.Join(base, "upper"), filepath.Join(base, "work"), mounts)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("requires isolated Linux SYS_ADMIN overlay mount capability: %v", err)
	}
	if err == nil {
		t.Fatal("escaping symlink accepted")
	}
	if _, err := os.Stat(filepath.Join(base, "upper/outside/new")); !os.IsNotExist(err) {
		t.Fatal("escaped mountpoint created", err)
	}
}
