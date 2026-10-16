package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Retire removes only this profile's revoked inventory, and only after complete
// task/exec/mount evidence. It never treats an absent current pointer as proof
// that the runtime has no consumers.
func (s *Store) Retire(ctx context.Context, id, profile string, evidence Evidence, fence func(context.Context) error) error {
	return s.retire(ctx, id, profile, evidence, fence, false)
}

// RetireRevoked keeps the authorization tombstone so new starts still report
// RuntimeImageRevoked after the approved resource's roots have drained.
func (s *Store) RetireRevoked(ctx context.Context, id, profile string, evidence Evidence, fence func(context.Context) error) error {
	return s.retire(ctx, id, profile, evidence, fence, true)
}
func (s *Store) retire(ctx context.Context, id, profile string, evidence Evidence, fence func(context.Context) error, keepAuthorization bool) error {
	if evidence == nil || fence == nil {
		return fmt.Errorf("complete cleanup evidence required")
	}
	f, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock(f)
	base := filepath.Join(s.Root, id)
	if err := s.trusted(base, true); err != nil {
		return err
	}
	var auth Authorization
	if err := s.read(filepath.Join(base, "authorization.json"), &auth); err != nil {
		return err
	}
	if auth.ProfileUID != profile || !auth.Revoked {
		return fmt.Errorf("cleanup requires owned revoked authorization")
	}
	tasks, err := os.ReadDir(filepath.Join(s.Root, ".tasks"))
	if err != nil {
		return err
	}
	for _, task := range tasks {
		var lease Lease
		if err := s.read(filepath.Join(s.Root, ".tasks", task.Name(), "lease.json"), &lease); err != nil {
			return err
		}
		if lease.Metadata.ID == id {
			return fmt.Errorf("runtime leases have not drained")
		}
	}
	epochs, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	var roots []string
	for _, epoch := range epochs {
		if epoch.Name() == ".retirements" {
			continue
		}
		if !epoch.IsDir() {
			continue
		}
		if !uidPattern.MatchString(epoch.Name()) {
			return fmt.Errorf("unknown runtime epoch")
		}
		generations, err := os.ReadDir(filepath.Join(base, epoch.Name()))
		if err != nil {
			return err
		}
		for _, generation := range generations {
			gen, err := strconv.ParseInt(generation.Name(), 10, 64)
			if err != nil || gen <= 0 {
				return fmt.Errorf("unknown runtime generation")
			}
			var m Metadata
			if err := s.read(filepath.Join(base, epoch.Name(), generation.Name(), "metadata.json"), &m); err != nil {
				return err
			}
			if err := m.Validate(); err != nil {
				return err
			}
			if m.ID != id || m.InventoryUID != epoch.Name() || m.Generation != gen {
				return fmt.Errorf("runtime cleanup metadata mismatch")
			}
			gone, err := evidence.Gone(ctx, Lease{Metadata: m})
			if err != nil {
				return err
			}
			if !gone {
				return fmt.Errorf("runtime mounts have not drained")
			}
		}
		roots = append(roots, filepath.Join(base, epoch.Name()))
	}
	mounts, ok := evidence.(MountEvidence)
	if !ok {
		return fmt.Errorf("complete inventory-prefix mount evidence required")
	}
	gone, err := mounts.MountsGone(ctx, base)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("runtime inventory-prefix mounts have not drained")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	if keepAuthorization {
		if err := os.Remove(filepath.Join(base, "current")); err != nil && !os.IsNotExist(err) {
			return err
		}
		for _, root := range roots {
			if err := fence(ctx); err != nil {
				return err
			}
			if err := os.RemoveAll(root); err != nil {
				return err
			}
		}
		return nil
	}
	return os.RemoveAll(base)
}
