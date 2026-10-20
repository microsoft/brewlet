package node

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// InRoot resolves container-absolute symlinks against root, never the host /.
func InRoot(root, name string) (string, error) {
	return resolveInRoot(root, name, false, false)
}

func resolveInRoot(root, name string, allowMissing, excludeApp bool) (string, error) {
	if !path.IsAbs(name) {
		return "", fmt.Errorf("container path must be absolute")
	}
	pending := strings.Split(path.Clean(name), "/")[1:]
	var parts []string
	links := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(parts) == 0 {
				return "", fmt.Errorf("root escape")
			}
			parts = parts[:len(parts)-1]
			continue
		}
		if excludeApp && len(parts) == 0 && component == "app" {
			return "", fmt.Errorf("runtime PATH traverses /app")
		}
		candidate := filepath.Join(append([]string{root}, append(parts, component)...)...)
		st, err := os.Lstat(candidate)
		if allowMissing && os.IsNotExist(err) {
			parts = append(parts, component)
			continue
		}
		if err != nil {
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", fmt.Errorf("runtime symlink loop")
			}
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", err
			}
			if path.IsAbs(target) {
				parts = nil
			}
			pending = append(strings.Split(target, "/"), pending...)
		} else {
			parts = append(parts, component)
		}
	}
	return filepath.Join(append([]string{root}, parts...)...), nil
}

func ValidateRoot(root string, m *Metadata) error {
	if err := m.Validate(); err != nil {
		return err
	}
	st, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime root must be a directory")
	}
	if err := validateRuntimeMountPaths(root, *m); err != nil {
		return err
	}
	env, err := runtimeEnvironment(m.Env)
	if err != nil {
		return err
	}
	var paths []string
	if value, present := env["PATH"]; present {
		paths = strings.Split(value, ":")
	}
	for _, p := range paths {
		resolved, err := resolveInRoot(root, p, true, true)
		if err != nil {
			return fmt.Errorf("runtime PATH: %w", err)
		}
		st, err := os.Stat(resolved)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !st.IsDir() || st.Mode().Perm()&0005 != 0005 {
			return fmt.Errorf("runtime PATH must be searchable by arbitrary users: %s", p)
		}
		rel, err := filepath.Rel(root, resolved)
		if err != nil {
			return err
		}
		if rel == "app" || strings.HasPrefix(rel, "app/") {
			return fmt.Errorf("runtime PATH resolves into /app")
		}
	}
	passwd, err := optionalAccountFile(root, "/etc/passwd")
	if err != nil {
		return err
	}
	m.Homes = map[uint32]string{}
	names := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(passwd)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 7 || fields[0] == "" || names[fields[0]] {
			return fmt.Errorf("malformed passwd entry")
		}
		names[fields[0]] = true
		uid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return err
		}
		_, err = strconv.ParseUint(fields[3], 10, 32)
		if err != nil {
			return err
		}
		if !path.IsAbs(fields[5]) || path.Clean(fields[5]) != fields[5] || strings.ContainsRune(fields[5], 0) {
			return fmt.Errorf("invalid passwd home")
		}
		if _, duplicate := m.Homes[uint32(uid)]; duplicate {
			return fmt.Errorf("ambiguous passwd UID")
		}
		m.Homes[uint32(uid)] = fields[5]
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	m.Home = m.Homes[m.UID]
	if m.Home == "" {
		m.Home = "/tmp"
	}
	group, err := optionalAccountFile(root, "/etc/group")
	if err != nil {
		return err
	}
	names = map[string]bool{}
	for _, line := range strings.Split(string(group), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 4 || fields[0] == "" || names[fields[0]] {
			return fmt.Errorf("malformed group entry")
		}
		names[fields[0]] = true
		_, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return err
		}
	}
	if err := filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		st, err := entry.Info()
		if err != nil {
			return err
		}
		if st.Mode().IsRegular() && st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			return fmt.Errorf("runtime setuid/setgid file: %s", p)
		}
		if st.Mode()&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket) != 0 {
			return fmt.Errorf("runtime special file: %s", p)
		}
		return checkCapabilities(p)
	}); err != nil {
		return err
	}
	m.ValidatedAt = time.Now().UTC()
	return nil
}

func optionalAccountFile(root, name string) ([]byte, error) {
	resolved, err := InRoot(root, name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return os.ReadFile(resolved)
}
