package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/artifact"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const runSynopsis = `Usage: brewlet run REF --runtime [ID=]IMAGE [flags] [-- ARGS...]
       brewlet run REF --runtime ID --from-cluster [flags] [-- ARGS...]

Run a locally built application artifact on an explicitly chosen runtime image
through Docker or Podman, for development testing only.

REF is resolved in the local OCI layout written by 'brewlet build REF' (or the
layout given by --store); registries are never contacted for the application.
The container is assembled the way the Brewlet shim does on a cluster:

  - the runtime image provides the root filesystem, PATH and base environment;
  - application and dependency layers are materialized into a private
    directory and bind-mounted read-only at /app, hiding any runtime /app;
  - the root filesystem is read-only, all capabilities are dropped and
    no-new-privileges is set;
  - launch.json supplies the entrypoint, args, working directory (default
    /app) and environment, overriding the runtime image environment; -e
    overrides launch.json and PATH stays runtime-owned;
  - ARGS after '--' replace launch.json args, like Kubernetes 'args'.

The runtime is never guessed. --runtime IMAGE runs on IMAGE; --runtime ID=IMAGE
also names the runtime image ID, which must appear in launch.json
runtimeImages when that allow-list is present (the cluster refuses other IDs).
--runtime ID --from-cluster reads RuntimeImage ID from the current (or
--kubeconfig/--context) cluster and uses its pinned spec.source.image and
approved spec.user. No merged application+runtime image is ever produced.

The cluster runs workloads as the approved non-root RuntimeImage spec.user.
Without --from-cluster or --user the runtime image's default user is used and
a warning is printed. UID or GID 0 is refused.

Not reproduced locally: node authorization, installation and rotation of
runtime images, admission defaults for pod fields, HOME synthesis for UIDs
without a passwd entry (the engine uses its own default), exec parity, and
RuntimeImage volumeMountPaths enforcement except for --tmpfs with
--from-cluster.

Examples:
  brewlet build example.com/hello:1 --launch launch.json --platform linux/arm64 \
      --layer app=./dist:/app
  brewlet run example.com/hello:1 --runtime python-3.12=python:3.12-slim -p 8000:8000
  brewlet run example.com/hello:1 --runtime python-3.12 --from-cluster -- --debug
  brewlet run example.com/hello:1 --runtime busybox:1.36 --user 1000:1000 --dry-run`

// runAppDirPlaceholder stands for the materialized /app tree in --dry-run output.
const runAppDirPlaceholder = "<app-dir>"

type runRuntime struct {
	ID, Image  string
	User       *api.User
	MountPaths []string
	FromPolicy bool
}

// lookupRuntimeImage reads a cluster-scoped RuntimeImage. Tests replace it.
var lookupRuntimeImage = func(ctx context.Context, kubeconfig, contextName, id string) (*api.RuntimeImage, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: contextName}).ClientConfig()
	if err != nil {
		return nil, err
	}
	config.Timeout = 30 * time.Second
	scheme := k8sruntime.NewScheme()
	if err = api.AddToScheme(scheme); err != nil {
		return nil, err
	}
	reader, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	var ri api.RuntimeImage
	if err = reader.Get(ctx, client.ObjectKey{Name: id}, &ri); err != nil {
		return nil, err
	}
	return &ri, nil
}

type exitStatusError struct{ code int }

func (e *exitStatusError) Error() string {
	return fmt.Sprintf("run: container exited with status %d", e.code)
}
func (e *exitStatusError) ExitCode() int { return e.code }

// runLocal implements `brewlet run`.
func runLocal(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var extra []string
	hasExtra := false
	for i, a := range args {
		if a == "--" {
			args, extra, hasExtra = args[:i], args[i+1:], true
			break
		}
	}
	fs := newFlagSet("run", runSynopsis, stdout)
	storeFlag := fs.String("store", "", "OCI layout directory (default .brewlet/images/<REF>)")
	platformFlag := fs.String("platform", "", "OS/ARCH[/VARIANT] to run (default linux/"+runtime.GOARCH+")")
	runtimeFlag := fs.String("runtime", "", "runtime image: IMAGE, ID=IMAGE, or ID with --from-cluster")
	fromCluster := fs.Bool("from-cluster", false, "resolve --runtime ID from the cluster's RuntimeImage")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig for --from-cluster (default standard loading rules)")
	contextName := fs.String("context", "", "kubeconfig context for --from-cluster (default current context)")
	engineDefault := os.Getenv("BREWLET_CONTAINER_ENGINE")
	if engineDefault == "" {
		engineDefault = "docker"
	}
	engine := fs.String("engine", engineDefault, "container engine: docker or podman (env BREWLET_CONTAINER_ENGINE)")
	userFlag := fs.String("user", "", "numeric UID:GID (UID alone with --from-cluster); 0 is refused")
	name := fs.String("name", "", "container name")
	tty := fs.Bool("tty", false, "allocate a pseudo-TTY")
	fs.BoolVar(tty, "t", false, "shorthand for --tty")
	dryRun := fs.Bool("dry-run", false, "print the engine command without running it")
	var envs, publish, tmpfs repeated
	fs.Var(&envs, "env", "NAME=VALUE environment override (repeatable)")
	fs.Var(&envs, "e", "shorthand for --env")
	fs.Var(&publish, "publish", "HOST:CONTAINER port mapping passed to the engine (repeatable)")
	fs.Var(&publish, "p", "shorthand for --publish")
	fs.Var(&tmpfs, "tmpfs", "writable tmpfs mount PATH (repeatable); with --from-cluster limited to /app/ and RuntimeImage volumeMountPaths")
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if *engine != "docker" && *engine != "podman" {
		return fmt.Errorf("run: --engine must be docker or podman, got %q", *engine)
	}
	rt, err := parseRunRuntime(*runtimeFlag, *fromCluster)
	if err != nil {
		return err
	}
	if !*fromCluster && (*kubeconfig != "" || *contextName != "") {
		return fmt.Errorf("run: --kubeconfig and --context require --from-cluster")
	}

	dir := storeForRef(ref, *storeFlag)
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("run: no local build for %s in %s; run `brewlet build %s ...` first", ref, dir, ref)
		}
		return err
	}
	store := artifact.Store{Dir: dir}
	target, err := store.Target(ref)
	if err != nil {
		if target, err = store.Target(""); err != nil {
			return fmt.Errorf("run: %s not found in local layout %s: %w", ref, dir, err)
		}
	}
	platformText := *platformFlag
	if platformText == "" {
		platformText = "linux/" + runtime.GOARCH
	}
	platform, err := artifact.ParsePlatform(platformText)
	if err != nil {
		return err
	}
	resolved, err := artifact.Resolve(ctx, store.Open, target, platform)
	if err != nil {
		return fmt.Errorf("run: %s (%s): %w", ref, platformText, err)
	}
	launch := resolved.Launch
	if rt.ID != "" && !launch.Allows(rt.ID) {
		return fmt.Errorf("run: RuntimeImageIncompatible: runtime image ID %q is not in launch.json runtimeImages %q", rt.ID, launch.RuntimeImages)
	}
	if *fromCluster {
		ri, err := lookupRuntimeImage(ctx, *kubeconfig, *contextName, rt.ID)
		if err != nil {
			return fmt.Errorf("run: RuntimeImage %s: %w", rt.ID, err)
		}
		if !strings.Contains(ri.Spec.Source.Image, "@sha256:") {
			return fmt.Errorf("run: RuntimeImage %s spec.source.image %q is not digest-pinned", rt.ID, ri.Spec.Source.Image)
		}
		u := ri.Spec.User
		rt.Image, rt.User, rt.MountPaths, rt.FromPolicy = ri.Spec.Source.Image, &u, ri.Spec.VolumeMountPaths, true
	}
	user, err := runUser(*userFlag, rt.User)
	if err != nil {
		return err
	}
	if user == "" {
		fmt.Fprintln(stderr, "warning: running as the runtime image's default user; on a cluster Brewlet runs the approved non-root RuntimeImage spec.user (use --user UID:GID or --from-cluster)")
	}
	env, err := mergeRunEnv(launch.Process.Env, envs)
	if err != nil {
		return err
	}
	for _, p := range tmpfs {
		if err := validateRunMount(p, rt); err != nil {
			return err
		}
	}
	process := launch.Process
	cmdArgs := append([]string{}, process.Entrypoint[1:]...)
	if hasExtra {
		cmdArgs = append(cmdArgs, extra...)
	} else {
		cmdArgs = append(cmdArgs, process.Args...)
	}
	build := func(appDir string) []string {
		argv := []string{"run", "--rm", "-i"}
		if *tty {
			argv = append(argv, "-t")
		}
		argv = append(argv, "--platform", platformText)
		if *name != "" {
			argv = append(argv, "--name", *name)
		}
		argv = append(argv, "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges")
		if user != "" {
			argv = append(argv, "--user", user)
		}
		argv = append(argv, "--mount", "type=bind,source="+appDir+",target=/app,readonly")
		for _, p := range tmpfs {
			argv = append(argv, "--tmpfs", p)
		}
		argv = append(argv, "--workdir", process.WorkingDir)
		for _, e := range env {
			argv = append(argv, "--env", e)
		}
		for _, p := range publish {
			argv = append(argv, "--publish", p)
		}
		if rt.ID != "" {
			argv = append(argv, "--label", api.RuntimeAnnotation+"="+rt.ID)
		}
		argv = append(argv, "--entrypoint", process.Entrypoint[0], rt.Image)
		return append(argv, cmdArgs...)
	}

	if *dryRun {
		fmt.Fprintf(stderr, "# %s: /app materialized from %s at run time (skipped by --dry-run)\n", runAppDirPlaceholder, dir)
		_, err := fmt.Fprintln(stdout, shellJoin(append([]string{*engine}, build(runAppDirPlaceholder)...)))
		return err
	}
	enginePath, err := exec.LookPath(*engine)
	if err != nil {
		return fmt.Errorf("run: container engine %q not found (use --engine or BREWLET_CONTAINER_ENGINE): %w", *engine, err)
	}
	// Materialize next to the layout, not in the system temp directory, so
	// the bind source lives in a path Docker Desktop/Podman machine share.
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp(filepath.Dir(absDir), ".brewlet-run-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	rootfs := filepath.Join(work, "rootfs")
	if err = artifact.MaterializeLayers(ctx, resolved.Layers, rootfs); err != nil {
		return fmt.Errorf("run: materialize /app: %w", err)
	}
	appDir := filepath.Join(rootfs, "app")
	if strings.Contains(appDir, ",") {
		return fmt.Errorf("run: materialization path %q must not contain a comma", appDir)
	}
	cmd := exec.CommandContext(ctx, enginePath, build(appDir)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return &exitStatusError{exitErr.ExitCode()}
	}
	return err
}

func parseRunRuntime(value string, fromCluster bool) (runRuntime, error) {
	if value == "" {
		return runRuntime{}, fmt.Errorf("run: --runtime required: IMAGE, ID=IMAGE, or ID with --from-cluster (the runtime is never guessed)")
	}
	var rt runRuntime
	if id, image, ok := strings.Cut(value, "="); ok {
		if fromCluster {
			return rt, fmt.Errorf("run: --from-cluster takes --runtime ID, not ID=IMAGE")
		}
		if image == "" {
			return rt, fmt.Errorf("run: --runtime %q has an empty image", value)
		}
		rt.ID, rt.Image = id, image
	} else if fromCluster {
		rt.ID = value
	} else {
		rt.Image = value
	}
	if rt.ID != "" {
		if err := artifact.ValidateRuntimeImageID(rt.ID); err != nil {
			return rt, fmt.Errorf("run: %w", err)
		}
	}
	return rt, nil
}

// runUser applies the §8.4 precedence: explicit --user, else the approved
// RuntimeImage user. A UID without a group uses the approved GID.
func runUser(flagValue string, approved *api.User) (string, error) {
	uid, gid := int64(-1), int64(-1)
	if approved != nil {
		uid, gid = approved.UID, approved.GID
	}
	if flagValue != "" {
		u, g, hasGroup := strings.Cut(flagValue, ":")
		var err error
		if uid, err = strconv.ParseInt(u, 10, 64); err != nil || uid < 0 {
			return "", fmt.Errorf("run: --user must be numeric UID:GID, got %q", flagValue)
		}
		if hasGroup {
			if gid, err = strconv.ParseInt(g, 10, 64); err != nil || gid < 0 {
				return "", fmt.Errorf("run: --user must be numeric UID:GID, got %q", flagValue)
			}
		} else if approved == nil {
			return "", fmt.Errorf("run: --user %s needs a GID (UID:GID) unless --from-cluster supplies the approved one", flagValue)
		}
	}
	if uid < 0 {
		return "", nil
	}
	if uid == 0 || gid == 0 {
		return "", fmt.Errorf("run: RootNotSupported: UID and GID must be non-zero, got %d:%d", uid, gid)
	}
	return fmt.Sprintf("%d:%d", uid, gid), nil
}

// mergeRunEnv overlays NAME=VALUE overrides on launch.json env in order,
// mirroring kubelet's env over image-config env; PATH stays runtime-owned.
func mergeRunEnv(base []artifact.Env, overrides []string) ([]string, error) {
	names := make([]string, 0, len(base)+len(overrides))
	values := map[string]string{}
	for _, e := range base {
		names = append(names, e.Name)
		values[e.Name] = e.Value
	}
	for _, o := range overrides {
		k, v, ok := strings.Cut(o, "=")
		if !ok || k == "" || strings.ContainsRune(o, 0) {
			return nil, fmt.Errorf("run: --env must be NAME=VALUE, got %q", o)
		}
		if k == "PATH" {
			return nil, fmt.Errorf("run: PATH is runtime-owned and cannot be overridden (use absolute paths or ecosystem variables)")
		}
		if _, seen := values[k]; !seen {
			names = append(names, k)
		}
		values[k] = v
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = n + "=" + values[n]
	}
	return out, nil
}

func validateRunMount(p string, rt runRuntime) error {
	if !path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, ",:\x00") {
		return fmt.Errorf("run: --tmpfs %q must be a canonical absolute path", p)
	}
	if strings.HasPrefix(p, "/app/") {
		return nil
	}
	for _, zone := range rt.MountPaths {
		if p == zone || strings.HasPrefix(p, zone+"/") {
			return nil
		}
	}
	if rt.FromPolicy || p == "/" || p == "/app" {
		return fmt.Errorf("run: RuntimeMountNotAllowed: --tmpfs %q is outside /app/ and RuntimeImage volumeMountPaths %q", p, rt.MountPaths)
	}
	return nil
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if shellSafe.MatchString(a) {
			quoted[i] = a
		} else {
			quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}
