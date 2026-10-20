package node

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func withinPath(value, parent string) bool {
	return value == parent || strings.HasPrefix(value, parent+"/")
}

func canonicalMountPath(value string) bool {
	return len(value) <= 1024 && value != "/" && path.IsAbs(value) &&
		path.Clean(value) == value && !strings.ContainsRune(value, '\\') &&
		!strings.ContainsFunc(value, unicode.IsControl)
}

// Mount locations are administrator-owned data/configuration zones, not runtime
// discovery. Keeping them immutable within an inventory epoch binds the policy
// to every installed generation and lease.
func ValidateVolumeMountPaths(paths []string) error {
	if len(paths) > 32 {
		return failure("InvalidRuntimeMountPolicy", "at most 32 volume mount paths are allowed")
	}
	for i, value := range paths {
		if !canonicalMountPath(value) || i > 0 && paths[i-1] >= value {
			return failure("InvalidRuntimeMountPolicy", "paths must be canonical, sorted and unique")
		}
		for _, reserved := range []string{"/app", "/dev", "/proc", "/sys", "/etc/hosts", "/etc/hostname", "/etc/resolv.conf"} {
			if withinPath(value, reserved) || withinPath(reserved, value) {
				return failure("InvalidRuntimeMountPolicy", "reserved mount location: "+value)
			}
		}
		for _, previous := range paths[:i] {
			if withinPath(value, previous) {
				return failure("InvalidRuntimeMountPolicy", "overlapping volume mount paths")
			}
		}
	}
	return nil
}

const serviceAccountMount = "/var/run/secrets/kubernetes.io/serviceaccount"

func ValidateWorkloadMount(destination string, readOnly bool, allowed []string) error {
	if !canonicalMountPath(destination) {
		return failure("RuntimeMountNotAllowed", "noncanonical mount destination: "+destination)
	}
	if destination == serviceAccountMount && readOnly {
		return nil
	}
	if strings.HasPrefix(destination, "/app/") {
		return nil
	}
	for _, parent := range allowed {
		if withinPath(destination, parent) {
			return nil
		}
	}
	return failure("RuntimeMountNotAllowed", "destination is not an approved data/configuration path: "+destination)
}

func ValidateMountDestinations(destinations []string) error {
	for i, destination := range destinations {
		for _, previous := range destinations[:i] {
			if withinPath(destination, previous) || withinPath(previous, destination) {
				return failure("RuntimeMountNotAllowed", "overlapping workload mounts/devices can redirect later mountpoints")
			}
		}
	}
	return nil
}

// Existing runtime components may not redirect an allowed volume into code.
// Missing mountpoint directories are created later in the private upper.
func validateMountPathRoot(root, destination string) error {
	parts := strings.Split(strings.TrimPrefix(destination, "/"), "/")
	for i := range parts {
		info, err := os.Lstat(filepath.Join(root, filepath.Join(parts[:i+1]...)))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return failure("RuntimeMountNotAllowed", "mount destination traverses a runtime symlink: "+destination)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return failure("RuntimeMountNotAllowed", "mount parent is not a directory: "+destination)
		}
	}
	return nil
}

func systemMount(m specs.Mount) bool {
	switch m.Destination {
	case "/proc":
		return m.Type == "proc"
	case "/dev":
		return m.Type == "tmpfs"
	case "/dev/pts":
		return m.Type == "devpts"
	case "/dev/mqueue":
		return m.Type == "mqueue"
	case "/dev/shm":
		return m.Type == "bind" || m.Type == "tmpfs"
	case "/sys":
		return m.Type == "sysfs" && slices.Contains(m.Options, "ro")
	case "/sys/fs/cgroup":
		return (m.Type == "cgroup" || m.Type == "cgroup2") && slices.Contains(m.Options, "ro")
	case "/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/dev/termination-log":
		return m.Type == "bind"
	}
	return false
}

func ValidateFilesystem(spec *specs.Spec, metadata Metadata, root string) error {
	if spec.Root == nil || !spec.Root.Readonly {
		return failure("ReadOnlyRootRequired", "Brewlet requires readOnlyRootFilesystem=true")
	}
	if err := ValidateVolumeMountPaths(metadata.VolumeMountPaths); err != nil {
		return err
	}
	var destinations, originalDestinations []string
	for _, m := range spec.Mounts {
		if !canonicalMountPath(m.Destination) {
			return failure("RuntimeMountNotAllowed", "noncanonical OCI mount destination")
		}
		if !systemMount(m) {
			if err := ValidateWorkloadMount(m.Destination, slices.Contains(m.Options, "ro"), metadata.VolumeMountPaths); err != nil {
				return err
			}
		}
		if strings.HasPrefix(m.Destination, "/app/") {
			destinations = append(destinations, m.Destination)
			originalDestinations = append(originalDestinations, m.Destination)
			continue // The private application boundary validates these separately.
		}
		destination := m.Destination
		if destination == serviceAccountMount {
			// Kubernetes' standard token path commonly traverses /var/run -> /run.
			resolved, err := resolveInRoot(root, destination, true, false)
			if err != nil {
				return err
			}
			relative, err := filepath.Rel(root, resolved)
			if err != nil || (relative != strings.TrimPrefix(destination, "/") &&
				relative != "run/secrets/kubernetes.io/serviceaccount") {
				return failure("RuntimeMountNotAllowed", "service account mount resolves outside its standard location")
			}
			destination = "/" + filepath.ToSlash(relative)
		}
		if err := validateMountPathRoot(root, destination); err != nil {
			return err
		}
		if !systemMount(m) {
			destinations = append(destinations, destination)
			originalDestinations = append(originalDestinations, m.Destination)
		}
	}
	if spec.Linux != nil {
		for _, device := range spec.Linux.Devices {
			standard := map[string][2]int64{
				"/dev/null": {1, 3}, "/dev/zero": {1, 5}, "/dev/full": {1, 7},
				"/dev/random": {1, 8}, "/dev/urandom": {1, 9}, "/dev/tty": {5, 0},
				"/dev/console": {5, 1}, "/dev/ptmx": {5, 2},
			}
			if numbers, ok := standard[device.Path]; ok && device.Type == "c" &&
				device.Major == numbers[0] && device.Minor == numbers[1] {
				continue
			}
			if err := ValidateWorkloadMount(device.Path, false, metadata.VolumeMountPaths); err != nil {
				return err
			}
			destinations = append(destinations, device.Path)
			originalDestinations = append(originalDestinations, device.Path)
			if !strings.HasPrefix(device.Path, "/app/") {
				if err := validateMountPathRoot(root, device.Path); err != nil {
					return err
				}
			}
		}
	}
	if err := ValidateMountDestinations(originalDestinations); err != nil {
		return err
	}
	return ValidateMountDestinations(destinations)
}

func validateRuntimeMountPaths(root string, metadata Metadata) error {
	for _, destination := range metadata.VolumeMountPaths {
		if err := validateMountPathRoot(root, destination); err != nil {
			return err
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(destination[1:])))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && !info.IsDir() {
			return failure("InvalidRuntimeMountPolicy", "approved mount location must be a directory")
		}
	}
	env, err := runtimeEnvironment(metadata.Env)
	if err != nil {
		return err
	}
	for _, executablePath := range strings.Split(env["PATH"], ":") {
		if executablePath == "" {
			continue
		}
		resolved, err := resolveInRoot(root, executablePath, true, true)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, resolved)
		if err != nil {
			return err
		}
		for _, dataPath := range metadata.VolumeMountPaths {
			if withinPath(executablePath, dataPath) || withinPath("/"+filepath.ToSlash(relative), dataPath) {
				return failure("InvalidRuntimeMountPolicy", fmt.Sprintf("data mount path %s overlaps runtime PATH", dataPath))
			}
		}
	}
	return nil
}
