// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package runnablestage

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type mount struct {
	device     string
	root       string
	point      string
	filesystem string
	options    string
}

type mountSnapshot struct {
	current []mount
	all     []mount
}

type procReader interface {
	ReadFile(string) ([]byte, error)
	ReadDir(string) ([]fs.DirEntry, error)
	Readlink(string) (string, error)
}

type procFS string

func (p procFS) ReadFile(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(string(p), name))
}
func (p procFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(filepath.Join(string(p), name))
}
func (p procFS) Readlink(name string) (string, error) {
	return os.Readlink(filepath.Join(string(p), name))
}

func hostMounts(ctx context.Context) (mountSnapshot, error) {
	return readMounts(ctx, procFS("/proc"))
}

func readMounts(ctx context.Context, proc procReader) (mountSnapshot, error) {
	// Linux reserves these inode numbers for the initial PID/user namespaces.
	// Merely comparing self with PID 1 would accept a container's private proc.
	for name, expected := range map[string]string{
		"self/ns/pid":  "pid:[4026531836]",
		"1/ns/pid":     "pid:[4026531836]",
		"self/ns/user": "user:[4026531837]",
	} {
		value, err := proc.Readlink(name)
		if err != nil {
			return mountSnapshot{}, fmt.Errorf("verify host namespace %s: %w", name, err)
		}
		if value != expected {
			return mountSnapshot{}, fmt.Errorf("reaper requires the initial host PID and user namespaces")
		}
	}
	selfNS, err := proc.Readlink("self/ns/mnt")
	if err != nil {
		return mountSnapshot{}, err
	}
	initNS, err := proc.Readlink("1/ns/mnt")
	if err != nil {
		return mountSnapshot{}, err
	}
	if selfNS != initNS {
		return mountSnapshot{}, fmt.Errorf("reaper requires the host mount namespace")
	}
	raw, err := proc.ReadFile("self/mountinfo")
	if err != nil {
		return mountSnapshot{}, err
	}
	current, err := parseMounts(raw)
	if err != nil {
		return mountSnapshot{}, err
	}
	procMount, ok := containingMount(current, "/proc")
	if !ok || procMount.filesystem != "proc" || procMount.point != "/proc" || procMount.root != "/" {
		return mountSnapshot{}, fmt.Errorf("cannot verify complete host proc visibility")
	}
	for _, option := range strings.Split(procMount.options, ",") {
		if strings.HasPrefix(option, "hidepid=") && option != "hidepid=0" && option != "hidepid=off" {
			return mountSnapshot{}, fmt.Errorf("refusing permission-limited proc mount (%s)", option)
		}
	}
	entries, err := proc.ReadDir(".")
	if err != nil {
		return mountSnapshot{}, err
	}
	snapshot := mountSnapshot{current: current, all: append([]mount(nil), current...)}
	pids := 0
	for _, entry := range entries {
		if !entry.IsDir() || !decimal(entry.Name()) {
			continue
		}
		pids++
		if err := ctx.Err(); err != nil {
			return mountSnapshot{}, err
		}
		raw, err := proc.ReadFile(entry.Name() + "/mountinfo")
		if os.IsNotExist(err) {
			// A vanished PID is harmless; an existing process with a hidden
			// mountinfo is not. Zombies can have an empty mountinfo.
			_, aliveErr := proc.ReadFile(entry.Name() + "/stat")
			if os.IsNotExist(aliveErr) {
				continue
			}
		}
		if err != nil {
			return mountSnapshot{}, fmt.Errorf("read PID %s mounts: %w", entry.Name(), err)
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			stat, statErr := proc.ReadFile(entry.Name() + "/stat")
			if os.IsNotExist(statErr) {
				continue
			}
			if statErr == nil {
				if end := strings.LastIndex(string(stat), ") "); end >= 0 {
					state := string(stat)[end+2:]
					if strings.HasPrefix(state, "Z ") || strings.HasPrefix(state, "X ") {
						continue
					}
				}
			}
			return mountSnapshot{}, fmt.Errorf("empty mountinfo for live or unreadable PID %s", entry.Name())
		}
		mounts, err := parseMounts(raw)
		if err != nil {
			return mountSnapshot{}, fmt.Errorf("parse PID %s mounts: %w", entry.Name(), err)
		}
		snapshot.all = append(snapshot.all, mounts...)
	}
	if pids == 0 {
		return mountSnapshot{}, fmt.Errorf("no visible processes in host proc")
	}
	return snapshot, nil
}

func decimal(value string) bool {
	if value == "" {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func parseMounts(raw []byte) ([]mount, error) {
	var result []mount
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || len(fields) != separator+4 {
			return nil, fmt.Errorf("malformed mountinfo record")
		}
		for _, id := range fields[:2] {
			if _, err := strconv.ParseUint(id, 10, 64); err != nil {
				return nil, fmt.Errorf("malformed mountinfo mount ID")
			}
		}
		device := strings.Split(fields[2], ":")
		if len(device) != 2 {
			return nil, fmt.Errorf("malformed mountinfo device")
		}
		for _, n := range device {
			if _, err := strconv.ParseUint(n, 10, 32); err != nil {
				return nil, fmt.Errorf("malformed mountinfo device")
			}
		}
		root, err := mountPath(fields[3])
		if err != nil {
			return nil, err
		}
		point, err := mountPath(fields[4])
		if err != nil {
			return nil, err
		}
		result = append(result, mount{
			device: fields[2], root: root, point: point,
			filesystem: fields[separator+1], options: fields[5] + "," + fields[separator+3],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty mountinfo")
	}
	return result, nil
}

func mountPath(value string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			out.WriteByte(value[i])
			continue
		}
		if i+3 >= len(value) {
			return "", fmt.Errorf("malformed mountinfo escape")
		}
		switch value[i+1 : i+4] {
		case "040":
			out.WriteByte(' ')
		case "011":
			out.WriteByte('\t')
		case "012":
			out.WriteByte('\n')
		case "134":
			out.WriteByte('\\')
		default:
			return "", fmt.Errorf("malformed mountinfo escape")
		}
		i += 3
	}
	path := out.String()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", fmt.Errorf("noncanonical mountinfo path %q", path)
	}
	return path, nil
}

func beneath(parent, path string) bool {
	return path == parent || strings.HasPrefix(path, strings.TrimSuffix(parent, "/")+"/")
}

func containingMount(mounts []mount, path string) (mount, bool) {
	var best mount
	found := false
	for _, m := range mounts {
		if beneath(m.point, path) && (!found || len(m.point) >= len(best.point)) {
			best, found = m, true
		}
	}
	return best, found
}

func (s mountSnapshot) protects(path, device string) (bool, error) {
	base, ok := containingMount(s.current, path)
	if !ok || base.device != device {
		return false, fmt.Errorf("cannot resolve staging filesystem for %q", path)
	}
	relative, err := filepath.Rel(base.point, path)
	if err != nil {
		return false, err
	}
	source := filepath.Join(base.root, relative)
	for _, m := range s.current {
		if beneath(path, m.point) {
			// Includes same-device bind mounts; RemoveAll must not cross
			// any mountpoint, not merely a change in st_dev.
			return true, nil
		}
	}
	for _, m := range s.all {
		if m.device == device && beneath(source, m.root) {
			// Bind sources live in mountinfo's root field, NOT source.
			return true, nil
		}
	}
	return false, nil
}
