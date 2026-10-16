//go:build !linux

package node

// Runtime roots are published only on Linux. Portable validation tests cover the
// remaining identity, path and mode rules; capability inspection is Linux-only.
func checkCapabilities(string) error { return nil }

func CheckFileCapabilities(string) error { return nil }
