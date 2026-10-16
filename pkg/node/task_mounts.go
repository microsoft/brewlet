package node

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// TaskMounts records protected containerd mount provenance before a lease can
// outlive Create. Shared runtime/cache lowers cannot identify a particular task.
type TaskMounts struct {
	Bundle string `json:"bundle"`
	Upper  string `json:"upper"`
	Work   string `json:"work"`
}

func (m TaskMounts) Validate(namespace, task string) error {
	if namespace == "" || namespace == "." || namespace == ".." || strings.ContainsAny(namespace, "/\\\x00 \t\n,:") || !uidPattern.MatchString(task) ||
		m.Bundle != filepath.Join("/run/containerd/io.containerd.runtime.v2.task", namespace, task) {
		return fmt.Errorf("canonical task-bound containerd bundle required")
	}
	base := filepath.Dir(m.Upper)
	id := filepath.Base(base)
	number, err := strconv.ParseUint(id, 10, 64)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != id ||
		base != filepath.Join("/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots", id) ||
		m.Upper != filepath.Join(base, "fs") || m.Work != filepath.Join(base, "work") {
		return fmt.Errorf("canonical managed containerd snapshot upper/work required")
	}
	return nil
}

func leaseMountPaths(lease Lease, runtimeRoot string) ([]string, error) {
	if lease.TaskMounts != nil {
		if err := lease.TaskMounts.Validate(lease.Namespace, lease.TaskID); err != nil {
			return nil, err
		}
		return []string{lease.TaskMounts.Bundle, lease.TaskMounts.Upper, lease.TaskMounts.Work}, nil
	}
	// Older leases have no per-task mount provenance. Keep their conservative
	// generation-wide protection rather than inventing a missing scope.
	if err := lease.Metadata.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(runtimeRoot) || filepath.Clean(runtimeRoot) != runtimeRoot {
		return nil, fmt.Errorf("canonical runtime evidence root required")
	}
	return []string{filepath.Join(runtimeRoot, lease.Metadata.ID, lease.Metadata.InventoryUID, strconv.FormatInt(lease.Metadata.Generation, 10))}, nil
}
