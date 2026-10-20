package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/microsoft/brewlet/pkg/artifact"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }
func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: brewlet push|compose|verify|inventory|imagepush|inspect|layer-set|index REF [options]")
	}
	if args[0] == "compose" || args[0] == "verify" {
		return runRelease(ctx, args)
	}
	if args[0] == "inventory" {
		return runInventory(ctx, args[1:])
	}
	if args[0] == "conformance" {
		if len(args) != 2 {
			return fmt.Errorf("usage: brewlet conformance FILE")
		}
		b, e := os.ReadFile(args[1])
		if e != nil {
			return e
		}
		results, e := artifact.RunFixtures(ctx, b)
		if e != nil {
			return e
		}
		if e = json.NewEncoder(os.Stdout).Encode(results); e != nil {
			return e
		}
		for _, r := range results {
			if !r.Passed {
				return fmt.Errorf("conformance fixture failed: %s", r.Name)
			}
		}
		return nil
	}
	command := args[0]
	args = args[1:]
	ref := ""
	// Accept the documented verb REF --flags form as well as --flags REF.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref = args[0]
		args = args[1:]
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	var platforms, layers, sets, manifests, trustKeys repeated
	fs.Var(&platforms, "platform", "explicit os/arch[/variant] (repeatable)")
	fs.Var(&layers, "layer", "app=directory:/app/path or dependency=directory:/app/path")
	fs.Var(&sets, "layer-set", "layer set reference (repeatable; local DIR[@sha256:DIGEST] or registry ref)")
	fs.Var(&manifests, "manifest", "explicit OCI manifest descriptor JSON file (index only)")
	fs.Var(&trustKeys, "trust-key", "SPKI public PEM trusted for layer-set provenance (repeatable)")
	storeDir := fs.String("store", "", "local OCI layout destination/source; omit to publish remotely")
	sourceRef := fs.String("source", "", "local root reference or sha256 digest (imagepush only; default unique root)")
	launchFile := fs.String("launch", "", "author-written launch.json")
	resultFile := fs.String("push-result", "", "write JSON {reference,digest,mediaType,size}")
	insecure := fs.Bool("insecure", false, "explicitly permit HTTP to this registry")
	registryScheme := fs.String("registry-scheme", "", "explicit https, including loopback TLS registries")
	configFile := fs.String("config", "", "layer set config JSON")
	lockFile := fs.String("lock", "", "optional layer set lock JSON")
	sbomFile := fs.String("sbom", "", "explicit CycloneDX 1.5 inventory; otherwise generated from lock/files")
	signingKey := fs.String("signing-key", "", "PKCS#8 ECDSA-P256 private PEM (layer-set only)")
	builderIdentity := fs.String("builder-identity", "", "explicit identity bound into layer-set provenance")
	signerIdentity := fs.String("signer-identity", "", "expected trusted layer-set signer identity")
	requireProvenance := fs.Bool("require-provenance", false, "require trusted signed layer-set provenance")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if ref == "" && fs.NArg() == 1 {
		ref = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if ref == "" {
		return fmt.Errorf("explicit reference required")
	}
	if command != "layer-set" && (*signingKey != "" || *builderIdentity != "" || *sbomFile != "") {
		return fmt.Errorf("--signing-key, --builder-identity and --sbom are layer-set publication options")
	}
	if command == "imagepush" && *storeDir == "" {
		return fmt.Errorf("imagepush requires --store OCI_LAYOUT")
	}
	if command == "imagepush" && (len(platforms) > 0 || len(layers) > 0 || len(sets) > 0 || len(manifests) > 0 || *launchFile != "" || *configFile != "" || *lockFile != "" || len(trustKeys) > 0 || *signerIdentity != "" || *requireProvenance) {
		return fmt.Errorf("imagepush verifies and copies OCI bytes only; build and Brewlet trust options do not apply")
	}
	if command != "imagepush" && *sourceRef != "" {
		return fmt.Errorf("--source is an imagepush option")
	}
	if *registryScheme != "" && *registryScheme != "https" {
		return fmt.Errorf("--registry-scheme supports https; use --insecure to explicitly permit HTTP")
	}
	if *registryScheme == "https" && *insecure {
		return fmt.Errorf("--registry-scheme https conflicts with --insecure")
	}
	dir := *storeDir
	if dir == "" && command == "inspect" {
		local, _, isLocal, e := localLayerSet(ref)
		if e != nil {
			return e
		}
		if isLocal {
			dir = local.Dir
		}
	}
	cleanup := false
	if dir == "" {
		if e := os.MkdirAll(".brewlet", 0700); e != nil {
			return e
		}
		var e error
		dir, e = os.MkdirTemp(".brewlet", "content-")
		if e != nil {
			return e
		}
		cleanup = true
	}
	if cleanup {
		defer os.RemoveAll(dir)
	}
	store := artifact.Store{Dir: dir}
	trust := artifact.LayerSetTrust{ExpectedIdentity: *signerIdentity, RequireProvenance: *requireProvenance}
	for _, file := range trustKeys {
		b, e := os.ReadFile(file)
		if e != nil {
			return e
		}
		key, e := artifact.ParseTrustKey(b)
		if e != nil {
			return e
		}
		trust.PublicKeys = append(trust.PublicKeys, key)
	}
	if (len(trust.PublicKeys) > 0 || trust.RequireProvenance || trust.ExpectedIdentity != "") && (len(trust.PublicKeys) == 0 || trust.ExpectedIdentity == "") {
		return fmt.Errorf("--trust-key and --signer-identity required together")
	}
	registry := func(ref string) (*artifact.Registry, error) {
		r, e := artifact.NewRegistry(ref, *insecure)
		if e != nil {
			return nil, e
		}
		r.Username = os.Getenv("BREWLET_REGISTRY_USERNAME")
		r.Password = os.Getenv("BREWLET_REGISTRY_PASSWORD")
		if *registryScheme == "https" {
			r.Origin.Scheme = "https"
		}
		return r, nil
	}
	var target v1.Descriptor
	var err error
	switch command {
	case "imagepush":
		target, err = store.Target(*sourceRef)
		if err == nil {
			err = artifact.VerifyGraph(ctx, store, target)
		}
	case "push", "layer-set":
		var specs []artifact.LayerSpec
		for _, raw := range layers {
			s, e := artifact.ParseLayerSpec(raw)
			if e != nil {
				return e
			}
			specs = append(specs, s)
		}
		if command == "layer-set" {
			if *configFile == "" {
				return fmt.Errorf("--config required")
			}
			b, e := os.ReadFile(*configFile)
			if e != nil {
				return e
			}
			var c artifact.LayerSetConfig
			if e = artifact.StrictJSON(b, &c); e != nil {
				return e
			}
			var lock *artifact.LayerSetLock
			if *lockFile != "" {
				b, e = os.ReadFile(*lockFile)
				if e != nil {
					return e
				}
				lock = &artifact.LayerSetLock{}
				if e = artifact.StrictJSON(b, lock); e != nil {
					return e
				}
			}
			options := artifact.LayerSetPublishOptions{Trust: trust}
			if *sbomFile != "" {
				options.SBOM, e = os.ReadFile(*sbomFile)
				if e != nil {
					return e
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
			target, err = artifact.BuildLayerSet(ctx, store, c, specs, lock, options)
		} else {
			if *launchFile == "" {
				return fmt.Errorf("--launch required")
			}
			b, e := os.ReadFile(*launchFile)
			if e != nil {
				return e
			}
			o := artifact.BuildOptions{Launch: b, Layers: specs, LayerSetTrust: trust}
			for _, p := range platforms {
				v, e := artifact.ParsePlatform(p)
				if e != nil {
					return e
				}
				o.Platforms = append(o.Platforms, v)
			}
			for _, setRef := range sets {
				var d v1.Descriptor
				// A directory containing an OCI layout is an explicit local input.
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
					r, e := registry(setRef)
					if e != nil {
						return e
					}
					d, e = r.Pull(ctx, store)
					if e != nil {
						return e
					}
				}
				o.LayerSets = append(o.LayerSets, d)
			}
			target, err = artifact.Build(ctx, store, o)
		}
	case "index":
		target, err = artifact.BuildIndex(ctx, store, manifests)
	case "inspect":
		if *storeDir == "" {
			source, localTarget, isLocal, e := localLayerSet(ref)
			if e != nil {
				return e
			}
			if isLocal {
				store = source
				target = localTarget
			} else {
				r, e := registry(ref)
				if e != nil {
					return e
				}
				target, err = r.Pull(ctx, store)
			}
		} else {
			target, err = store.Target(ref)
		}
		if err != nil {
			return err
		}
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
						lock, e = store.Read(descriptor)
						if e != nil {
							return e
						}
					}
				}
				return json.NewEncoder(os.Stdout).Encode(struct {
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
			if target.MediaType == v1.MediaTypeImageIndex {
				b, e := store.Read(target)
				if e != nil {
					return e
				}
				var idx v1.Index
				if e = json.Unmarshal(b, &idx); e != nil {
					return e
				}
				entries = idx.Manifests
			} else {
				b, e := store.Read(target)
				if e != nil {
					return e
				}
				var m v1.Manifest
				if e = json.Unmarshal(b, &m); e != nil {
					return e
				}
				b, e = store.Read(m.Config)
				if e != nil {
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
			return json.NewEncoder(os.Stdout).Encode(struct {
				Digest    string               `json:"digest"`
				Platforms []platformInspection `json:"platforms"`
			}{target.Digest.String(), output})
		}
		if len(platforms) != 1 {
			return fmt.Errorf("inspect accepts at most one --platform")
		}
		p, e := artifact.ParsePlatform(platforms[0])
		if e != nil {
			return e
		}
		resolved, e := artifact.Resolve(ctx, store.Open, target, p)
		if e != nil {
			return e
		}
		var raw json.RawMessage = resolved.LaunchJSON
		return json.NewEncoder(os.Stdout).Encode(struct {
			Digest string          `json:"digest"`
			Launch json.RawMessage `json:"launch"`
			Layers []v1.Descriptor `json:"layers"`
		}{target.Digest.String(), raw, resolved.Manifest.Layers})
	default:
		return fmt.Errorf("unknown command %q", command)
	}
	if err != nil {
		return err
	}
	if *storeDir != "" && command != "imagepush" {
		if err = store.WriteIndex(target, ref); err != nil {
			return err
		}
	} else {
		r, e := registry(ref)
		if e != nil {
			return e
		}
		if e = r.Push(ctx, store, target); e != nil {
			return e
		}
	}
	result := struct {
		Reference string `json:"reference"`
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
	}{ref, target.Digest.String(), target.MediaType, target.Size}
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if *resultFile != "" {
		if err = os.WriteFile(*resultFile, append(b, '\n'), 0644); err != nil {
			return err
		}
	}
	fmt.Println(string(b))
	return nil
}
