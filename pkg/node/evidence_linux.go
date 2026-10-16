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
	for _, namespace := range all {
		nsctx := namespaces.WithNamespace(ctx, namespace)
		containers, err := e.Client.Containers(nsctx)
		if err != nil {
			return false, err
		}
		for _, c := range containers {
			if l.TaskID == "" {
				continue
			}
			if l.TaskID != "" && (namespace != l.Namespace || c.ID() != l.TaskID) {
				continue
			}
			task, err := c.Task(nsctx, nil)
			if errdefs.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			pids, err := task.Pids(nsctx)
			if err != nil {
				return false, err
			}
			if len(pids) > 0 {
				return false, nil
			}
			// Even a stopped task retains rootfs/mount obligations until Delete.
			return false, nil
		}
	}
	root := filepath.Join(e.RuntimeRoot, l.Metadata.ID, l.Metadata.InventoryUID, strconv.FormatInt(l.Metadata.Generation, 10))
	if !filepath.IsAbs(root) || l.Metadata.ID == "" {
		return false, fmt.Errorf("invalid runtime evidence path")
	}
	return e.MountsGone(ctx, root)
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
