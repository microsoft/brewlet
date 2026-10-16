package controller

import (
	"context"
	"fmt"
	"slices"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Profile history may disappear only after every inventory epoch has retained
// its independent resolution. This also prevents a successor profile from
// overwriting the only genuine per-ID Node receipt before it is consumed.
func inventoryTargetsResolved(ctx context.Context, r client.Reader, p *api.NodeProfile, targets []api.Target) error {
	var images api.RuntimeImageList
	if err := r.List(ctx, &images); err != nil {
		return err
	}
	for _, im := range images.Items {
		for _, t := range im.Status.RevocationTargets {
			if t.ProfileUID == p.UID && !t.Resolved && slices.ContainsFunc(targets, func(old api.Target) bool { return old.UID == t.NodeUID }) {
				return fmt.Errorf("inventory %s/%s still retains an unresolved original root obligation for Node %s/%s", im.Name, im.UID, t.NodeName, t.NodeUID)
			}
		}
	}
	return nil
}
