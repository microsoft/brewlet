// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/microsoft/brewlet/internal/artifact"
)

// OCI runtime-spec subset sufficient to launch `java -jar` under runc.
type ociSpec struct {
	OCIVersion string     `json:"ociVersion"`
	Process    ociProcess `json:"process"`
	Root       ociRoot    `json:"root"`
	Hostname   string     `json:"hostname,omitempty"`
	Mounts     []ociMount `json:"mounts"`
	Linux      ociLinux   `json:"linux"`
}

type ociProcess struct {
	Terminal bool     `json:"terminal"`
	User     ociUser  `json:"user"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
}

type ociUser struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

const (
	// DefaultProcessUID and DefaultProcessGID are the secure identity used by
	// standalone OCI bundles when the trusted runtime caller does not select one.
	DefaultProcessUID uint32 = 65532
	DefaultProcessGID uint32 = 65532
	// MaxProcessID excludes the all-ones value Linux reserves as the invalid
	// (uid_t)-1/(gid_t)-1 sentinel.
	MaxProcessID uint32 = 1<<32 - 2
)

// ProcessIdentity is the trusted runtime-side UID/GID for a standalone OCI
// bundle. Artifact metadata never controls this value.
type ProcessIdentity struct {
	UID uint32
	GID uint32
}

// DefaultProcessIdentity returns Brewlet's unprivileged standalone bundle
// identity.
func DefaultProcessIdentity() ProcessIdentity {
	return ProcessIdentity{UID: DefaultProcessUID, GID: DefaultProcessGID}
}

type ociRoot struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly"`
}

type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type,omitempty"`
	Source      string   `json:"source,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type ociLinux struct {
	Namespaces []ociNamespace `json:"namespaces"`
	Resources  ociResources   `json:"resources"`
}

type ociNamespace struct {
	Type string `json:"type"`
}

type ociResources struct {
	Memory *ociMemory `json:"memory,omitempty"`
	CPU    *ociCPU    `json:"cpu,omitempty"`
}

type ociMemory struct {
	Limit *int64 `json:"limit,omitempty"`
}

type ociCPU struct {
	Quota  *int64  `json:"quota,omitempty"`
	Period *uint64 `json:"period,omitempty"`
}

// GenerateBundle writes an OCI runtime bundle (config.json) that runc can run:
// rootfs = node JDK runtime root (lower) with the JAR mounted at /app. This is
// exactly what containerd-shim-brewlet-v2 produces on a Linux node.
func GenerateBundle(cfg artifact.JVMConfig, jdkRoot, jarHostPath, outDir string, res Resources, extraArgs []string) error {
	return GenerateBundleWithLayers(cfg, jdkRoot, jarHostPath, nil, nil, outDir, res, extraArgs)
}

// GenerateBundleWithLayers is GenerateBundle with optional dependency layers.
//
// classpathTars are optional dependency-layer tars (see
// https://github.com/microsoft/brewlet/blob/main/docs/layered-classpath-deployment.md). When present they are extracted into a
// host staging dir and bind-mounted read-only at /app/lib, so a class-mode
// `-cp /app/app.jar:/app/lib/*` resolves the dependency JARs.
//
// modulepathTars are optional library-module tars (see https://github.com/microsoft/brewlet/blob/main/docs/jpms-support.md).
// When present they are extracted into a host staging dir and bind-mounted
// read-only at /app/mods, so a module-mode `-p /app/app.jar:/app/mods` resolves
// the library modules.
func GenerateBundleWithLayers(cfg artifact.JVMConfig, jdkRoot, jarHostPath string, classpathTars, modulepathTars []string, outDir string, res Resources, extraArgs []string) error {
	return GenerateBundleWithCDS(cfg, jdkRoot, jarHostPath, classpathTars, modulepathTars, "", outDir, res, extraArgs)
}

// GenerateBundleWithCDS is GenerateBundleWithLayers with an optional AppCDS
// archive. When cdsHostPath is non-empty the `.jsa` file it names is bind-mounted
// read-only at /app/<archive> (cfg.CDS.Archive), matching the -XX:SharedArchiveFile
// path BuildJVMArgs derives, so the JVM maps the app archive on startup. Pass ""
// for the common no-CDS case. See https://github.com/microsoft/brewlet/blob/main/docs/appcds.md.
func GenerateBundleWithCDS(cfg artifact.JVMConfig, jdkRoot, jarHostPath string, classpathTars, modulepathTars []string, cdsHostPath, outDir string, res Resources, extraArgs []string) error {
	return GenerateBundleWithIdentity(
		cfg, jdkRoot, jarHostPath,
		classpathTars, modulepathTars, cdsHostPath, outDir, res, extraArgs,
		DefaultProcessIdentity(),
	)
}

// GenerateBundleWithIdentity generates a bundle with an explicit trusted runtime
// identity. Artifact metadata never controls the process UID/GID.
func GenerateBundleWithIdentity(cfg artifact.JVMConfig, jdkRoot, jarHostPath string, classpathTars, modulepathTars []string, cdsHostPath, outDir string, res Resources, extraArgs []string, identity ProcessIdentity) error {
	resources, err := buildResources(res)
	if err != nil {
		return err
	}
	if identity.UID > MaxProcessID || identity.GID > MaxProcessID {
		return fmt.Errorf("process UID/GID must be between 0 and %d", MaxProcessID)
	}
	if err := os.MkdirAll(filepath.Join(outDir, "rootfs"), 0o755); err != nil {
		return err
	}

	// Re-derive the JVM args (resource mapping) but target in-sandbox paths.
	// MainJarName re-checks the name: it becomes both a mount destination here
	// and (below) a host path the staged JAR copy is written to.
	mainJar, err := artifact.MainJarName(cfg)
	if err != nil {
		return err
	}
	inSandboxJar := "/app/" + mainJar
	jvmArgs, err := BuildJVMArgs(cfg, inSandboxJar, extraArgs)
	if err != nil {
		return err
	}

	pinMtime := shipsCDS(cfg)

	// Stage classpath dependency layers into a host dir bind-mounted at /app/lib.
	var libMount []ociMount
	if len(classpathTars) > 0 {
		libHost := filepath.Join(outDir, "lib")
		if err := StageClasspathLayers(classpathTars, libHost); err != nil {
			return err
		}
		if pinMtime {
			if err := PinCDSModTimesUnder(libHost); err != nil {
				return err
			}
		}
		libMount = []ociMount{{
			Destination: "/app/lib", Type: "bind",
			Source: libHost, Options: []string{"rbind", "ro"},
		}}
	}

	// Stage modulepath library-module layers into a host dir bind-mounted at
	// /app/mods, the module-path twin of /app/lib. See https://github.com/microsoft/brewlet/blob/main/docs/jpms-support.md.
	var modsMount []ociMount
	if len(modulepathTars) > 0 {
		modsHost := filepath.Join(outDir, "mods")
		if err := StageModulepathLayers(modulepathTars, modsHost); err != nil {
			return err
		}
		if pinMtime {
			if err := PinCDSModTimesUnder(modsHost); err != nil {
				return err
			}
		}
		modsMount = []ociMount{{
			Destination: "/app/mods", Type: "bind",
			Source: modsHost, Options: []string{"rbind", "ro"},
		}}
	}

	// The app JAR is normally bind-mounted read-only straight from its host blob.
	// When the artifact ships an archive, mount a
	// canonical-mtime copy instead: the archive records the JAR's mtime and
	// HotSpot rejects it under -Xshare:auto if the on-disk mtime differs (see
	// CDSModTime), and a bind-mount inherits the source blob's non-deterministic
	// mtime.
	jarSource := jarHostPath
	if pinMtime {
		staged, err := StageCDSJar(jarHostPath, filepath.Join(outDir, "brewlet-app"), mainJar)
		if err != nil {
			return err
		}
		jarSource = staged
	}

	// Bind-mount the optional AppCDS archive read-only at /app/<archive> so the
	// -XX:SharedArchiveFile path BuildJVMArgs emitted resolves. See https://github.com/microsoft/brewlet/blob/main/docs/appcds.md.
	var cdsMount []ociMount
	if cdsHostPath != "" && cfg.CDS != nil && cfg.CDS.Archive != "" {
		cdsMount = []ociMount{{
			Destination: "/app/" + cfg.CDS.Archive, Type: "bind",
			Source: cdsHostPath, Options: []string{"rbind", "ro"},
		}}
	}

	args := append([]string{"/opt/jdk/bin/java"}, jvmArgs...)

	env := []string{
		"PATH=/opt/jdk/bin:/usr/bin:/bin",
		"JAVA_HOME=/opt/jdk",
		"HOME=/tmp",
	}
	for _, e := range cfg.Env {
		env = append(env, e.Name+"="+e.Value)
	}

	spec := ociSpec{
		OCIVersion: "1.1.0",
		Process: ociProcess{
			User: ociUser{UID: identity.UID, GID: identity.GID},
			Args: args,
			Env:  env,
			Cwd:  "/app",
		},
		Root:     ociRoot{Path: "rootfs", Readonly: true},
		Hostname: "brewlet",
		Mounts: append([]ociMount{
			// Node-resident JDK mounted read-only (shared across all sandboxes).
			{Destination: "/opt/jdk", Type: "bind", Source: jdkRoot, Options: []string{"rbind", "ro"}},
			// The JAR payload from the artifact, read-only.
			{Destination: inSandboxJar, Type: "bind", Source: jarSource, Options: []string{"rbind", "ro"}},
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "noexec", "nodev", "mode=1777", "size=65536k"}},
			{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			// cgroup2 mounted read-only so the container-aware JVM can read its
			// own cpu.max/memory.max (via the cgroup namespace) and size the heap
			// and GC/JIT thread pools from the sandbox limits — exactly as it does
			// in an ordinary container. Without this the JVM sees the host's
			// resources and mis-sizes itself.
			{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
			{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777"}},
		}, append(append(libMount, modsMount...), cdsMount...)...),
		Linux: ociLinux{
			// PoC omits the "network" namespace so it shares the host netns and
			// can be curled directly. The real shim instead JOINS the pod netns
			// that the kubelet/CNI prepared (so the JVM gets a normal pod IP).
			Namespaces: []ociNamespace{
				{Type: "pid"}, {Type: "ipc"}, {Type: "uts"},
				{Type: "mount"}, {Type: "cgroup"},
			},
			Resources: resources,
		},
	}

	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "config.json"), b, 0o644)
}

// Validate checks resource limits before a caller creates bundle-owned files.
func (res Resources) Validate() error {
	_, err := buildResources(res)
	return err
}

func buildResources(res Resources) (ociResources, error) {
	out := ociResources{}
	if res.MemoryLimit != "" {
		bytes, err := parseMemory(res.MemoryLimit)
		if err != nil {
			return ociResources{}, fmt.Errorf("invalid memory limit %q: %w (use bytes, 512Mi, or 1Gi; omit for unlimited)", res.MemoryLimit, err)
		}
		out.Memory = &ociMemory{Limit: &bytes}
	}
	if res.CPULimit != "" {
		millis, err := parseCPU(res.CPULimit)
		if err != nil {
			return ociResources{}, fmt.Errorf("invalid CPU limit %q: %w (use 2 or 500m; omit for unlimited)", res.CPULimit, err)
		}
		if millis > math.MaxInt64/100 {
			return ociResources{}, fmt.Errorf("invalid CPU limit %q: cgroup quota exceeds int64", res.CPULimit)
		}
		period := uint64(100000)
		quota := millis * 100
		if quota < 1000 {
			return ociResources{}, fmt.Errorf("invalid CPU limit %q: must be at least 10m (0.01 CPU) for the 100ms cgroup period", res.CPULimit)
		}
		out.CPU = &ociCPU{Quota: &quota, Period: &period}
	}
	return out, nil
}
