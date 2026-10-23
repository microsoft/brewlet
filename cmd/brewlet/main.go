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
	hidden        bool
	run           func(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

const (
	groupBuild   = "Build and publish application images"
	groupRelease = "Managed dependencies and release composition"
	groupLocal   = "Local testing"
	groupCluster = "Kubernetes"
	groupOther   = "Other"
)

var groups = []string{groupBuild, groupRelease, groupLocal, groupCluster, groupOther}

func commands() []command {
	return []command{
		{"build", "Package app/dependency layers and launch.json into a local OCI layout", groupBuild, false, runBuild},
		{"push", "Upload a local build to a registry, byte for byte", groupBuild, false, runPush},
		{"inspect", "Show the effective launch.json and layer table of an image or layer set", groupBuild, false, runInspect},
		{"index", "Advanced: combine separately built platform manifests into one index", groupBuild, false, runIndex},
		{"layer-set", "Publish an independently governed dependency layer set", groupRelease, false, runLayerSet},
		{"bundle", "Bundle a pinned application with one approved layer set into a signed release image", groupRelease, false, releaseCommand("bundle")},
		{"verify", "Verify a pinned release bundle against a release policy", groupRelease, false, releaseCommand("verify")},
		{"run", "Run a local build on a chosen runtime image with Docker or Podman", groupLocal, false, runLocal},
		{"k8s", "Manage runtimes on a Brewlet cluster (list, add, rotate, remove, doctor)", groupCluster, false, runK8s},
		{"inventory", "Report which dependency sets running workloads were built from", groupCluster, false, func(ctx context.Context, args []string, _, _ io.Writer) error {
			return runInventory(ctx, args)
		}},
		{"version", "Print version, commit and platform as JSON", groupOther, false, func(_ context.Context, args []string, stdout, _ io.Writer) error {
			return writeVersion(stdout, args)
		}},
		{"help", "Show help for a command", groupOther, false, nil},
		{"conformance", "Run shared producer/shim conformance fixtures (maintainers)", groupOther, true, runConformance},
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
			if c.group == group && !c.hidden {
				fmt.Fprintf(out, "  %-10s %s\n", c.name, c.summary)
			}
		}
		fmt.Fprintln(out)
	}
	fmt.Fprint(out, `Typical flow:
  brewlet build registry.example.com/apps/api:v1 --platform linux/amd64 --platform linux/arm64 \
    --launch launch.json --layer dependency=./deps:/app/lib --layer app=./build:/app
  brewlet run   registry.example.com/apps/api:v1 --runtime java-21=eclipse-temurin:21-jre
  brewlet push  registry.example.com/apps/api:v1

Run "brewlet <command> --help" for details on a command.
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
		if !c.hidden {
			known = append(known, c.name)
		}
	}
	switch name {
	case "imagepush":
		return fmt.Errorf("imagepush was folded into push: use `brewlet push REF --store DIR [--source REF_OR_DIGEST]`")
	case "compose":
		return fmt.Errorf("compose was renamed: use `brewlet bundle`")
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

// trustFlags select publishers trusted for consumed layer sets.
type trustFlags struct {
	keys     repeated
	identity *string
	require  *bool
}

func addTrustFlags(fs *flag.FlagSet) *trustFlags {
	t := &trustFlags{}
	fs.Var(&t.keys, "trust-key", "SPKI public PEM trusted for layer-set provenance (repeatable)")
	t.identity = fs.String("signer-identity", "", "expected trusted layer-set signer identity")
	t.require = fs.Bool("require-provenance", false, "require trusted signed layer-set provenance")
	return t
}

func (t *trustFlags) load() (artifact.LayerSetTrust, error) {
	trust := artifact.LayerSetTrust{ExpectedIdentity: *t.identity, RequireProvenance: *t.require}
	for _, file := range t.keys {
		b, e := os.ReadFile(file)
		if e != nil {
			return trust, e
		}
		key, e := artifact.ParseTrustKey(b)
		if e != nil {
			return trust, e
		}
		trust.PublicKeys = append(trust.PublicKeys, key)
	}
	if (len(trust.PublicKeys) > 0 || trust.RequireProvenance || trust.ExpectedIdentity != "") && (len(trust.PublicKeys) == 0 || trust.ExpectedIdentity == "") {
		return trust, fmt.Errorf("--trust-key and --signer-identity required together")
	}
	return trust, nil
}

func writeResult(stdout io.Writer, file, ref string, target v1.Descriptor, store string) error {
	result := struct {
		Reference string `json:"reference"`
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
		Store     string `json:"store,omitempty"`
	}{ref, target.Digest.String(), target.MediaType, target.Size, store}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if file != "" {
		if err = os.WriteFile(file, append(b, '\n'), 0644); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(stdout, string(b))
	return err
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

func publish(ctx context.Context, stdout io.Writer, reg *registryFlags, store artifact.Store, explicitStore bool, ref, resultFile string, target v1.Descriptor) error {
	if explicitStore {
		if e := store.WriteIndex(target, ref); e != nil {
			return e
		}
		return writeResult(stdout, resultFile, ref, target, store.Dir)
	}
	r, e := reg.registry(ref)
	if e != nil {
		return e
	}
	if e = r.Push(ctx, store, target); e != nil {
		return e
	}
	return writeResult(stdout, resultFile, ref, target, "")
}

const buildSynopsis = `Usage: brewlet build REF --platform OS/ARCH[/VARIANT]... --launch launch.json --layer ROLE=DIR:/app/PATH... [flags]

Packages explicitly declared directories and an author-written launch.json into
an OCI image in a local layout. Nothing is inferred: no language detection, no
entrypoint guessing, no runtime selection. Upload the result with "brewlet push".

Each --platform produces one platform manifest; repeating it builds a
multi-platform image (an OCI index) in one step. Layers apply to every platform
unless prefixed with a platform, which keeps native files per architecture:

  --layer app=./build:/app
  --layer linux/amd64:dependency=./native/amd64:/app/native
  --layer linux/arm64:dependency=./native/arm64:/app/native

Layers are stacked in the order given; --layer-set layers come first and the
launch layer is always last. The default layout is .brewlet/images/<REF>.

Example:
  brewlet build registry.example.com/apps/api:v1 --platform linux/amd64 --platform linux/arm64 \
    --launch launch.json --layer dependency=./venv/site-packages:/app/site-packages --layer app=./src:/app/src`

func runBuild(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("build", buildSynopsis, stdout)
	var platforms, layers, sets repeated
	fs.Var(&platforms, "platform", "target OS/ARCH[/VARIANT] (repeatable; required)")
	fs.Var(&layers, "layer", "[OS/ARCH:]app|dependency=DIR:/app/PATH (repeatable; at least one app layer per platform)")
	fs.Var(&sets, "layer-set", "dependency layer set: registry ref or local DIR[@sha256:DIGEST] (repeatable)")
	launchFile := fs.String("launch", "", "author-written launch.json (required)")
	storeDir := fs.String("store", "", "output OCI layout (default .brewlet/images/<REF>)")
	resultFile := fs.String("result", "", "also write the JSON result {reference,digest,mediaType,size,store} to FILE")
	trustOpts := addTrustFlags(fs)
	reg := addRegistryFlags(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if err = reg.validate(); err != nil {
		return err
	}
	if *launchFile == "" {
		return fmt.Errorf("--launch required")
	}
	trust, err := trustOpts.load()
	if err != nil {
		return err
	}
	launch, err := os.ReadFile(*launchFile)
	if err != nil {
		return err
	}
	o := artifact.BuildOptions{Launch: launch, LayerSetTrust: trust}
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
	for _, setRef := range sets {
		var d v1.Descriptor
		source, localTarget, isLocal, e := localLayerSet(setRef)
		if e != nil {
			return e
		}
		if isLocal {
			d = localTarget
			if e = artifact.CopyGraph(ctx, source, store, d); e != nil {
				return e
			}
		} else {
			r, e := reg.registry(setRef)
			if e != nil {
				return e
			}
			if d, e = r.Pull(ctx, store); e != nil {
				return e
			}
		}
		o.LayerSets = append(o.LayerSets, d)
	}
	target, err := artifact.Build(ctx, store, o)
	if err != nil {
		return err
	}
	if err = store.WriteIndex(target, ref); err != nil {
		return err
	}
	return writeResult(stdout, *resultFile, ref, target, store.Dir)
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

var buildOnlyFlags = []string{"launch", "layer", "layer-set", "platform", "config", "lock", "manifest"}

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
	reg := addRegistryFlags(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
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
	return writeResult(stdout, *resultFile, ref, target, "")
}

const layerSetSynopsis = `Usage: brewlet layer-set REF --config CONFIG.json --layer dependency=DIR:/app/PATH... [flags]

Publishes an independently governed dependency layer set that applications
consume with "brewlet build --layer-set". Without --store the set is published
to the registry named by REF; with --store it is written to that local layout.

Example:
  brewlet layer-set registry.example.com/platform/deps:2026.10 --config set.json --lock lock.json \
    --layer dependency=./deps:/app/lib --signing-key key.pem --builder-identity https://ci.example.com/deps`

func runLayerSet(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("layer-set", layerSetSynopsis, stdout)
	var layers repeated
	fs.Var(&layers, "layer", "dependency=DIR:/app/PATH (repeatable)")
	configFile := fs.String("config", "", "layer set config JSON (required)")
	lockFile := fs.String("lock", "", "optional layer set lock JSON")
	sbomFile := fs.String("sbom", "", "explicit CycloneDX 1.5 inventory; otherwise generated from lock/files")
	signingKey := fs.String("signing-key", "", "PKCS#8 ECDSA-P256 private PEM for publisher provenance")
	builderIdentity := fs.String("builder-identity", "", "identity bound into the provenance (requires --signing-key)")
	storeDir := fs.String("store", "", "write to this local OCI layout instead of publishing")
	resultFile := fs.String("push-result", "", "also write the JSON result {reference,digest,mediaType,size} to FILE")
	trustOpts := addTrustFlags(fs)
	reg := addRegistryFlags(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if err = reg.validate(); err != nil {
		return err
	}
	if *configFile == "" {
		return fmt.Errorf("--config required")
	}
	trust, err := trustOpts.load()
	if err != nil {
		return err
	}
	var specs []artifact.LayerSpec
	for _, raw := range layers {
		s, e := artifact.ParseLayerSpec(raw)
		if e != nil {
			return e
		}
		specs = append(specs, s)
	}
	b, err := os.ReadFile(*configFile)
	if err != nil {
		return err
	}
	var c artifact.LayerSetConfig
	if err = artifact.StrictJSON(b, &c); err != nil {
		return err
	}
	var lock *artifact.LayerSetLock
	if *lockFile != "" {
		if b, err = os.ReadFile(*lockFile); err != nil {
			return err
		}
		lock = &artifact.LayerSetLock{}
		if err = artifact.StrictJSON(b, lock); err != nil {
			return err
		}
	}
	options := artifact.LayerSetPublishOptions{Trust: trust}
	if *sbomFile != "" {
		if options.SBOM, err = os.ReadFile(*sbomFile); err != nil {
			return err
		}
	}
	if *signingKey != "" {
		b, e := os.ReadFile(*signingKey)
		if e != nil {
			return e
		}
		key, e := artifact.ParseSigningKey(b)
		if e != nil {
			return e
		}
		options.Signer = &artifact.LayerSetSigner{Key: key, Identity: *builderIdentity}
	} else if *builderIdentity != "" {
		return fmt.Errorf("--builder-identity requires --signing-key")
	}
	return withStore(*storeDir, func(store artifact.Store) error {
		target, e := artifact.BuildLayerSet(ctx, store, c, specs, lock, options)
		if e != nil {
			return e
		}
		return publish(ctx, stdout, reg, store, *storeDir != "", ref, *resultFile, target)
	})
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
	reg := addRegistryFlags(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
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
		return publish(ctx, stdout, reg, store, *storeDir != "", ref, *resultFile, target)
	})
}

const inspectSynopsis = `Usage: brewlet inspect REF [--store DIR] [--platform OS/ARCH] [flags]

Prints the effective launch.json and layer table of an application image, or
the config, lock, layers and provenance evidence of a layer set. REF may be a
registry reference, a local OCI layout directory (DIR[@sha256:DIGEST]), or a
reference inside --store.`

func runInspect(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("inspect", inspectSynopsis, stdout)
	var platforms repeated
	fs.Var(&platforms, "platform", "show only this OS/ARCH[/VARIANT]")
	storeDir := fs.String("store", "", "local OCI layout to read REF from")
	trustOpts := addTrustFlags(fs)
	reg := addRegistryFlags(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if err = reg.validate(); err != nil {
		return err
	}
	if len(platforms) > 1 {
		return fmt.Errorf("inspect accepts at most one --platform")
	}
	trust, err := trustOpts.load()
	if err != nil {
		return err
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
		source, localTarget, isLocal, e := localLayerSet(ref)
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
	return inspectTarget(ctx, stdout, store, target, platforms, trust)
}

func inspectTarget(ctx context.Context, stdout io.Writer, store artifact.Store, target v1.Descriptor, platforms []string, trust artifact.LayerSetTrust) error {
	out := json.NewEncoder(stdout)
	if target.MediaType == v1.MediaTypeImageManifest {
		b, e := store.Read(target)
		if e != nil {
			return e
		}
		var manifest v1.Manifest
		if e = json.Unmarshal(b, &manifest); e != nil {
			return e
		}
		if manifest.ArtifactType == artifact.MediaTypeLayerSet {
			layers, evidence, e := artifact.ConsumeLayerSet(ctx, store, target, trust)
			if e != nil {
				return e
			}
			config, e := store.Read(manifest.Config)
			if e != nil {
				return e
			}
			var lock json.RawMessage
			for _, descriptor := range manifest.Layers {
				if descriptor.MediaType == artifact.MediaTypeLayerSetLock {
					if lock, e = store.Read(descriptor); e != nil {
						return e
					}
				}
			}
			return out.Encode(struct {
				Digest   string                    `json:"digest"`
				Config   json.RawMessage           `json:"layerSet"`
				Lock     json.RawMessage           `json:"lock,omitempty"`
				Layers   []artifact.BuiltLayer     `json:"layers"`
				Evidence artifact.LayerSetEvidence `json:"evidence"`
			}{target.Digest.String(), config, lock, layers, evidence})
		}
	}
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
		type platformInspection struct {
			Platform *v1.Platform    `json:"platform"`
			Launch   json.RawMessage `json:"launch"`
			Layers   []v1.Descriptor `json:"layers"`
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
		return out.Encode(struct {
			Digest    string               `json:"digest"`
			Platforms []platformInspection `json:"platforms"`
		}{target.Digest.String(), output})
	}
	p, e := artifact.ParsePlatform(platforms[0])
	if e != nil {
		return e
	}
	resolved, e := artifact.Resolve(ctx, store.Open, target, p)
	if e != nil {
		return e
	}
	return out.Encode(struct {
		Digest string          `json:"digest"`
		Launch json.RawMessage `json:"launch"`
		Layers []v1.Descriptor `json:"layers"`
	}{target.Digest.String(), json.RawMessage(resolved.LaunchJSON), resolved.Manifest.Layers})
}

const conformanceSynopsis = `Usage: brewlet conformance FILE

Maintainer command: executes shared producer/shim conformance fixtures.`

func runConformance(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("conformance", conformanceSynopsis, stdout)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: brewlet conformance FILE")
	}
	b, e := os.ReadFile(fs.Arg(0))
	if e != nil {
		return e
	}
	results, e := artifact.RunFixtures(ctx, b)
	if e != nil {
		return e
	}
	if e = json.NewEncoder(stdout).Encode(results); e != nil {
		return e
	}
	for _, r := range results {
		if !r.Passed {
			return fmt.Errorf("conformance fixture failed: %s", r.Name)
		}
	}
	return nil
}
