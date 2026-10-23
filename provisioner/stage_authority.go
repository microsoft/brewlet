package provisioner

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/node"
)

func (w *Worker) stage(ctx context.Context, r *api.RuntimeImage) (string, node.Metadata, error) {
	completed := errors.New("runtime staging completed")
	ctx, cancel := context.WithCancelCause(ctx)
	stopped := make(chan struct{})
	lost := make(chan error, 1)
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		renewed := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, err := w.check(ctx)
				if err == nil {
					current, readErr := w.runtime(ctx, r.Name)
					err = readErr
					if err == nil && (current.UID != r.UID || current.Status.Generation != r.Status.Generation || current.Status.Digest != r.Status.Digest) {
						err = fmt.Errorf("runtime approval changed during installation")
					}
				}
				if err != nil {
					if errors.Is(err, context.Canceled) && context.Cause(ctx) == completed {
						return
					}
					lost <- err
					cancel(err)
					return
				}
				if time.Since(renewed) >= time.Minute {
					auths, readErr := w.Store.Authorizations()
					if readErr == nil {
						for _, a := range auths {
							if a.ID == r.Name {
								if a.Revoked || a.InventoryUID != string(r.UID) || a.Generation != r.Status.Generation || a.Digest != r.Status.Digest {
									readErr = fmt.Errorf("local runtime authorization changed during installation")
									break
								}
								a.ExpiresAt = time.Now().Add(5 * time.Minute).UTC()
								readErr = w.Store.WriteAuthorization(ctx, a)
								break
							}
						}
					}
					if readErr != nil {
						if errors.Is(readErr, context.Canceled) && context.Cause(ctx) == completed {
							return
						}
						lost <- readErr
						cancel(readErr)
						return
					}
					renewed = time.Now()
				}
			}
		}
	}()
	stage, m, err := w.Installer.Stage(ctx, r, w.Store.Root)
	// Joining a completed stage can cancel an in-flight monitor API read.
	cancel(completed)
	<-stopped
	select {
	case fenceErr := <-lost:
		return stage, m, fenceErr
	default:
	}
	return stage, m, err
}
