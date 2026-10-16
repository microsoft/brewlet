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
		candidate := filepath.Join(append([]string{root}, append(parts, component)...)...)
		st, err := os.Lstat(candidate)
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
	app := filepath.Join(root, "app")
	if st, err := os.Lstat(app); err == nil {
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("runtime /app must be an empty directory")
		}
		entries, err := os.ReadDir(app)
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return fmt.Errorf("runtime /app must be empty")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	env, err := runtimeEnvironment(m.Env)
	if err != nil {
		return err
	}
	for _, p := range strings.Split(env["PATH"], ":") {
		resolved, err := InRoot(root, p)
		if err != nil {
			return fmt.Errorf("runtime PATH: %w", err)
		}
		st, err := os.Stat(resolved)
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
	passwd, err := InRoot(root, "/etc/passwd")
	if err != nil {
		return err
	}
	file, err := os.Open(passwd)
	if err != nil {
		return err
	}
	defer file.Close()
	m.Homes = map[uint32]string{}
	found := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 7 {
			return fmt.Errorf("malformed passwd entry")
		}
		uid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return err
		}
		gid, err := strconv.ParseUint(fields[3], 10, 32)
		if err != nil {
			return err
		}
		if !path.IsAbs(fields[5]) || path.Clean(fields[5]) != fields[5] {
			return fmt.Errorf("invalid passwd home")
		}
		if _, duplicate := m.Homes[uint32(uid)]; duplicate {
			return fmt.Errorf("ambiguous passwd UID")
		}
		m.Homes[uint32(uid)] = fields[5]
		if fields[0] == "app" {
			if found || uint32(uid) != m.UID || uint32(gid) != m.GID {
				return fmt.Errorf("app account identity mismatch")
			}
			m.Home = fields[5]
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("missing app account")
	}
	home, err := InRoot(root, m.Home)
	if err != nil {
		return err
	}
	if st, err := os.Stat(home); err != nil || !st.IsDir() {
		return fmt.Errorf("app home directory missing")
	}
	group, err := InRoot(root, "/etc/group")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(group)
	if err != nil {
		return err
	}
	groupFound := false
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) != 4 {
			return fmt.Errorf("malformed group entry")
		}
		gid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return err
		}
		if uint32(gid) == m.GID {
			if groupFound {
				return fmt.Errorf("ambiguous primary group")
			}
			groupFound = true
		}
	}
	if !groupFound {
		return fmt.Errorf("missing app primary group")
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
