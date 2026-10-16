package node

import (
	"fmt"
	"path"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func runtimeEnvironment(env []string) (map[string]string, error) {
	values := make(map[string]string, len(env))
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" || strings.ContainsAny(k+v, "\x00") {
			return nil, fmt.Errorf("invalid runtime environment")
		}
		if _, exists := values[k]; exists {
			return nil, fmt.Errorf("duplicate runtime environment %s", k)
		}
		values[k] = v
	}
	p, exists := values["PATH"]
	if !exists || p == "" {
		return nil, fmt.Errorf("runtime must supply PATH")
	}
	for _, dir := range strings.Split(p, ":") {
		if !path.IsAbs(dir) || path.Clean(dir) != dir || dir == "/app" || strings.HasPrefix(dir, "/app/") {
			return nil, fmt.Errorf("unsafe runtime PATH component %q", dir)
		}
	}
	return values, nil
}

func CheckProcess(p *specs.Process) error {
	if p == nil {
		return fmt.Errorf("missing OCI process")
	}
	if p.User.UID == 0 || p.User.GID == 0 || p.User.UID == ^uint32(0) || p.User.GID == ^uint32(0) {
		return failure("RootNotSupported", "UID and primary GID must be nonzero")
	}
	for _, gid := range p.User.AdditionalGids {
		if gid == 0 || gid == ^uint32(0) {
			return failure("RootNotSupported", "supplementary GID must be nonzero")
		}
	}
	if p.Capabilities != nil && (len(p.Capabilities.Bounding)+len(p.Capabilities.Effective)+len(p.Capabilities.Inheritable)+len(p.Capabilities.Permitted)+len(p.Capabilities.Ambient) != 0) {
		return failure("PrivilegeEscalationNotSupported", "capabilities must be empty")
	}
	if !p.NoNewPrivileges {
		return failure("PrivilegeEscalationNotSupported", "noNewPrivileges is required")
	}
	if len(p.Args) == 0 || !path.IsAbs(p.Cwd) {
		return fmt.Errorf("missing argv or absolute cwd")
	}
	return nil
}

// MergeProcess preserves CRI argv/cwd and applies only the runtime environment
// precedence, synthesized HOME, and mandatory non-root restrictions.
func MergeProcess(p *specs.Process, m Metadata) error {
	if err := CheckProcess(p); err != nil {
		return err
	}
	base, err := runtimeEnvironment(m.Env)
	if err != nil {
		return err
	}
	var order []string
	seen := map[string]bool{}
	for _, entry := range m.Env {
		k, _, _ := strings.Cut(entry, "=")
		order = append(order, k)
		seen[k] = true
	}
	explicitHome := false
	for _, entry := range p.Env {
		k, v, ok := strings.Cut(entry, "=")
		if !ok || k == "" || strings.ContainsAny(k+v, "\x00") {
			return fmt.Errorf("invalid CRI environment")
		}
		if k == "PATH" {
			continue
		}
		if k == "HOME" {
			explicitHome = true
		}
		if !seen[k] {
			order = append(order, k)
			seen[k] = true
		}
		base[k] = v
	}
	if !explicitHome {
		home := m.Homes[p.User.UID]
		if home == "" {
			home = "/tmp"
		}
		base["HOME"] = home
		if !seen["HOME"] {
			order = append(order, "HOME")
		}
	}
	p.Env = make([]string, 0, len(order))
	for _, k := range order {
		p.Env = append(p.Env, k+"="+base[k])
	}
	p.NoNewPrivileges = true
	p.Capabilities = &specs.LinuxCapabilities{}
	return nil
}

// RewriteExec deliberately ignores CRI's independently synthesized environment
// and credentials; the durable initial state is the execution authority.
func RewriteExec(p *specs.Process, state InitialState) error {
	if p == nil {
		return failure("ExecStateUnavailable", "missing exec process")
	}
	if p.User.UID == 0 || p.User.GID == 0 || p.User.UID == ^uint32(0) || p.User.GID == ^uint32(0) {
		return failure("RootNotSupported", "exec requested root identity")
	}
	for _, gid := range p.User.AdditionalGids {
		if gid == 0 || gid == ^uint32(0) {
			return failure("RootNotSupported", "exec requested root group")
		}
	}
	if p.Capabilities != nil && len(p.Capabilities.Bounding)+len(p.Capabilities.Effective)+len(p.Capabilities.Inheritable)+len(p.Capabilities.Permitted)+len(p.Capabilities.Ambient) != 0 {
		return failure("PrivilegeEscalationNotSupported", "exec requested capabilities")
	}
	p.User = state.Process.User
	p.Env = append([]string(nil), state.Process.Env...)
	p.NoNewPrivileges = true
	p.Capabilities = &specs.LinuxCapabilities{}
	return CheckProcess(p)
}
