//go:build linux

package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	tasktypes "github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"google.golang.org/grpc"
)

type evidenceNamespaces struct{ namespaces.Store }

func TestMountLocationsMapsHostPathsToFilesystemRoots(t *testing.T) {
	for _, tc := range []struct {
		name, table, root string
		want              []filesystemMount
	}{
		{"root filesystem", "1 0 0:1 / / rw - ext4 disk rw\n", "/var/lib/cache/item", []filesystemMount{{device: "0:1", root: "/var/lib/cache/item", point: "/"}}},
		{"separate volume", "1 0 0:1 / / rw - ext4 disk rw\n2 1 0:2 /pool /var/lib/cache rw - ext4 volume rw\n", "/var/lib/cache/item", []filesystemMount{{device: "0:2", root: "/pool/item", point: "/var/lib/cache"}}},
		{"escaped paths", "1 0 0:1 / / rw - ext4 disk rw\n2 1 0:2 /pool\\040data /cache\\040data rw - tmpfs tmpfs rw\n", "/cache data/item", []filesystemMount{{device: "0:2", root: "/pool data/item", point: "/cache data"}}},
		{"prefix boundary", "1 0 0:1 / / rw - ext4 disk rw\n2 1 0:2 /pool /cache-other rw - tmpfs tmpfs rw\n", "/cache/item", []filesystemMount{{device: "0:1", root: "/cache/item", point: "/"}}},
		{"stacked mounts", "1 0 0:1 / / rw - ext4 disk rw\n2 1 0:2 /old /cache rw - tmpfs tmpfs rw\n3 1 0:3 /new /cache rw - tmpfs tmpfs rw\n", "/cache/item", []filesystemMount{{device: "0:2", root: "/old/item", point: "/cache"}, {device: "0:3", root: "/new/item", point: "/cache"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := filepath.Join(testDirectory(t), "mountinfo")
			if err := os.WriteFile(name, []byte(tc.table), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := mountLocations(context.Background(), name, tc.root)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("filesystem source mapping changed: got=%+v want=%+v error=%v", got, tc.want, err)
			}
		})
	}
}

func TestMountLocationsRejectsIncompleteFilesystemEvidence(t *testing.T) {
	for _, table := range []string{
		"",
		"malformed\n",
		"1 0 0:1 / /elsewhere rw - ext4 disk rw\n",
		"1 0 invalid / / rw - ext4 disk rw\n",
		"1 0 0:invalid / / rw - ext4 disk rw\n",
		"1 0 0:1 relative / rw - ext4 disk rw\n",
		"1 0 0:1 / relative rw - ext4 disk rw\n",
		"1 0 0:1 / / rw ext4 disk rw\n",
	} {
		name := filepath.Join(testDirectory(t), "mountinfo")
		if err := os.WriteFile(name, []byte(table), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := mountLocations(context.Background(), name, "/cache/item"); err == nil {
			t.Fatalf("incomplete source mapping accepted: %q", table)
		}
	}
	if _, err := mountLocations(context.Background(), filepath.Join(testDirectory(t), "missing"), "/cache/item"); err == nil {
		t.Fatal("missing host mount table accepted")
	}
}

func (evidenceNamespaces) List(context.Context) ([]string, error) { return []string{"k8s.io"}, nil }

type evidenceTasks struct {
	tasksapi.TasksClient
	processes []*tasktypes.Process
	err       error
}

func (e *evidenceTasks) List(context.Context, *tasksapi.ListTasksRequest, ...grpc.CallOption) (*tasksapi.ListTasksResponse, error) {
	return &tasksapi.ListTasksResponse{Tasks: e.processes}, e.err
}

func TestLinuxEvidenceReapsFailedCreateWithoutConfusingSharedRuntimeMounts(t *testing.T) {
	directory := testDirectory(t)
	proc := filepath.Join(directory, "proc")
	for _, path := range []string{"self/ns", "1/ns", "77"} {
		if err := os.MkdirAll(filepath.Join(proc, path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, process := range []string{"self", "1"} {
		for _, namespace := range []string{"pid", "user", "mnt"} {
			if err := os.Symlink(namespace+":[initial]", filepath.Join(proc, process, "ns", namespace)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(proc, "self/mountinfo"), []byte("9 0 0:1 / / rw - overlay overlay rw\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tasks := &evidenceTasks{}
	c, err := client.New("", client.WithServices(client.WithNamespaceService(evidenceNamespaces{}), client.WithTaskClient(tasks)))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	meta := testMetadata()
	meta.Platform.OS, meta.Platform.Architecture = "linux", "amd64"
	scope := testTaskMounts("failed-create")
	lease := Lease{TaskID: "failed-create", Namespace: "k8s.io", Metadata: meta, TaskMounts: &scope}
	evidence := LinuxEvidence{Client: c, Proc: proc, RuntimeRoot: filepath.Join(directory, "runtimes")}
	runtime := filepath.Join(evidence.RuntimeRoot, meta.ID, meta.InventoryUID, "1", "rootfs")
	mountFile := filepath.Join(proc, "77/mountinfo")
	mount := "1 0 0:1 / / rw - overlay overlay rw,lowerdir=" + runtime + ",upperdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/43/fs,workdir=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/43/work\n"
	writeMount := func(line string) {
		t.Helper()
		if err := os.WriteFile(mountFile, []byte(line), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeMount(mount)
	ctx := context.Background()
	if gone, err := evidence.Gone(ctx, lease); err != nil || !gone {
		t.Fatal("healthy sibling's shared lower pinned failed Create", gone, err)
	}
	legacy := lease
	legacy.TaskMounts = nil
	if gone, err := evidence.Gone(ctx, legacy); err != nil || gone {
		t.Fatal("legacy lease without proven scope lost generation-wide protection", gone, err)
	}
	for _, process := range []*tasktypes.Process{
		{ID: lease.TaskID, ContainerID: lease.TaskID, Pid: 123, Status: tasktypes.Status_RUNNING},
		{ID: lease.TaskID, ContainerID: lease.TaskID, Status: tasktypes.Status_STOPPED},
	} {
		tasks.processes = []*tasktypes.Process{process}
		if gone, err := evidence.Gone(ctx, lease); err != nil || gone {
			t.Fatal("existing running/stopped/orphan-capable task was treated as absent", gone, err)
		}
	}
	tasks.processes = nil
	writeMount(strings.ReplaceAll(mount, "/43/", "/42/"))
	if gone, err := evidence.Gone(ctx, lease); err != nil || gone {
		t.Fatal("residual per-task overlay mount ignored", gone, err)
	}
	writeMount(mount + "2 1 0:2 / " + scope.Bundle + "/rootfs rw - tmpfs tmpfs rw\n")
	if gone, err := evidence.Gone(ctx, lease); err != nil || gone {
		t.Fatal("residual bundle mount ignored", gone, err)
	}
	writeMount(mount)
	tasks.err = errors.New("task listing unavailable")
	if gone, err := evidence.Gone(ctx, lease); err == nil || gone {
		t.Fatal("incomplete task evidence accepted", gone, err)
	}
	tasks.err = nil
	writeMount("malformed mount evidence\n")
	if gone, err := evidence.Gone(ctx, lease); err == nil || gone {
		t.Fatal("incomplete mount evidence accepted", gone, err)
	}
	// Reproduce the diagnosed post-lease process rejection, with a genuine
	// durable lease and no initial state, while a sibling uses the same root.
	store := testStore(t)
	if err := store.WriteAuthorization(ctx, testAuthorization()); err != nil {
		t.Fatal(err)
	}
	meta = fixtureGeneration(t, store, 1)
	if err := os.Symlink(filepath.Join(meta.InventoryUID, "1"), filepath.Join(store.Root, meta.ID, "current")); err != nil {
		t.Fatal(err)
	}
	failed, _, err := store.ResolveTask(ctx, meta.ID, "k8s.io", "failed-create", nil, time.Second, scope)
	if err != nil {
		t.Fatal(err)
	}
	process := testProcess()
	process.NoNewPrivileges = false
	if err := MergeProcess(&process, meta); reason(err) != "PrivilegeEscalationNotSupported" {
		t.Fatal("did not reproduce diagnosed post-lease rejection", err)
	}
	if _, err := store.LoadInitial(failed.TaskID); reason(err) != "ExecStateUnavailable" {
		t.Fatal("failed Create unexpectedly has initial state", err)
	}
	siblingScope := testTaskMounts("healthy-sibling")
	siblingScope.Upper = strings.ReplaceAll(siblingScope.Upper, "/42/", "/43/")
	siblingScope.Work = strings.ReplaceAll(siblingScope.Work, "/42/", "/43/")
	sibling, _, err := store.ResolveTask(ctx, meta.ID, "k8s.io", "healthy-sibling", nil, time.Second, siblingScope)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveInitial(InitialState{Lease: sibling, Process: testProcess()}); err != nil {
		t.Fatal(err)
	}
	tasks.processes = []*tasktypes.Process{{ID: sibling.TaskID, ContainerID: sibling.TaskID, Pid: 123, Status: tasktypes.Status_RUNNING}}
	evidence.RuntimeRoot = store.Root
	writeMount(strings.ReplaceAll(mount, runtime, filepath.Join(store.Root, meta.ID, meta.InventoryUID, "1", "rootfs")))
	if err := store.Recover(ctx, evidence); err != nil {
		t.Fatal(err)
	}
	retained, err := store.Leases()
	if err != nil || len(retained) != 1 || retained[0].TaskID != sibling.TaskID {
		t.Fatal("failed Create not recovered independently of healthy sibling", retained, err)
	}
	if _, err := store.LoadInitial(sibling.TaskID); err != nil {
		t.Fatal("healthy sibling process state changed", err)
	}
}
