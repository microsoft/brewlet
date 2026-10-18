package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (w *Worker) revokeRuntime(ctx context.Context, id string) error {
	if _, err := w.check(ctx); err != nil {
		return err
	}
	auths, err := w.Store.Authorizations()
	if err != nil {
		return err
	}
	var auth *node.Authorization
	for i := range auths {
		if auths[i].ID == id {
			auth = &auths[i]
			break
		}
	}
	if auth == nil {
		if _, err := control.Withdraw(ctx, w.Client, w.Reader, w.Authority, []string{"brewlet.sh/runtime." + id}); err != nil {
			return err
		}
		if err := w.Store.RemoveRequest(id); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(w.Store.Root, id)); !os.IsNotExist(err) {
			return fmt.Errorf("unproven uninstalled runtime")
		}
		leases, err := w.Store.Leases()
		if err != nil {
			return err
		}
		for _, lease := range leases {
			if lease.Metadata.ID == id {
				return fmt.Errorf("runtime lease remains without inventory")
			}
		}
		r := new(api.RuntimeImage)
		if err := w.Reader.Get(ctx, client.ObjectKey{Name: id}, r); apierrors.IsNotFound(err) {
			return nil
		} else if err != nil {
			return err
		}
		receipt := node.RevocationReceipt{InventoryUID: string(r.UID), Generation: r.Status.Generation, Digest: r.Status.Digest, ProfileUID: string(w.Authority.ProfileUID), ProfileGeneration: w.Authority.Generation, NodeUID: string(w.Authority.NodeUID), RevokedAt: time.Now().UTC(), AuthorizationExpiresAt: time.Now().UTC()}
		stagingClean := true
		var cleanupErr error
		if recovery, ok := w.Installer.(interface {
			RecoverStaging(context.Context, string) error
		}); ok {
			if err := recovery.RecoverStaging(ctx, w.Store.Root); err != nil {
				stagingClean = false
				cleanupErr = fmt.Errorf("staging recovery: %w", err)
			}
		}
		if mounts, ok := w.Evidence.(node.MountEvidence); ok {
			gone, err := mounts.MountsGone(ctx, filepath.Join(w.Store.Root, id))
			if err != nil {
				return err
			}
			receipt.RootsCleaned = gone && stagingClean
			if !gone {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("runtime inventory-prefix mounts remain"))
			}
		} else {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("complete inventory-prefix mount evidence required"))
		}
		return w.finishRevocation(ctx, id, receipt, false, cleanupErr)
	}
	if auth.ProfileUID != string(w.Authority.ProfileUID) {
		return fmt.Errorf("ownership-fence-failed: foreign local runtime")
	}
	revocationErr := w.Store.Revoke(ctx, id, string(w.Authority.ProfileUID))
	if os.IsNotExist(revocationErr) {
		revocationErr = nil
	}
	if _, err := control.Withdraw(ctx, w.Client, w.Reader, w.Authority, []string{"brewlet.sh/runtime." + id}); err != nil {
		return errors.Join(revocationErr, err)
	}
	if revocationErr != nil {
		return revocationErr
	}
	if _, err := w.check(ctx); err != nil {
		return err
	}
	if err := w.Store.RemoveRequest(id); err != nil {
		return err
	}
	receipt := node.RevocationReceipt{InventoryUID: auth.InventoryUID, Generation: auth.Generation, Digest: auth.Digest, ProfileUID: string(w.Authority.ProfileUID), ProfileGeneration: w.Authority.Generation, NodeUID: string(w.Authority.NodeUID), RevokedAt: time.Now().UTC(), AuthorizationExpiresAt: auth.ExpiresAt}
	target := new(api.RuntimeImage)
	if err := w.Reader.Get(ctx, client.ObjectKey{Name: id}, target); err == nil && string(target.UID) == auth.InventoryUID {
		// Cleaning every local generation also proves that a newer target
		// revoked before this node installed it has no remaining roots here.
		receipt.Generation = target.Status.Generation
		receipt.Digest = target.Status.Digest
	}
	fence := func(ctx context.Context) error { _, err := w.check(ctx); return err }
	stagingClean := true
	cleanupErr := fmt.Errorf("complete task/exec/mount evidence required")
	if recovery, ok := w.Installer.(interface {
		RecoverStaging(context.Context, string) error
	}); ok {
		if err := recovery.RecoverStaging(ctx, w.Store.Root); err != nil {
			stagingClean = false
			cleanupErr = fmt.Errorf("staging recovery: %w", err)
		}
	}
	if w.Evidence != nil && stagingClean {
		cleanupErr = w.Store.Recover(ctx, w.Evidence)
		if cleanupErr == nil {
			cleanupErr = w.Store.RetireRevoked(ctx, id, string(w.Authority.ProfileUID), w.Evidence, fence)
		}
		receipt.RootsCleaned = cleanupErr == nil
	}
	return w.finishRevocation(ctx, id, receipt, auth.Revoked, cleanupErr)
}

func (w *Worker) finishRevocation(ctx context.Context, id string, receipt node.RevocationReceipt, alreadyRevoked bool, cleanupErr error) error {
	if err := w.publishRevocation(ctx, id, receipt, alreadyRevoked); err != nil {
		return err
	}
	if cleanupErr != nil {
		deferred := fmt.Errorf("runtime-revocation-cleanup-deferred: id=%s inventoryUID=%s generation=%d profileUID=%s nodeUID=%s: %w",
			id, receipt.InventoryUID, receipt.Generation, receipt.ProfileUID, receipt.NodeUID, cleanupErr)
		if w.Authority.Mode == "cleanup" {
			return deferred
		}
		log.Print(deferred)
	}
	return nil
}
func (w *Worker) publishRevocation(ctx context.Context, id string, receipt node.RevocationReceipt, alreadyRevoked bool) error {
	s, err := w.check(ctx)
	if err != nil {
		return err
	}

	receipts := map[string]node.RevocationReceipt{}
	if raw := s.Node.Annotations[node.RevocationsAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &receipts); err != nil {
			return err
		}
	}
	if old, ok := receipts[id]; ok && alreadyRevoked && old.InventoryUID == receipt.InventoryUID && old.Generation == receipt.Generation && old.Digest == receipt.Digest && old.ProfileUID == receipt.ProfileUID && old.NodeUID == receipt.NodeUID {
		receipt.RevokedAt = old.RevokedAt
	}
	receipts[id] = receipt
	b, err := json.Marshal(receipts)
	if err != nil {
		return err
	}
	n := s.Node.DeepCopy()
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	n.Annotations[node.RevocationsAnnotation] = string(b)
	if err := w.Client.Update(ctx, n); err != nil {
		return err
	}
	s, err = w.check(ctx)
	if err != nil {
		return err
	}
	if s.Node.Annotations[node.RevocationsAnnotation] != string(b) {
		return fmt.Errorf("runtime revocation receipt readback failed")
	}
	return nil
}
