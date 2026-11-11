//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/node"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type drainingCleanupEvidence struct{ calls int }

func (e *drainingCleanupEvidence) Gone(context.Context, node.Lease) (bool, error) {
	e.calls++
	return e.calls > 1, nil
}
func (*drainingCleanupEvidence) MountsGone(context.Context, string) (bool, error) { return true, nil }

type uninstalledCleanupEvidence struct{ mountsGone bool }

func (uninstalledCleanupEvidence) Gone(context.Context, node.Lease) (bool, error) {
	return true, nil
}
func (e uninstalledCleanupEvidence) MountsGone(context.Context, string) (bool, error) {
	return e.mountsGone, nil
}

func TestCleanupReceiptsForUninstalledRuntimeObligations(t *testing.T) {
	for _, historical := range []bool{false, true} {
		for _, mountsGone := range []bool{false, true} {
			name := "offered"
			if historical {
				name = "historical"
			}
			t.Run(name+"/mountsGone="+strconv.FormatBool(mountsGone), func(t *testing.T) {
				w, _ := authorityFixture(t)
				requireRootWorkdir(t, w)
				approveStatic(t, w)
				ctx := context.Background()
				p := new(api.NodeProfile)
				if err := w.Client.Get(ctx, client.ObjectKey{Name: w.Authority.ProfileName}, p); err != nil {
					t.Fatal(err)
				}
				if historical {
					p.Spec.RuntimeImages = nil
				}
				p.Status.Retirement = &api.Retirement{Generation: w.Authority.Generation, Spec: p.Spec,
					Targets: p.Status.Targets, Phase: "Cleaning"}
				if err := w.Client.Update(ctx, p); err != nil {
					t.Fatal(err)
				}
				w.Authority.Mode = "cleanup"
				evidence := uninstalledCleanupEvidence{mountsGone: mountsGone}
				w.Evidence = evidence
				err := w.Cleanup(ctx, evidence)
				n := readinessNode(t, w)
				if !mountsGone {
					if err == nil || n.Annotations["brewlet.sh/cleanup-complete"] != "" {
						t.Fatalf("cleanup certified an unproven runtime: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var receipts map[string]node.RevocationReceipt
				if err := json.Unmarshal([]byte(n.Annotations[node.RevocationsAnnotation]), &receipts); err != nil {
					t.Fatal(err)
				}
				receipt := receipts["static"]
				if !receipt.RootsCleaned || receipt.InventoryUID != "epoch-one" ||
					receipt.ProfileUID != string(w.Authority.ProfileUID) || receipt.NodeUID != string(w.Authority.NodeUID) {
					t.Fatalf("missing genuine uninstalled-runtime receipt: %+v", receipt)
				}
				if n.Annotations["brewlet.sh/cleanup-complete"] == "" {
					t.Fatal("completed cleanup was not recorded")
				}
			})
		}
	}
}

func TestCleanupCannotForgetAuthorizationBeforeCompleteRevocationReceipt(t *testing.T) {
	w, _ := recoveryFixture(t, map[string]string{"static": "OnDemand"}, false)
	ctx := context.Background()
	p := new(api.NodeProfile)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: w.Authority.ProfileName}, p); err != nil {
		t.Fatal(err)
	}
	p.Status.Retirement = &api.Retirement{Generation: w.Authority.Generation, Spec: p.Spec,
		Targets: p.Status.Targets, Phase: "Cleaning"}
	if err := w.Client.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	w.Authority.Mode = "cleanup"
	evidence := new(drainingCleanupEvidence)
	w.Evidence = evidence
	if err := w.Cleanup(ctx, evidence); err == nil ||
		!strings.Contains(err.Error(), "runtime-revocation-cleanup-deferred: id=static inventoryUID=epoch-static generation=2") ||
		!strings.Contains(err.Error(), "runtime mounts have not drained") {
		t.Fatal("cleanup forgot identity or hid the incomplete receipt's actual cause", err)
	}
	auths, err := w.Store.Authorizations()
	if err != nil || len(auths) != 1 || !auths[0].Revoked {
		t.Fatal("incomplete receipt lost the original authorization identity", auths, err)
	}
	if readinessNode(t, w).Annotations["brewlet.sh/cleanup-complete"] != "" {
		t.Fatal("incomplete receipt certified profile cleanup")
	}
	if err := w.Cleanup(ctx, evidence); err != nil {
		t.Fatal("fresh complete evidence could not finish normal cleanup retry", err)
	}
	var receipts map[string]node.RevocationReceipt
	if err := json.Unmarshal([]byte(readinessNode(t, w).Annotations[node.RevocationsAnnotation]), &receipts); err != nil {
		t.Fatal(err)
	}
	receipt := receipts["static"]
	if !receipt.RootsCleaned || receipt.InventoryUID != "epoch-static" ||
		receipt.ProfileUID != string(w.Authority.ProfileUID) || receipt.Generation != 2 {
		t.Fatal("successful cleanup lost its complete original-identity receipt", receipt)
	}
	if auths, err := w.Store.Authorizations(); err != nil || len(auths) != 0 {
		t.Fatal("successful receipt did not precede inventory removal", auths, err)
	}
}
