// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package runnablestage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

type fakeProc struct {
	files fstest.MapFS
	links map[string]string
	fail  string
	// einval names a file whose read fails like a zombie's mountinfo.
	einval string
}

func (p fakeProc) ReadFile(name string) ([]byte, error) {
	if name == p.fail {
		return nil, os.ErrPermission
	}
	if name == p.einval {
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.EINVAL}
	}
	return fs.ReadFile(p.files, name)
}
func (p fakeProc) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == p.fail {
		return nil, os.ErrPermission
	}
	return fs.ReadDir(p.files, name)
}
func (p fakeProc) Readlink(name string) (string, error) {
	if name == p.fail {
		return "", os.ErrPermission
	}
	if value, ok := p.links[name]; ok {
		return value, nil
	}
	return "", os.ErrNotExist
}

const hostMountinfo = "1 0 8:1 / / rw - ext4 /dev/root rw\n2 1 0:2 / /proc rw - proc proc rw\n"

func newProc() fakeProc {
	return fakeProc{
		files: fstest.MapFS{
			"self/mountinfo": {Data: []byte(hostMountinfo)},
			"1/mountinfo":    {Data: []byte(hostMountinfo)},
			"42/mountinfo":   {Data: []byte("3 0 8:1 /stage/immutable-v2/hash/app/app.jar /app/app.jar ro - ext4 /dev/root rw\n4 0 0:4 net:[4026532282] /run/netns/cni-1 rw - nsfs nsfs rw\n")},
		},
		links: map[string]string{
			"self/ns/pid": "pid:[4026531836]", "1/ns/pid": "pid:[4026531836]",
			"self/ns/user": "user:[4026531837]", "self/ns/mnt": "mnt:[1]", "1/ns/mnt": "mnt:[1]",
		},
	}
}

func TestMountsAcrossProcesses(t *testing.T) {
	proc := newProc()
	snapshot, err := readMounts(context.Background(), proc, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := snapshot.protects("/stage/immutable-v2/hash", "8:1")
	if err != nil || !got {
		t.Fatalf("missed bind source in another PID namespace: %t %v", got, err)
	}
	if got, err := snapshot.protects("/stage/immutable-v2/has", "8:1"); err != nil || got {
		t.Fatalf("non-component prefix matched: %t %v", got, err)
	}
}

func TestMountMapping(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mounts []mount
		want   bool
	}{
		{"file source", []mount{{device: "8:1", root: "/subvolume/cache/hash/app.jar", point: "/app.jar"}}, true},
		{"directory source", []mount{{device: "8:1", root: "/subvolume/cache/hash", point: "/app"}}, true},
		{"exporter ancestor source", []mount{{device: "8:1", root: "/subvolume/cache", point: "/cache"}}, false},
		{"filesystem root source", []mount{{device: "8:1", root: "/", point: "/host-root"}}, false},
		{"other device", []mount{{device: "8:2", root: "/subvolume/cache/hash", point: "/app"}}, false},
		{"prefix only", []mount{{device: "8:1", root: "/subvolume/cache/hash-other", point: "/app"}}, false},
		{"unrelated", []mount{{device: "8:1", root: "/other/hash", point: "/app"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base := mount{device: "8:1", root: "/subvolume", point: "/host"}
			snapshot := mountSnapshot{current: []mount{base}, all: append([]mount{base}, tt.mounts...)}
			got, err := snapshot.protects("/host/cache/hash", "8:1")
			if err != nil || got != tt.want {
				t.Fatalf("protected=%t error=%v want=%t", got, err, tt.want)
			}
		})
	}
	for _, device := range []string{"8:1", "8:2"} {
		base := mount{device: "8:1", root: "/", point: "/"}
		inside := mount{device: device, root: "/", point: "/stage/hash/mount"}
		snapshot := mountSnapshot{current: []mount{base, inside}, all: []mount{base}}
		if protected, err := snapshot.protects("/stage/hash", "8:1"); err != nil || !protected {
			t.Fatalf("mount inside stage on %s not protected: %t %v", device, protected, err)
		}
	}
}

func TestProcFailClosed(t *testing.T) {
	for _, failure := range []string{"hidepid", "pidns", "userns", "mountns", "missing-proc", "permission", "parse", "empty-live", "missing-live", "einval-live", "enumeration", "namespace-permission"} {
		t.Run(failure, func(t *testing.T) {
			proc := newProc()
			switch failure {
			case "hidepid":
				proc.files["self/mountinfo"].Data = []byte(strings.ReplaceAll(hostMountinfo, "proc proc rw", "proc proc rw,hidepid=2"))
			case "pidns":
				proc.links["self/ns/pid"] = "pid:[999]"
			case "userns":
				proc.links["self/ns/user"] = "user:[999]"
			case "mountns":
				proc.links["self/ns/mnt"] = "mnt:[999]"
			case "missing-proc":
				proc.files["self/mountinfo"].Data = []byte("1 0 8:1 / / rw - ext4 /dev/root rw\n")
			case "permission":
				proc.fail = "42/mountinfo"
			case "parse":
				proc.files["42/mountinfo"].Data = []byte("broken\n")
			case "empty-live":
				proc.files["42/mountinfo"].Data = nil
				proc.files["42/stat"] = &fstest.MapFile{Data: []byte("42 (jvm) S 1 2")}
			case "missing-live":
				delete(proc.files, "42/mountinfo")
				proc.files["42/stat"] = &fstest.MapFile{Data: []byte("42 (jvm) S 1 2")}
			case "einval-live":
				proc.einval = "42/mountinfo"
				proc.files["42/stat"] = &fstest.MapFile{Data: []byte("42 (jvm) S 1 2")}
			case "enumeration":
				proc.fail = "."
			case "namespace-permission":
				proc.fail = "1/ns/mnt"
			}
			if _, err := readMounts(context.Background(), proc, false); err == nil {
				t.Fatal("accepted incomplete mount snapshot")
			}
		})
	}
}

func TestExitedProcesses(t *testing.T) {
	t.Run("zombie-einval", func(t *testing.T) {
		// Zombies have no mount namespace; the kernel fails mountinfo opens
		// with EINVAL (seen on AKS Ubuntu 24.04 nodes).
		proc := newProc()
		proc.einval = "42/mountinfo"
		proc.files["42/stat"] = &fstest.MapFile{Data: []byte("42 (inotifywait) Z 1 2")}
		if _, err := readMounts(context.Background(), proc, false); err != nil {
			t.Fatal(err)
		}
	})
	for _, zombie := range []bool{false, true} {
		t.Run(fmt.Sprint(zombie), func(t *testing.T) {
			proc := newProc()
			if zombie {
				proc.files["42/mountinfo"].Data = nil
				proc.files["42/stat"] = &fstest.MapFile{Data: []byte("42 (name with ) parenthesis) Z 1 2")}
			} else {
				delete(proc.files, "42/mountinfo")
				proc.files["42"] = &fstest.MapFile{Mode: fs.ModeDir}
			}
			if _, err := readMounts(context.Background(), proc, false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMountinfoParsing(t *testing.T) {
	mounts, err := parseMounts([]byte(`1 0 8:1 /stage\040root/app\134name /app\011jar ro shared:3 - ext4 /dev/root rw` + "\n"))
	if err != nil || mounts[0].root != "/stage root/app\\name" || mounts[0].point != "/app\tjar" {
		t.Fatalf("escaped mount paths: %+v %v", mounts, err)
	}
	// CNI network namespace bind mounts are present on every node running pods.
	mounts, err = parseMounts([]byte("963 1078 0:4 net:[4026532282] /run/netns/cni-1 rw shared:9 - nsfs nsfs rw\n"))
	if err != nil || mounts[0].root != "net:[4026532282]" || mounts[0].point != "/run/netns/cni-1" {
		t.Fatalf("nsfs mount: %+v %v", mounts, err)
	}
	for _, line := range []string{
		"", "broken", "1 0 8:1 / / rw - ext4 source", "x 0 8:1 / / rw - ext4 source rw",
		"1 0 bad / / rw - ext4 source rw", "1 0 8:x / / rw - ext4 source rw",
		"1 0 8:1 relative / rw - ext4 source rw", `1 0 8:1 /bad\777 / rw - ext4 source rw`,
		"1 0 8:1 /bad/../path / rw - ext4 source rw",
		"1 0 8:1 net:[4026532282] / rw - ext4 source rw",
		"1 0 0:4 net:[x] / rw - nsfs nsfs rw", "1 0 0:4 Net:[1] / rw - nsfs nsfs rw",
		"1 0 0:4 relative / rw - nsfs nsfs rw",
	} {
		if _, err := parseMounts([]byte(line)); err == nil {
			t.Errorf("accepted malformed mountinfo %q", line)
		}
	}
}

// Realistic AKS records: the node provisioner bind-mounts containerd's socket,
// containerd then restarts and recreates it, so the stale bind's root is
// reported by the kernel with the "//deleted" suffix.
const deletedSocketMountinfo = "1 0 8:1 / / rw - ext4 /dev/root rw\n" +
	"2 1 0:2 / /proc rw - proc proc rw\n" +
	"3 1 0:25 / /run rw,nosuid,nodev shared:5 - tmpfs tmpfs rw,mode=755\n" +
	"4 1 8:1 /var/lib/brewlet /stage rw shared:1 - ext4 /dev/root rw\n"

const deletedSocketPodMountinfo = "3054 2981 0:25 /containerd/containerd.sock//deleted /run/containerd/containerd.sock rw,nosuid,nodev,noexec - tmpfs tmpfs rw,mode=755\n" +
	"3055 2981 8:1 /var/lib/brewlet/immutable-v2/live/app.jar /app/app.jar ro - ext4 /dev/root rw\n"

func TestDeletedMountRoots(t *testing.T) {
	mounts, err := parseMounts([]byte(deletedSocketPodMountinfo))
	if err != nil {
		t.Fatal(err)
	}
	if !mounts[0].deleted || mounts[0].root != "/containerd/containerd.sock" || mounts[0].point != "/run/containerd/containerd.sock" || mounts[1].deleted {
		t.Fatalf("deleted root: %+v", mounts)
	}
	// A real path ending in "/deleted" is canonical and not special.
	mounts, err = parseMounts([]byte("1 0 8:1 /var/deleted /deleted rw - ext4 /dev/root rw\n"))
	if err != nil || mounts[0].deleted || mounts[0].root != "/var/deleted" {
		t.Fatalf("literal deleted component: %+v %v", mounts, err)
	}
	mounts, err = parseMounts([]byte(`1 0 8:1 /a\040b/deleted//deleted /x rw - ext4 /dev/root rw` + "\n"))
	if err != nil || !mounts[0].deleted || mounts[0].root != "/a b/deleted" {
		t.Fatalf("escaped deleted root: %+v %v", mounts, err)
	}

	t.Run("gc proceeds with protections intact", func(t *testing.T) {
		proc := newProc()
		proc.files["self/mountinfo"].Data = []byte(deletedSocketMountinfo)
		proc.files["1/mountinfo"].Data = []byte(deletedSocketMountinfo)
		proc.files["42/mountinfo"].Data = []byte(deletedSocketPodMountinfo)
		snapshot, err := readMounts(context.Background(), proc, false)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := snapshot.protects("/stage/immutable-v2/live", "8:1"); err != nil || !got {
			t.Fatalf("live stage not protected: %t %v", got, err)
		}
		if got, err := snapshot.protects("/stage/immutable-v2/stale", "8:1"); err != nil || got {
			t.Fatalf("stale stage not collectable: %t %v", got, err)
		}
	})

	t.Run("deleted root still protects its former source", func(t *testing.T) {
		base := mount{device: "8:1", root: "/var/lib/brewlet", point: "/stage"}
		gone := mount{device: "8:1", root: "/var/lib/brewlet/immutable-v2/hash/app", point: "/app", deleted: true}
		snapshot := mountSnapshot{current: []mount{base}, all: []mount{base, gone}}
		if got, err := snapshot.protects("/stage/immutable-v2/hash", "8:1"); err != nil || !got {
			t.Fatalf("deleted bind source not conservatively protected: %t %v", got, err)
		}
	})

	t.Run("deleted containing mount refuses", func(t *testing.T) {
		host := "1 0 8:1 / / rw - ext4 /dev/root rw\n2 1 0:2 / /proc rw - proc proc rw\n" +
			"4 1 8:1 /var/lib/brewlet//deleted /stage rw - ext4 /dev/root rw\n"
		proc := newProc()
		proc.files["self/mountinfo"].Data = []byte(host)
		proc.files["1/mountinfo"].Data = []byte(host)
		snapshot, err := readMounts(context.Background(), proc, false)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := snapshot.protects("/stage/immutable-v2/stale", "8:1"); err == nil {
			t.Fatalf("authorized deletion through deleted mount root: %t", got)
		}
	})

	t.Run("deleted proc root refuses", func(t *testing.T) {
		proc := newProc()
		host := strings.Replace(hostMountinfo, "0:2 / /proc", "0:2 /x//deleted /proc", 1)
		proc.files["self/mountinfo"].Data = []byte(host)
		if _, err := readMounts(context.Background(), proc, false); err == nil {
			t.Fatal("accepted deleted proc root")
		}
	})

	for _, line := range []string{
		"1 0 8:1 //deleted / rw - ext4 source rw",
		"1 0 8:1 ///deleted / rw - ext4 source rw",
		"1 0 8:1 /a/deleted/ / rw - ext4 source rw",
		"1 0 8:1 /a//b / rw - ext4 source rw",
		"1 0 8:1 /a/../b//deleted / rw - ext4 source rw",
		"1 0 8:1 /a//deleted//deleted / rw - ext4 source rw",
		"1 0 8:1 relative//deleted / rw - ext4 source rw",
		"1 0 8:1 /a/deleted /a//deleted rw - ext4 source rw",
		"1 0 8:1 /a//deleted/b / rw - ext4 source rw",
		"1 0 8:1 /a/ /x rw - ext4 source rw",
	} {
		if _, err := parseMounts([]byte(line)); err == nil {
			t.Errorf("accepted malformed mountinfo %q", line)
		}
	}
}

func TestNestedPIDNamespaceOptIn(t *testing.T) {
	nested := func() fakeProc {
		proc := newProc()
		proc.links["self/ns/pid"] = "pid:[4026533705]"
		proc.links["1/ns/pid"] = "pid:[4026533705]"
		return proc
	}
	if _, err := readMounts(context.Background(), nested(), false); err == nil {
		t.Fatal("accepted nested PID namespace without opt-in")
	}
	if _, err := readMounts(context.Background(), nested(), true); err != nil {
		t.Fatalf("rejected nested PID namespace with opt-in: %v", err)
	}
	if _, err := readMounts(context.Background(), newProc(), true); err != nil {
		t.Fatalf("opt-in rejected the initial PID namespace: %v", err)
	}
	for _, failure := range []string{"self-differs", "userns", "unreadable"} {
		t.Run(failure, func(t *testing.T) {
			proc := nested()
			switch failure {
			case "self-differs":
				proc.links["self/ns/pid"] = "pid:[4026533706]"
			case "userns":
				proc.links["self/ns/user"] = "user:[999]"
			case "unreadable":
				proc.fail = "1/ns/pid"
			}
			if _, err := readMounts(context.Background(), proc, true); err == nil {
				t.Fatal("accepted mismatched namespaces with opt-in")
			}
		})
	}
}
