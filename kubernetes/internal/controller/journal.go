package controller

import (
	"context"
	"encoding/json"
	"fmt"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const inventoryJournalKey = "node.brewlet.sh/inventory-journal"

// Metadata is a write-ahead allocation journal independent of the status
// subresource schema. A pruned/lost status cannot reset an existing epoch's
// generation counter or forget its immutable account.
type inventoryJournal struct {
	UID           types.UID         `json:"uid"`
	Generation    int64             `json:"generation"`
	Source        string            `json:"source"`
	User          api.User          `json:"user"`
	Compatibility api.Compatibility `json:"compatibility"`
}

func journalFor(im *api.RuntimeImage, g int64) inventoryJournal {
	return inventoryJournal{UID: im.UID, Generation: g, Source: im.Spec.Source.Image, User: im.Spec.User, Compatibility: im.Spec.Compatibility}
}
func encodeJournal(im *api.RuntimeImage, j inventoryJournal) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if im.Annotations == nil {
		im.Annotations = map[string]string{}
	}
	im.Annotations[inventoryJournalKey] = string(data)
	return nil
}
func readJournal(im *api.RuntimeImage) (inventoryJournal, error) {
	var j inventoryJournal
	if err := json.Unmarshal([]byte(im.Annotations[inventoryJournalKey]), &j); err != nil {
		return j, fmt.Errorf("missing/corrupt inventory allocation journal; refusing generation reuse: %w", err)
	}
	if j.UID != im.UID || j.Generation <= 0 || policy.Source(j.Source) != nil {
		return j, fmt.Errorf("inventory allocation journal epoch/source/generation invalid")
	}
	if j.User != im.Spec.User || j.Compatibility != im.Spec.Compatibility {
		return j, fmt.Errorf("immutable runtime identity differs from allocation journal")
	}
	if im.Status.Generation > j.Generation {
		return j, fmt.Errorf("status generation exceeds durable allocation journal")
	}
	return j, nil
}
func (r *RuntimeReconciler) persistJournal(ctx context.Context, im *api.RuntimeImage, j inventoryJournal) error {
	base := im.DeepCopy()
	if err := encodeJournal(im, j); err != nil {
		return err
	}
	if err := r.Patch(ctx, im, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return err
	}
	fresh := new(api.RuntimeImage)
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(im), fresh); err != nil {
		return err
	}
	if fresh.UID != im.UID || fresh.Annotations[inventoryJournalKey] != im.Annotations[inventoryJournalKey] {
		return fmt.Errorf("allocation journal readback failed")
	}
	return nil
}
