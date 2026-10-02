// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/microsoft/brewlet/internal/artifact"
	"github.com/microsoft/brewlet/internal/registry/registrytest"
)

// isolateCredentials points credential discovery at an empty Docker config
// so tests never read the developer's real credentials.
func isolateCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("BREWLET_REGISTRY_USERNAME", "")
	t.Setenv("BREWLET_REGISTRY_PASSWORD", "")
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
}

func testJar(t *testing.T) string {
	t.Helper()
	jar := filepath.Join(t.TempDir(), "orders.jar")
	writeCLIZip(t, jar, "com/example/Orders.class")
	return jar
}

func TestResolvePushTarget(t *testing.T) {
	cases := []struct {
		name       string
		ref        string
		store      bool
		pushRes    string
		insecure   []string
		wantRemote bool
		wantErr    string
	}{
		{name: "host-less is local", ref: "demo/hello:1"},
		{name: "host-less never Docker Hub", ref: "hello:1", pushRes: "p.json", wantErr: "never defaults to Docker Hub"},
		{name: "registry host is remote", ref: "myacr.azurecr.io/team/app:1", wantRemote: true},
		{name: "explicit store keeps local", ref: "registry.example.com/team/app:1.4.2", store: true},
		{name: "remote flag with store", ref: "registry.example.com/team/app:1", store: true, insecure: []string{"registry.example.com"}, wantErr: "drop --store"},
		{name: "digest ref rejected", ref: "myacr.azurecr.io/app@sha256:0123456789012345678901234567890123456789012345678901234567890123", wantErr: "digest-pinned"},
		{name: "bad insecure value", ref: "myacr.azurecr.io/app:1", insecure: []string{"https://myacr.azurecr.io"}, wantErr: "bare registry authority"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rp, err := resolvePushTarget(c.ref, c.store, c.pushRes, c.insecure, nil)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (rp != nil) != c.wantRemote {
				t.Fatalf("remote = %v, want %v", rp != nil, c.wantRemote)
			}
		})
	}
}

func TestCmdPushRemoteRejectsDependencyBundle(t *testing.T) {
	isolateCredentials(t)
	err := cmdPush([]string{testJar(t), "myacr.azurecr.io/apps/orders:1", "--dependency-bundle", "platform/approved:1"})
	if err == nil || !strings.Contains(err.Error(), "--dependency-bundle is not supported when pushing to a registry") {
		t.Fatalf("err = %v", err)
	}
}

func TestCmdPushExplicitStoreStaysLocal(t *testing.T) {
	isolateCredentials(t)
	store := filepath.Join(t.TempDir(), "oci")
	ref := "registry.example.com/team/orders:1"
	if err := cmdPush([]string{testJar(t), ref, "--store", store}); err != nil {
		t.Fatal(err)
	}
	if _, err := (artifact.Store{Root: store}).DescriptorByRef(ref); err != nil {
		t.Fatalf("local layout missing %s: %v", ref, err)
	}
}

func TestCmdPushToRegistry(t *testing.T) {
	for _, format := range []string{"image", "artifact"} {
		t.Run(format, func(t *testing.T) {
			isolateCredentials(t)
			reg := registrytest.New(registrytest.Basic)
			reg.Username, reg.Password = "ci-bot", "pw"
			defer reg.Close()
			t.Setenv("BREWLET_REGISTRY_USERNAME", "ci-bot")
			t.Setenv("BREWLET_REGISTRY_PASSWORD", "pw")

			ref := reg.Host() + "/team/orders:1.2.3"
			pushResult := filepath.Join(t.TempDir(), "out", "push.json")
			if err := cmdPush([]string{testJar(t), ref, "--format", format, "--push-result", pushResult}); err != nil {
				t.Fatal(err)
			}
			m, ok := reg.Manifest("team/orders", "1.2.3")
			if !ok {
				t.Fatalf("tag not pushed; puts=%v", reg.ManifestPuts())
			}
			wantType := artifact.OCIImageIndexMediaType
			if format == "artifact" {
				wantType = "application/vnd.oci.image.manifest.v1+json"
			}
			if m.MediaType != wantType {
				t.Fatalf("media type %q, want %q", m.MediaType, wantType)
			}

			raw, err := os.ReadFile(pushResult)
			if err != nil {
				t.Fatal(err)
			}
			var got pushResultFile
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			digest := registrytest.Digest(m.Body)
			want := pushResultFile{Image: ref, Digest: digest, DeployImage: reg.Host() + "/team/orders@" + digest, Format: format}
			if got != want {
				t.Fatalf("push.json = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCmdPushToRegistryAuthFailureHints(t *testing.T) {
	isolateCredentials(t)
	reg := registrytest.New(registrytest.Basic)
	reg.Username, reg.Password = "ci-bot", "pw"
	defer reg.Close()
	err := cmdPush([]string{testJar(t), reg.Host() + "/team/orders:1"})
	if err == nil || !strings.Contains(err.Error(), "BREWLET_REGISTRY_USERNAME") {
		t.Fatalf("err = %v", err)
	}
}
