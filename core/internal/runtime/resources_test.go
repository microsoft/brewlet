// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/microsoft/brewlet/internal/artifact"
)

func TestBuildResourcesValidLimits(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits Resources
		memory int64
		quota  int64
	}{
		{"unlimited", Resources{}, 0, 0},
		{"whole CPU and binary memory", Resources{"2", "512Mi"}, 512 << 20, 200000},
		{"millicores and decimal memory", Resources{"500m", "1G"}, 1000000000, 50000},
		{"fractional quantities", Resources{"1.5", "1.5Gi"}, 1610612736, 150000},
		{"minimum CPU and memory", Resources{"0.01", "1"}, 1, 1000},
		{"minimum millicores", Resources{"10m", ""}, 0, 1000},
		{"decimal boundary", Resources{"1.001", "1Ki"}, 1024, 100100},
		{"whitespace", Resources{" 500m ", " 1Mi "}, 1 << 20, 50000},
		{"existing decimal aliases", Resources{"1e-2", "1m"}, 1000000, 1000},
		{"maximum memory", Resources{"", "9223372036854775807"}, math.MaxInt64, 0},
		{"maximum scaled memory", Resources{"", "9223372036854775.807k"}, math.MaxInt64, 0},
		{"maximum CPU quota", Resources{"92233720368547758m", ""}, 0, 9223372036854775800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildResources(tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			if tc.memory == 0 {
				if got.Memory != nil {
					t.Fatal("unexpected memory limit")
				}
			} else if got.Memory == nil || got.Memory.Limit == nil || *got.Memory.Limit != tc.memory {
				t.Fatalf("memory = %+v, want %d", got.Memory, tc.memory)
			}
			if tc.quota == 0 {
				if got.CPU != nil {
					t.Fatal("unexpected CPU limit")
				}
			} else if got.CPU == nil || got.CPU.Quota == nil || *got.CPU.Quota != tc.quota ||
				got.CPU.Period == nil || *got.CPU.Period != 100000 {
				t.Fatalf("CPU = %+v, want quota %d and period 100000", got.CPU, tc.quota)
			}
			dir := t.TempDir()
			jar := filepath.Join(dir, "app.jar")
			if err := os.WriteFile(jar, []byte("application"), 0o644); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "bundle")
			cfg := artifact.JVMConfig{SchemaVersion: 1, Entry: artifact.Entry{Mode: "jar"}}
			if err := GenerateBundle(cfg, dir, jar, out, tc.limits, nil); err != nil {
				t.Fatal(err)
			}
			if spec := readBundleSpec(t, out); !reflect.DeepEqual(spec.Linux.Resources, got) {
				t.Fatalf("bundle resources = %+v, want %+v", spec.Linux.Resources, got)
			}
		})
	}
}

func TestBundleRejectsInvalidResourceLimitsBeforeWriting(t *testing.T) {
	for kind, values := range map[string][]string{
		"CPU": {
			"invalid", " ", "-1", "-500m", "0", "0m", "NaN", "+Inf", "-Inf",
			"0.0001", "0.001", "1m", "9m", "0.009", "1e309", "9223372036854775808m", "92233720368547759m",
		},
		"memory": {
			"512MB", " ", "-1", "-1Gi", "0", "0Mi", "NaNMi", "+InfGi", "-Inf",
			"0.0001Ki", "1e309Gi", "9223372036854775808", "8388608Ti",
			"9223372036854775.808k", "9223372036854775.8071k",
		},
	} {
		for _, value := range values {
			t.Run(kind+"/"+value, func(t *testing.T) {
				limits := Resources{}
				if kind == "CPU" {
					limits.CPULimit = value
				} else {
					limits.MemoryLimit = value
				}
				out := filepath.Join(t.TempDir(), "bundle")
				err := GenerateBundle(
					artifact.JVMConfig{Entry: artifact.Entry{Mode: "jar"}},
					"jdk", "app.jar", out, limits, nil,
				)
				if err == nil || !strings.Contains(err.Error(), "invalid "+kind+" limit") {
					t.Fatalf("error = %v, want actionable %s limit rejection", err, kind)
				}
				if !strings.Contains(err.Error(), strconv.Quote(value)) {
					t.Fatalf("error does not identify the invalid value: %v", err)
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatalf("invalid resource limits created bundle output: %v", err)
				}
			})
		}
	}
}
