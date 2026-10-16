//go:build linux

package main

import (
	"testing"
	"time"
)

func TestProductionGCDurationsRejectFakeOrNonpositiveTime(t *testing.T) {
	t.Setenv("BREWLET_GC_GRACE", "20s")
	if duration, err := productionDuration("BREWLET_GC_GRACE", time.Hour); err != nil || duration != 20*time.Second {
		t.Fatal(duration, err)
	}
	for _, value := range []string{"0s", "-1s", "fake", ""} {
		t.Setenv("BREWLET_GC_GRACE", value)
		duration, err := productionDuration("BREWLET_GC_GRACE", time.Hour)
		if value == "" {
			if err != nil || duration != time.Hour {
				t.Fatal(duration, err)
			}
		} else if err == nil {
			t.Fatal("nonpositive/invalid duration accepted", value)
		}
	}
}
