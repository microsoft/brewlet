//go:build linux

package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/microsoft/brewlet/pkg/node"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// Mountpoint scaffolding belongs to the managed per-container upper, not to
// immutable payload caches. It must exist before runc binds packaged /app RO.
func prepareAppMountpoints(bundle, rootfsPath string, lowers []string, upper, work string, mounts []specs.Mount, extras ...string) error {
	var targets []specs.Mount
	for _, m := range mounts {
		clean := path.Clean(m.Destination)
		if clean == "/app" || strings.HasPrefix(clean, "/app/") {
			parents := false
			for _, part := range strings.Split(m.Destination, "/") {
				parents = parents || part == ".."
			}
			if parents || strings.ContainsRune(m.Destination, 0) {
				return fmt.Errorf("unsafe application volume destination")
			}
			m.Destination = clean
			targets = append(targets, m)
		}
	}
	if len(targets) == 0 {
		return nil
	}
	if rootfsPath != "rootfs" || upper == "" || work == "" {
		return fmt.Errorf("application volumes require a containerd-managed writable overlay snapshot")
	}
	stage, err := os.MkdirTemp(bundle, ".brewlet-mountpoints-")
	if err != nil {
		return err
	}
	options := []string{"lowerdir=" + strings.Join(lowers, ":"), "upperdir=" + upper, "workdir=" + work}
	options = append(options, extras...)
	if err := mount.All([]mount.Mount{{Type: "overlay", Source: "overlay", Options: options}}, stage); err != nil {
		os.Remove(stage)
		return err
	}
	prepare := func() error {
		root, err := os.OpenRoot(stage)
		if err != nil {
			return err
		}
		defer root.Close()
		for _, m := range targets {
			directory := true
			if m.Type == "bind" {
				source, err := os.Stat(m.Source)
				if err != nil {
					return err
				}
				directory = source.IsDir()
			}
			parts := strings.Split(strings.TrimPrefix(m.Destination, "/"), "/")
			for i := range parts {
				name := path.Join(parts[:i+1]...)
				last := i == len(parts)-1
				info, err := root.Lstat(name)
				if os.IsNotExist(err) {
					if !last || directory {
						err = root.Mkdir(name, 0755)
					} else {
						f, openErr := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
						if openErr == nil {
							openErr = f.Close()
						}
						err = openErr
					}
					if err != nil {
						return err
					}
					info, err = root.Lstat(name)
				}
				if err != nil {
					return err
				}
				if info.Mode()&os.ModeSymlink != 0 {
					return &node.Error{Reason: "RuntimeMountNotAllowed", Detail: "application mount destination traverses a symlink"}
				}
				resolved, err := node.InRoot(stage, "/"+name)
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(stage, resolved)
				if err != nil {
					return err
				}
				if rel != "app" && !strings.HasPrefix(rel, "app/") {
					return fmt.Errorf("application volume traverses a symlink outside /app")
				}
				if !last || directory {
					st, err := os.Stat(resolved)
					if err != nil || !st.IsDir() {
						return fmt.Errorf("application volume parent is not a directory: %s", name)
					}
				} else if info.IsDir() {
					return fmt.Errorf("file volume destination is a directory")
				}
			}
		}
		return nil
	}
	err = prepare()
	if unmountErr := mount.UnmountAll(stage, 0); unmountErr != nil {
		return fmt.Errorf("application mountpoint unmount: %w", unmountErr)
	}
	if removeErr := os.Remove(stage); err == nil {
		err = removeErr
	}
	return err
}
