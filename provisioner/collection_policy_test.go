package provisioner

import (
	"testing"
	"time"
)

func TestCollectionPolicyUsesRealPositiveProductionDurations(t *testing.T) {
	w := new(Worker)
	grace, interval, err := w.collectionPolicy()
	if err != nil || grace != time.Hour || interval != time.Minute {
		t.Fatal(grace, interval, err)
	}
	w.GCGrace = 20 * time.Second
	w.GCSweepInterval = 2 * time.Second
	grace, interval, err = w.collectionPolicy()
	if err != nil || grace != 20*time.Second || interval != 2*time.Second {
		t.Fatal(grace, interval, err)
	}
	w.GCGrace = -time.Second
	if _, _, err := w.collectionPolicy(); err == nil {
		t.Fatal("negative GC grace accepted")
	}
	w.GCGrace = time.Second
	w.GCSweepInterval = -time.Second
	if _, _, err := w.collectionPolicy(); err == nil {
		t.Fatal("negative sweep accepted")
	}
}
