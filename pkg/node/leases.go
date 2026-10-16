package node

import (
	"fmt"
	"os"
	"path/filepath"
)

func (s *Store) Leases() ([]Lease, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	base := filepath.Join(s.Root, ".tasks")
	if err := s.trusted(base, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var leases []Lease
	for _, entry := range entries {
		if !uidPattern.MatchString(entry.Name()) {
			return nil, fmt.Errorf("unrecognized durable lease")
		}
		if err := s.trusted(filepath.Join(base, entry.Name()), true); err != nil {
			return nil, err
		}
		var lease Lease
		if err := s.read(filepath.Join(base, entry.Name(), "lease.json"), &lease); err != nil {
			return nil, err
		}
		if lease.TaskID != entry.Name() || lease.Namespace == "" {
			return nil, fmt.Errorf("durable lease identity mismatch")
		}
		if err := lease.Metadata.Validate(); err != nil {
			return nil, err
		}
		leases = append(leases, lease)
	}
	return leases, nil
}
