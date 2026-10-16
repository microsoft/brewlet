package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/internal/admission"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metrics "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	wa "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func main() {
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil)))
	var hosts, certDir string
	var port int
	flag.StringVar(&hosts, "allowed-source-mirror-hosts", "", "comma-separated exact mirror hosts")
	flag.StringVar(&certDir, "cert-dir", "/certs", "serving tls.crt and tls.key directory")
	flag.IntVar(&port, "port", 9443, "TLS webhook port")
	flag.Parse()
	s := runtime.NewScheme()
	must(corev1.AddToScheme(s))
	must(api.AddToScheme(s))
	server := webhook.NewServer(webhook.Options{Port: port, CertDir: certDir})
	m, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: s, Metrics: metrics.Options{BindAddress: "0"}, HealthProbeBindAddress: ":8081", WebhookServer: server})
	must(err)
	// Manager registers its configured webhook server lazily. Registering routes
	// directly on the options value does not add that server to the runnable set.
	server = m.GetWebhookServer()
	allowed := []string{}
	if hosts != "" {
		allowed = strings.Split(hosts, ",")
	}
	reader := m.GetAPIReader()
	server.Register("/mutate-pods", &wa.Webhook{Handler: &admission.PodHandler{Mutator: &admission.PodMutator{Reader: reader, AllowedMirrorHosts: allowed}}})
	server.Register("/validate-nodeprofiles", &wa.Webhook{Handler: &admission.InventoryHandler{Reader: reader, AllowedMirrorHosts: allowed}})
	server.Register("/validate-runtimeimages", &wa.Webhook{Handler: &admission.InventoryHandler{Reader: reader, Runtime: true}})
	server.Register("/validate-retirement-evidence", &wa.Webhook{Handler: &admission.InventoryHandler{Reader: reader, Evidence: true}})
	must(m.AddHealthzCheck("healthz", healthz.Ping))
	must(m.AddReadyzCheck("readyz", server.StartedChecker()))
	must(m.Start(ctrl.SetupSignalHandler()))
}
func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
