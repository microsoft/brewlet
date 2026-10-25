//go:build darwin

package artifact

import "golang.org/x/sys/unix"

func rejectSourceXattrs(p string) error {
	for {
		n, e := unix.Llistxattr(p, nil)
		if e == unix.ENOTSUP {
			return nil
		}
		if e != nil {
			return e
		}
		if n == 0 {
			return nil
		}
		buf := make([]byte, n)
		n, e = unix.Llistxattr(p, buf)
		if e == unix.ERANGE {
			continue
		}
		if e != nil {
			return e
		}
		if name := firstRejectedXattr(buf[:n], darwinHostLocalXattrs); name != "" {
			return fail("LayerPathViolation", "extended attribute %s on %s", name, p)
		}
		return nil
	}
}
