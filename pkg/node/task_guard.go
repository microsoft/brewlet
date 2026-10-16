package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// AcquireTaskGuard excludes recovery while Create can still publish mounts or
// delegate a task. The lock is outside .tasks so removing a lease cannot split it.
// Fixed stripes bound persistent lock files; a collision only delays a caller.
func (s *Store) AcquireTaskGuard(ctx context.Context, task string) (io.Closer, error) {
	if !uidPattern.MatchString(task) {
		return nil, failure("ExecStateUnavailable", "invalid task identity")
	}
	if err := s.Check(); err != nil {
		return nil, err
	}
	directory := filepath.Join(s.Root, ".task-locks")
	if err := os.Mkdir(directory, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err := s.trusted(directory, true); err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(task))
	name := filepath.Join(directory, hex.EncodeToString(key[:])[:3])
	f, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := s.trusted(name, false); err != nil {
		f.Close()
		return nil, err
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
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
