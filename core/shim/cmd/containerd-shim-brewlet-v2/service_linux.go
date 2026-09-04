// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	containersapi "github.com/containerd/containerd/api/services/containers/v1"
	apitypes "github.com/containerd/containerd/api/types"
	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/task"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/microsoft/brewlet/internal/artifact"
	kcruntime "github.com/microsoft/brewlet/internal/runtime"
	"github.com/microsoft/brewlet/internal/telemetry"
)

// Node-level locations the provisioner (https://github.com/microsoft/brewlet/tree/main/specs §5.2) materializes on
// every opted-in node. Overridable via env for testing / non-standard layouts.
const (
	defaultJDKRootsDir      = "/opt/brewlet/jdks"
	defaultLauncherRootsDir = "/opt/brewlet/launchers"
	defaultCDSRegenPolicy   = "/opt/brewlet/policy/appcds-regeneration-enabled"
)

var (
	cdsRegenPolicyPath     = defaultCDSRegenPolicy
	cdsRegenPolicyOwnerUID = uint32(0)
)

// CRI stamps the container role onto every OCI spec it hands the shim. The pod
// sandbox (the "pause" container) carries annContainerType == sandbox; only the
// workload containers should be rewritten into a `java -jar` launch. See
// containerd's pkg/cri/annotations.
const (
	annContainerType       = "io.kubernetes.cri.container-type"
	annCRISandboxNamespace = "io.kubernetes.cri.sandbox-namespace"
	containerTypeSandbox   = "sandbox"
)

func init() {
	// Register the Brewlet task service as the shim's TTRPC "task" plugin. The
	// containerd shim framework (shim.RunShim -> run) walks the plugin graph
	// and serves whatever implements RegisterTTRPC as the Task service.
	//
	// containerd 2.x split 1.7's monolithic containerd/plugin package: the
	// registry lives in github.com/containerd/plugin{,/registry} while the
	// plugin *type* constants live in containerd/v2/plugins. That package
	// advises external plugins to copy the constants rather than import it, but
	// these IDs must match byte-for-byte what the shim framework we already
	// link (containerd/v2/pkg/shim) looks up, and we already depend directly on
	// the containerd/v2 module — so importing costs no extra dependency surface
	// and removes a silent drift risk. Upstream's own runc shim does the same.
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.EventPlugin,
			plugins.InternalPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			pp, err := ic.GetByID(plugins.EventPlugin, "publisher")
			if err != nil {
				return nil, err
			}
			ss, err := ic.GetByID(plugins.InternalPlugin, "shutdown")
			if err != nil {
				return nil, err
			}
			shutdownService := ss.(shutdown.Service)
			inner, err := task.NewTaskService(ic.Context, pp.(shim.Publisher), shutdownService)
			if err != nil {
				return nil, err
			}
			// containerd 2.x replaced InitContext's typed Root/State/Address/
			// TTRPCAddress fields with a generic Properties map, populated by
			// pkg/shim's plugin.NewContext call.
			target := ic.Properties[plugins.PropertyGRPCAddress]
			if !strings.Contains(target, "://") {
				target = "unix://" + target
			}
			connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				return nil, fmt.Errorf("connect to containerd for image identity: %w", err)
			}
			shutdownService.RegisterCallback(func(context.Context) error {
				return connection.Close()
			})
			return &brewletTaskService{
				TTRPCTaskService: inner,
				imageIdentity: newContainerdImageIdentityResolver(
					containersapi.NewContainersClient(connection),
					envOr("BREWLET_CONTENT_ROOT", defaultContentRoot),
				),
				pending: map[string]launchInfo{},
				writers: map[string]func(){},
			}, nil
		},
	})
}

// brewletTaskService decorates containerd's runc-backed Task service. Every
// method (State/Start/Kill/Delete/Exec/Wait/…) is delegated to the embedded
// runc implementation, so `kubectl logs/exec`, probes, signals and Services all
// behave exactly like an ordinary container (https://github.com/microsoft/brewlet/tree/main/specs §6.3). The one
// Brewlet-specific step is Create(): before runc ever sees the bundle, we
// disassemble the runnable OCI image and rewrite the OCI spec into a `java -jar`
// sandbox backed by the node-resident JDK.
type brewletTaskService struct {
	taskAPI.TTRPCTaskService
	imageIdentity imageIdentityResolver
	mu            sync.Mutex
	pending       map[string]launchInfo
	writers       map[string]func()
}

type launchInfo struct {
	entryMode   string
	format      string
	writerLease *kcruntime.CDSWriterLease
}

// RegisterTTRPC binds THIS decorator (not the embedded runc service) so
// containerd dispatches Create() to our artifact-assembly hook.
func (s *brewletTaskService) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

// Create assembles the Brewlet bundle in place, then hands off to the runc task
// service which performs the real `runc create` (rootfs mounts, cgroups,
// namespaces). This is §6.1 steps 1-4; step 5 (delegate to runc) is the inner
// Create.
func (s *brewletTaskService) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (*taskAPI.CreateTaskResponse, error) {
	// containerd's CRI plugin only emits the runc-native options.Options for the
	// built-in runc runtime types; for a custom runtime handler like ours it
	// wraps the runtime config in a generic runtimeoptions.Options. The embedded
	// runc task service unconditionally type-asserts *options.Options, so we
	// must translate before delegating or the shim panics
	// ("interface conversion: *runtimeoptions_v1.Options, not *options.Options")
	// and no brewlet pod can start under kubelet/CRI.
	if err := normalizeRuncOptions(r); err != nil {
		return nil, err
	}
	// The pod sandbox (pause) container inherits the pod's brewlet.sh/*
	// annotations but must NOT be rewritten into a JVM launch — it just holds
	// the pod's namespaces. Only workload containers get the artifact treatment.
	sandbox, err := isSandboxBundle(r.Bundle)
	if err != nil {
		return nil, err
	}
	if !sandbox {
		info, err := assembleBrewletBundle(ctx, r, s.imageIdentity)
		if err != nil {
			emitLaunch(info, err)
			return nil, err
		}
		s.trackWriter(r.ID, info.writerLease)
		start := time.Now()
		resp, err := s.TTRPCTaskService.Create(ctx, r)
		emitPhase("runc_create", start, err)
		if err != nil {
			s.releaseWriter(r.ID)
			emitLaunchWithReason(info, err, "RuntimeCreate")
			return nil, err
		}
		s.mu.Lock()
		s.pending[r.ID] = info
		s.mu.Unlock()
		return resp, nil
	}
	return s.TTRPCTaskService.Create(ctx, r)
}

func (s *brewletTaskService) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	start := time.Now()
	resp, err := s.TTRPCTaskService.Start(ctx, r)
	if r.GetExecID() != "" {
		return resp, err
	}
	s.mu.Lock()
	info, ok := s.pending[r.ID]
	delete(s.pending, r.ID)
	s.mu.Unlock()
	if err != nil {
		s.releaseWriter(r.ID)
	}
	if ok {
		emitPhase("process_start", start, err)
		reason := ""
		if err != nil {
			reason = "ProcessStart"
		}
		emitLaunchWithReason(info, err, reason)
	}
	return resp, err
}

func (s *brewletTaskService) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	resp, err := s.TTRPCTaskService.Delete(ctx, r)
	if err == nil && r.GetExecID() == "" {
		s.mu.Lock()
		delete(s.pending, r.ID)
		s.mu.Unlock()
		s.releaseWriter(r.ID)
	}
	return resp, err
}

func (s *brewletTaskService) trackWriter(id string, lease *kcruntime.CDSWriterLease) {
	if lease == nil {
		return
	}
	stop := lease.StartHeartbeat(kcruntime.DefaultWriterTTL)
	s.mu.Lock()
	if s.writers == nil {
		s.writers = map[string]func(){}
	}
	previous := s.writers[id]
	s.writers[id] = stop
	s.mu.Unlock()
	if previous != nil {
		previous()
	}
}

func (s *brewletTaskService) releaseWriter(id string) {
	s.mu.Lock()
	stop := s.writers[id]
	delete(s.writers, id)
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// isSandboxBundle reports whether the OCI spec at <bundle>/config.json belongs
// to a CRI pod sandbox (the pause container). Production workload tasks must
// carry CRI container metadata; local harnesses use prepare-bundle instead of
// entering this TTRPC path.
func isSandboxBundle(bundle string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return false, fmt.Errorf("read oci spec: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return false, fmt.Errorf("parse oci spec: %w", err)
	}
	return spec.Annotations[annContainerType] == containerTypeSandbox, nil
}

// normalizeRuncOptions rewrites a generic CRI runtimeoptions.Options request
// into the runc-native options.Options the embedded task service expects,
// preserving the runc knobs the provisioner configures on the brewlet runtime
// (notably SystemdCgroup, which must match the kubelet cgroup driver). Requests
// that already carry runc options, or none at all, are left untouched.
func normalizeRuncOptions(r *taskAPI.CreateTaskRequest) error {
	if r.Options == nil || r.Options.GetValue() == nil {
		return nil
	}
	v, err := typeurl.UnmarshalAny(r.Options)
	if err != nil {
		return fmt.Errorf("decode runtime options: %w", err)
	}
	switch o := v.(type) {
	case *runcoptions.Options:
		return nil
	case *runtimeoptions.Options:
		runcOpts, err := runcOptionsFromGeneric(o)
		if err != nil {
			return err
		}
		any, err := anypb.New(runcOpts)
		if err != nil {
			return fmt.Errorf("encode runc options: %w", err)
		}
		r.Options = any
		return nil
	default:
		return nil
	}
}

// runcOptionsFromGeneric decodes the runc option knobs the CRI plugin preserved
// in a generic runtimeoptions.Options. When no ConfigPath is set, containerd
// stashes the runtime's TOML options section verbatim in ConfigBody; otherwise
// it points at a file. Either way the keys mirror runc's options.Options fields.
func runcOptionsFromGeneric(o *runtimeoptions.Options) (*runcoptions.Options, error) {
	body := o.GetConfigBody()
	if len(body) == 0 && o.GetConfigPath() != "" {
		b, err := os.ReadFile(o.GetConfigPath())
		if err != nil {
			return nil, fmt.Errorf("read runtime config %q: %w", o.GetConfigPath(), err)
		}
		body = b
	}
	opts := &runcoptions.Options{}
	if len(body) == 0 {
		return opts, nil
	}
	var cfg struct {
		BinaryName    string
		Root          string
		SystemdCgroup bool
		NoPivotRoot   bool
		NoNewKeyring  bool
	}
	if err := toml.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parse runc runtime options: %w", err)
	}
	opts.BinaryName = cfg.BinaryName
	opts.Root = cfg.Root
	opts.SystemdCgroup = cfg.SystemdCgroup
	opts.NoPivotRoot = cfg.NoPivotRoot
	opts.NoNewKeyring = cfg.NoNewKeyring
	return opts, nil
}

// assembleBrewletBundle rewrites <bundle>/config.json (the OCI spec containerd's
// CRI layer wrote from the pod) into the Brewlet launch: it resolves the JAR
// artifact, selects the node JDK/launcher, and injects the `java -jar` process,
// JDK/JAR mounts and JVM env — while preserving everything CRI set up
// (namespaces incl. the CNI-provided network namespace, cgroup resources, user,
// and standard pod mounts).
func assembleBrewletBundle(ctx context.Context, r *taskAPI.CreateTaskRequest, identityResolver imageIdentityResolver) (launchInfo, error) {
	specPath := filepath.Join(r.Bundle, "config.json")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return launchInfo{}, fmt.Errorf("read oci spec: %w", err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return launchInfo{}, fmt.Errorf("parse oci spec: %w", err)
	}

	if identityResolver == nil {
		return launchInfo{}, fmt.Errorf("containerd image identity resolver is not configured for task %q", r.ID)
	}
	identity, err := identityResolver.Resolve(ctx, r.ID)
	if err != nil {
		return launchInfo{}, fmt.Errorf("resolve runtime image identity: %w", err)
	}
	if err := validateCRIImageIdentity(spec.Annotations, identity.TargetDigest); err != nil {
		return launchInfo{}, err
	}
	if err := validateRuntimeImageIdentity(spec.Annotations, identity.TargetDigest); err != nil {
		return launchInfo{}, err
	}
	manifestDigest, err := requireSHA256Digest("resolved platform manifest", identity.ManifestDigest)
	if err != nil {
		return launchInfo{}, err
	}

	ic := imageConfig{
		Ref:              identity.ImageName,
		JDKRootsDir:      envOr("BREWLET_JDK_ROOTS", defaultJDKRootsDir),
		LauncherRootsDir: envOr("BREWLET_LAUNCHER_ROOTS", defaultLauncherRootsDir),
		// The JDK/launcher the deployment descriptor requested, carried on the
		// pod annotations and propagated onto the OCI spec. The artifact config
		// no longer carries these — the descriptor is the single source of truth.
		JDKRequest:   spec.Annotations[annRequestedJDK],
		LauncherName: spec.Annotations[annRequestedLauncher],
		// Kubernetes execution is always bound to the image containerd resolved
		// for this CRI container. The layout backend remains available only to
		// the explicit prepare-bundle/local harness path.
		Backend:        "containerd",
		ContentRoot:    envOr("BREWLET_CONTENT_ROOT", defaultContentRoot),
		ManifestDigest: manifestDigest,
	}
	resolveStart := time.Now()
	ra, err := resolveArtifact(ic)
	backend := artifactBackend(ic)
	format := ra.Format
	if format == "" {
		format = "unknown"
	}
	if err == nil && format != "image" {
		err = fmt.Errorf("Brewlet Kubernetes workloads require a runnable OCI image, got %q format for %q", format, identity.ImageName)
	}
	_ = telemetry.Emit(telemetry.Event{
		Kind:            telemetry.KindArtifactResolution,
		Backend:         backend,
		Format:          format,
		Outcome:         telemetry.Outcome(err),
		DurationSeconds: time.Since(resolveStart).Seconds(),
	})
	emitPhase("artifact_resolve", resolveStart, err)
	if err != nil {
		return launchInfo{format: format}, err // NoCompatibleJDK / NoCompatibleLauncher surface as task-create failures
	}
	info := launchInfo{entryMode: ra.Config.Entry.Mode, format: format}
	if info.entryMode == "" {
		info.entryMode = "jar"
	}

	bundleStart := time.Now()
	var writerLease *kcruntime.CDSWriterLease
	if err := applyBrewletLaunchWithWriterLease(&spec, ra, r.Bundle, &writerLease); err != nil {
		emitPhase("bundle_prepare", bundleStart, err)
		return info, err
	}
	info.writerLease = writerLease
	releaseWriterLease := true
	defer func() {
		if releaseWriterLease && writerLease != nil {
			writerLease.Release()
		}
	}()
	bundleDuration := time.Since(bundleStart)

	overlayStart := time.Now()
	if err := setupOverlayRootfs(r, ra); err != nil {
		emitPhase("overlay_setup", overlayStart, err)
		return info, err
	}
	emitPhase("overlay_setup", overlayStart, nil)

	bundleStart = time.Now()
	if err := mountClasspathLayers(&spec, ra, r.Bundle); err != nil {
		emitPhaseDuration("bundle_prepare", bundleDuration+time.Since(bundleStart), err)
		return info, err
	}
	if err := mountModulepathLayers(&spec, ra, r.Bundle); err != nil {
		emitPhaseDuration("bundle_prepare", bundleDuration+time.Since(bundleStart), err)
		return info, err
	}
	spec.Root = &specs.Root{Path: "rootfs", Readonly: false}

	out, err := json.MarshalIndent(&spec, "", "  ")
	if err != nil {
		emitPhaseDuration("bundle_prepare", bundleDuration+time.Since(bundleStart), err)
		return info, err
	}
	err = os.WriteFile(specPath, out, 0o644)
	if err == nil {
		releaseWriterLease = false
	}
	emitPhaseDuration("bundle_prepare", bundleDuration+time.Since(bundleStart), err)
	return info, err
}

func artifactBackend(ic imageConfig) string {
	if ic.Backend != "" {
		return ic.Backend
	}
	if ic.StoreRoot != "" {
		return "layout"
	}
	return "containerd"
}

func emitPhase(phase string, start time.Time, err error) {
	emitPhaseDuration(phase, time.Since(start), err)
}

func emitPhaseDuration(phase string, duration time.Duration, err error) {
	_ = telemetry.Emit(telemetry.Event{
		Kind:            telemetry.KindLaunchPhase,
		Phase:           phase,
		Outcome:         telemetry.Outcome(err),
		DurationSeconds: duration.Seconds(),
	})
}

func emitLaunch(info launchInfo, err error) {
	emitLaunchWithReason(info, err, "")
}

func emitLaunchWithReason(info launchInfo, err error, reason string) {
	entryMode := info.entryMode
	if entryMode == "" {
		entryMode = "unknown"
	}
	format := info.format
	if format == "" {
		format = "unknown"
	}
	if reason == "" {
		reason = telemetry.Reason(err)
	}
	_ = telemetry.Emit(telemetry.Event{
		Kind:      telemetry.KindLaunch,
		Outcome:   telemetry.Outcome(err),
		Reason:    reason,
		EntryMode: entryMode,
		Format:    format,
	})
}

// setupOverlayRootfs implements §6.1 step 3: the sandbox rootfs is an overlay
// whose read-only lower layer is the shared node JDK runtime root and whose
// upper/work layers are per-container writable scratch. containerd's runc
// container setup mounts r.Rootfs onto <bundle>/rootfs, so replacing the
// image snapshot mounts with this overlay gives the JVM a writable root backed
// by the shared JDK userland. The runnable image snapshot establishes CRI image
// identity and unpackability, but its OS-less rootfs is not executed.
func setupOverlayRootfs(r *taskAPI.CreateTaskRequest, ra resolvedArtifact) error {
	scratch := filepath.Join(r.Bundle, "brewlet")
	upper := filepath.Join(scratch, "upper")
	work := filepath.Join(scratch, "work")
	for _, d := range []string{upper, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("overlay scratch: %w", err)
		}
	}
	// lowerdir is highest-priority first: a custom launcher layer (if any)
	// overlays the JDK userland so its binaries win on PATH.
	lowers := []string{ra.JDKRoot}
	if !artifact.IsVanillaLauncher(ra.LauncherName) && ra.LauncherRoot != "" {
		lowers = append([]string{ra.LauncherRoot}, lowers...)
	}
	r.Rootfs = []*apitypes.Mount{{
		Type:   "overlay",
		Source: "overlay",
		Options: []string{
			"lowerdir=" + strings.Join(lowers, ":"),
			"upperdir=" + upper,
			"workdir=" + work,
		},
	}}
	return nil
}

// applyBrewletLaunch mutates spec in place to run the JAR under the selected
// JDK, mirroring the mount/env/arg layout of runtime.GenerateBundleWithLauncher
// but leaving CRI's namespaces, resources and pod mounts untouched. bundleDir is
// the container bundle root (r.Bundle), used to stage a canonical-mtime copy of
// the app JAR when the artifact ships an AppCDS archive (see below).
func applyBrewletLaunch(spec *specs.Spec, ra resolvedArtifact, bundleDir string) error {
	return applyBrewletLaunchWithWriterLease(spec, ra, bundleDir, nil)
}

func applyBrewletLaunchWithWriterLease(
	spec *specs.Spec,
	ra resolvedArtifact,
	bundleDir string,
	writerLeaseOut **kcruntime.CDSWriterLease,
) (err error) {
	var writerLease *kcruntime.CDSWriterLease
	defer func() {
		if err != nil && writerLease != nil {
			writerLease.Release()
		}
	}()

	// The JAR filename is attacker-influenced metadata (the runnable image's
	// brewlet.sh/jvm-config annotation): it becomes a mount destination and, via
	// StageCDSJar/staging resolution, a root bind-mount SOURCE. Resolution
	// already validates it, but this is the trust boundary — re-check and fail
	// closed rather than mount a host path the image chose.
	mainJar, err := artifact.MainJarName(ra.Config)
	if err != nil {
		return err
	}
	inSandboxJar := "/app/" + mainJar
	if spec.Process == nil {
		return fmt.Errorf("CRI OCI spec is missing process configuration")
	}
	writerOwner := &kcruntime.RegenOwner{
		UID: int(spec.Process.User.UID),
		GID: int(spec.Process.User.GID),
	}

	// Node-side AppCDS regeneration is a deployment/fleet decision, carried on the
	// pod as brewlet.sh/cds-regenerate (set by the operator from
	// spec.jvm.cds.regenerate). Suppress the shipped-archive args when it is set;
	// the regen args are injected below.
	regenerate := strings.EqualFold(strings.TrimSpace(spec.Annotations[annCDSRegenerate]), "true")

	jvmArgs, _ := kcruntime.BuildJVMArgs(ra.Config, inSandboxJar, nil, regenerate)

	// Node-side AppCDS regeneration is authorized by a root-owned host policy and
	// isolated by the trusted CRI namespace plus the verified resolved manifest.
	// Only the resulting private entry directory is mounted into the workload.
	var cdsCacheMount []specs.Mount
	if regenerate {
		if err := requireCDSRegenPolicy(); err != nil {
			return err
		}
		namespace := strings.TrimSpace(spec.Annotations[annCRISandboxNamespace])
		if namespace == "" {
			return fmt.Errorf("AppCDS regeneration requires trusted CRI sandbox namespace annotation %q", annCRISandboxNamespace)
		}
		if ra.ManifestDigest == "" {
			return fmt.Errorf("AppCDS regeneration requires a verified resolved manifest digest")
		}
		cacheDir := envOr("BREWLET_CDS_CACHE", kcruntime.DefaultCDSCacheDir)
		seed := ""
		if ra.CDSHostPath != "" && ra.Config.CDS != nil && ra.Config.CDS.Archive != "" {
			seed = ra.CDSHostPath
		}
		dec, err := kcruntime.DecideCDSRegen(kcruntime.RegenParams{
			CacheDir:       cacheDir,
			CacheScope:     namespace,
			JDKRoot:        ra.JDKHome,
			ArtifactDigest: ra.ManifestDigest,
			SeedArchive:    seed,
			WriterOwner:    writerOwner,
			ArchiveArgDir:  kcruntime.InSandboxCDSDir,
			MetricsDir:     envOr("BREWLET_METRICS_DIR", ""),
		})
		if err != nil {
			return err
		}
		writerLease = dec.WriterLease
		if len(dec.Args) > 0 {
			jvmArgs = append(append([]string{}, dec.Args...), jvmArgs...)
		}
		if dec.Role == kcruntime.RegenConsume || dec.Role == kcruntime.RegenWrite {
			if !privateCacheEntry(cacheDir, dec.HostMount) {
				return fmt.Errorf("AppCDS regeneration refused unsafe cache mount %q under %q", dec.HostMount, cacheDir)
			}
			opts := []string{"rbind"}
			if dec.MountRW {
				opts = append(opts, "rw")
			} else {
				opts = append(opts, "ro")
			}
			opts = append(opts, "nosuid", "nodev", "noexec")
			cdsCacheMount = []specs.Mount{{
				Destination: kcruntime.InSandboxCDSDir,
				Type:        "bind",
				Source:      dec.HostMount,
				Options:     opts,
			}}
		}
	}

	spec.Process.Args = append([]string{artifact.LauncherName(ra.LauncherName)}, jvmArgs...)
	spec.Process.Cwd = "/app"

	// PATH: prepend the custom launcher layer when present so e.g. `jaz`
	// resolves ahead of the JDK's own bin.
	path := "/opt/jdk/bin:/usr/bin:/bin"
	var launcherMount []specs.Mount
	if !artifact.IsVanillaLauncher(ra.LauncherName) && ra.LauncherRoot != "" {
		launcherMount = []specs.Mount{{
			Destination: "/opt/brewlet/launcher",
			Type:        "bind",
			Source:      ra.LauncherRoot,
			Options:     []string{"rbind", "ro"},
		}}
		path = "/opt/brewlet/launcher/bin:" + path
	}

	env := []string{"PATH=" + path, "JAVA_HOME=/opt/jdk"}
	for _, e := range ra.Config.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	spec.Process.Env = mergeEnv(spec.Process.Env, env)

	// The app JAR is normally bind-mounted read-only straight from its
	// content-store blob. When the artifact ships an AppCDS archive, mount a
	// canonical-mtime copy instead: HotSpot validates the archive against the
	// JAR's mtime and rejects it (silently, under -Xshare:auto) if it differs
	// from dump time, and the blob's mtime is the node's non-deterministic pull
	// time (see runtime.CDSModTime).
	jarSource := ra.JarHostPath
	if ra.Config.CDS != nil || regenerate {
		staged, err := kcruntime.StageCDSJar(ra.JarHostPath, filepath.Join(bundleDir, "brewlet", "app"), mainJar)
		if err != nil {
			return fmt.Errorf("stage cds jar: %w", err)
		}
		jarSource = staged
	}

	// The JDK runtime root doubles as the overlay rootfs lower layer
	// (setupOverlayRootfs), so it already supplies the base userland the JVM
	// links against — §5.3 requires it to be a self-contained minimal
	// userland+JDK. We bind the source image's declared Java home at /opt/jdk so
	// JAVA_HOME stays stable regardless of the runtime's internal image path.
	// The JAR payload is mounted read-only at /app.
	brewletMounts := []specs.Mount{
		{Destination: "/opt/jdk", Type: "bind", Source: ra.JDKHome, Options: []string{"rbind", "ro"}},
		{Destination: inSandboxJar, Type: "bind", Source: jarSource, Options: []string{"rbind", "ro"}},
	}
	// Optional AppCDS archive: bind-mount read-only at /app/<archive> so the
	// -XX:SharedArchiveFile path BuildJVMArgs emitted resolves. See https://github.com/microsoft/brewlet/blob/main/docs/appcds.md.
	// Skipped under node-side regeneration: there the shipped archive is only seed
	// data for the node cache (bind-mounted at InSandboxCDSDir instead).
	if !regenerate && ra.CDSHostPath != "" && ra.Config.CDS != nil && ra.Config.CDS.Archive != "" {
		if err := artifact.ValidateBareFilename("cds.archive", ra.Config.CDS.Archive); err != nil {
			return err
		}
		brewletMounts = append(brewletMounts, specs.Mount{
			Destination: "/app/" + ra.Config.CDS.Archive,
			Type:        "bind",
			Source:      ra.CDSHostPath,
			Options:     []string{"rbind", "ro"},
		})
	}
	brewletMounts = append(brewletMounts, cdsCacheMount...)
	brewletMounts = append(brewletMounts, launcherMount...)
	spec.Mounts = append(spec.Mounts, brewletMounts...)
	if writerLeaseOut != nil {
		*writerLeaseOut = writerLease
	}
	return nil
}

func requireCDSRegenPolicy() error {
	info, err := os.Lstat(cdsRegenPolicyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("AppCDS regeneration is disabled by node policy")
		}
		return fmt.Errorf("read AppCDS regeneration policy: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("AppCDS regeneration policy %q must be a non-group-writable regular file", cdsRegenPolicyPath)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != cdsRegenPolicyOwnerUID {
		return fmt.Errorf("AppCDS regeneration policy %q must be owned by uid %d", cdsRegenPolicyPath, cdsRegenPolicyOwnerUID)
	}
	return nil
}

func privateCacheEntry(cacheDir, entry string) bool {
	cacheDir = filepath.Clean(cacheDir)
	entry = filepath.Clean(entry)
	return entry != cacheDir && filepath.Dir(entry) == cacheDir
}

// mountClasspathLayers implements the §6.1 rootfs step for layered-classpath
// deployment (https://github.com/microsoft/brewlet/blob/main/docs/layered-classpath-deployment.md): it stages each optional
// classpath.layer.v1+tar into a per-container host dir and bind-mounts it
// read-only at /app/lib, so a `-cp /app/app.jar:/app/lib/*` launch resolves the
// dependency JARs. This mirrors runtime.GenerateBundleWithLauncher so the
// production shim and the CLI/harness bundle path behave identically. A no-op
// when the artifact carries no classpath layers.
func mountClasspathLayers(spec *specs.Spec, ra resolvedArtifact, bundleDir string) error {
	if len(ra.ClasspathHostPaths) == 0 {
		return nil
	}
	libHost := filepath.Join(bundleDir, "brewlet", "lib")
	if err := kcruntime.StageClasspathLayers(ra.ClasspathHostPaths, libHost); err != nil {
		return err
	}
	// Pin dependency-JAR mtimes to the canonical CDS value when the artifact
	// ships an AppCDS archive, so each classpath entry matches the archive's
	// recorded timestamps (see runtime.CDSModTime).
	if ra.Config.CDS != nil {
		if err := kcruntime.PinCDSModTimesUnder(libHost); err != nil {
			return err
		}
	}
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: "/app/lib",
		Type:        "bind",
		Source:      libHost,
		Options:     []string{"rbind", "ro"},
	})
	return nil
}

// mountModulepathLayers is the module-path twin of mountClasspathLayers: it
// stages each optional modulepath.layer.v1+tar into a per-container host dir and
// bind-mounts it read-only at /app/mods, so a `-p /app/app.jar:/app/mods` launch
// resolves the library modules. This mirrors runtime.GenerateBundleWithLauncher
// so the production shim and the CLI/harness bundle path behave identically. A
// no-op when the artifact carries no modulepath layers. See https://github.com/microsoft/brewlet/blob/main/docs/jpms-support.md.
func mountModulepathLayers(spec *specs.Spec, ra resolvedArtifact, bundleDir string) error {
	if len(ra.ModulepathHostPaths) == 0 {
		return nil
	}
	modsHost := filepath.Join(bundleDir, "brewlet", "mods")
	if err := kcruntime.StageModulepathLayers(ra.ModulepathHostPaths, modsHost); err != nil {
		return err
	}
	if ra.Config.CDS != nil {
		if err := kcruntime.PinCDSModTimesUnder(modsHost); err != nil {
			return err
		}
	}
	spec.Mounts = append(spec.Mounts, specs.Mount{
		Destination: "/app/mods",
		Type:        "bind",
		Source:      modsHost,
		Options:     []string{"rbind", "ro"},
	})
	return nil
}

// mergeEnv overlays Brewlet's launch env onto whatever CRI provided: Brewlet's
// PATH/JAVA_HOME/launcher/app values win over pod defaults for the same key,
// but pod-specified variables Brewlet doesn't set are preserved.
func mergeEnv(base, overlay []string) []string {
	idx := make(map[string]int, len(base))
	out := append([]string(nil), base...)
	for i, kv := range out {
		if k := envKey(kv); k != "" {
			idx[k] = i
		}
	}
	for _, kv := range overlay {
		k := envKey(kv)
		if k == "" {
			continue
		}
		if i, ok := idx[k]; ok {
			out[i] = kv
		} else {
			idx[k] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

func envKey(kv string) string {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i]
		}
	}
	return kv
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// compile-time check that the decorator still satisfies the Task service.
var _ taskAPI.TTRPCTaskService = (*brewletTaskService)(nil)
