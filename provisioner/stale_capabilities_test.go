package provisioner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type stalePinClient struct {
	client.Client
	key    string
	pinned bool
}

func (c stalePinClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if node, ok := obj.(*corev1.Node); ok && c.pinned && node.Labels[c.key] != "ready" {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, node.Name,
			fmt.Errorf("provider-pinned stale capability"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestNewStaleProviderPinFencesHealthyNodeBeforeAnyHostMutation(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprint(pinned), func(t *testing.T) {
			w, root := authorityFixture(t)
			ctx := context.Background()
			n := new(corev1.Node)
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			key := "brewlet.sh/runtime.stale"
			n.Labels[key] = "ready"
			if err := w.Client.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			base := w.Client
			w.Client = stalePinClient{Client: base, key: key, pinned: pinned}
			err := w.Reconcile(ctx)
			if err == nil || !strings.Contains(err.Error(), "stale capabilities") {
				t.Fatalf("stale capability was ignored: %v", err)
			}
			if err := base.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			if pinned {
				if n.Labels[key] != "ready" || !control.StartupFenced(n) {
					t.Fatal("pinned stale capability remained eligible without confirmed startup fence")
				}
				if err := w.Reconcile(ctx); err == nil {
					t.Fatal("retained stale pin released recovery")
				}
				if err := base.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
					t.Fatal(err)
				}
				if n.Labels[key] != "ready" || !control.StartupFenced(n) {
					t.Fatal("recovery normalized retained provider evidence or released its fence")
				}
			} else if _, exists := n.Labels[key]; exists {
				t.Fatal("mutable stale capability was not actually withdrawn")
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("stale advertisement handling mutated host state: %v, %v", entries, err)
			}
			if n.Labels[api.OwnerUID] != string(w.Authority.ProfileUID) {
				t.Fatal("stale capability handling changed ownership")
			}
		})
	}
}
