package controller

import (
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAllocationJournalRetainsImmutableMountPolicyAcrossStatusLoss(t *testing.T) {
	im := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{UID: "epoch"},
		Spec: api.RuntimeImageSpec{Source: api.Source{Image: "registry.example.com/runtime@sha256:" + strings.Repeat("a", 64)},
			VolumeMountPaths: []string{"/tmp"}}}
	journal := journalFor(im, 1)
	if err := encodeJournal(im, journal); err != nil {
		t.Fatal(err)
	}
	if _, err := readJournal(im); err != nil {
		t.Fatal(err)
	}
	im.Spec.VolumeMountPaths[0] = "/data"
	if journal.VolumeMountPaths[0] != "/tmp" {
		t.Fatal("allocation journal shares mutable policy storage")
	}
	if _, err := readJournal(im); err == nil || !strings.Contains(err.Error(), "immutable runtime identity") {
		t.Fatalf("status loss permitted mount policy replacement: %v", err)
	}
}
