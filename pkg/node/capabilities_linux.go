//go:build linux

package node

import (
	"fmt"
	"golang.org/x/sys/unix"
)

func checkCapabilities(path string) error {
	n, err := unix.Lgetxattr(path, "security.capability", nil)
	if err == unix.ENODATA || err == unix.ENOTSUP {
		return nil
	}

	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("runtime file capability: %s", path)
	}
	return nil
}

func CheckFileCapabilities(path string) error { return checkCapabilities(path) }
