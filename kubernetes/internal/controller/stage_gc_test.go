// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"
	"time"
)

func TestStageGCValidation(t *testing.T) {
	for _, duration := range []time.Duration{0, -time.Second, time.Millisecond, time.Second + time.Nanosecond, 2147483648 * time.Second} {
		for _, field := range []string{"interval", "minAge"} {
			cfg := Config{StageGCInterval: 5 * time.Minute, StageGCMinAge: 24 * time.Hour}
			if field == "interval" {
				cfg.StageGCInterval = duration
			} else {
				cfg.StageGCMinAge = duration
			}
			if err := cfg.ValidateStageGC(); err == nil {
				t.Errorf("%s accepted %s", field, duration)
			}
		}
	}
	for _, duration := range []time.Duration{time.Second, 5 * time.Minute, 24 * time.Hour, 2147483647 * time.Second} {
		cfg := Config{StageGCInterval: duration, StageGCMinAge: duration}
		if err := cfg.ValidateStageGC(); err != nil {
			t.Errorf("rejected %s: %v", duration, err)
		}
	}
}

func TestStageGCProvisionerConfiguration(t *testing.T) {
	for _, metrics := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			cfg := testConfig()
			cfg.MetricsEnabled = metrics
			cfg.StageGCEnabled = enabled
			cfg.StageGCInterval = 7 * time.Minute
			cfg.StageGCMinAge = 48 * time.Hour
			cfg.StageGCUpgradeAcknowledged = true
			cfg.StageGCAllowNestedPIDNamespace = enabled
			p := profileNamed("external", []string{"java"}, jdk("temurin", 21))
			ds := buildProfileDaemonSet(cfg, &p, "agentpool", nil)
			env := map[string]string{}
			for _, item := range ds.Spec.Template.Spec.Containers[0].Env {
				env[item.Name] = item.Value
			}
			wantEnabled := "false"
			if enabled {
				wantEnabled = "true"
			}
			for key, want := range map[string]string{
				"BREWLET_STAGE_GC_ENABLED":                    wantEnabled,
				"BREWLET_STAGE_GC_INTERVAL_SECONDS":           "420",
				"BREWLET_STAGE_GC_MIN_AGE_SECONDS":            "172800",
				"BREWLET_STAGE_GC_UPGRADE_ACKNOWLEDGED":       "true",
				"BREWLET_STAGE_GC_ALLOW_NESTED_PID_NAMESPACE": wantEnabled,
			} {
				if env[key] != want {
					t.Errorf("metrics=%t GC=%t: %s=%q, want %q", metrics, enabled, key, env[key], want)
				}
			}
			if !ds.Spec.Template.Spec.HostPID {
				t.Fatal("GC requires host PID visibility")
			}
			if !metrics && len(ds.Spec.Template.Spec.Containers) != 1 {
				t.Fatal("GC must not depend on an exporter sidecar")
			}
		}
	}
}
