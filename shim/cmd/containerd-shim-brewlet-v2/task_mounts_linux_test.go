//go:build linux

package main

import (
	"testing"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	apitypes "github.com/containerd/containerd/api/types"
)

func TestFailedCreateMountProvenanceComesOnlyFromProtectedManagedRequest(t *testing.T) {
	r := &taskapi.CreateTaskRequest{
		ID: "task-one", Bundle: "/run/containerd/io.containerd.runtime.v2.task/k8s.io/task-one",
		Rootfs: []*apitypes.Mount{{Type: "overlay", Options: []string{
			"lowerdir=/ordinary/image/lower",
			"upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/42/fs",
			"workdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/42/work",
		}}},
	}
	if _, err := taskMountScope(r, "k8s.io"); err != nil {
		t.Fatal(err)
	}
	r.Rootfs[0].Options = append(r.Rootfs[0].Options, "upperdir=/another/upper")
	if _, err := taskMountScope(r, "k8s.io"); err == nil {
		t.Fatal("ambiguous upperdir accepted")
	}
	r.Rootfs = []*apitypes.Mount{nil}
	if _, err := taskMountScope(r, "k8s.io"); err == nil {
		t.Fatal("missing managed mount accepted")
	}
}
