//go:build linux

package artifact

import "golang.org/x/sys/unix"

func overlayWhiteout(p string) error { return unix.Mknod(p, unix.S_IFCHR, 0) }
func overlayOpaque(p string) error   { return unix.Setxattr(p, "trusted.overlay.opaque", []byte("y"), 0) }
