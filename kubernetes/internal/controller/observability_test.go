package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRestartOutcomeEventsMatchStatusAndAreNotRepeatedWithoutChange(t *testing.T) {
	f := restartSetup(t)
	workload := restartWorkload{object: f.deployment(t, "observed")}
	recorder := record.NewFakeRecorder(8)
	f.engine.Recorder = recorder
	status := new(api.WorkloadRestart)
	for _, tc := range []struct{ phase, reason, eventType string }{
		{"NotApplicable", "RestartNotApplicable", "Normal"},
		{"Deferred", "RestartDeferred", "Warning"},
		{"Failed", "RestartFailed", "Warning"},
		{"Complete", "RestartComplete", "Normal"},
	} {
		f.engine.outcome(workload, status, tc.phase, tc.reason, "actual outcome")
		f.engine.outcome(workload, status, tc.phase, tc.reason, "actual outcome")
		if status.Phase != tc.phase || status.Reason != tc.reason {
			t.Fatal("event/status drift", status)
		}
		select {
		case event := <-recorder.Events:
			if !strings.HasPrefix(event, tc.eventType+" "+tc.reason+" ") {
				t.Fatal("incorrect workload event", event)
			}
		default:
			t.Fatal("status change omitted its workload event", tc)
		}
		select {
		case event := <-recorder.Events:
			t.Fatal("unchanged reconciliation duplicated a workload event", event)
		default:
		}
	}
	if status.CompletedAt == nil {
		t.Fatal("completed event did not checkpoint completion")
	}
}

func TestRestartCountsExcludePreviousEpochAndSupersededTarget(t *testing.T) {
	f := restartSetup(t)
	f.image.Status.Restarts = api.RestartSummary{}
	for _, phase := range []string{"Pending", "Patching", "Rolling", "Complete",
		"Deferred", "NotApplicable", "Failed", "EpochMismatch"} {
		f.image.Status.Restarts.Workloads = append(f.image.Status.Restarts.Workloads,
			api.WorkloadRestart{InventoryUID: f.image.UID, Generation: f.image.Status.Generation, Phase: phase})
	}
	f.image.Status.Restarts.Workloads = append(f.image.Status.Restarts.Workloads,
		api.WorkloadRestart{InventoryUID: "old-epoch", Generation: f.image.Status.Generation, Phase: "Complete"},
		api.WorkloadRestart{InventoryUID: f.image.UID, Generation: f.image.Status.Generation - 1, Phase: "Complete"})
	countRestarts(f.image)
	s := f.image.Status.Restarts
	if s.Pending != 1 || s.Rolling != 2 || s.Complete != 1 || s.Deferred != 1 ||
		s.NotApplicable != 1 || s.Failed != 2 {
		t.Fatalf("restart counts mixed states or targets: %+v", s)
	}
}

func TestRuntimeMetricsReportStatusAndFailScrapeOnFreshAPIError(t *testing.T) {
	f := restartSetup(t)
	_ = f.deployment(t, "api")
	f.run(t)
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewRuntimeMetrics(f.engine.Reader))
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range families {
		if m.GetName() == "brewlet_runtime_image_restart_info" && len(m.Metric) == 1 {
			found = true
		}
	}
	if !found {
		t.Fatal("durable UID/epoch restart outcome absent from metrics")
	}
	reader := interceptor.NewClient(f.engine.Client.(client.WithWatch), interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return fmt.Errorf("API unavailable")
	}})
	broken := prometheus.NewRegistry()
	broken.MustRegister(NewRuntimeMetrics(reader))
	if _, err := broken.Gather(); err == nil {
		t.Fatal("API failure produced success-shaped zero metrics")
	}
}
func TestOwnedNodeStateCountsAndStartupFenceObservation(t *testing.T) {
	r, p, n := profileFixture(t)
	ctx := context.Background()
	advance(t, r, p, 6)
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	if n.Annotations["brewlet.sh/provision-state"] != "Provisioning" {
		t.Fatal("startup-tainted template counted ready")
	}
	n.Spec.Taints = nil
	if err := r.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := r.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	advance(t, r, p, 1)
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	if n.Annotations["brewlet.sh/provision-state"] != "Ready" {
		t.Fatal("owned validated baseline was not mirrored")
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if p.Status.AssignedNodes != 1 || p.Status.ReadyNodes != 1 {
		t.Fatal("assigned/effective-ready counts absent")
	}
	n.Spec.Taints = []corev1.Taint{{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}}
	_ = r.Update(ctx, n)
	advance(t, r, p, 1)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	_ = r.Get(ctx, client.ObjectKeyFromObject(n), n)
	if p.Status.ReadyNodes != 0 || n.Annotations["brewlet.sh/provision-state"] != "Provisioning" {
		t.Fatal("fence did not override retained raw ready label")
	}
}
func TestRestartGlobalLeadershipGateDoesNotMutateWhileStandby(t *testing.T) {
	f := restartSetup(t)
	_ = f.deployment(t, "api")
	gate := &RestartLeadership{}
	f.engine.Leadership = gate
	f.run(t)
	if f.patches != 0 || len(f.image.Status.Restarts.Workloads) != 0 {
		t.Fatal("standby global restart engine mutated workload/status")
	}
	ctx, cancel := context.WithCancel(context.Background())
	gate.set(ctx)
	f.run(t)
	cancel()
	if f.patches != 1 {
		t.Fatal("active global restart lease did not permit restart")
	}
	f.run(t)
	if f.patches != 1 {
		t.Fatal("canceled global leadership allowed another patch")
	}
}
