// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/microsoft/brewlet/internal/artifact"
)

func TestPushAOTFlagValidation(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "app.jar")
	writeCLIZip(t, jar, "com/example/Main.class")
	store := filepath.Join(dir, "oci")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"aot+cache", []string{"--aot", "--aot-cache", "x.aot"}, "mutually exclusive"},
		{"appcds+archive", []string{"--appcds", "--appcds-archive", "y.jsa"}, "mutually exclusive"},
		{"cache+bundle", []string{"--aot-cache", "x.aot", "--dependency-bundle", "b"}, "does not support"},
		{"aot+layer", []string{"--aot", "--classpath-layer", "l.tar"}, "--aot supports fat-JAR only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{jar, "apps/a:1", "--store", store}, tc.args...)
			err := cmdPush(args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "appcds-archive") && tc.name == "aot+layer" {
				t.Fatalf("CDS advice leaked into --aot error: %v", err)
			}
		})
	}
}

func TestPushAOTCacheRecordsConfig(t *testing.T) {
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
	dir := t.TempDir()
	jar := filepath.Join(dir, "app.jar")
	writeCLIZip(t, jar, "com/example/Main.class")
	cache := filepath.Join(dir, "app.aot")
	if err := os.WriteFile(cache, []byte("aot"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "oci")
	for _, format := range []string{"image", "artifact"} {
		if err := cmdPush([]string{jar, "apps/a:" + format, "--store", store, "--format", format, "--aot-cache", cache}); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		blobs, err := (artifact.Store{Root: store}).ResolveBlobs("apps/a:" + format)
		if err != nil {
			t.Fatal(err)
		}
		if blobs.Config.AOT == nil || blobs.Config.AOT.Cache != "app.aot" || blobs.Config.CDS != nil {
			t.Fatalf("%s: config = %+v", format, blobs.Config)
		}
	}
}

func TestPushAOTTimeoutHasNoCDSAdvice(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "app.jar")
	writeCLIZip(t, jar, "com/example/Main.class")
	java := filepath.Join(dir, "java")
	if err := os.WriteFile(java, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := cmdPush([]string{jar, "apps/a:1", "--store", filepath.Join(dir, "oci"), "--aot", "--aot-java", java, "--aot-timeout", "1"})
	if err == nil || !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("err = %v, want timeout", err)
	}
	if strings.Contains(err.Error(), "appcds") {
		t.Fatalf("CDS advice leaked: %v", err)
	}
}

func TestPushBothArchivesRecordsBothHints(t *testing.T) {
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
	dir := t.TempDir()
	jar := filepath.Join(dir, "app.jar")
	writeCLIZip(t, jar, "com/example/Main.class")
	cache := filepath.Join(dir, "app.aot")
	archive := filepath.Join(dir, "app.jsa")
	for p, b := range map[string]string{cache: "AOT", archive: "JSA"} {
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := filepath.Join(dir, "oci")
	for _, format := range []string{"image", "artifact"} {
		if err := cmdPush([]string{jar, "apps/b:" + format, "--store", store, "--format", format, "--aot-cache", cache, "--appcds-archive", archive}); err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		blobs, err := (artifact.Store{Root: store}).ResolveBlobs("apps/b:" + format)
		if err != nil {
			t.Fatal(err)
		}
		if blobs.Config.AOT == nil || blobs.Config.AOT.Cache != "app.aot" || blobs.Config.CDS == nil || blobs.Config.CDS.Archive != "app.jsa" {
			t.Fatalf("%s: config aot=%+v cds=%+v, want both hints", format, blobs.Config.AOT, blobs.Config.CDS)
		}
		for path, want := range map[string]string{blobs.AOTHostPath: "AOT", blobs.CDSHostPath: "JSA"} {
			if b, err := os.ReadFile(path); err != nil || string(b) != want {
				t.Errorf("%s: %q = %q (err %v), want %q", format, path, b, err, want)
			}
		}
	}
}

func TestPushRejectsSameArchiveAndCacheName(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "app.jar")
	writeCLIZip(t, jar, "com/example/Main.class")
	for _, sub := range []string{"x", "y"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, sub, "app.bin"), []byte(sub), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := cmdPush([]string{jar, "apps/c:1", "--store", filepath.Join(dir, "oci"),
		"--appcds-archive", filepath.Join(dir, "x", "app.bin"), "--aot-cache", filepath.Join(dir, "y", "app.bin")})
	if err == nil || !strings.Contains(err.Error(), "cds.archive and aot.cache must differ") {
		t.Fatalf("err = %v, want cds.archive and aot.cache must differ", err)
	}
}
