//go:build linux

package node

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tasksapi "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
)

// LinuxEvidence requires a complete host procfs. Containerized hosts are not
// silently treated as initial host PID/user namespaces.
type LinuxEvidence struct {
	Client      *client.Client
	Proc        string
	RuntimeRoot string
}

func (e LinuxEvidence) Gone(ctx context.Context, l Lease) (bool, error) {
	if e.Client == nil {
		return false, fmt.Errorf("containerd evidence unavailable")
	}
	proc := e.Proc
	if proc == "" {
		proc = "/proc"
	}
	for _, ns := range []string{"pid", "user", "mnt"} {
		self, err := os.Readlink(filepath.Join(proc, "self/ns", ns))
		if err != nil {
			return false, err
		}
		init, err := os.Readlink(filepath.Join(proc, "1/ns", ns))
		if err != nil {
			return false, err
		}
		if self != init {
			return false, fmt.Errorf("complete initial host %s namespace evidence required", ns)
		}
	}
	all, err := e.Client.NamespaceService().List(ctx)
	if err != nil {
		return false, err
	}
	if l.TaskID != "" && l.Namespace == "" {
		return false, fmt.Errorf("task namespace evidence unavailable")
	}
	if l.TaskID != "" {
		all = append(all, l.Namespace)
	}
	seen := map[string]bool{}
	for _, namespace := range all {
		if l.TaskID == "" || seen[namespace] {
			continue
		}
		seen[namespace] = true
		nsctx := namespaces.WithNamespace(ctx, namespace)
		tasks, err := e.Client.TaskService().List(nsctx, &tasksapi.ListTasksRequest{})
		if err != nil {
			return false, err
		}
		if tasks == nil {
			return false, fmt.Errorf("incomplete task listing")
		}
		for _, task := range tasks.Tasks {
			if task == nil || (task.ID == "" && task.ContainerID == "") {
				return false, fmt.Errorf("incomplete task listing")
			}
			if namespace != l.Namespace || (task.ID != l.TaskID && task.ContainerID != l.TaskID) {
				continue
			}
			// Query actual tasks, not container metadata: a stopped main or
			// orphan Exec still owns obligations until the task is deleted.
			return false, nil
		}
	}
	paths, err := leaseMountPaths(l, e.RuntimeRoot)
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		gone, err := e.MountsGone(ctx, path)
		if err != nil || !gone {
			return false, err
		}
	}
	return true, nil
}

func (e LinuxEvidence) MountsGone(ctx context.Context, root string) (bool, error) {
	proc := e.Proc
	if proc == "" {
		proc = "/proc"
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return false, fmt.Errorf("canonical host evidence path required")
	}
	for _, ns := range []string{"pid", "user", "mnt"} {
		self, err := os.Readlink(filepath.Join(proc, "self/ns", ns))
		if err != nil {
			return false, err
		}
		init, err := os.Readlink(filepath.Join(proc, "1/ns", ns))
		if err != nil {
			return false, err
		}
		if self != init {
			return false, fmt.Errorf("complete initial host %s namespace evidence required", ns)
		}
	}
	processes, err := os.ReadDir(proc)
	if err != nil {
		return false, err
	}
	for _, p := range processes {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if _, err := strconv.ParseUint(p.Name(), 10, 64); err != nil {
			continue
		}
		mounts := filepath.Join(proc, p.Name(), "mountinfo")
		f, err := os.Open(mounts)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			stat, readErr := os.ReadFile(filepath.Join(proc, p.Name(), "stat"))
			if readErr == nil {
				end := strings.LastIndex(string(stat), ") ")
				if end >= 0 && end+2 < len(stat) && (stat[end+2] == 'Z' || stat[end+2] == 'X') {
					continue
				}
			}
			return false, fmt.Errorf("incomplete mount evidence: %w", err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 4096), 4<<20)
		for scanner.Scan() {
			line := scanner.Text()
			fields := strings.Fields(line)
			if len(fields) < 10 {
				f.Close()
				return false, fmt.Errorf("ambiguous mountinfo")
			}
			decoded := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(line)
			if mountReferencesPath(decoded, root) {
				f.Close()
				return false, nil
			}

		}
		err = scanner.Err()
		f.Close()
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func mountReferencesPath(line, root string) bool {
	for start := 0; start < len(line); {
		index := strings.Index(line[start:], root)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(root)
		if end == len(line) || strings.ContainsRune("/,: \t\n", rune(line[end])) {
			return true
		}
		start = end
	}
	return false
}

func (e LinuxEvidence) RuntimeGone(ctx context.Context) (bool, error) {
	if e.Client == nil {
		return false, fmt.Errorf("containerd evidence unavailable")
	}
	all, err := e.Client.NamespaceService().List(ctx)
	if err != nil {
		return false, err
	}
	for _, namespace := range all {
		nsctx := namespaces.WithNamespace(ctx, namespace)
		containers, err := e.Client.Containers(nsctx)
		if err != nil {
			return false, err
		}
		for _, c := range containers {
			info, err := c.Info(nsctx)
			if err != nil {
				return false, err
			}
			if info.Runtime.Name != "io.containerd.brewlet.v2" {
				continue
			}
			if _, err := c.Task(nsctx, nil); errdefs.IsNotFound(err) {
				continue
			} else if err != nil {
				return false, err
			}
			return false, nil
		}
	}
	return true, nil
}
