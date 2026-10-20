package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/microsoft/brewlet/pkg/artifact"
	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"sigs.k8s.io/yaml"
)

type registryCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type releaseRegistryOptions struct {
	Insecure    bool
	HTTPS       bool
	Credentials map[string]registryCredential
}

func (o releaseRegistryOptions) registry(ref string) (*artifact.Registry, error) {
	registry, err := artifact.NewRegistry(ref, o.Insecure)
	if err != nil {
		return nil, err
	}
	if o.HTTPS {
		registry.Origin.Scheme = "https"
	}
	registry.Client.Timeout = 5 * time.Minute
	credential := o.Credentials[registry.Origin.String()]
	registry.Username, registry.Password = credential.Username, credential.Password
	return registry, nil
}

func pinnedReleaseInput(ctx context.Context, store artifact.Store, ref string, options releaseRegistryOptions) (v1.Descriptor, error) {
	if !strings.Contains(ref, "@sha256:") {
		return v1.Descriptor{}, fmt.Errorf("digest-pinned input required: %s", ref)
	}
	local, target, isLocal, err := localLayerSet(ref)
	if err != nil {
		return target, err
	}
	if isLocal {
		err = artifact.CopyGraph(ctx, local, store, target)
		return target, err
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return target, err
	}
	if named.String() != ref {
		return target, fmt.Errorf("canonical digest reference required: %s", ref)
	}
	if _, ok := named.(reference.Digested); !ok {
		return target, fmt.Errorf("digest reference required")
	}
	if _, ok := named.(reference.Tagged); ok {
		return target, fmt.Errorf("tag plus digest is not a canonical release input")
	}
	registry, err := options.registry(ref)
	if err != nil {
		return target, err
	}
	return registry.Pull(ctx, store)
}

func readReleasePolicy(file string) (artifact.ReleasePolicy, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return artifact.ReleasePolicy{}, err
	}
	if !json.Valid(raw) {
		raw, err = yaml.YAMLToJSONStrict(raw)
		if err != nil {
			return artifact.ReleasePolicy{}, err
		}
	}
	return artifact.ParseReleasePolicy(raw)
}

func atomicReleaseResult(file string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(file), ".brewlet-result-")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), file)
}

func runRelease(ctx context.Context, args []string) error {
	command := args[0]
	args = args[1:]
	ref := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		ref, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	var applications, sets, platforms repeated
	fs.Var(&applications, "application", "digest-pinned application artifact")
	fs.Var(&sets, "layer-set", "one digest-pinned complete managed dependency set")
	fs.Var(&platforms, "platform", "one explicit linux/arch[/variant]")
	policyFile := fs.String("policy", "", "explicit release policy JSON or YAML")
	storeDir := fs.String("store", "", "local output layout (compose only)")
	keyFile := fs.String("signing-key", "", "PKCS#8 P-256 composer key file")
	identity := fs.String("builder-identity", "", "authorized release composer identity")
	launchFile := fs.String("launch", "", "explicit launch replacement")
	resultFile := fs.String("push-result", "", "atomic successful composition receipt")
	authFile := fs.String("registry-auth", "", "JSON credentials keyed by exact scheme://host[:port]")
	insecure := fs.Bool("insecure", false, "explicitly allow HTTP registries")
	scheme := fs.String("registry-scheme", "", "explicit https for loopback TLS")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if ref == "" && fs.NArg() == 1 {
		ref = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	if ref == "" || *policyFile == "" || len(platforms) != 1 {
		return fmt.Errorf("%s requires REF, --policy and exactly one --platform", command)
	}
	if *scheme != "" && *scheme != "https" || *scheme == "https" && *insecure {
		return fmt.Errorf("invalid/conflicting registry scheme")
	}
	if command == "verify" && (len(applications) > 0 || len(sets) > 0 || *keyFile != "" || *identity != "" || *launchFile != "" || *storeDir != "" || *resultFile != "") {
		return fmt.Errorf("verify accepts a pinned input, policy and platform, not composition options")
	}
	if command == "compose" && (len(applications) != 1 || len(sets) != 1) {
		return fmt.Errorf("compose requires exactly one --application and --layer-set")
	}
	policy, err := readReleasePolicy(*policyFile)
	if err != nil {
		return err
	}
	platform, err := artifact.ParsePlatform(platforms[0])
	if err != nil {
		return err
	}
	registries, err := loadReleaseRegistries(*authFile, *insecure, *scheme == "https")
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "brewlet-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	store := artifact.Store{Dir: temp}
	if command == "verify" {
		target, err := pinnedReleaseInput(ctx, store, ref, registries)
		if err != nil {
			return err
		}
		result, err := artifact.VerifyComposition(ctx, store, target, platform, policy)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(result)
	}
	application, err := pinnedReleaseInput(ctx, store, applications[0], registries)
	if err != nil {
		return err
	}
	dependency, err := pinnedReleaseInput(ctx, store, sets[0], registries)
	if err != nil {
		return err
	}
	options := artifact.ComposeOptions{Application: application, DependencySet: dependency, Platform: platform, Policy: policy}
	if *launchFile != "" {
		options.Launch, err = os.ReadFile(*launchFile)
		if err != nil {
			return err
		}
	}
	if *keyFile != "" {
		raw, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		key, err := artifact.ParseSigningKey(raw)
		if err != nil {
			return err
		}
		options.Signer = &artifact.LayerSetSigner{Key: key, Identity: *identity}
	} else if *identity != "" {
		return fmt.Errorf("--builder-identity requires --signing-key")
	}
	target, inspection, err := artifact.Compose(ctx, store, options)
	if err != nil {
		return err
	}
	if *storeDir != "" {
		destination := artifact.Store{Dir: *storeDir}
		if err = artifact.CopyGraph(ctx, store, destination, target); err != nil {
			return err
		}
		if err = destination.WriteIndex(target, ref); err != nil {
			return err
		}
	} else {
		registry, err := registries.registry(ref)
		if err != nil {
			return err
		}
		if err = registry.Push(ctx, store, target); err != nil {
			return err
		}
	}
	result := struct {
		Reference    string                         `json:"reference"`
		Digest       digest.Digest                  `json:"digest"`
		MediaType    string                         `json:"mediaType"`
		Size         int64                          `json:"size"`
		Verification artifact.CompositionInspection `json:"verification"`
	}{ref, target.Digest, target.MediaType, target.Size, inspection}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *resultFile != "" {
		if err = atomicReleaseResult(*resultFile, data); err != nil {
			return err
		}
	}
	_, err = os.Stdout.Write(data)
	return err
}

func loadReleaseRegistries(authFile string, insecure, https bool) (releaseRegistryOptions, error) {
	registries := releaseRegistryOptions{Insecure: insecure, HTTPS: https, Credentials: map[string]registryCredential{}}
	if authFile != "" {
		raw, err := os.ReadFile(authFile)
		if err != nil {
			return registries, err
		}
		if err = artifact.StrictJSON(raw, &registries.Credentials); err != nil {
			return registries, err
		}
		if registries.Credentials == nil {
			return registries, fmt.Errorf("registry-auth must be a nonnull object")
		}
	}
	if username, password := os.Getenv("BREWLET_REGISTRY_USERNAME"), os.Getenv("BREWLET_REGISTRY_PASSWORD"); username != "" || password != "" {
		origin := os.Getenv("BREWLET_REGISTRY_ORIGIN")
		if origin == "" {
			return registries, fmt.Errorf("release commands require BREWLET_REGISTRY_ORIGIN for environment credentials; alternatively use --registry-auth")
		}
		if _, exists := registries.Credentials[origin]; exists {
			return registries, fmt.Errorf("duplicate registry credential origin")
		}
		registries.Credentials[origin] = registryCredential{username, password}
	}
	for origin := range registries.Credentials {
		probe, err := artifact.NewRegistry(strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")+"/origin-check", insecure)
		if err != nil {
			return registries, err
		}
		if strings.HasPrefix(origin, "https://") {
			probe.Origin.Scheme = "https"
		}
		if probe.Origin.String() != origin {
			return registries, fmt.Errorf("credential key must be an exact registry origin")
		}
	}
	return registries, nil
}
