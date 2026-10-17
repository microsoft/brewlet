//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/microsoft/brewlet/pkg/node"
)

const readyMarker = "/run/brewlet/ready"

func healthCheck() error {
	return healthCheckAt(readyMarker)
}
func healthCheckAt(marker string) error {
	if err := node.NewStore(filepath.Dir(marker)).Check(); err != nil {
		return err
	}
	info, err := os.Lstat(marker)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	age := time.Since(info.ModTime())
	if !ok || stat.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || age < 0 || age > 2*time.Minute {
		return fmt.Errorf("node worker completion gate unavailable or stale")
	}
	return nil
}
func removeReady() error {
	return removeReadyAt(readyMarker)
}
func removeReadyAt(marker string) error {
	err := os.Remove(marker)
	if os.IsNotExist(err) {
		return nil
	}

	return err
}
func syncProvisioningReadinessAt(marker, identity string, reconciled bool, fence func() error) error {
	if !reconciled {
		return removeReadyAt(marker)
	}
	if err := fence(); err != nil {
		return errors.Join(err, removeReadyAt(marker))
	}
	return markReadyAt(marker, identity)
}

func markReady(identity string) error {
	return markReadyAt(readyMarker, identity)
}
func markReadyAt(marker, identity string) error {
	dir := filepath.Dir(marker)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := node.NewStore(dir).Check(); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".ready-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(identity + "\n"); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), marker)
}
