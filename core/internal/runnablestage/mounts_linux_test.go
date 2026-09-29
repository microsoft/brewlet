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
	"testing"
	"testing/fstest"
)

type fakeProc struct {
	files fstest.MapFS
	links map[string]string
	fail  string
}

func (p fakeProc) ReadFile(name string) ([]byte, error) {
	if name == p.fail {
		return nil, os.ErrPermission
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
			"42/mountinfo":   {Data: []byte("3 0 8:1 /stage/immutable-v2/hash/app/app.jar /app/app.jar ro - ext4 /dev/root rw\n")},
		},
		links: map[string]string{
			"self/ns/pid": "pid:[4026531836]", "1/ns/pid": "pid:[4026531836]",
			"self/ns/user": "user:[4026531837]", "self/ns/mnt": "mnt:[1]", "1/ns/mnt": "mnt:[1]",
		},
	}
}

func TestMountsAcrossProcesses(t *testing.T) {
	proc := newProc()
	snapshot, err := readMounts(context.Background(), proc)
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
	for _, failure := range []string{"hidepid", "pidns", "userns", "mountns", "missing-proc", "permission", "parse", "empty-live", "missing-live", "enumeration", "namespace-permission"} {
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
			case "enumeration":
				proc.fail = "."
			case "namespace-permission":
				proc.fail = "1/ns/mnt"
			}
			if _, err := readMounts(context.Background(), proc); err == nil {
				t.Fatal("accepted incomplete mount snapshot")
			}
		})
	}
}

func TestExitedProcesses(t *testing.T) {
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
			if _, err := readMounts(context.Background(), proc); err != nil {
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
	for _, line := range []string{
		"", "broken", "1 0 8:1 / / rw - ext4 source", "x 0 8:1 / / rw - ext4 source rw",
		"1 0 bad / / rw - ext4 source rw", "1 0 8:x / / rw - ext4 source rw",
		"1 0 8:1 relative / rw - ext4 source rw", `1 0 8:1 /bad\777 / rw - ext4 source rw`,
		"1 0 8:1 /bad/../path / rw - ext4 source rw",
	} {
		if _, err := parseMounts([]byte(line)); err == nil {
			t.Errorf("accepted malformed mountinfo %q", line)
		}
	}
}
