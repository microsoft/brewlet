package provisioner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/microsoft/brewlet/pkg/node"
)

func (w *Worker) clearRevocation(ctx context.Context, id string) error {
	s, err := w.check(ctx)
	if err != nil {
		return err
	}
	receipts := map[string]node.RevocationReceipt{}
	raw := s.Node.Annotations[node.RevocationsAnnotation]
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), &receipts); err != nil {
		return err
	}
	if _, exists := receipts[id]; !exists {
		return nil
	}
	delete(receipts, id)
	n := s.Node.DeepCopy()
	if len(receipts) == 0 {
		delete(n.Annotations, node.RevocationsAnnotation)
	} else {
		data, err := json.Marshal(receipts)
		if err != nil {
			return err
		}
		n.Annotations[node.RevocationsAnnotation] = string(data)
	}
	if err := w.Client.Update(ctx, n); err != nil {
		return err
	}
	s, err = w.check(ctx)
	if err != nil {
		return err
	}
	remaining := map[string]node.RevocationReceipt{}
	if raw := s.Node.Annotations[node.RevocationsAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &remaining); err != nil {
			return err
		}
	}
	if _, exists := remaining[id]; exists {
		return fmt.Errorf("stale revocation receipt withdrawal not observed")
	}
	return nil
}
