// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command manager runs the brewlet-operator node lifecycle controller
// (https://github.com/microsoft/brewlet/tree/main/specs): it watches nodes opted into provisioning,
// brewlet-node-provisioner DaemonSet and the brewlet RuntimeClass, and surfaces
// each node's provisioning state via annotations and events.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	appsv1alpha1 "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/controller"
	"brewlet-operator/internal/uninstall"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var scheme = runtime.NewScheme()

func init() {
	// client-go's scheme registers core/v1, apps/v1, autoscaling/v1 and node/v1,
	// which covers the node controller and everything the JavaApplication
	// controller generates. Add our own apps.brewlet.sh/v1alpha1 types on top.
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
	utilruntime.Must(nodev1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		cfg              controller.Config
		metricsAddr      string
		probeAddr        string
		enableLeaderElec bool
		allowedMirrors   string
	)
	flag.StringVar(&cfg.Namespace, "namespace", "brewlet", "operator namespace (uninstall inventories workers cluster-wide)")
	flag.StringVar(&cfg.ProvisionerImage, "provisioner-image", "ghcr.io/microsoft/brewlet-node-provisioner:0.1.0", "brewlet-node-provisioner image to run")
	flag.IntVar(&cfg.MetricsPort, "node-metrics-port", 9090, "node provisioner metrics exporter port")
	flag.BoolVar(&cfg.MetricsEnabled, "node-metrics-enabled", false, "run the node-local metrics exporter in provisioner pods")
	flag.StringVar(&allowedMirrors, "allowed-source-mirror-hosts", "", "comma-separated exact registry hosts approved as runtime source mirror destinations")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "address the metric endpoint binds to; 0 disables metrics")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	flag.BoolVar(&enableLeaderElec, "leader-elect", false, "enable leader election for HA (single active manager)")
	cleanupFlags := bindUninstallFlags(flag.CommandLine)
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	cleanup, cleanupMode, err := cleanupFlags.options(flag.CommandLine, cfg.Namespace)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid uninstall arguments: %v\n", err)
		os.Exit(1)
	}
	if cleanupMode {
		ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
		if err := runUninstall(ctrl.SetupSignalHandler(), cleanup); err != nil {
			ctrl.Log.WithName("uninstall").Error(err, "cleanup failed; operator and RBAC must remain installed")
			os.Exit(1)
		}
		ctrl.Log.WithName("uninstall").Info("NodeProfiles and workers removed; operator uninstall may proceed")
		return
	}
	cfg.AllowedSourceMirrorHosts, err = controller.ParseAllowedSourceMirrorHosts(allowedMirrors)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --allowed-source-mirror-hosts: %v\n", err)
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElec,
		LeaderElectionID:       "brewlet-operator.brewlet.sh",
		// The managed provisioner/cleanup DaemonSets only ever live in the
		// operator's namespace, so the informer is scoped to it and the
		// operator needs no cluster-wide DaemonSet authority (a namespaced Role
		// is enough). Every other watched type stays cluster-wide.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&appsv1.DaemonSet{}: {
					Namespaces: map[string]cache.Config{cfg.Namespace: {}},
				},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.NodeReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("brewlet-operator"),
		Config:   cfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Node")
		os.Exit(1)
	}

	if err := (&controller.NodeProfileReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorderFor("brewlet-operator"),
		Config:    cfg,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NodeProfile")
		os.Exit(1)
	}

	if err := (&controller.JavaApplicationReconciler{
		Client:    mgr.GetClient(),
		APIReader: mgr.GetAPIReader(),
		Scheme:    mgr.GetScheme(),
		Recorder:  mgr.GetEventRecorderFor("brewlet-operator"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "JavaApplication")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting brewlet-operator",
		"namespace", cfg.Namespace, "provisionerImage", cfg.ProvisionerImage)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

type uninstallFlags struct {
	value uninstall.Options
}

func bindUninstallFlags(fs *flag.FlagSet) *uninstallFlags {
	f := &uninstallFlags{}
	fs.StringVar(&f.value.ReleaseName, "uninstall-release-name", "", "Helm release name to clean up before uninstalling the operator (requires --uninstall-release-namespace)")
	fs.StringVar(&f.value.ReleaseNamespace, "uninstall-release-namespace", "", "Helm release namespace to clean up (requires --uninstall-release-name)")
	fs.DurationVar(&f.value.Timeout, "uninstall-timeout", uninstall.DefaultTimeout, "maximum time to wait for NodeProfile and worker cleanup before failing uninstall")
	return f
}

func (f *uninstallFlags) options(fs *flag.FlagSet, namespace string) (uninstall.Options, bool, error) {
	o := f.value
	o.Namespace = namespace
	if fs.NArg() != 0 {
		return o, false, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	var name, releaseNamespace, timeout bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "uninstall-release-name":
			name = true
		case "uninstall-release-namespace":
			releaseNamespace = true
		case "uninstall-timeout":
			timeout = true
		}
	})
	if !name && !releaseNamespace && !timeout {
		return o, false, nil
	}
	if !name || !releaseNamespace {
		return o, false, fmt.Errorf("both --uninstall-release-name and --uninstall-release-namespace are required for cleanup mode")
	}
	return o, true, o.Validate()
}

func runUninstall(ctx context.Context, o uninstall.Options) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("loading in-cluster uninstall configuration: %w", err)
	}
	config.Timeout = o.Timeout
	c, err := newUninstallClient(config)
	if err != nil {
		return fmt.Errorf("creating direct uninstall client: %w", err)
	}
	return uninstall.Run(ctx, c, o)
}

func newUninstallClient(config *rest.Config) (client.Client, error) {
	// All cleanup resources are known. A static mapper avoids background
	// discovery requests outside Run's deadline and additional discovery RBAC.
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		nodev1alpha1.GroupVersion, appsv1.SchemeGroupVersion, corev1.SchemeGroupVersion,
	})
	mapper.Add(nodev1alpha1.GroupVersion.WithKind("NodeProfile"), meta.RESTScopeRoot)
	mapper.Add(appsv1.SchemeGroupVersion.WithKind("DaemonSet"), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Pod"), meta.RESTScopeNamespace)
	return client.New(config, client.Options{Scheme: scheme, Mapper: mapper})
}
