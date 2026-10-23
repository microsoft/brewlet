//go:build linux

package provisioner

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type baselineSuccess struct{}

func (baselineSuccess) Validate(context.Context) error { return nil }

type neverInstall struct{ calls int }

func (i *neverInstall) Stage(context.Context, *api.RuntimeImage, string) (string, node.Metadata, error) {
	i.calls++
	return "", node.Metadata{}, fmt.Errorf("cold runtime must not be fetched")
}
func approveStatic(t *testing.T, w *Worker) {
	t.Helper()
	source := "registry.example.com/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	r := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "static", UID: "epoch-one", Generation: 1}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: source}, User: api.User{UID: 1000, GID: 1000}}, Status: api.RuntimeImageStatus{Generation: 1, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceImage: source, Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}, RevocationTargets: []api.InventoryTarget{{NodeName: w.Authority.NodeName, NodeUID: w.Authority.NodeUID, ProfileName: w.Authority.ProfileName, ProfileUID: w.Authority.ProfileUID, ProfileGeneration: w.Authority.Generation, Generation: 1, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}}
	if err := w.Client.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}
func requireRootWorkdir(t *testing.T, w *Worker) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires root-owned isolated Linux working directory")
	}
	if err := w.Store.Initialize(); err != nil {
		t.Skipf("requires root-owned isolated Linux working directory: %v", err)
	}
}
func TestOnDemandOnlyFakeAPINeverFetchesAndReleasesFence(t *testing.T) {
	w, _ := authorityFixture(t)
	requireRootWorkdir(t, w)
	w.Baseline = baselineSuccess{}
	installer := new(neverInstall)
	w.Installer = installer
	approveStatic(t, w)
	ctx := context.Background()
	n := new(corev1.Node)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	n.Spec.Taints = []corev1.Taint{{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}}
	if err := w.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := w.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if installer.calls != 0 {
		t.Fatal("cold runtime fetched")
	}
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	if n.Labels[api.RuntimeLabel] != "ready" || n.Labels["brewlet.sh/runtime.static"] != "available" || control.StartupFenced(n) {
		t.Fatalf("node state %+v", n)
	}
	if _, err := os.Stat(w.Store.Root + "/static/current"); !os.IsNotExist(err) {
		t.Fatal("cold runtime published", err)
	}
}

func TestUnauthorizedRequestRemovalAndLoggingRequireFreshOwner(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			w, _ := authorityFixture(t)
			requireRootWorkdir(t, w)
			w.Baseline = baselineSuccess{}
			w.Installer = new(neverInstall)
			approveStatic(t, w)
			marker := filepath.Join(w.Store.Root, ".requests", "not-offered")
			if err := os.WriteFile(marker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if !valid {
				w.Authority.NodeUID = "foreign-node"
			}
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			err := w.Reconcile(context.Background())
			if !valid {
				if err == nil || !strings.Contains(err.Error(), "ownership-fence-failed") {
					t.Fatalf("stale authority did not fail closed: %v", err)
				}
				if _, err := os.Stat(marker); err != nil || output.Len() != 0 {
					t.Fatalf("unauthorized owner removed or logged the request: stat=%v log=%q", err, output.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("current owner retained unauthorized request", err)
			}
			if !strings.Contains(output.String(), "unauthorized-runtime-request: removed id=not-offered not offered by profile=profile uid=profile-one generation=1") {
				t.Fatalf("request removal lost authority diagnostics: %q", output.String())
			}
		})
	}
}

type pinnedClient struct{ client.Client }

func (c pinnedClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if n, ok := obj.(*corev1.Node); ok {
		if _, present := n.Labels["brewlet.sh/runtime.static"]; !present {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, n.Name, fmt.Errorf("provider pins available label"))
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}
func TestPinnedOnDemandFakeAPIKeepsConfirmedNodeFence(t *testing.T) {
	w, _ := authorityFixture(t)
	requireRootWorkdir(t, w)
	w.Baseline = baselineSuccess{}
	w.Installer = new(neverInstall)
	approveStatic(t, w)
	ctx := context.Background()
	n := new(corev1.Node)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	n.Labels["brewlet.sh/runtime.static"] = "available"
	if err := w.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	w.Client = pinnedClient{w.Client}
	if err := w.Reconcile(ctx); err == nil {
		t.Fatal("pinned OnDemand accepted")
	}
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	if !control.StartupFenced(n) || n.Labels["brewlet.sh/runtime.static"] != "available" {
		t.Fatal("provider refusal did not preserve confirmed broader fence")
	}
}
