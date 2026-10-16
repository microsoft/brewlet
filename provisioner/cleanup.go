package provisioner

import (
	"context"
	"errors"
	"fmt"
	"log"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
)

func (w *Worker) Cleanup(ctx context.Context, evidence node.Evidence) error {
	if w.Authority.Mode != "cleanup" {
		return fmt.Errorf("cleanup authority required")
	}
	if _, err := w.check(ctx); err != nil {
		return err
	}
	defer func() {
		if err := w.publishBindings(ctx); err != nil {
			log.Printf("runtime binding observation unavailable: %v", err)
		}
	}()
	authorizations, err := w.Store.Authorizations()
	if err != nil {
		return err
	}
	var keys = []string{api.RuntimeLabel}
	var revocationErrors []error
	for _, a := range authorizations {
		if a.ProfileUID != string(w.Authority.ProfileUID) {
			return fmt.Errorf("ownership-fence-failed: foreign inventory")
		}
		if _, err := w.check(ctx); err != nil {
			return err
		}
		if err := w.Store.Revoke(ctx, a.ID, string(w.Authority.ProfileUID)); err != nil {
			revocationErrors = append(revocationErrors, err)
		}
		keys = append(keys, "brewlet.sh/runtime."+a.ID)
	}
	if _, err := control.Withdraw(ctx, w.Client, w.Reader, w.Authority, keys); err != nil {
		return errors.Join(append(revocationErrors, err)...)
	}
	if len(revocationErrors) > 0 {
		return errors.Join(revocationErrors...)
	}
	if _, err := w.check(ctx); err != nil {
		return err
	}
	if recovery, ok := w.Installer.(interface {
		RecoverStaging(context.Context, string) error
	}); ok {
		if err := recovery.RecoverStaging(ctx, w.Store.Root); err != nil {
			return err
		}
	}
	if err := w.Store.Recover(ctx, evidence); err != nil {
		return err
	}
	for _, a := range authorizations {
		if _, err := w.check(ctx); err != nil {
			return err
		}
		if err := w.revokeRuntime(ctx, a.ID); err != nil {
			return err
		}
		fence := func(ctx context.Context) error { _, err := w.check(ctx); return err }
		if err := w.Store.Retire(ctx, a.ID, string(w.Authority.ProfileUID), evidence, fence); err != nil {
			return err
		}
		if _, err := w.check(ctx); err != nil {
			return err
		}
		if err := w.Store.RemoveRequest(a.ID); err != nil {
			return err
		}
	}
	if w.Activation != nil {
		drain, ok := evidence.(interface {
			RuntimeGone(context.Context) (bool, error)
		})
		if !ok {
			return fmt.Errorf("complete shim/sandbox task drain evidence required before host reversal")
		}
		gone, err := drain.RuntimeGone(ctx)
		if err != nil {
			return err
		}
		if !gone {
			return fmt.Errorf("Brewlet runtime tasks, including sandboxes, remain")
		}
		fence := func(ctx context.Context) error { _, err := w.check(ctx); return err }
		if err := w.Activation.Restore(ctx, fence); err != nil {
			return err
		}
	}
	s, err := w.check(ctx)
	if err != nil {
		return err
	}
	for key := range s.Node.Labels {
		if key == api.RuntimeLabel || len(key) > len("brewlet.sh/runtime.") && key[:len("brewlet.sh/runtime.")] == "brewlet.sh/runtime." {
			return fmt.Errorf("node-advertisement-failed: retained provider labels prevent cleanup completion")
		}
	}
	n := s.Node.DeepCopy()
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	delete(n.Annotations, api.InventoryAnnotation)
	n.Annotations["brewlet.sh/cleanup-complete"] = fmt.Sprintf("%s/%d", w.Authority.ProfileUID, w.Authority.Generation)
	if err := w.Client.Update(ctx, n); err != nil {
		return err
	}
	s, err = w.check(ctx)
	if err != nil {
		return err
	}
	if s.Node.Annotations["brewlet.sh/cleanup-complete"] != n.Annotations["brewlet.sh/cleanup-complete"] {
		return fmt.Errorf("cleanup completion readback failed")
	}
	return nil
}
