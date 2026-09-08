// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/microsoft/brewlet/internal/artifact"
)

func TestManagedDependencyBundleCLIFlow(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "oci")
	dependency := []byte("approved dependency")
	layer := filepath.Join(dir, "dependencies.tar")
	writeCLITar(t, layer, "approved.jar", dependency)
	lockPath := filepath.Join(dir, "dependency-lock.json")
	privateKey := filepath.Join(dir, "signing-key.pem")
	publicKey := filepath.Join(dir, "signing-key.pub.pem")
	if err := artifact.GenerateECDSAKeyPair(privateKey, publicKey); err != nil {
		t.Fatal(err)
	}
	lock := artifact.DependencyLock{
		SchemaVersion: 1,
		Artifacts: []artifact.DependencyLockEntry{{
			GroupID: "com.example", ArtifactID: "approved", Version: "1.0.0",
			Type: "jar", Scope: "runtime", FileName: "approved.jar", SHA256: sha256Hex(dependency),
		}},
	}
	lockRaw, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, lockRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmdDependencyBundle([]string{
		layer, "platform/approved:1",
		"--store", store,
		"--name", "approved",
		"--version", "1",
		"--source-bom", "com.example:approved-bom:1",
		"--lock", lockPath,
		"--compatible-jdks", "21,25",
		"--signing-key", privateKey,
		"--signer-identity", "test-builder",
	}); err != nil {
		t.Fatalf("cmdDependencyBundle: %v", err)
	}

	jar := filepath.Join(dir, "orders.jar")
	writeCLIZip(t, jar, "com/example/Orders.class")
	if err := cmdPush([]string{
		jar, "apps/orders:1",
		"--store", store,
		"--dependency-bundle", "platform/approved:1",
		"--dependency-lock", lockPath,
		"--trusted-public-key", publicKey,
		"--trusted-signer-identity", "test-builder",
		"--signing-key", privateKey,
		"--builder-identity", "test-builder",
		"--main-class", "com.example.Orders",
	}); err != nil {
		t.Fatalf("cmdPush: %v", err)
	}

	manifest, _, err := (artifact.Store{Root: store}).ResolveManifestByRef("apps/orders:1")
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok, err := manifest.ManagedDependencyEvidence()
	if err != nil || !ok {
		t.Fatalf("managed evidence: ok=%v err=%v", ok, err)
	}
	if evidence.SourceBOM != "com.example:approved-bom:1" {
		t.Fatalf("sourceBom = %q", evidence.SourceBOM)
	}
	imageDesc, err := (artifact.Store{Root: store}).DescriptorByRef("apps/orders:1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := artifact.LoadECDSAPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (artifact.Store{Root: store}).VerifyManagedAttestation(imageDesc, key, "test-builder"); err != nil {
		t.Fatalf("VerifyManagedAttestation: %v", err)
	}
}

func TestManagedDependencyBundleRejectsFatJar(t *testing.T) {
	dir := t.TempDir()
	jar := filepath.Join(dir, "fat.jar")
	writeCLIZip(t, jar, "BOOT-INF/lib/dependency.jar")
	err := cmdPush([]string{
		jar, "apps/fat:1",
		"--store", filepath.Join(dir, "missing-store"),
		"--dependency-bundle", "platform/approved:1",
		"--dependency-lock", filepath.Join(dir, "dependency-lock.json"),
		"--main-class", "com.example.Main",
	})
	if err == nil {
		t.Fatal("expected fat JAR rejection")
	}
}

func TestParseJDKFeaturesRejectsPartialInteger(t *testing.T) {
	if _, err := parseJDKFeatures("21x"); err == nil {
		t.Fatal("expected invalid JDK feature")
	}
}

func TestBundleCLIRejectsInvalidResourceLimits(t *testing.T) {
	t.Setenv("BREWLET_RUNNABLE_STAGE", t.TempDir())
	dir := t.TempDir()
	jar := filepath.Join(dir, "orders.jar")
	writeCLIZip(t, jar, "com/example/Orders.class")
	store := filepath.Join(dir, "oci")
	config := filepath.Join(dir, "config.json")
	if err := os.WriteFile(config, []byte(`{"schemaVersion":1,"entry":{"mode":"jar"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdPush([]string{jar, "apps/orders:1", "--store", store, "--config", config}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		flag, value, reason string
	}{
		{"--cpu", "invalid", "CPU"},
		{"--memory", "512MB", "memory"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "bundle")
			err := cmdBundle([]string{"apps/orders:1", "--store", store, "--out", out, tc.flag, tc.value})
			if err == nil || !strings.Contains(err.Error(), "invalid "+tc.reason+" limit") {
				t.Fatalf("cmdBundle error = %v, want resource validation failure", err)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("invalid limits wrote bundle: %v", err)
			}
		})
	}
}

func TestParseProcessIDFlag(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    uint32
		wantErr bool
	}{
		{name: "default non-root", value: "65532", want: 65532},
		{name: "explicit root", value: "0", want: 0},
		{name: "max Linux ID", value: "4294967294", want: 4294967294},
		{name: "reserved uint32 sentinel", value: "4294967295", wantErr: true},
		{name: "negative", value: "-1", wantErr: true},
		{name: "out of range", value: "4294967296", wantErr: true},
		{name: "partial integer", value: "1000x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseProcessIDFlag("uid", tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseProcessIDFlag(%q) succeeded, want error", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseProcessIDFlag(%q): %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("parseProcessIDFlag(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func writeCLITar(t *testing.T, path, name string, content []byte) {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCLIZip(t *testing.T, path, name string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("content")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buffer.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256Hex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
