//go:build linux

package provisioner

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type publicationRaceClient struct {
	client.Client
	advanced bool
}

func (c *publicationRaceClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	if n, ok := object.(*corev1.Node); ok && !c.advanced && n.Labels[api.RuntimeLabel] == "ready" {
		c.advanced = true
		r := new(api.RuntimeImage)
		if err := c.Client.Get(ctx, client.ObjectKey{Name: "unaffected"}, r); err != nil {
			return err
		}
		r.Status.Generation = 3
		r.Status.Digest = "sha256:" + strings.Repeat("d", 64)
		r.Spec.Source.Image = "registry.example.com/unaffected@" + r.Status.Digest
		r.Status.SourceImage = r.Spec.Source.Image
		for i := range r.Status.RevocationTargets {
			r.Status.RevocationTargets[i].Generation = 3
			r.Status.RevocationTargets[i].Digest = r.Status.Digest
		}
		if err := c.Client.Update(ctx, r); err != nil {
			return err
		}
	}
	return c.Client.Update(ctx, object, opts...)
}
func TestRecoveryRechecksEveryTargetAfterAdvertisementReadback(t *testing.T) {
	w, inventory := recoveryFixture(t, map[string]string{"static": "Eager", "unaffected": "Eager"}, true)
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("c", 64)
	for id := range inventory {
		inventory[id] = setRecoveryCurrent(t, w, id, "Eager", 2, digest)
	}
	racing := &publicationRaceClient{Client: w.Client}
	w.Client = racing
	if err := w.publish(ctx, inventory); err == nil {
		t.Fatal("target advanced during publication but node-wide fence released")
	}
	n := new(corev1.Node)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	if !control.StartupFenced(n) {
		t.Fatal("unaffected ID race released startup fence")
	}
	newDigest := "sha256:" + strings.Repeat("d", 64)
	if err := w.Store.WriteAuthorization(ctx, node.Authorization{ID: "unaffected", InventoryUID: "epoch-unaffected", Generation: 3, Digest: newDigest, ProfileUID: string(w.Authority.ProfileUID), ProfileGeneration: 1, ExpiresAt: time.Now().Add(5 * time.Minute), Install: "Eager", State: "ready"}); err != nil {
		t.Fatal(err)
	}
	inventory["unaffected"] = setRecoveryCurrent(t, w, "unaffected", "Eager", 3, newDigest)
	if err := w.publish(ctx, inventory); err != nil {
		t.Fatal("eventual all-latest release failed", err)
	}
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	if control.HasStartupTaint(n) {
		t.Fatal("corrected all-ID target remained fenced")
	}
}
