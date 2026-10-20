package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestVolumeMountPolicyIsExplicitCanonicalAndBounded(t *testing.T) {
	for _, paths := range [][]string{nil, {}, {"/cache", "/tmp", "/work"}, {"/etc/myapp"}} {
		if err := ValidateVolumeMountPaths(paths); err != nil {
			t.Fatalf("valid policy %v: %v", paths, err)
		}
	}
	for _, paths := range [][]string{
		{"/"}, {"/app"}, {"/app/data"}, {"/usr", "/usr/lib"}, {"/cache", "/cache"},
		{"/work", "/tmp"}, {"/tmp/../usr"}, {"/tmp/"}, {"tmp"}, {"/tmp\x00"},
		{"/dev/data"}, {"/proc"}, {"/sys/cache"}, {"/etc"}, {"/etc/hosts"}, {"/tmp\\data"},
		make([]string, 33),
	} {
		if err := ValidateVolumeMountPaths(paths); err == nil {
			t.Fatalf("invalid policy accepted: %q", paths)
		}
	}
	for _, test := range []struct {
		path        string
		ro, allowed bool
	}{
		{"/tmp", false, true}, {"/tmp/child", false, true}, {"/cache", true, true},
		{"/app/data", false, true}, {serviceAccountMount, true, true},
		{"/usr/bin", false, false}, {"/usr/bin", true, false}, {"/tmp-other", false, false},
		{"/app", false, false}, {"/app/../usr", false, false},
		{serviceAccountMount, false, false}, {"/dev/shm", false, false},
	} {
		err := ValidateWorkloadMount(test.path, test.ro, []string{"/cache", "/tmp"})
		if (err == nil) != test.allowed {
			t.Errorf("mount %s ro=%v: %v", test.path, test.ro, err)
		}
	}
}

func TestFilesystemRequiresReadOnlyRootAndRejectsRuntimeMountOverlays(t *testing.T) {
	root := t.TempDir()
	metadata := Metadata{VolumeMountPaths: []string{"/cache", "/tmp"}}
	for _, readonly := range []bool{false, true} {
		for _, destination := range []string{"/tmp", "/usr/bin", "/usr/lib", "/"} {
			spec := &specs.Spec{Root: &specs.Root{Readonly: readonly},
				Mounts: []specs.Mount{{Type: "bind", Source: "/volume", Destination: destination}}}
			err := ValidateFilesystem(spec, metadata, root)
			want := readonly && destination == "/tmp"
			if (err == nil) != want {
				t.Fatalf("readonly=%v mount=%s: %v", readonly, destination, err)
			}
		}
	}
	for _, spec := range []*specs.Spec{{}, {Root: &specs.Root{Readonly: false}}} {
		if err := ValidateFilesystem(spec, metadata, root); err == nil || !strings.Contains(err.Error(), "ReadOnlyRootRequired") {
			t.Fatalf("writable root accepted: %v", err)
		}
	}
	for _, mount := range []specs.Mount{
		{Destination: "/proc", Type: "proc"}, {Destination: "/dev", Type: "tmpfs"},
		{Destination: "/dev/pts", Type: "devpts"}, {Destination: "/dev/mqueue", Type: "mqueue"},
		{Destination: "/dev/shm", Type: "bind"}, {Destination: "/sys", Type: "sysfs", Options: []string{"ro"}},
		{Destination: "/sys/fs/cgroup", Type: "cgroup", Options: []string{"ro"}},
		{Destination: "/etc/hosts", Type: "bind"}, {Destination: "/etc/hostname", Type: "bind"},
		{Destination: "/etc/resolv.conf", Type: "bind"}, {Destination: "/dev/termination-log", Type: "bind"},
		{Destination: serviceAccountMount, Type: "bind", Options: []string{"ro"}},
	} {
		if err := ValidateFilesystem(&specs.Spec{Root: &specs.Root{Readonly: true}, Mounts: []specs.Mount{mount}}, metadata, root); err != nil {
			t.Fatalf("standard Kubernetes mount %s: %v", mount.Destination, err)
		}
		mount.Type = "overlay"
		if err := ValidateFilesystem(&specs.Spec{Root: &specs.Root{Readonly: true}, Mounts: []specs.Mount{mount}}, metadata, root); err == nil && mount.Destination != serviceAccountMount {
			t.Fatalf("forged system mount accepted: %s", mount.Destination)
		}
	}
	for _, destination := range []string{"/usr/bin", "/tmp/device"} {
		spec := &specs.Spec{Root: &specs.Root{Readonly: true}, Linux: &specs.Linux{
			Devices: []specs.LinuxDevice{{Path: destination, Type: "b", Major: 8, Minor: 0}}}}
		if err := ValidateFilesystem(spec, metadata, root); (err == nil) != (destination == "/tmp/device") {
			t.Fatalf("device destination %s: %v", destination, err)
		}
	}
}

func TestRuntimeMountPolicyRejectsSymlinkAliasesIncludingDanglingLinks(t *testing.T) {
	for _, link := range []string{"/usr/lib", "../usr/lib", "/missing"} {
		root := t.TempDir()
		if err := os.Symlink(link, filepath.Join(root, "tmp")); err != nil {
			t.Fatal(err)
		}
		metadata := Metadata{VolumeMountPaths: []string{"/tmp"}}
		if err := validateRuntimeMountPaths(root, metadata); err == nil {
			t.Fatalf("runtime data alias accepted: %s", link)
		}
		spec := &specs.Spec{Root: &specs.Root{Readonly: true}, Mounts: []specs.Mount{
			{Type: "bind", Destination: "/tmp/child", Source: "/volume"}}}
		if err := ValidateFilesystem(spec, metadata, root); err == nil {
			t.Fatalf("container data alias accepted: %s", link)
		}
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "var"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/run", filepath.Join(root, "var/run")); err != nil {
		t.Fatal(err)
	}
	spec := &specs.Spec{Root: &specs.Root{Readonly: true}, Mounts: []specs.Mount{
		{Type: "bind", Destination: serviceAccountMount, Source: "/volume", Options: []string{"ro"}}}}
	if err := ValidateFilesystem(spec, Metadata{}, root); err != nil {
		t.Fatalf("standard service-account alias rejected: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "var/run")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin", filepath.Join(root, "var/run")); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFilesystem(spec, Metadata{}, root); err == nil {
		t.Fatal("service account alias into runtime programs accepted")
	}
}

func TestRuntimeDataZonesCannotOverlapExecutableSearchPaths(t *testing.T) {
	root := t.TempDir()
	for _, env := range []string{"PATH=/tmp/bin:/usr/bin", "PATH=/tmp"} {
		if err := validateRuntimeMountPaths(root, Metadata{Env: []string{env}, VolumeMountPaths: []string{"/tmp"}}); err == nil {
			t.Fatalf("executable search path inside data zone accepted: %s", env)
		}
	}
}

func TestMountsCannotRedirectLaterMountsThroughAnEarlierVolume(t *testing.T) {
	for _, targets := range [][]string{
		{"/tmp", "/tmp/child"}, {"/app/data", "/app/data/child"}, {"/tmp", "/tmp"},
		{"/run", serviceAccountMount}, {"/var", serviceAccountMount},
	} {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "var"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/run", filepath.Join(root, "var/run")); err != nil {
			t.Fatal(err)
		}
		spec := &specs.Spec{Root: &specs.Root{Readonly: true}}
		for _, destination := range targets {
			spec.Mounts = append(spec.Mounts, specs.Mount{Type: "bind", Destination: destination, Options: []string{"ro"}})
		}
		if err := ValidateFilesystem(spec, Metadata{VolumeMountPaths: []string{"/run", "/tmp", "/var"}}, root); err == nil {
			t.Fatalf("overlapping mount targets accepted: %v", targets)
		}
	}
}
