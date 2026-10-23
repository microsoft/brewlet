//go:build linux

package main

import "testing"

func TestHostMountAliasesCannotExposePrivateRuntimeOrCacheState(t *testing.T) {
	table := []hostMount{
		{"0:1", "/", "/"},
		{"0:1", "/var/lib/brewlet/layers/v1-3-digest/rootfs/app/data", "/var/lib/kubelet/pods/p/volume-subpaths/host/c/0"},
	}
	device, root := mountReferent("/var/lib/kubelet/pods/p/volume-subpaths/host/c/0", table)
	otherDevice, otherRoot := mountReferent("/var/lib/brewlet/layers", table)
	if device != otherDevice || !overlapsPath(root, otherRoot) {
		t.Fatal("bind/subPath alias hid managed cache source", device, root)
	}
	for _, source := range []string{"/opt/brewlet/runtimes/.requests", "/opt/brewlet/runtimes/id/current/rootfs/home/app", "/opt/brewlet", "/"} {
		if !overlapsPath(source, inventoryRoot) {
			t.Fatal("private root overlap missed", source)
		}
	}
	if overlapsPath("/var/lib/kubelet/pods/p/volumes/data", cacheRoot) {
		t.Fatal("ordinary explicit volume forbidden")
	}
}
