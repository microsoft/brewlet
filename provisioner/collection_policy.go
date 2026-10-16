package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"
)

func (w *Worker) collectionPolicy() (time.Duration, time.Duration, error) {
	grace, interval := w.GCGrace, w.GCSweepInterval
	if grace == 0 {
		grace = time.Hour
	}
	if interval == 0 {
		interval = time.Minute
	}
	if grace <= 0 || interval <= 0 {
		return 0, 0, fmt.Errorf("GC grace and sweep interval must be positive real durations")
	}
	return grace, interval, nil
}
func (w *Worker) recoverLeases(ctx context.Context) error {
	if _, err := w.check(ctx); err != nil {
		return err
	}
	before, err := w.Store.Leases()
	if err != nil {
		return err
	}
	if err := w.Store.Recover(ctx, w.Evidence); err != nil {
		return err
	}
	if _, err := w.check(ctx); err != nil {
		return err
	}
	after, err := w.Store.Leases()
	if err != nil {
		return err
	}
	retained := []string{}
	removed := []string{}
	live := map[string]bool{}
	for _, lease := range after {
		retained = append(retained, lease.TaskID)
		live[lease.Namespace+"\x00"+lease.TaskID] = true
	}
	for _, lease := range before {
		if !live[lease.Namespace+"\x00"+lease.TaskID] {
			removed = append(removed, lease.TaskID)
		}
	}
	sort.Strings(retained)
	sort.Strings(removed)
	data, err := json.Marshal(struct {
		RetainedTasks []string `json:"retainedTasks"`
		RemovedTasks  []string `json:"removedTasks"`
		Complete      bool     `json:"complete"`
	}{retained, removed, true})
	if err != nil {
		return err
	}
	log.Printf("runtime-lease-recovery: %s", data)
	return nil
}
