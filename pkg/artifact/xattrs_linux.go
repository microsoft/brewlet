//go:build linux

package artifact

import "golang.org/x/sys/unix"

func rejectSourceXattrs(p string) error {
	n, e := unix.Llistxattr(p, nil)
	if e == unix.ENOTSUP {
		return nil
	}
	if e != nil {
		return e
	}
	if n > 0 {
		return fail("LayerPathViolation", "extended attributes on %s", p)
	}
	return nil
}
