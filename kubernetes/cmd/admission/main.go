// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command admission runs the brewlet pod admission/scheduling webhook
// (https://github.com/microsoft/brewlet/tree/main/specs). It intercepts pods on CREATE, overwrites
// brewlet.sh/artifact-ref + artifact-digest informational hints from the image,
// validates any requested JDK/launcher against the ready node fleet
// (NoCompatibleJDK / NoCompatibleLauncher), and injects nodeAffinity so the
// scheduler only lands brewlet pods on capable nodes.
package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"brewlet-operator/internal/admission"
	"brewlet-operator/internal/controller"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	admissionpkg "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(nodev1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr    string
		probeAddr      string
		certDir        string
		webhookPort    int
		allowedMirrors string
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "address the metric endpoint binds to; 0 disables metrics")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	flag.StringVar(&certDir, "cert-dir", "/tmp/k8s-webhook-server/serving-certs", "directory holding tls.crt/tls.key for the webhook server")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "port the webhook server listens on")
	flag.StringVar(&allowedMirrors, "allowed-source-mirror-hosts", "", "comma-separated exact registry hosts approved as runtime source mirror destinations")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	policyHosts, err := controller.ParseAllowedSourceMirrorHosts(allowedMirrors)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --allowed-source-mirror-hosts: %v\n", err)
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	certWatcher, err := certwatcher.New(
		filepath.Join(certDir, "tls.crt"),
		filepath.Join(certDir, "tls.key"),
	)
	if err != nil {
		setupLog.Error(err, "unable to initialize serving certificate watcher")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		WebhookServer: webhook.NewServer(webhook.Options{
			Port:    webhookPort,
			CertDir: certDir,
			TLSOpts: []func(*tls.Config){
				func(config *tls.Config) {
					config.GetCertificate = certWatcher.GetCertificate
				},
			},
		}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	if err := mgr.Add(certWatcher); err != nil {
		setupLog.Error(err, "unable to add serving certificate watcher")
		os.Exit(1)
	}

	decoder := admissionpkg.NewDecoder(scheme)
	mgr.GetWebhookServer().Register("/mutate-pods", &admissionpkg.Webhook{
		Handler: &admission.PodMutator{
			Client:  mgr.GetClient(),
			Decoder: decoder,
		},
	})
	mgr.GetWebhookServer().Register("/validate-nodeprofiles", &admissionpkg.Webhook{
		Handler: &admission.NodeProfileValidator{
			Client:  mgr.GetClient(),
			Decoder: decoder,
			Policy: controller.NodeProfilePolicy{
				AllowedSourceMirrorHosts: policyHosts,
			},
		},
	})

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting brewlet-admission webhook", "port", webhookPort, "certDir", certDir)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running webhook manager")
		os.Exit(1)
	}
}
