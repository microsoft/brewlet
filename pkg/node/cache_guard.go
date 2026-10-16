package node

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

func AcquireCacheGuard(ctx context.Context, root string, exclusive bool) (io.Closer, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	store := NewStore(root)
	if err := store.Check(); err != nil {
		return nil, err
	}
	name := filepath.Join(root, ".consumers-lock")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := store.trusted(name, false); err != nil {
		f.Close()
		return nil, err
	}
	mode := unix.LOCK_SH
	if exclusive {
		mode = unix.LOCK_EX
	}
	for {
		err = unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return writerLock{f}, nil
		}
		if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
