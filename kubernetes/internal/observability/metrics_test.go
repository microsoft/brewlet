// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAdmissionAndNodeProfileMetrics(t *testing.T) {
	admissionRequests.Reset()
	nodeProfileNodes.Reset()
	nodeProfileCondition.Reset()
	nodeProfileDetachedRetirements.Reset()

	ObserveAdmission("denied", "NoCompatibleArch")
	if got := testutil.ToFloat64(admissionRequests.WithLabelValues("denied", "NoCompatibleArch")); got != 1 {
		t.Fatalf("admission counter = %v", got)
	}

	SetNodeProfile("batch", 3, 2, "Provisioning", false)
	if got := testutil.ToFloat64(nodeProfileNodes.WithLabelValues("batch", "assigned")); got != 3 {
		t.Fatalf("assigned gauge = %v", got)
	}
	if got := testutil.ToFloat64(nodeProfileNodes.WithLabelValues("batch", "ready")); got != 2 {
		t.Fatalf("ready gauge = %v", got)
	}

	SetNodeProfileDetachedRetirements("batch", 2)
	if got := testutil.ToFloat64(nodeProfileDetachedRetirements.WithLabelValues("batch")); got != 2 {
		t.Fatalf("detached retirements gauge = %v", got)
	}
	DeleteNodeProfile("batch")
	if got := testutil.CollectAndCount(nodeProfileDetachedRetirements); got != 0 {
		t.Fatalf("detached retirements series after delete = %d", got)
	}
}
