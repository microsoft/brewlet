//go:build linux

package provisioner

import (
	"context"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRecoveryRetainedInvalidAndUnknownCapabilitiesRequirePoolCorrection(t *testing.T) {
	for _, value := range []string{"available", "invalid-state"} {
		t.Run(value, func(t *testing.T) {
			w, inventory := recoveryFixture(t, map[string]string{"static": "Eager"}, true)
			inventory["static"] = setRecoveryCurrent(t, w, "static", "Eager", 2, "sha256:"+strings.Repeat("c", 64))
			ctx := context.Background()
			n := new(corev1.Node)
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			n.Labels["brewlet.sh/runtime.static"] = value
			if err := w.Client.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			if err := w.Reconcile(ctx); err == nil {
				t.Fatal("retained invalid Eager state normalized into readiness")
			}
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			if !control.StartupFenced(n) || n.Labels["brewlet.sh/runtime.static"] != value {
				t.Fatal("invalid pool-label evidence lost")
			}
			n.Labels["brewlet.sh/runtime.static"] = "ready"
			if err := w.Client.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			if err := w.publish(ctx, inventory); err != nil {
				t.Fatal(err)
			}
		})
	}
	w, inventory := recoveryFixture(t, map[string]string{"static": "Eager"}, true)
	inventory["static"] = setRecoveryCurrent(t, w, "static", "Eager", 2, "sha256:"+strings.Repeat("c", 64))
	ctx := context.Background()
	p := new(api.NodeProfile)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "profile"}, p); err != nil {
		t.Fatal(err)
	}
	p.Spec.RuntimeImages = append(p.Spec.RuntimeImages, api.RuntimeEntry{ID: "missing", Install: "Eager"})
	copy := p.Spec
	p.Status.ProvisioningSpec = &copy
	if err := w.Client.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	n := new(corev1.Node)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	n.Labels["brewlet.sh/runtime.missing"] = "ready"
	if err := w.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := w.Reconcile(ctx); err == nil {
		t.Fatal("unknown retained capability removed and fence released")
	}
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	if !control.StartupFenced(n) || n.Labels["brewlet.sh/runtime.missing"] != "ready" {
		t.Fatal("unknown capability evidence lost")
	}
	delete(n.Labels, "brewlet.sh/runtime.missing")
	if err := w.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := w.publish(ctx, inventory); err != nil {
		t.Fatal("unknown ID without retained advertisement blocked valid siblings", err)
	}
}
