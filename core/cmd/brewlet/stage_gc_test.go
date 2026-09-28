// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageGCRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--min-age=0"}, {"--min-age=-1h"}, {"--min-age=invalid"}, {"unexpected"},
	} {
		if err := cmdStageGC(args); err == nil {
			t.Fatalf("stage-gc %v succeeded", args)
		}
		if err := cmdStageGC([]string{"--help"}); err != nil {
			t.Fatalf("stage-gc help: %v", err)
		}
	}
}

func TestRunnableBundleSurvivesStageRemoval(t *testing.T) {
	stage := t.TempDir()
	t.Setenv("BREWLET_RUNNABLE_STAGE", stage)
	dir := t.TempDir()
	jar := filepath.Join(dir, "orders.jar")
	writeCLIZip(t, jar, "com/example/Orders.class")
	archive := filepath.Join(dir, "app.jsa")
	if err := os.WriteFile(archive, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte(`{"schemaVersion":1,"entry":{"mode":"jar"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cds := range []bool{false, true} {
		name := "plain"
		if cds {
			name = "cds"
		}
		t.Run(name, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "oci")
			args := []string{jar, "apps/orders:1", "--store", store, "--config", config}
			if cds {
				args = append(args, "--appcds-archive", archive)
			}
			if err := cmdPush(args); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "bundle")
			if err := cmdBundle([]string{"apps/orders:1", "--store", store, "--out", out}); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(stage, "immutable-v2")); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(out, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			var spec struct {
				Mounts []struct{ Source, Destination string }
			}
			if err := json.Unmarshal(data, &spec); err != nil {
				t.Fatal(err)
			}
			payloads := 0
			for _, mount := range spec.Mounts {
				if !strings.HasPrefix(mount.Destination, "/app/") {
					continue
				}
				payloads++
				if !strings.HasPrefix(mount.Source, out+string(os.PathSeparator)) {
					t.Errorf("payload is not bundle-owned: %s", mount.Source)
				}
				if _, err := os.ReadFile(mount.Source); err != nil {
					t.Errorf("bundle payload missing after eviction: %v", err)
				}
			}
			want := 1
			if cds {
				want = 2
			}
			if payloads != want {
				t.Fatalf("payload mounts = %d, want %d", payloads, want)
			}
		})
	}
}
