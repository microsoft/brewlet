//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// The private opaque lowest app layer hides runtime /app without changing
// approved runtime bytes or shared payload caches. Its lifetime is the bundle's.
func applicationRuntimeLowers(bundle string, layers []string, runtimeRoot string) ([]string, error) {
	if len(layers) == 0 {
		return nil, fmt.Errorf("application payload layers required")
	}
	boundary, err := os.MkdirTemp(bundle, ".brewlet-app-boundary-")
	if err != nil {
		return nil, err
	}
	app := filepath.Join(boundary, "app")
	if err := os.Mkdir(app, 0755); err != nil {
		return nil, err
	}
	if err := unix.Setxattr(app, "trusted.overlay.opaque", []byte("y"), 0); err != nil {
		return nil, fmt.Errorf("application-prefix opaque boundary unavailable: %w", err)
	}
	lowers := append([]string(nil), layers...)
	slices.Reverse(lowers)
	return append(lowers, boundary, runtimeRoot), nil
}
