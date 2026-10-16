package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Recover reconciles durable leases rather than resetting reference counts at
// shim/provisioner restart. Uncertainty is an error, never permission to collect.
func (s *Store) Recover(ctx context.Context, evidence Evidence) error {
	if evidence == nil {
		return fmt.Errorf("complete evidence required")
	}
	if err := s.Check(); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, ".tasks"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !uidPattern.MatchString(entry.Name()) {
			return fmt.Errorf("unrecognized durable lease")
		}
		var lease Lease
		if err := s.read(filepath.Join(s.Root, ".tasks", entry.Name(), "lease.json"), &lease); err != nil {
			return err
		}
		gone, err := evidence.Gone(ctx, lease)
		if err != nil {
			return err
		}
		if !gone {
			continue
		}
		if err := s.Release(ctx, entry.Name(), evidence); err != nil {
			return err
		}
	}
	return nil
}
