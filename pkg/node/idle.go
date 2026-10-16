package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type usage struct {
	IdleSince time.Time `json:"idleSince"`
}

func (s *Store) IdleUninstall(ctx context.Context, id string, ttl time.Duration, evidence Evidence, fence func(context.Context) error) (bool, error) {
	if ttl <= 0 || evidence == nil || fence == nil {
		return false, fmt.Errorf("idle reclamation requires positive TTL, complete evidence and a fresh fence")
	}
	f, err := s.lock(ctx, id)
	if err != nil {
		return false, err
	}
	defer unlock(f)
	a, err := s.auth(id, time.Now())
	if err != nil {
		return false, err
	}
	if a.Install != "OnDemand" {
		return false, fmt.Errorf("idle TTL requires OnDemand")
	}
	m, _, err := s.current(id, a)
	if err != nil {
		return false, err
	}
	tasks, err := os.ReadDir(filepath.Join(s.Root, ".tasks"))
	if err != nil {
		return false, err
	}
	for _, task := range tasks {
		var l Lease
		if err := s.read(filepath.Join(s.Root, ".tasks", task.Name(), "lease.json"), &l); err != nil {
			return false, err
		}
		if l.Metadata.ID == id {
			return false, nil
		}
	}
	var u usage
	err = s.read(filepath.Join(s.Root, id, "usage.json"), &u)
	if os.IsNotExist(err) {
		u.IdleSince = m.ValidatedAt
	} else if err != nil {
		return false, err
	}
	// A crash after acquiring a lease but before normal release leaves an
	// unknown idle start; recovery starts a new conservative TTL interval.
	if u.IdleSince.IsZero() {
		if err := fence(ctx); err != nil {
			return false, err
		}
		return false, atomicJSON(filepath.Join(s.Root, id, "usage.json"), usage{IdleSince: time.Now().UTC()})
	}
	if time.Since(u.IdleSince) < ttl {
		return false, nil
	}
	base := filepath.Join(s.Root, id)
	entries, err := os.ReadDir(base)
	if err != nil {
		return false, err
	}
	var roots []string
	for _, entry := range entries {
		if entry.Name() == ".retirements" {
			continue
		}
		if !entry.IsDir() {
			continue
		}
		if !uidPattern.MatchString(entry.Name()) {
			return false, fmt.Errorf("unknown runtime epoch")
		}
		generations, err := os.ReadDir(filepath.Join(base, entry.Name()))
		if err != nil {
			return false, err
		}
		for _, generation := range generations {
			gen, err := strconv.ParseInt(generation.Name(), 10, 64)
			if err != nil || gen <= 0 {
				return false, fmt.Errorf("unknown runtime generation")
			}
			var metadata Metadata
			if err := s.read(filepath.Join(base, entry.Name(), generation.Name(), "metadata.json"), &metadata); err != nil {
				return false, err
			}
			if err := metadata.Validate(); err != nil {
				return false, err
			}
			if metadata.ID != id || metadata.InventoryUID != entry.Name() || metadata.Generation != gen {
				return false, fmt.Errorf("runtime metadata mismatch")
			}
			gone, err := evidence.Gone(ctx, Lease{Metadata: metadata})
			if err != nil {
				return false, err
			}
			if !gone {
				return false, nil
			}
		}
		roots = append(roots, filepath.Join(base, entry.Name()))
	}
	mounts, ok := evidence.(MountEvidence)
	if !ok {
		return false, fmt.Errorf("complete inventory-prefix mount evidence required")
	}
	gone, err := mounts.MountsGone(ctx, base)
	if err != nil {
		return false, err
	}
	if !gone {
		return false, nil
	}
	if err := fence(ctx); err != nil {
		return false, err
	}
	a.State = "available"
	if err := atomicJSON(filepath.Join(base, "authorization.json"), a); err != nil {
		return false, err
	}
	if err := os.Remove(filepath.Join(base, "current")); err != nil {
		return false, err
	}
	for _, root := range roots {
		if err := fence(ctx); err != nil {
			return false, err
		}
		if err := os.RemoveAll(root); err != nil {
			return false, err
		}
	}
	return true, nil
}
