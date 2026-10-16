package controller

import (
	"context"
	"fmt"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

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
