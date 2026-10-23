//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	"github.com/microsoft/brewlet/provisioner"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--health-check" {
		if err := healthCheck(); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	if err := removeReady(); err != nil {
		return err
	}
	defer func() {
		if err := removeReady(); err != nil {
			log.Printf("worker readiness removal failed: %v", err)
		}
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := api.AddToScheme(scheme); err != nil {
		return err
	}
	cfg, err := config.GetConfig()
	if err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	a := control.Authority{NodeName: os.Getenv("NODE_NAME"), ProfileName: os.Getenv("BREWLET_PROFILE_NAME"), ProfileUID: types.UID(os.Getenv("BREWLET_PROFILE_UID")), Mode: os.Getenv("BREWLET_MODE")}
	a.Generation, err = strconv.ParseInt(os.Getenv("BREWLET_PROFILE_GENERATION"), 10, 64)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(os.Getenv("BREWLET_TARGET_UIDS")), &a.TargetUIDs); err != nil {
		return err
	}
	if a.NodeName == "" || a.ProfileName == "" || a.ProfileUID == "" {
		return fmt.Errorf("literal worker authority environment is required")
	}
	n := new(corev1.Node)
	if err := c.Get(ctx, client.ObjectKey{Name: a.NodeName}, n); err != nil {
		return err
	}
	a.NodeUID = n.UID
	if _, err := control.Check(ctx, c, a); err != nil {
		return err
	}
	host := "/host"
	store := node.NewStore(host + "/opt/brewlet/runtimes")
	if err := store.Initialize(); err != nil {
		return err
	}
	writer, err := store.AcquireWriter(ctx)
	if err != nil {
		return err
	}
	defer writer.Close()
	if _, err := control.Check(ctx, c, a); err != nil {
		return err
	}
	cd, err := containerd.New(host + "/run/containerd/containerd.sock")
	if err != nil {
		return err
	}
	defer cd.Close()
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	installer := &provisioner.ContainerdInstaller{Client: cd, Snapshotter: "overlayfs", RegistryHostsDir: host + "/etc/containerd/certs.d"}
	installer.ProfileUID = string(a.ProfileUID)
	installer.NodeUID = string(a.NodeUID)
	installer.ProfileGeneration = a.Generation
	installer.Fence = func(ctx context.Context) error { _, err := control.Check(ctx, c, a); return err }
	installer.MountEvidence = provisioner.HostEvidence{HostRoot: host, Client: cd}
	worker := &provisioner.Worker{Client: c, Reader: c, Authority: a, Store: store, Installer: installer, Baseline: provisioner.HostBaseline{HostRoot: host}}
	worker.Activation = provisioner.HostActivation{HostRoot: host, InventoryRoot: store.Root, SourceBinary: "/containerd-shim-brewlet-v2", ProfileUID: string(a.ProfileUID), NodeUID: string(a.NodeUID)}
	worker.Evidence = provisioner.HostEvidence{HostRoot: host, Client: cd}
	worker.Tasks = cd
	worker.GCGrace, err = productionDuration("BREWLET_GC_GRACE", time.Hour)
	if err != nil {
		return err
	}
	worker.GCSweepInterval, err = productionDuration("BREWLET_GC_SWEEP_INTERVAL", time.Minute)
	if err != nil {
		return err
	}
	worker.CacheDir = host + "/var/lib/brewlet/layers"
	worker.CacheHostPath = "/var/lib/brewlet/layers"
	if address := os.Getenv("BREWLET_METRICS_ADDRESS"); address != "" {
		registry := prometheus.NewRegistry()
		worker.Metrics = provisioner.NewMetrics(registry, store, host+"/var/lib/brewlet/layers", cd)
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}
		server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
				log.Printf("metrics: %v", err)
			}
		}()
		defer func() {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		}()
	}
	var allowed []string
	if raw := os.Getenv("SOURCE_ALLOWED_MIRROR_HOSTS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &allowed); err != nil {
			return fmt.Errorf("invalid SOURCE_ALLOWED_MIRROR_HOSTS: %w", err)
		}
	}
	worker.AllowedMirrorHosts = allowed
	backoff := time.Second
	cleanupDone := false
	for {
		s, err := control.Check(ctx, c, a)
		if err != nil {
			return err
		}
		if a.Mode == "cleanup" {
			if !cleanupDone {
				if err := worker.Cleanup(ctx, provisioner.HostEvidence{HostRoot: host, Client: cd}); err != nil {
					return err
				}
				cleanupDone = true
			}
			if err := markReady(fmt.Sprintf("%s/%d/%s/cleanup", a.ProfileUID, a.Generation, a.NodeUID)); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}
		installer.Rewrite = func(source string) (string, error) {
			return provisioner.SourceReference(source, s.Spec.Registry.Mirrors, allowed)
		}
		reconcileErr := worker.Reconcile(ctx)
		if err := syncProvisioningReadinessAt(readyMarker,
			fmt.Sprintf("%s/%d/%s/provisioning", a.ProfileUID, a.Generation, a.NodeUID),
			reconcileErr == nil, func() error {
				_, err := control.Check(ctx, c, a)
				return err
			}); err != nil {
			return errors.Join(reconcileErr, err)
		}
		if reconcileErr != nil {
			log.Printf("provision-error: %v", reconcileErr)
			if _, fenceErr := control.Check(ctx, c, a); fenceErr != nil {
				return errors.Join(reconcileErr, fenceErr)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}
