//go:build linux

package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/microsoft/brewlet/pkg/node"
	"golang.org/x/sys/unix"
)

// HostEvidence executes the maintenance entrypoint in the initial host mount
// namespace, without weakening the collector's namespace checks.
type HostEvidence struct {
	HostRoot       string
	Client         *client.Client
	FallbackBinary string
}

func (e HostEvidence) query(ctx context.Context, request node.EvidenceRequest) (bool, error) {
	binary := "/usr/local/bin/containerd-shim-brewlet-v2"
	var fallback *os.File
	if err := trustedFile(e.HostRoot + binary); err != nil {
		if !os.IsNotExist(err) || e.FallbackBinary == "" {
			return false, err
		}
		fallback, err = os.OpenFile(e.FallbackBinary, os.O_RDONLY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return false, fmt.Errorf("open bundled evidence collector: %w", err)
		}
		defer fallback.Close()
		info, err := fallback.Stat()
		if err != nil {
			return false, err
		}
		if err := trustedFileInfo(e.FallbackBinary, info); err != nil {
			return false, err
		}
		binary = "/proc/self/fd/3"
	}
	data, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nsenter", "--mount="+e.HostRoot+"/proc/1/ns/mnt", "--root="+e.HostRoot+"/proc/1/root", "--wd=/", "--", binary, "--brewlet-evidence")
	if fallback != nil {
		cmd.ExtraFiles = []*os.File{fallback}
	}
	cmd.Stdin = bytes.NewReader(data)
	result, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("host task/mount inspection: %w", err)
	}
	var response node.EvidenceResponse
	if err := json.Unmarshal(result, &response); err != nil {
		return false, err
	}
	if response.Error != "" {
		return false, fmt.Errorf("host task/mount inspection: %s", response.Error)
	}
	return response.Gone, nil
}
func (e HostEvidence) Gone(ctx context.Context, l node.Lease) (bool, error) {
	return e.query(ctx, node.EvidenceRequest{Lease: l})
}
func (e HostEvidence) RuntimeGone(ctx context.Context) (bool, error) {
	if e.Client != nil {
		return (node.LinuxEvidence{Client: e.Client}).RuntimeGone(ctx)
	}

	return e.query(ctx, node.EvidenceRequest{AllRuntime: true})
}

func (e HostEvidence) MountsGone(ctx context.Context, path string) (bool, error) {
	if strings.HasPrefix(path, e.HostRoot+"/") {
		path = strings.TrimPrefix(path, e.HostRoot)
	}
	return e.query(ctx, node.EvidenceRequest{Path: path})
}
