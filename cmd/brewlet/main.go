package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/microsoft/brewlet/pkg/artifact"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func main() {
	err := run(context.Background(), os.Args[1:])
	if isHelp(err) {
		return
	}
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var exit exitCodeError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}

// exitCodeError lets subcommands such as run propagate a child exit status.
type exitCodeError interface {
	error
	ExitCode() int
}

var errUsage = errors.New("usage")

type command struct {
	name, summary string
	group         string
	run           func(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

const (
	groupBuild   = "Build and publish application images"
	groupVerify  = "Verification"
	groupLocal   = "Local testing"
	groupCluster = "Kubernetes"
	groupOther   = "Other"
)

var groups = []string{groupBuild, groupVerify, groupLocal, groupCluster, groupOther}

func commands() []command {
	return []command{
		{"build", "Package application files and launch.json into a local OCI layout", groupBuild, runBuild},
		{"push", "Upload a local build to a registry, byte for byte", groupBuild, runPush},
		{"inspect", "Show the effective launch.json and layer table of an image", groupBuild, runInspect},
		{"index", "Advanced: combine separately built platform manifests into one index", groupBuild, runIndex},
		{"verify", "Verify a pinned application's OCI graph and launch contract", groupVerify, runVerify},
		{"run", "Run a local build on a chosen runtime image with Docker or Podman", groupLocal, runLocal},
		{"k8s", "Manage runtimes on a Brewlet cluster (dashboard, list, add, rotate, remove, doctor)", groupCluster, runK8s},
		{"inventory", "Report observed application images and runtime bindings", groupCluster, runInventory},
		{"version", "Print version, commit and platform", groupOther, func(_ context.Context, args []string, stdout, _ io.Writer) error {
			return writeVersion(stdout, args)
		}},
		{"help", "Show help for a command", groupOther, nil},
	}
}

func usage(out io.Writer) {
	fmt.Fprint(out, `Brewlet packages applications without a language runtime; the runtime is
supplied by the platform (an approved RuntimeImage on each node).

Usage:
  brewlet <command> [arguments] [flags]

`)
	all := commands()
	for _, group := range groups {
		fmt.Fprintf(out, "%s:\n", group)
		for _, c := range all {
			if c.group == group {
				fmt.Fprintf(out, "  %-10s %s\n", c.name, c.summary)
			}
		}
		fmt.Fprintln(out)
	}
	fmt.Fprint(out, `Typical flow:
  brewlet build registry.example.com/apps/api:v1 --platform linux/amd64 --platform linux/arm64 \
    --launch launch.json --layer ./deps:/app/lib --layer ./build:/app
  brewlet run   registry.example.com/apps/api:v1 --runtime java-21=eclipse-temurin:21-jre
  brewlet push  registry.example.com/apps/api:v1

Run "brewlet <command> --help" for details on a command.

Output: commands print readable text by default. Add -o json (or --output json)
for machine-readable JSON; it is never chosen automatically, even when piped.
`)
}

func run(ctx context.Context, args []string) error {
	return dispatch(ctx, args, os.Stdout, os.Stderr)
}

func dispatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errUsage
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		if name == "help" && len(args) > 1 {
			return dispatch(ctx, []string{args[1], "--help"}, stdout, stderr)
		}
		usage(stdout)
		return nil
	}
	if name == "--version" {
		name = "version"
	}
	var known []string
	for _, c := range commands() {
		if c.name == name && c.run != nil {
			return c.run(ctx, args[1:], stdout, stderr)
		}
		known = append(known, c.name)
	}
	sort.Strings(known)
	return fmt.Errorf("unknown command %q; available: %s (see brewlet --help)", name, strings.Join(known, ", "))
}

// registryFlags configures registry transport for commands that talk to registries.
type registryFlags struct {
	insecure *bool
	scheme   *string
}

func addRegistryFlags(fs *flag.FlagSet) *registryFlags {
	return &registryFlags{
		insecure: fs.Bool("insecure", false, "explicitly permit plain HTTP to the registry"),
		scheme:   fs.String("registry-scheme", "", "force https, including for loopback TLS registries"),
	}
}

func (r *registryFlags) validate() error {
	if *r.scheme != "" && *r.scheme != "https" {
		return fmt.Errorf("--registry-scheme supports https; use --insecure to explicitly permit HTTP")
	}
	if *r.scheme == "https" && *r.insecure {
		return fmt.Errorf("--registry-scheme https conflicts with --insecure")
	}
	return nil
}

func (r *registryFlags) registry(ref string) (*artifact.Registry, error) {
	reg, e := artifact.NewRegistry(ref, *r.insecure)
	if e != nil {
		return nil, e
	}
	reg.Username = os.Getenv("BREWLET_REGISTRY_USERNAME")
	reg.Password = os.Getenv("BREWLET_REGISTRY_PASSWORD")
	if *r.scheme == "https" {
		reg.Origin.Scheme = "https"
	}
	return reg, nil
}

// writeResult reports a build, push or publish. The optional result file is
// always compact JSON for tooling; stdout follows format and describes the
// outcome with action (for example "Pushed image").
func writeResult(stdout io.Writer, format, action, file, ref string, target v1.Descriptor, store string) error {
	result := struct {
		Reference string `json:"reference"`
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Store     string `json:"store,omitempty"`
	}{ref, target.Digest.String(), target.MediaType, target.Size, store}
	if file != "" {
		b, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err = os.WriteFile(file, append(b, '\n'), 0644); err != nil {
			return err
		}
	}
	if format == outputJSON {
		return writeJSON(stdout, result)
	}
	if _, err := fmt.Fprintf(stdout, "%s %s\n", action, ref); err != nil {
		return err
	}
	return writeFields(stdout, "  ",
		field{"Digest", result.Digest},
		field{"Media type", result.MediaType},
		field{"Size", humanSize(result.Size)},
		field{"Store", store})
}

// withStore runs fn against an explicit layout, or a temporary one removed afterwards.
func withStore(dir string, fn func(artifact.Store) error) error {
	if dir != "" {
		return fn(artifact.Store{Dir: dir})
	}
	temp, e := os.MkdirTemp("", "brewlet-content-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(temp)
	return fn(artifact.Store{Dir: temp})
}

func publish(ctx context.Context, stdout io.Writer, reg *registryFlags, store artifact.Store, explicitStore bool, noun, format, ref, resultFile string, target v1.Descriptor) error {
	if explicitStore {
		if e := store.WriteIndex(target, ref); e != nil {
			return e
		}
		return writeResult(stdout, format, "Wrote "+noun, resultFile, ref, target, store.Dir)
	}
	r, e := reg.registry(ref)
	if e != nil {
		return e
	}
	if e = r.Push(ctx, store, target); e != nil {
		return e
	}
	return writeResult(stdout, format, "Published "+noun, resultFile, ref, target, "")
}

const buildSynopsis = `Usage: brewlet build REF --platform OS/ARCH[/VARIANT]... --launch launch.json --layer [OS/ARCH[/VARIANT]:]DIR:/app/PATH... [flags]

Packages explicitly declared directories and an author-written launch.json into
an OCI image in a local layout. Nothing is inferred: no language detection, no
entrypoint guessing, no runtime selection. Upload the result with "brewlet push".

Each --platform produces one platform manifest; repeating it builds a
multi-platform image (an OCI index) in one step. Layers apply to every platform
unless prefixed with a platform, which keeps native files per architecture:

  --layer ./build:/app
  --layer linux/amd64:./native/amd64:/app/native
  --layer linux/arm64:./native/arm64:/app/native

Libraries are ordinary application files. Layers are stacked in the order
given; the launch layer is always last. The default layout is .brewlet/images/<REF>.

Example:
  brewlet build registry.example.com/apps/api:v1 --platform linux/amd64 --platform linux/arm64 \
    --launch launch.json --layer ./venv/site-packages:/app/site-packages --layer ./src:/app/src`

func runBuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("build", buildSynopsis, stdout)
	var platforms, layers repeated
	fs.Var(&platforms, "platform", "target OS/ARCH[/VARIANT] (repeatable; required)")
	fs.Var(&layers, "layer", "[OS/ARCH[/VARIANT]:]DIR:/app/PATH (repeatable; at least one payload layer per platform)")
	launchFile := fs.String("launch", "", "author-written launch.json (required)")
	storeDir := fs.String("store", "", "output OCI layout (default .brewlet/images/<REF>)")
	resultFile := fs.String("result", "", "also write the JSON result {reference,digest,mediaType,size,store} to FILE")
	output := addOutputFlag(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if err = checkTextOutput("build", *output); err != nil {
		return err
	}
	if *launchFile == "" {
		return fmt.Errorf("--launch required")
	}
	launch, err := os.ReadFile(*launchFile)
	if err != nil {
		return err
	}
	o := artifact.BuildOptions{Launch: launch}
	for _, raw := range layers {
		s, e := artifact.ParseLayerSpec(raw)
		if e != nil {
			return e
		}
		o.Layers = append(o.Layers, s)
	}
	for _, p := range platforms {
		v, e := artifact.ParsePlatform(p)
		if e != nil {
			return e
		}
		o.Platforms = append(o.Platforms, v)
	}
	store := artifact.Store{Dir: storeForRef(ref, *storeDir)}
	target, err := artifact.Build(ctx, store, o)
	if err != nil {
		return err
	}
	if err = store.WriteIndex(target, ref); err != nil {
		return err
	}
	return writeResult(stdout, *output, "Built image", *resultFile, ref, target, store.Dir)
}

const pushSynopsis = `Usage: brewlet push REF [--store DIR] [--source REF_OR_DIGEST] [flags]

Verifies an image in a local OCI layout and uploads it to the registry named by
REF without rewriting any bytes, so the pushed digest equals the built digest.
The layout defaults to the one "brewlet build REF" wrote (.brewlet/images/<REF>);
--store selects any other layout, for example one produced by the Maven plugin.

The image is selected by --source, else by REF's name in the layout, else the
layout's only image. Credentials come from BREWLET_REGISTRY_USERNAME and
BREWLET_REGISTRY_PASSWORD.

Example:
  brewlet push registry.example.com/apps/api:v1`

var buildOnlyFlags = []string{"launch", "layer", "platform", "manifest"}

func runPush(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	for _, a := range args {
		for _, f := range buildOnlyFlags {
			if a == "--"+f || a == "-"+f || strings.HasPrefix(a, "--"+f+"=") || strings.HasPrefix(a, "-"+f+"=") {
				return fmt.Errorf("push uploads an existing build and does not accept --%s; run `brewlet build` first", f)
			}
		}
	}
	fs := newFlagSet("push", pushSynopsis, stdout)
	storeDir := fs.String("store", "", "source OCI layout (default .brewlet/images/<REF>)")
	sourceRef := fs.String("source", "", "image in the layout: reference name or sha256 digest")
	resultFile := fs.String("push-result", "", "also write the JSON result {reference,digest,mediaType,size} to FILE")
	output := addOutputFlag(fs)
	reg := addRegistryFlags(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if err = checkTextOutput("push", *output); err != nil {
		return err
	}
	if err = reg.validate(); err != nil {
		return err
	}
	dir := storeForRef(ref, *storeDir)
	if _, e := os.Stat(dir); os.IsNotExist(e) {
		if *storeDir == "" {
			return fmt.Errorf("no local build for %s in %s; run `brewlet build %s ...` first or pass --store", ref, dir, ref)
		}
		return fmt.Errorf("layout %s does not exist", dir)
	}
	store := artifact.Store{Dir: dir}
	var target v1.Descriptor
	if *sourceRef != "" {
		target, err = store.Target(*sourceRef)
	} else if target, err = store.Target(ref); err != nil {
		target, err = store.Target("")
	}
	if err != nil {
		return err
	}
	if err = artifact.VerifyGraph(ctx, store, target); err != nil {
		return err
	}
	r, err := reg.registry(ref)
	if err != nil {
		return err
	}
	if err = r.Push(ctx, store, target); err != nil {
		return err
	}
	return writeResult(stdout, *output, "Pushed image", *resultFile, ref, target, "")
}

const indexSynopsis = `Usage: brewlet index REF --manifest DESCRIPTOR.json... [--store DIR] [flags]

Advanced. Combines separately built platform manifests into one OCI index. Most
multi-platform images need only "brewlet build" with repeated --platform (and
platform-prefixed --layer flags for native files); use index when platform
manifests come from different builds or machines.`

func runIndex(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("index", indexSynopsis, stdout)
	var manifests repeated
	fs.Var(&manifests, "manifest", "OCI manifest descriptor JSON file with an explicit platform (repeatable)")
	storeDir := fs.String("store", "", "local OCI layout holding the manifests; the index is written there")
	resultFile := fs.String("push-result", "", "also write the JSON result {reference,digest,mediaType,size} to FILE")
	output := addOutputFlag(fs)
	reg := addRegistryFlags(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if err = checkTextOutput("index", *output); err != nil {
		return err
	}
	if err = reg.validate(); err != nil {
		return err
	}
	return withStore(*storeDir, func(store artifact.Store) error {
		target, e := artifact.BuildIndex(ctx, store, manifests)
		if e != nil {
			return e
		}
		return publish(ctx, stdout, reg, store, *storeDir != "", "index", *output, ref, *resultFile, target)
	})
}

const inspectSynopsis = `Usage: brewlet inspect REF [--store DIR] [--platform OS/ARCH] [flags]

Prints the effective launch.json and layer table of an application image. REF may be a
registry reference, a local OCI layout directory (DIR[@sha256:DIGEST]), or a
reference inside --store.`

func runInspect(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("inspect", inspectSynopsis, stdout)
	var platforms repeated
	fs.Var(&platforms, "platform", "show only this OS/ARCH[/VARIANT]")
	storeDir := fs.String("store", "", "local OCI layout to read REF from")
	output := addOutputFlag(fs)
	reg := addRegistryFlags(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if err = checkTextOutput("inspect", *output); err != nil {
		return err
	}
	if err = reg.validate(); err != nil {
		return err
	}
	if len(platforms) > 1 {
		return fmt.Errorf("inspect accepts at most one --platform")
	}
	var store artifact.Store
	var target v1.Descriptor
	var dir string
	if *storeDir != "" {
		store = artifact.Store{Dir: *storeDir}
		if target, err = store.Target(ref); err != nil {
			return err
		}
	} else {
		source, localTarget, isLocal, e := localLayout(ref)
		if e != nil {
			return e
		}
		if isLocal {
			store, target = source, localTarget
		} else {
			if dir, e = os.MkdirTemp("", "brewlet-inspect-"); e != nil {
				return e
			}
			defer os.RemoveAll(dir)
			store = artifact.Store{Dir: dir}
			r, e := reg.registry(ref)
			if e != nil {
				return e
			}
			if target, e = r.Pull(ctx, store); e != nil {
				return e
			}
		}
	}
	return inspectTarget(ctx, stdout, *output, store, target, platforms)
}

func inspectTarget(ctx context.Context, stdout io.Writer, format string, store artifact.Store, target v1.Descriptor, platforms []string) error {
	if len(platforms) == 0 {
		var entries []v1.Descriptor
		b, e := store.Read(target)
		if e != nil {
			return e
		}
		if target.MediaType == v1.MediaTypeImageIndex {
			var idx v1.Index
			if e = json.Unmarshal(b, &idx); e != nil {
				return e
			}
			entries = idx.Manifests
		} else {
			var m v1.Manifest
			if e = json.Unmarshal(b, &m); e != nil {
				return e
			}
			if b, e = store.Read(m.Config); e != nil {
				return e
			}
			var image v1.Image
			if e = json.Unmarshal(b, &image); e != nil {
				return e
			}
			target.Platform = &image.Platform
			entries = []v1.Descriptor{target}
		}
		var output []platformInspection
		for _, entry := range entries {
			if entry.Platform == nil {
				return fmt.Errorf("index descriptor has no explicit platform")
			}
			r, e := artifact.Resolve(ctx, store.Open, entry, *entry.Platform)
			if e != nil {
				return e
			}
			output = append(output, platformInspection{entry.Platform, r.LaunchJSON, r.Manifest.Layers})
		}
		return renderImage(stdout, format, target.Digest.String(), output)
	}
	p, e := artifact.ParsePlatform(platforms[0])
	if e != nil {
		return e
	}
	resolved, e := artifact.Resolve(ctx, store.Open, target, p)
	if e != nil {
		return e
	}
	return renderPlatform(stdout, format, platformDetail{target.Digest.String(), json.RawMessage(resolved.LaunchJSON), resolved.Manifest.Layers}, &resolved.Image.Platform)
}
