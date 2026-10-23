package main

import (
	"path/filepath"
	"strings"
)

// defaultStoreRoot holds one OCI layout per reference built without --store.
const defaultStoreRoot = ".brewlet/images"

// storeForRef returns the explicit layout directory, or the per-reference
// default layout shared by build, push, run and inspect.
func storeForRef(ref, explicit string) string {
	if explicit != "" {
		return explicit
	}
	var b strings.Builder
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return filepath.Join(defaultStoreRoot, b.String())
}
