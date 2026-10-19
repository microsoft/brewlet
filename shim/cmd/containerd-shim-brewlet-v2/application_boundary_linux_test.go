//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

func TestApplicationBoundaryHidesRuntimeContentWithoutChangingApprovedRoots(t *testing.T) {
	for _, kind := range []string{"directory", "file", "symlink"} {
		for _, mutation := range []string{"ordinary", "opaque", "whiteout"} {
			t.Run(kind+"/"+mutation, func(t *testing.T) {
				base, err := os.MkdirTemp(".", ".app-boundary-test-")
				if err != nil {
					t.Fatal(err)
				}
				base, err = filepath.Abs(base)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(base) })
				for _, dir := range []string{"runtime/etc", "runtime/hidden", "dep/app/lib", "app/app", "bundle", "upper", "work", "volume", "final"} {
					if err := os.MkdirAll(filepath.Join(base, dir), 0755); err != nil {
						t.Fatal(err)
					}
				}
				write := func(name, value string) {
					t.Helper()
					if err := os.WriteFile(filepath.Join(base, name), []byte(value), 0644); err != nil {
						t.Fatal(err)
					}
				}
				write("runtime/etc/sentinel", "approved-runtime")
				write("runtime/hidden/foreign", "not-application")
				switch kind {
				case "directory":
					if err := os.Mkdir(filepath.Join(base, "runtime/app"), 0755); err != nil {
						t.Fatal(err)
					}
					write("runtime/app/foreign", "not-application")
				case "file":
					write("runtime/app", "not-application")
				case "symlink":
					if err := os.Symlink("/hidden", filepath.Join(base, "runtime/app")); err != nil {
						t.Fatal(err)
					}
				}
				write("dep/app/lib/dependency", "dependency")
				write("dep/app/order", "lower")
				write("app/app/main", "application")
				write("app/app/order", "upper")
				lowers, err := applicationRuntimeLowers(filepath.Join(base, "bundle"),
					[]string{filepath.Join(base, "dep"), filepath.Join(base, "app")}, filepath.Join(base, "runtime"))
				if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
					t.Skipf("requires isolated Linux SYS_ADMIN: %v", err)
				}
				if err != nil {
					t.Fatal(err)
				}
				switch mutation {
				case "opaque":
					dir := filepath.Join(base, "app/app/lib")
					if err := os.Mkdir(dir, 0755); err != nil {
						t.Fatal(err)
					}
					if err := unix.Setxattr(dir, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
						t.Fatal(err)
					}
					write("app/app/lib/replacement", "new-dependency")
				case "whiteout":
					name := filepath.Join(base, "app/app/order")
					if err := os.Remove(name); err != nil {
						t.Fatal(err)
					}
					if err := unix.Mknod(name, unix.S_IFCHR, 0); err != nil {
						t.Fatal(err)
					}
				}
				if err := prepareAppMountpoints(filepath.Join(base, "bundle"), "rootfs", lowers, filepath.Join(base, "upper"), filepath.Join(base, "work"),
					[]specs.Mount{{Destination: "/app/new/volume", Type: "bind", Source: filepath.Join(base, "volume")}}); err != nil {
					t.Fatal(err)
				}
				options := []string{"ro", "lowerdir=" + strings.Join(lowers, ":"), "upperdir=" + filepath.Join(base, "upper"), "workdir=" + filepath.Join(base, "work")}
				if err := mount.All([]mount.Mount{{Type: "overlay", Source: "overlay", Options: options}}, filepath.Join(base, "final")); err != nil {
					t.Fatal(err)
				}
				defer mount.UnmountAll(filepath.Join(base, "final"), 0)
				expected := map[string]string{"app/main": "application", "app/order": "upper", "app/lib/dependency": "dependency", "etc/sentinel": "approved-runtime"}
				absent := "app/foreign"
				if mutation == "opaque" {
					delete(expected, "app/lib/dependency")
					expected["app/lib/replacement"] = "new-dependency"
					absent = "app/lib/dependency"
				} else if mutation == "whiteout" {
					delete(expected, "app/order")
					absent = "app/order"
				}
				for name, value := range expected {
					data, err := os.ReadFile(filepath.Join(base, "final", name))
					if err != nil || string(data) != value {
						t.Fatal(name, string(data), err)
					}
				}
				if _, err := os.Lstat(filepath.Join(base, "final/app/foreign")); !os.IsNotExist(err) {
					t.Fatal("runtime content leaked into application", err)
				}
				if _, err := os.Lstat(filepath.Join(base, "final", absent)); !os.IsNotExist(err) {
					t.Fatal("payload whiteout/opaque metadata lost", absent, err)
				}
				if _, err := os.Stat(filepath.Join(base, "final/app/new/volume")); err != nil {
					t.Fatal("explicit volume mountpoint lost", err)
				}
				if _, err := os.Lstat(filepath.Join(base, "runtime/app")); err != nil {
					t.Fatal("approved app entry was removed", err)
				}
				if _, err := os.Lstat(filepath.Join(base, "app/app/new")); !os.IsNotExist(err) {
					t.Fatal("payload cache was mutated", err)
				}
				if err := os.WriteFile(filepath.Join(base, "final/app/write"), nil, 0644); err == nil {
					t.Fatal("read-only composition became writable")
				}
			})
		}
	}
}
