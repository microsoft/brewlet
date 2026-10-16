package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/internal/controller"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricregistry "sigs.k8s.io/controller-runtime/pkg/metrics"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil)))
	var namespace, image, hosts, metricsAddr, health, nodeMetrics string
	var leader, restarts, cleanup bool
	var release string
	var timeout int
	var nodeDeadline time.Duration
	var maxConcurrent int
	var workloadDeadline time.Duration
	var gcGrace, gcSweep time.Duration
	flag.StringVar(&namespace, "namespace", "brewlet-system", "namespace for managed workers")
	flag.StringVar(&image, "worker-image", "brewlet-node:2.0", "node worker image")
	flag.StringVar(&hosts, "allowed-source-mirror-hosts", "", "comma-separated exact mirror hosts")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics address (0 disables)")
	flag.StringVar(&nodeMetrics, "node-metrics-address", "", "optional worker metrics bind address")
	flag.StringVar(&health, "health-probe-bind-address", ":8081", "health address")
	flag.BoolVar(&leader, "leader-elect", true, "use leader election")
	flag.BoolVar(&restarts, "rotation-restarts", false, "opt in to controller-driven runtime rotation restarts")
	flag.IntVar(&maxConcurrent, "restart-max-concurrent", 5, "cluster-wide concurrent workload rotation rollouts")
	flag.DurationVar(&workloadDeadline, "restart-workload-progress-deadline", 10*time.Minute, "maximum time to observe a workload rotation rollout")
	flag.BoolVar(&cleanup, "cleanup-coordinator", false, "run unprivileged Helm uninstall coordinator instead of manager")
	flag.StringVar(&release, "release-name", "", "Helm release name for uninstall")
	flag.IntVar(&timeout, "uninstall-timeout-seconds", 240, "bounded cleanup coordinator timeout (1-86400)")
	flag.DurationVar(&nodeDeadline, "node-progress-deadline", 10*time.Minute, "rotation node progress deadline before confirmed quarantine")
	flag.DurationVar(&gcGrace, "gc-grace", time.Hour, "node unleased runtime generation collection grace")
	flag.DurationVar(&gcSweep, "gc-sweep-interval", time.Minute, "node runtime/cache collection sweep interval")
	flag.Parse()
	if gcGrace <= 0 || gcSweep <= 0 {
		fmt.Fprintln(os.Stderr, "gc-grace and gc-sweep-interval must be positive")
		os.Exit(1)
	}
	if nodeMetrics != "" {
		_, port, err := net.SplitHostPort(nodeMetrics)
		n, parseErr := strconv.Atoi(port)
		if err != nil || parseErr != nil || n < 1 || n > 65535 {
			fmt.Fprintln(os.Stderr, "node-metrics-address must contain a valid explicit TCP port")
			os.Exit(1)
		}
	}
	if nodeDeadline <= 0 {
		fmt.Fprintln(os.Stderr, "node-progress-deadline must be positive")
		os.Exit(1)
	}
	if restarts && (!leader || maxConcurrent < 1 || workloadDeadline <= 0) {
		fmt.Fprintln(os.Stderr, "rotation restarts require leader election, a positive concurrency limit and a positive workload deadline")
		os.Exit(1)
	}
	s := runtime.NewScheme()
	must(corev1.AddToScheme(s))
	must(appsv1.AddToScheme(s))
	must(api.AddToScheme(s))
	if cleanup {
		if timeout < 1 || timeout > 86400 {
			fmt.Fprintln(os.Stderr, "uninstall timeout must be 1-86400 whole seconds")
			os.Exit(1)
		}
		c, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: s})
		must(err)
		ctx, cancel := context.WithTimeout(ctrl.SetupSignalHandler(), time.Duration(timeout)*time.Second)
		defer cancel()
		must((&controller.UninstallCoordinator{Client: c, ReleaseName: release, ReleaseNamespace: namespace}).Run(ctx))
		return
	}
	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: s, LeaderElection: leader, LeaderElectionID: "brewlet-operator.node.brewlet.sh", LeaderElectionNamespace: namespace, Metrics: metrics.Options{BindAddress: metricsAddr}, HealthProbeBindAddress: health})
	must(err)
	allowed := []string{}
	if hosts != "" {
		allowed = strings.Split(hosts, ",")
	}
	if metricsAddr != "0" {
		metricregistry.Registry.MustRegister(controller.NewRuntimeMetrics(m.GetAPIReader()))
	}
	must((&controller.ProfileReconciler{Client: m.GetClient(), Reader: m.GetAPIReader(), Namespace: namespace, WorkerImage: image, AllowedMirrorHosts: allowed, Recorder: m.GetEventRecorderFor("brewlet-nodeprofile"), NodeMetricsAddress: nodeMetrics, GCGrace: gcGrace, GCSweepInterval: gcSweep}).SetupWithManager(m))
	runtimeReconciler := &controller.RuntimeReconciler{Client: m.GetClient(), Reader: m.GetAPIReader(), NodeProgressDeadline: nodeDeadline, AllowedMirrorHosts: allowed}
	if restarts {
		global := &controller.RestartLeadership{Config: m.GetConfig()}
		must(m.Add(global))
		runtimeReconciler.Restarts = &controller.RestartEngine{Client: m.GetClient(), Reader: m.GetAPIReader(), MaxConcurrent: maxConcurrent, WorkloadProgressDeadline: workloadDeadline, Recorder: m.GetEventRecorderFor("brewlet-rotation"), Leadership: global}
	}
	must(runtimeReconciler.SetupWithManager(m))
	must((&controller.EvidenceReconciler{Client: m.GetClient(), Reader: m.GetAPIReader()}).SetupWithManager(m))
	must(m.AddHealthzCheck("healthz", healthz.Ping))
	must(m.AddReadyzCheck("readyz", healthz.Ping))
	must(m.Start(ctrl.SetupSignalHandler()))
}
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
