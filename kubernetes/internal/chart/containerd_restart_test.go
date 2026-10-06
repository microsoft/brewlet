// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chart_test

import (
	"slices"
	"strings"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
)

func TestContainerdRestartPolicies(t *testing.T) {
	const jdks = `[{"distribution":"temurin","feature":21,"source":{"image":"registry.example.com/jdk@sha256:1111111111111111111111111111111111111111111111111111111111111111","javaHome":"/opt/java"}}]`
	for _, profile := range []string{"provisioner", "profiles[0]"} {
		base := []string{"--set", profile + ".pools={workers}", "--set-json", profile + ".jdks=" + jdks}
		if profile == "provisioner" {
			base = append(base, "--set", "defaultProfile.enabled=true")
		} else {
			base = append(base, "--set", profile+".name=workers")
		}
		modes := []string{"omitted", "validated", "none", "sighup", "reboot", "false"}
		if profile != "provisioner" {
			modes = append(modes, "null-rollout")
		}
		for _, mode := range modes {
			t.Run(profile+"/"+mode, func(t *testing.T) {
				args := slices.Clone(base)
				field := profile + ".rollout.containerdRestart"
				if mode == "null-rollout" {
					args = append(args, "--set-json", profile+".rollout=null")
				} else if mode != "omitted" {
					args = append(args, "--set", field+"="+mode)
				}
				out, err := helmCommand(t, args...).CombinedOutput()
				if mode == "sighup" || mode == "reboot" || mode == "false" {
					if err == nil || !strings.Contains(string(out), field+" must be validated or none") {
						t.Fatalf("expected policy rejection: %v\n%s", err, out)
					}
					return
				}
				if err != nil {
					t.Fatalf("render: %v\n%s", err, out)
				}
				found := false
				for _, object := range parseManifest(t, out) {
					if object.GetKind() != "NodeProfile" {
						continue
					}
					found = true
					p := convert[nodev1alpha1.NodeProfile](t, object)
					want := mode
					if mode == "omitted" || mode == "null-rollout" {
						want = ""
						if profile == "provisioner" {
							want = "validated"
						}
					}
					if p.Spec.Rollout.ContainerdRestart != want {
						t.Fatalf("rendered mode %q, want %q", p.Spec.Rollout.ContainerdRestart, want)
					}
				}
				if !found {
					t.Fatal("missing rendered NodeProfile")
				}
			})
		}
	}
}
