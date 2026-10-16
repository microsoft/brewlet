//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type completeEvidence struct{}

func (completeEvidence) Gone(context.Context, node.Lease) (bool, error)   { return true, nil }
func (completeEvidence) MountsGone(context.Context, string) (bool, error) { return true, nil }
func TestRevocationReceiptCoversNewerUninstalledTarget(t *testing.T) {
	w, _ := authorityFixture(t)
	requireRootWorkdir(t, w)
	approveStatic(t, w)
	ctx := context.Background()
	w.Evidence = completeEvidence{}
	a := node.Authorization{ID: "static", InventoryUID: "epoch-one", Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64), ProfileUID: "profile-one", ProfileGeneration: 1, ExpiresAt: time.Now().Add(time.Minute), Install: "OnDemand", State: "available"}
	if err := w.Store.WriteAuthorization(ctx, a); err != nil {
		t.Fatal(err)
	}
	r := new(api.RuntimeImage)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "static"}, r); err != nil {
		t.Fatal(err)
	}
	r.Status.Generation = 2
	r.Status.Digest = "sha256:" + strings.Repeat("c", 64)
	if err := w.Client.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := w.revokeRuntime(ctx, "static"); err != nil {
		t.Fatal(err)
	}
	n := new(corev1.Node)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	var receipts map[string]node.RevocationReceipt
	if err := json.Unmarshal([]byte(n.Annotations[node.RevocationsAnnotation]), &receipts); err != nil {
		t.Fatal(err)
	}
	receipt := receipts["static"]
	if receipt.InventoryUID != "epoch-one" || receipt.Generation != 2 || receipt.Digest != r.Status.Digest || !receipt.RootsCleaned || receipt.NodeUID != "node-one" || receipt.ProfileUID != "profile-one" {
		t.Fatal(receipt)
	}
	if _, _, err := w.Store.Resolve(ctx, "static", "k8s.io", "new-start", nil, time.Millisecond); err == nil || !strings.HasPrefix(err.Error(), "RuntimeImageRevoked:") {
		t.Fatal("revoked tombstone was not retained", err)
	}
}
