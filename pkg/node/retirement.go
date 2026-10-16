package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type retirement struct {
	InventoryUID string    `json:"inventoryUID"`
	Generation   int64     `json:"generation"`
	RetiredAt    time.Time `json:"retiredAt"`
}

func (s *Store) retirementName(id, uid string, generation int64) string {
	return filepath.Join(s.Root, id, ".retirements", uid+"-"+strconv.FormatInt(generation, 10)+".json")
}
func (s *Store) markRetired(id, uid string, generation int64) error {
	if !uidPattern.MatchString(uid) || generation < 1 {
		return fmt.Errorf("invalid retirement identity")
	}
	directory := filepath.Join(s.Root, id, ".retirements")
	if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := s.trusted(directory, true); err != nil {
		return err
	}
	return atomicJSON(s.retirementName(id, uid, generation), retirement{InventoryUID: uid, Generation: generation, RetiredAt: time.Now().UTC()})
}
