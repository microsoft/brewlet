// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"
	"brewlet-operator/internal/uninstall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func isolatedUninstallClient(t *testing.T) client.Client {
	t.Helper()
	requireEnvtest(t)
	// The shared suite has no garbage collector, so earlier controller tests
	// leave worker objects after deleting their namespaces/profiles. Give
	// cluster-wide uninstall inventory its own API server, not a filtered reader.
	env := &envtest.Environment{
		CRDDirectoryPaths: testEnv.CRDDirectoryPaths, ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping uninstall envtest: %v", err)
		}
	})
	// Millisecond fixture polling must expire through the context, not fail
	// early because client-go cannot reserve a token before that deadline.
	cfg.QPS = -1
	c, err := client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func createUninstallProfile(t *testing.T, ctx context.Context, c client.Client) *nodev1alpha1.NodeProfile {
	t.Helper()
	p := createProfile(t, ctx, c, uniqueName("uninstall"), nodev1alpha1.NodeProfileSpec{
		JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 21)},
	})
	p.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	p.Annotations = map[string]string{
		"meta.helm.sh/release-name": "brewlet", "meta.helm.sh/release-namespace": "releases",
	}
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func uninstallOptions(namespace string) uninstall.Options {
	return uninstall.Options{
		ReleaseName: "brewlet", ReleaseNamespace: "releases", Namespace: namespace,
		Timeout: 2 * time.Second, PollInterval: 5 * time.Millisecond,
	}
}

type uninstallRaceClient struct {
	client.Client
	t            *testing.T
	beforeDelete func(context.Context, client.Object)
	attempts     int
	conflicts    int
}

func (c *uninstallRaceClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.t.Helper()
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if _, ok := obj.(*nodev1alpha1.NodeProfile); !ok || options.Preconditions == nil ||
		options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil ||
		*options.Preconditions.UID != obj.GetUID() ||
		*options.Preconditions.ResourceVersion != obj.GetResourceVersion() {
		c.t.Fatal("runner attempted an unguarded or collateral deletion")
	}
	c.attempts++
	if c.attempts == 1 {
		c.beforeDelete(ctx, obj)
	}
	err := c.Client.Delete(ctx, obj, opts...)
	if apierrors.IsConflict(err) {
		c.conflicts++
	}
	return err
}

func TestUninstallRealAPIDeletionPreconditions(t *testing.T) {
	base := isolatedUninstallClient(t)
	for _, change := range []string{"status", "ownership", "replacement"} {
		t.Run(change, func(t *testing.T) {
			ctx := testContext(t)
			namespace := createNamespace(t, ctx, base)
			p := createUninstallProfile(t, ctx, base)
			c := &uninstallRaceClient{Client: base, t: t}
			c.beforeDelete = func(ctx context.Context, _ client.Object) {
				fresh := getProfile(t, ctx, base, p.Name)
				switch change {
				case "status":
					fresh.Status.ObservedGeneration = fresh.Generation
					if err := base.Status().Update(ctx, &fresh); err != nil {
						t.Fatal(err)
					}
				case "ownership":
					fresh.Annotations["meta.helm.sh/release-name"] = "another-release"
					if err := base.Update(ctx, &fresh); err != nil {
						t.Fatal(err)
					}
				case "replacement":
					if err := base.Delete(ctx, &fresh); err != nil {
						t.Fatal(err)
					}
					replacement := &nodev1alpha1.NodeProfile{
						ObjectMeta: metav1.ObjectMeta{
							Name: fresh.Name, Labels: fresh.Labels, Annotations: fresh.Annotations,
						},
						Spec: fresh.Spec,
					}
					if err := base.Create(ctx, replacement); err != nil {
						t.Fatal(err)
					}
					if replacement.UID == p.UID {
						t.Fatal("API did not create a different profile incarnation")
					}
				}
			}
			err := uninstall.Run(ctx, c, uninstallOptions(namespace))
			if c.conflicts != 1 {
				t.Fatalf("real API conflicts=%d, want exactly one", c.conflicts)
			}
			if change == "status" {
				if err != nil || c.attempts != 2 {
					t.Fatalf("status-only conflict was not retried: %v (attempts=%d)", err, c.attempts)
				}
				if err := base.Get(ctx, client.ObjectKeyFromObject(p), &nodev1alpha1.NodeProfile{}); !apierrors.IsNotFound(err) {
					t.Fatalf("owned profile was not deleted after guarded retry: %v", err)
				}
			} else {
				if err == nil || c.attempts != 1 {
					t.Fatalf("unsafe retry after %s: %v (attempts=%d)", change, err, c.attempts)
				}
				current := getProfile(t, ctx, base, p.Name)
				if !current.DeletionTimestamp.IsZero() {
					t.Fatal("replacement or newly independent profile was deleted")
				}
				if change == "replacement" && !strings.Contains(err.Error(), "was replaced") {
					t.Fatalf("missing actionable replacement refusal: %v", err)
				}
			}
		})
	}
}

func TestUninstallRealAPIKeepsHeldFinalizers(t *testing.T) {
	c := isolatedUninstallClient(t)
	ctx := testContext(t)
	namespace := createNamespace(t, ctx, c)
	p := createUninstallProfile(t, ctx, c)
	p.Finalizers = []string{brewlet.FinalizerCleanup, "test.brewlet.sh/hold"}
	if err := c.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	o := uninstallOptions(namespace)
	o.Timeout = 250 * time.Millisecond
	err := uninstall.Run(ctx, c, o)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), p.Name) ||
		!strings.Contains(err.Error(), "keep the operator") {
		t.Fatalf("held finalizer did not block uninstall: %v", err)
	}
	current := getProfile(t, ctx, c, p.Name)
	if current.DeletionTimestamp.IsZero() || len(current.Finalizers) != 2 ||
		current.Finalizers[0] != brewlet.FinalizerCleanup {
		t.Fatalf("cleanup runner bypassed finalization: %+v", current.ObjectMeta)
	}
}
