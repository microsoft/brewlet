//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/brewlet/pkg/node"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

type hostMount struct{ device, root, target string }

func overlapsPath(a, b string) bool {
	return a == b || a == "/" || b == "/" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func mountReferent(path string, mounts []hostMount) (string, string) {
	var selected *hostMount
	for i := range mounts {
		m := &mounts[i]
		if path != m.target && m.target != "/" && !strings.HasPrefix(path, m.target+"/") {
			continue
		}
		if selected == nil || len(m.target) > len(selected.target) {
			selected = m
		}
	}
	if selected == nil {
		return "", ""
	}
	relative, _ := filepath.Rel(selected.target, path)
	return selected.device, filepath.Join(selected.root, relative)
}
func validateHostMounts(mounts []specs.Mount) error {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer f.Close()
	var table []hostMount
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 {
			return fmt.Errorf("incomplete host mount evidence")
		}
		table = append(table, hostMount{fields[2], unescape.Replace(fields[3]), unescape.Replace(fields[4])})
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	for _, m := range mounts {
		if !filepath.IsAbs(m.Source) {
			continue
		}
		source, err := filepath.EvalSymlinks(m.Source)
		if err != nil {
			return err
		}
		device, root := mountReferent(source, table)
		if device == "" {
			return fmt.Errorf("host mount source evidence unavailable")
		}
		for _, protected := range []string{inventoryRoot, cacheRoot, "/usr/local/bin/containerd-shim-brewlet-v2"} {
			otherDevice, otherRoot := mountReferent(protected, table)
			if overlapsPath(source, protected) || (device == otherDevice && overlapsPath(root, otherRoot)) {
				return &node.Error{Reason: "PrivilegeEscalationNotSupported", Detail: "node-owned runtime, cache, lease and request state cannot be mounted into a workload"}
			}
		}
	}
	return nil
}
