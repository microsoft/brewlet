//go:build linux

package main

import (
	"context"
	"os"
	"testing"

	"github.com/microsoft/brewlet/pkg/artifact"
)

func TestPublishedProducerShimConformance(t *testing.T) {
	data, err := os.ReadFile("../../../conformance/artifact-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	results, err := artifact.RunFixtures(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("no published conformance fixtures executed")
	}
	for _, result := range results {
		if !result.Passed {
			t.Errorf("shared producer/shim fixture failed: %+v", result)
		}
	}
}
