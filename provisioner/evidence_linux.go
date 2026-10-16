//go:build linux

package provisioner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/client"
	"github.com/microsoft/brewlet/pkg/node"
)

// HostEvidence executes the installed maintenance entrypoint in the initial
// host mount namespace. It does not weaken the collector's namespace checks to
// make a containerized provisioner appear to have complete host evidence.
type HostEvidence struct {
	HostRoot string
	Client   *client.Client
}

func (e HostEvidence) query(ctx context.Context, request node.EvidenceRequest) (bool, error) {
	if err := trustedFile(e.HostRoot + "/usr/local/bin/containerd-shim-brewlet-v2"); err != nil {
		return false, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nsenter", "--mount="+e.HostRoot+"/proc/1/ns/mnt", "--root="+e.HostRoot+"/proc/1/root", "--wd=/", "--", "/usr/local/bin/containerd-shim-brewlet-v2", "--brewlet-evidence")
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
