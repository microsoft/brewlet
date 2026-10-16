package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testTaskMounts(task string) TaskMounts {
	return TaskMounts{
		Bundle: "/run/containerd/io.containerd.runtime.v2.task/k8s.io/" + task,
		Upper:  "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/42/fs",
		Work:   "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/42/work",
	}
}

func TestTaskMountScopeRejectsUnboundOrAmbiguousProvenance(t *testing.T) {
	valid := testTaskMounts("task-one")
	if err := valid.Validate("k8s.io", "task-one"); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*TaskMounts){
		func(m *TaskMounts) { m.Bundle += "/.." },
		func(m *TaskMounts) { m.Bundle = filepath.Dir(m.Bundle) },
		func(m *TaskMounts) { m.Bundle = testTaskMounts("other-task").Bundle },
		func(m *TaskMounts) { m.Upper = "/opt/brewlet/runtimes/shared/rootfs" },
		func(m *TaskMounts) { m.Upper += "/../fs" },
		func(m *TaskMounts) {
			m.Work = "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/43/work"
		},
		func(m *TaskMounts) { m.Work = "" },
	} {
		bad := valid
		mutate(&bad)
		if err := bad.Validate("k8s.io", "task-one"); err == nil {
			t.Fatal("unsafe task scope accepted", bad)
		}
	}
}

func TestPendingCreateLeaseScopeIsAtomicImmutableAndRecoveredOnlyAfterGuard(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	auth := testAuthorization()
	if err := store.WriteAuthorization(ctx, auth); err != nil {
		t.Fatal(err)
	}
	meta := fixtureGeneration(t, store, 1)
	if err := os.Symlink(filepath.Join(meta.InventoryUID, "1"), filepath.Join(store.Root, meta.ID, "current")); err != nil {
		t.Fatal(err)
	}
	scope := testTaskMounts("task-one")
	guard, err := store.AcquireTaskGuard(ctx, "task-one")
	if err != nil {
		t.Fatal(err)
	}
	lease, _, err := store.ResolveTask(ctx, meta.ID, "k8s.io", "task-one", nil, time.Second, scope)
	if err != nil {
		guard.Close()
		t.Fatal(err)
	}
	if lease.TaskMounts == nil || *lease.TaskMounts != scope {
		t.Fatal("lease publication did not bind mount provenance")
	}
	rebound := scope
	rebound.Upper = "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/43/fs"
	rebound.Work = "/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/43/work"
	if _, _, err := store.ResolveTask(ctx, meta.ID, "k8s.io", "task-one", nil, time.Second, rebound); err == nil {
		t.Fatal("durable scope silently replaced")
	}
	// This is exactly a failed pre-initial Create, not a fabricated process state.
	if _, err := store.LoadInitial("task-one"); reason(err) != "ExecStateUnavailable" {
		t.Fatal("unexpected initial state", err)
	}
	bounded, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := store.Recover(bounded, fixedEvidence{gone: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("recovery reaped a Create still holding its process lock", err)
	}
	if leases, err := store.Leases(); err != nil || len(leases) != 1 {
		t.Fatal("active Create lease lost", leases, err)
	}
	guard.Close()
	for _, evidence := range []fixedEvidence{{gone: false}, {err: errors.New("uncertain Exec/mount evidence")}} {
		_ = store.Recover(ctx, evidence)
		if leases, err := store.Leases(); err != nil || len(leases) != 1 {
			t.Fatal("uncertain or live task reaped", leases, err)
		}
	}
	if err := store.Recover(ctx, fixedEvidence{gone: true}); err != nil {
		t.Fatal(err)
	}
	if leases, err := store.Leases(); err != nil || len(leases) != 0 {
		t.Fatal("drained failed Create not recovered", leases, err)
	}
}
