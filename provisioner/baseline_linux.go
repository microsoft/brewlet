//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/pelletier/go-toml/v2"
)

// HostBaseline validates the effective host config rather than assuming that
// editing a drop-in makes containerd consume it. Activation is an explicit
// platform operation; this validator never restarts containerd speculatively.
type HostBaseline struct{ HostRoot string }

func (b HostBaseline) Validate(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "nsenter", "--mount="+b.HostRoot+"/proc/1/ns/mnt", "--root="+b.HostRoot+"/proc/1/root", "--wd=/", "--", "containerd", "config", "dump")
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("containerd effective config validation: %w", err)
	}
	var config map[string]any
	if err := toml.Unmarshal(output, &config); err != nil {
		return err
	}
	found := false
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if runtime, ok := v["brewlet"].(map[string]any); ok && runtime["runtime_type"] == "io.containerd.brewlet.v2" {
				allow, ok := runtime["pod_annotations"].([]any)
				selection, barrier := false, false
				if ok {
					for _, raw := range allow {
						str, _ := raw.(string)
						selection = selection || str == "brewlet.sh/runtime-image" || str == "brewlet.sh/*"
						barrier = barrier || str == "brewlet.sh/rotated-to-generation" || str == "brewlet.sh/*"
					}
				}
				found = selection && barrier
			}
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(config)
	if !found {
		return fmt.Errorf("brewlet handler or required pod_annotations forwarding missing")
	}
	live := exec.CommandContext(ctx, "nsenter", "--mount="+b.HostRoot+"/proc/1/ns/mnt", "--root="+b.HostRoot+"/proc/1/root", "--wd=/", "--", "crictl", "--runtime-endpoint", "unix:///run/containerd/containerd.sock", "info", "-o", "json")
	liveBytes, err := live.Output()
	if err != nil {
		return fmt.Errorf("running CRI handler validation requires host crictl: %w", err)
	}
	if err := validateLiveConfig(liveBytes); err != nil {
		return err
	}
	p := b.HostRoot + "/usr/local/bin/containerd-shim-brewlet-v2"
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("trusted executable host shim missing")
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		return fmt.Errorf("host shim must be root owned")
	}
	if strings.Contains(b.HostRoot, "\x00") {
		return fmt.Errorf("invalid host root")
	}
	return nil
}

func validateLiveConfig(data []byte) error {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(data, &response); err != nil {
		return err
	}
	config := response["config"]
	if config == nil {
		var info map[string]json.RawMessage
		if err := json.Unmarshal(response["info"], &info); err != nil {
			return fmt.Errorf("CRI info configuration unavailable")
		}
		config = info["config"]
	}
	var encoded string
	if json.Unmarshal(config, &encoded) == nil {
		config = []byte(encoded)
	}
	var runtimeConfig struct {
		Containerd struct {
			Runtimes map[string]struct {
				Type        string   `json:"runtimeType"`
				Annotations []string `json:"PodAnnotations"`
			} `json:"runtimes"`
		} `json:"containerd"`
	}
	if err := json.Unmarshal(config, &runtimeConfig); err != nil {
		return fmt.Errorf("invalid live CRI config: %w", err)
	}
	handler, ok := runtimeConfig.Containerd.Runtimes["brewlet"]
	if !ok || handler.Type != "io.containerd.brewlet.v2" {
		return fmt.Errorf("running CRI brewlet handler missing")
	}
	selection, barrier := false, false
	for _, annotation := range handler.Annotations {
		selection = selection || annotation == "brewlet.sh/runtime-image" || annotation == "brewlet.sh/*"
		barrier = barrier || annotation == "brewlet.sh/rotated-to-generation" || annotation == "brewlet.sh/*"
	}
	if !selection || !barrier {
		return fmt.Errorf("running CRI required annotation forwarding missing")
	}
	return nil
}
