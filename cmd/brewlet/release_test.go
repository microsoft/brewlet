package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/brewlet/pkg/artifact"
)

func TestReleaseCommandsRequireExplicitInputsAndPolicy(t *testing.T) {
	for _, args := range [][]string{
		{"bundle", "example.com/app:candidate"},
		{"verify", "example.com/app:mutable", "--platform", "linux/arm64"},
		{"bundle", "app", "--policy", "missing", "--platform", "linux/arm64", "--platform", "linux/amd64"},
		{"bundle", "app", "--policy", "missing", "--platform", "linux/arm64", "--application", "a", "--layer-set", "one", "--layer-set", "two"},
		{"verify", "a", "--policy", "missing", "--platform", "linux/arm64", "--signing-key", "not-a-verification-option"},
		{"inventory", "--namespace", "apps"},
	} {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("implicit/unsupported release input accepted: %v", args)
		}
	}
	dir := t.TempDir()
	policy := artifact.ReleasePolicy{SchemaVersion: 1, Mode: "development", NotAfter: time.Now().Add(time.Hour)}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "policy.json")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = run(context.Background(), []string{"bundle", "local", "--policy", file, "--platform", "linux/arm64",
		"--application", "example.com/app:mutable", "--layer-set", "example.com/deps:mutable", "--store", filepath.Join(dir, "result")}); err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Fatalf("mutable release inputs accepted: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "result", "index.json")); !os.IsNotExist(err) {
		t.Fatal("failed composition published accepted local output")
	}
}

func TestReleaseRegistryCredentialsAreExplicitlyOriginScoped(t *testing.T) {
	t.Setenv("BREWLET_REGISTRY_USERNAME", "publisher")
	t.Setenv("BREWLET_REGISTRY_PASSWORD", "test-password")
	t.Setenv("BREWLET_REGISTRY_ORIGIN", "")
	if _, err := loadReleaseRegistries("", false, false); err == nil {
		t.Fatal("unscoped credentials accepted")
	}
	t.Setenv("BREWLET_REGISTRY_ORIGIN", "https://approved.example")
	options, err := loadReleaseRegistries("", false, false)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := options.registry("approved.example/platform/dependencies@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := options.registry("unrelated.example/apps/payload@sha256:" + strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if approved.Username != "publisher" || foreign.Username != "" || foreign.Password != "" {
		t.Fatal("credentials crossed registry origin")
	}
}

func TestReleasePolicyYAMLAndAtomicReceipt(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(file, []byte("schemaVersion: 1\nmode: development\nnotAfter: "+time.Now().Add(time.Hour).UTC().Format(time.RFC3339)+
		"\npublisher:\n  identity: ''\n  keys: []\ncomposer:\n  identity: ''\n  keys: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReleasePolicy(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("schemaVersion: 1\nschemaVersion: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readReleasePolicy(file); err == nil {
		t.Fatal("duplicate YAML keys accepted")
	}
	result := filepath.Join(dir, "receipt.json")
	if err := atomicReleaseResult(result, []byte("{\"digest\":\"accepted\"}\n")); err != nil {
		t.Fatal(err)
	}
	if err := atomicReleaseResult(filepath.Join(dir, "missing", "receipt.json"), []byte("not-accepted")); err == nil {
		t.Fatal("failed result write accepted")
	}
	raw, err := os.ReadFile(result)
	if err != nil || string(raw) != "{\"digest\":\"accepted\"}\n" {
		t.Fatal("unrelated receipt was damaged")
	}
}
