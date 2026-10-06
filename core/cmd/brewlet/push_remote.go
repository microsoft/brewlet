// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/microsoft/brewlet/internal/artifact"
	"github.com/microsoft/brewlet/internal/progress"
	"github.com/microsoft/brewlet/internal/registry"
)

// remotePush holds a validated registry push target.
type remotePush struct {
	target registry.Reference
	policy registry.TrustPolicy
}

// pushResultFile is the push.json handoff, schema-compatible with the Maven
// plugin's target/brewlet/push.json.
type pushResultFile struct {
	Image       string `json:"image"`
	Digest      string `json:"digest"`
	DeployImage string `json:"deployImage"`
	Format      string `json:"format"`
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// resolvePushTarget decides between a local OCI layout and a registry push.
// A ref naming a registry host goes to that registry unless --store was given
// explicitly to select a local layout. Registry-only
// flags are rejected for local pushes so nothing is silently ignored.
func resolvePushTarget(ref string, storeExplicit bool, pushResult string, insecure, realms []string) (*remotePush, error) {
	remoteOnly := pushResult != "" || len(insecure) > 0 || len(realms) > 0
	if !registry.HasExplicitRegistry(ref) {
		if remoteOnly {
			return nil, fmt.Errorf("--push-result, --insecure-registry and --allowed-token-realm apply only to a registry push, but %q has no registry host; prefix it with your registry (e.g. myregistry.azurecr.io/%s). Brewlet never defaults to Docker Hub", ref, ref)
		}
		return nil, nil
	}
	if storeExplicit {
		if remoteOnly {
			return nil, fmt.Errorf("--push-result, --insecure-registry and --allowed-token-realm apply only to a registry push; drop --store to push %s to its registry", ref)
		}
		return nil, nil
	}
	target, err := registry.ParseReference(ref)
	if err != nil {
		return nil, err
	}
	policy, err := registry.NewTrustPolicy(insecure, realms)
	if err != nil {
		return nil, err
	}
	return &remotePush{target: target, policy: policy}, nil
}

// pushToRegistry copies the object tagged localRef in the temporary store to
// the target registry and reports the digest-pinned deploy image.
func pushToRegistry(rp *remotePush, store artifact.Store, localRef, format, pushResult string) (registry.PushResult, error) {
	t := rp.target
	cred := registry.CredentialResolver{
		Diagnostics: func(msg string) { fmt.Fprintln(os.Stderr, "  warning:", msg) },
	}.Resolve(t.Registry)
	if cred != nil {
		fmt.Fprintf(os.Stderr, "  credentials: %s\n", cred.Source)
	} else {
		fmt.Fprintf(os.Stderr, "  credentials: none found for %s (anonymous)\n", t.Registry)
	}
	client, err := registry.NewClient(t.Registry, t.Repository, cred, rp.policy, nil)
	if err != nil {
		return registry.PushResult{}, err
	}
	rep := progress.New(os.Stderr)
	prog := &registry.Progress{}
	var res registry.PushResult
	err = rep.Await("pushing to "+t.Registry, func(context.Context) string { return prog.Status() }, func() error {
		var err error
		res, err = registry.PushLayout(context.Background(), store, localRef, client, t.Tag, registry.PushOptions{
			Logf:     rep.Logf,
			Progress: prog,
		})
		return err
	})
	if err != nil {
		return registry.PushResult{}, fmt.Errorf("push %s: %w", t, err)
	}
	if pushResult != "" {
		b, err := json.MarshalIndent(pushResultFile{
			Image: t.String(), Digest: res.Digest, DeployImage: t.Pinned(res.Digest), Format: format,
		}, "", "  ")
		if err != nil {
			return registry.PushResult{}, err
		}
		if dir := filepath.Dir(pushResult); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return registry.PushResult{}, err
			}
		}
		if err := os.WriteFile(pushResult, append(b, '\n'), 0o644); err != nil {
			return registry.PushResult{}, fmt.Errorf("write --push-result: %w", err)
		}
	}
	return res, nil
}
