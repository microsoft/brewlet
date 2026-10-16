package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedConformanceCommand(t *testing.T) {
	if e := run(context.Background(), []string{"conformance", "../../conformance/artifact-v2.json"}); e != nil {
		t.Fatal(e)
	}
}
func TestExplicitProducerInputs(t *testing.T) {
	dir, e := os.MkdirTemp(".", ".test-cli-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	launch := filepath.Join(dir, "launch.json")
	if e = os.WriteFile(launch, []byte(`{"schemaVersion":2,"process":{"entrypoint":["runner"]}}`), 0644); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"push", "example.com/app", "--store", filepath.Join(dir, "store"), "--launch", launch}, "explicit platforms required"},
		{[]string{"push", "example.com/app", "--store", filepath.Join(dir, "store"), "--platform", "linux/amd64"}, "--launch required"},
		{[]string{"push", "example.com/app", "--store", filepath.Join(dir, "store"), "--launch", launch, "--platform", "linux/amd64", "--layer", "app=source:/etc"}, "LayerPathViolation"},
		{[]string{"push", "example.com/app", "--store", filepath.Join(dir, "store"), "--launch", launch, "--platform", "linux/amd64"}, "at least one app layer required"},
	} {
		if e = run(context.Background(), tc.args); e == nil || !strings.Contains(e.Error(), tc.want) {
			t.Errorf("%v: got %v, want %s", tc.args, e, tc.want)
		}
	}
}
