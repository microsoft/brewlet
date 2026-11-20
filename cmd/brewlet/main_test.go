package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/microsoft/brewlet/pkg/artifact"
)

func TestExplicitProducerInputs(t *testing.T) {
	dir, e := os.MkdirTemp(".", ".test-cli-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	launch := filepath.Join(dir, "launch.json")
	if e = os.WriteFile(launch, []byte(`{"schemaVersion":1,"process":{"entrypoint":["runner"]}}`), 0644); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"build", "example.com/app", "--store", filepath.Join(dir, "store"), "--launch", launch}, "explicit platforms required"},
		{[]string{"build", "example.com/app", "--store", filepath.Join(dir, "store"), "--platform", "linux/amd64"}, "--launch required"},
		{[]string{"build", "example.com/app", "--store", filepath.Join(dir, "store"), "--launch", launch, "--platform", "linux/amd64", "--layer", "app=source:/etc"}, "LayerPathViolation"},
		{[]string{"build", "example.com/app", "--store", filepath.Join(dir, "store"), "--launch", launch, "--platform", "linux/amd64"}, "at least one app layer required"},
	} {
		if e = run(context.Background(), tc.args); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Errorf("%v: got %v, want %s", tc.args, e, tc.want)
		}
	}
}

func TestHelpAndDispatch(t *testing.T) {
	var out, errOut strings.Builder
	if e := dispatch(context.Background(), []string{"--help"}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"build", "push", "verify", "run", "k8s", "inventory"} {
		if !strings.Contains(out.String(), "  "+want+" ") {
			t.Errorf("help missing %q:\n%s", want, out.String())
		}
	}
	for _, hidden := range []string{"imagepush", "compose"} {
		if strings.Contains(out.String(), hidden) {
			t.Errorf("help lists %q", hidden)
		}
	}
	for _, args := range [][]string{{"build", "--help"}, {"help", "push"}, {"verify", "-h"}, {"inspect", "--help"}} {
		out.Reset()
		if e := dispatch(context.Background(), args, &out, &errOut); !isHelp(e) || !strings.Contains(out.String(), "Usage: brewlet") {
			t.Errorf("%v: err=%v output=%q", args, e, out.String())
		}
	}
	if e := dispatch(context.Background(), nil, &out, &errOut); e != errUsage {
		t.Errorf("no command: %v", e)
	}
	for args, want := range map[string]string{"imagepush": "unknown command", "compose": "unknown command", "nope": "unknown command"} {
		if e := dispatch(context.Background(), []string{args, "x"}, &out, &errOut); e == nil || !strings.Contains(e.Error(), want) {
			t.Errorf("%s: %v", args, e)
		}
	}
}

func TestBuildDefaultsToPerReferenceLayoutAndPushFindsIt(t *testing.T) {
	t.Chdir(t.TempDir())
	if e := os.MkdirAll("src", 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile("src/main.sh", []byte("echo hi\n"), 0644); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile("launch.json", []byte(`{"schemaVersion":1,"process":{"entrypoint":["/bin/sh","/app/main.sh"]}}`), 0644); e != nil {
		t.Fatal(e)
	}
	var out, errOut, jsonOut strings.Builder
	ref := "registry.example.test/apps/demo:v1"
	if e := dispatch(context.Background(), []string{"build", ref, "--platform", "linux/amd64", "--launch", "launch.json", "--layer", "app=src:/app"}, &out, &errOut); e != nil {
		if artifact.Reason(e) == "LayerPathViolation" && strings.Contains(e.Error(), "extended attributes") {
			t.Skipf("host attaches undeletable source xattrs; full producer cases run on Linux: %v", e)
		}
		t.Fatal(e)
	}
	want := filepath.Join(".brewlet", "images", "registry.example.test_apps_demo_v1")
	if !strings.Contains(out.String(), "Built image "+ref) || !strings.Contains(out.String(), want) || strings.HasPrefix(out.String(), "{") {
		t.Fatalf("unexpected build result: %s", out.String())
	}
	if e := dispatch(context.Background(), []string{"build", ref, "--platform", "linux/amd64", "--launch", "launch.json", "--layer", "app=src:/app", "-o", "json"}, &jsonOut, &errOut); e != nil {
		t.Fatal(e)
	}
	var result struct{ Reference, Store, Digest string }
	if e := json.Unmarshal([]byte(jsonOut.String()), &result); e != nil || result.Reference != ref || result.Store != want {
		t.Fatalf("unexpected JSON build result (%v): %s", e, jsonOut.String())
	}
	if _, e := os.Stat(filepath.Join(want, "index.json")); e != nil {
		t.Fatal(e)
	}
	jsonOut.Reset()
	if e := dispatch(context.Background(), []string{"verify", want + "@" + result.Digest, "--platform", "linux/amd64", "-o", "json"}, &jsonOut, &errOut); e != nil {
		t.Fatal(e)
	}
	var verification artifact.ApplicationReport
	if e := json.Unmarshal([]byte(jsonOut.String()), &verification); e != nil || !verification.Structural || verification.Root.Digest.String() != result.Digest {
		t.Fatalf("unexpected verification (%v): %s", e, jsonOut.String())
	}
	if e := dispatch(context.Background(), []string{"push", "registry.example.test/apps/other:v1"}, &out, &errOut); e == nil || !strings.Contains(e.Error(), "no local build") {
		t.Fatalf("push of unbuilt reference: %v", e)
	}
}
