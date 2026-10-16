package provisioner

import (
	"context"
	"fmt"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
)

func (w *Worker) validateRuntimePlatforms(ctx context.Context, r *api.RuntimeImage) error {
	required, err := w.requiredPlatforms(ctx, r.Name)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(required))
	for _, p := range required {
		keys = append(keys, runtimePlatformKey(p))
	}
	key := string(r.UID) + "\x00" + r.Status.Digest + "\x00" + strings.Join(keys, ",")
	if w.platformProofs[key] {
		return nil
	}
	validator := w.Platforms
	if validator == nil {
		validator, _ = w.Installer.(RuntimePlatformValidator)
	}
	if validator == nil {
		return fmt.Errorf("runtime platform metadata validator unavailable")
	}
	if err := validator.ValidatePlatforms(ctx, r, required); err != nil {
		return err
	}
	if _, err := w.check(ctx); err != nil {
		return err
	}
	if w.platformProofs == nil {
		w.platformProofs = map[string]bool{}
	}
	w.platformProofs[key] = true
	return nil
}
