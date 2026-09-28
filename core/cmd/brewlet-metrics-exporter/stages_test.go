// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestStageCollector(t *testing.T) {
	root := filepath.Join(t.TempDir(), "stages")
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(newStageCollector(root))
	check := func(value string) {
		t.Helper()
		expected := `
# HELP brewlet_runnable_stage_bytes Logical bytes remaining in runnable staging trees, including legacy and pending stages.
# TYPE brewlet_runnable_stage_bytes gauge
brewlet_runnable_stage_bytes ` + value + "\n"
		if err := testutil.GatherAndCompare(reg, strings.NewReader(expected)); err != nil {
			t.Fatal(err)
		}
	}
	check("0")
	dir := filepath.Join(root, "immutable-v2", strings.Repeat("a", 64))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(dir, "payload")
	if err := os.WriteFile(payload, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	check("5")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	check("0")
}

func TestStageCollectorReportsReadErrors(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(newStageCollector(root))
	if _, err := reg.Gather(); err == nil {
		t.Fatal("expected a scrape error, not a misleading zero")
	}
}
