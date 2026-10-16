package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkerGCConfigurationDefaultsAndOverrides(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "bounded-live-validation"}[override], func(t *testing.T) {
			r, p, _ := profileFixture(t)
			if override {
				r.GCGrace = 20 * time.Second
				r.GCSweepInterval = 2 * time.Second
			}
			advance(t, r, p, 6)
			ds := new(appsv1.DaemonSet)
			if err := r.Get(context.Background(), client.ObjectKey{Name: "brewlet-node-provisioner-web", Namespace: r.Namespace}, ds); err != nil {
				t.Fatal(err)
			}
			grace, err := time.ParseDuration(workerEnv(ds, "BREWLET_GC_GRACE"))
			if err != nil {
				t.Fatal(err)
			}
			sweep, err := time.ParseDuration(workerEnv(ds, "BREWLET_GC_SWEEP_INTERVAL"))
			if err != nil {
				t.Fatal(err)
			}
			wantGrace, wantSweep := time.Hour, time.Minute
			if override {
				wantGrace = 20 * time.Second
				wantSweep = 2 * time.Second
			}
			if grace != wantGrace || sweep != wantSweep {
				t.Fatalf("incorrect GC policy rendered: grace=%v sweep=%v", grace, sweep)
			}
		})
	}
}
