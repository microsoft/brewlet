package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type Store struct {
	Root         string
	ownerUID     uint32
	testRootOnly bool
}

func NewStore(root string) *Store { return &Store{Root: root} }

func (s *Store) trusted(name string, directory bool) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != s.ownerUID || info.Mode()&0022 != 0 || info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory {
		return fmt.Errorf("untrusted node state %s", name)
	}
	if !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("nonregular node state %s", name)
	}
	return nil
}

func (s *Store) Check() error {
	if !filepath.IsAbs(s.Root) || filepath.Clean(s.Root) != s.Root {
		return fmt.Errorf("absolute canonical inventory root required")
	}
	for dir := s.Root; ; dir = filepath.Dir(dir) {
		if err := s.trusted(dir, true); err != nil {
			return err
		}
		if dir == "/" {
			break
		}
		// Tests may exercise the store under the invoking user's checkout.
		if s.testRootOnly && dir == s.Root {
			break
		}
	}
	return nil
}

func (s *Store) Initialize() error {
	if err := os.MkdirAll(s.Root, 0700); err != nil {
		return err
	}
	if err := s.Check(); err != nil {
		return err
	}
	for _, dir := range []string{".locks", ".requests", ".tasks"} {
		p := filepath.Join(s.Root, dir)
		if err := os.Mkdir(p, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		if err := s.trusted(p, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) lock(ctx context.Context, id string) (*os.File, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	if err := s.Check(); err != nil {
		return nil, err
	}
	if err := s.trusted(filepath.Join(s.Root, ".locks"), true); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Root, ".locks", id), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := s.trusted(filepath.Join(s.Root, ".locks", id), false); err != nil {
		f.Close()
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return f, nil
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
func unlock(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }

func atomicJSON(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicFile(name, data)
}
func atomicFile(name string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(name), ".pending-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), name); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *Store) read(name string, value any) error {
	if err := s.trusted(name, false); err != nil {
		return err
	}
	f, err := os.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 2<<20 {
		return fmt.Errorf("node state exceeds size limit")
	}
	return strictJSON(b, value)
}
func (s *Store) auth(id string, now time.Time) (Authorization, error) {
	var a Authorization
	dir := filepath.Join(s.Root, id)
	if err := s.trusted(dir, true); os.IsNotExist(err) {
		return a, failure("RuntimeImageNotInstalled", id)
	} else if err != nil {
		return a, err
	}
	if err := s.read(filepath.Join(dir, "authorization.json"), &a); err != nil {
		return a, err
	}
	if a.ID != id || !uidPattern.MatchString(a.InventoryUID) || a.Generation < 1 || !digestPattern.MatchString(a.Digest) {
		return a, fmt.Errorf("invalid runtime authorization")
	}
	if a.Revoked {
		return a, failure("RuntimeImageRevoked", id)
	}
	if !now.Before(a.ExpiresAt) {
		return a, failure("RuntimeImageAuthorizationExpired", id)
	}
	if a.State == "failed" {
		return a, failure("RuntimeImageInstallFailed", id)
	}
	return a, nil
}
func (s *Store) WriteAuthorization(ctx context.Context, a Authorization) error {
	f, err := s.lock(ctx, a.ID)
	if err != nil {
		return err
	}
	defer unlock(f)
	if !uidPattern.MatchString(a.InventoryUID) || a.Generation < 1 || !digestPattern.MatchString(a.Digest) ||
		!uidPattern.MatchString(a.ProfileUID) || a.ProfileGeneration < 1 ||
		(a.Install != "Eager" && a.Install != "OnDemand") ||
		(a.State != "available" && a.State != "installing" && a.State != "ready" && a.State != "failed") {
		return fmt.Errorf("invalid authorization")
	}
	dir := filepath.Join(s.Root, a.ID)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := s.trusted(dir, true); err != nil {
		return err
	}
	var previous Authorization
	if err := s.read(filepath.Join(dir, "authorization.json"), &previous); err == nil {
		if previous.ProfileUID != a.ProfileUID {
			return failure("ownership-fence-failed", "foreign local authorization")
		}
		if previous.ProfileGeneration > a.ProfileGeneration {
			return failure("ownership-fence-failed", "stale profile generation")
		}
		if previous.InventoryUID == a.InventoryUID && (previous.Generation > a.Generation || (previous.Generation == a.Generation && previous.Digest != a.Digest)) {
			return fmt.Errorf("runtime authorization generation regression or identity reuse")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicJSON(filepath.Join(dir, "authorization.json"), a)
}

func (s *Store) current(id string, a Authorization) (Metadata, string, error) {
	var m Metadata
	base := filepath.Join(s.Root, id)
	target, err := os.Readlink(filepath.Join(base, "current"))
	if err != nil {
		return m, "", err
	}
	parts := strings.Split(target, "/")
	if len(parts) != 2 || !uidPattern.MatchString(parts[0]) {
		return m, "", fmt.Errorf("unsafe current pointer")
	}
	gen, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || gen < 1 || strconv.FormatInt(gen, 10) != parts[1] {
		return m, "", fmt.Errorf("invalid generation path")
	}
	for _, dir := range []string{filepath.Join(base, parts[0]), filepath.Join(base, target)} {
		if err := s.trusted(dir, true); err != nil {
			return m, "", err
		}
	}
	dir := filepath.Join(base, target)
	if err := s.read(filepath.Join(dir, "metadata.json"), &m); err != nil {
		return m, "", err
	}
	if err := m.Validate(); err != nil {
		return m, "", err
	}
	if m.ValidatedAt.IsZero() {
		return m, "", fmt.Errorf("runtime validation receipt missing")
	}
	if m.ID != id || m.InventoryUID != parts[0] || m.Generation != gen {
		return m, "", fmt.Errorf("metadata path mismatch")
	}
	if m.InventoryUID != a.InventoryUID {
		return m, "", failure("RuntimeImageEpochMismatch", id)
	}
	if m.Generation > a.Generation || (m.Generation == a.Generation && m.SourceDigest != a.Digest) {
		return m, "", fmt.Errorf("runtime exceeds or contradicts authorization")
	}
	root := filepath.Join(dir, "rootfs")
	if err := s.trusted(root, true); err != nil {
		return m, "", err
	}
	marker := filepath.Join(root, ".brewlet-source")
	if err := s.trusted(marker, false); err != nil {
		return m, "", err
	}
	source, err := os.ReadFile(marker)
	if err != nil {
		return m, "", err
	}
	if string(source) != m.SourceDigest+"\n" {
		return m, "", fmt.Errorf("runtime source receipt mismatch")
	}
	return m, root, nil
}

// Resolve leases current under the same cross-process lock as publication and
// collection. Only an uninstalled OnDemand entry creates an empty request.
func (s *Store) Resolve(ctx context.Context, id, namespace, task string, barrier *Barrier, wait time.Duration) (Lease, string, error) {
	return s.resolve(ctx, id, namespace, task, barrier, wait, nil)
}

func (s *Store) ResolveTask(ctx context.Context, id, namespace, task string, barrier *Barrier, wait time.Duration, mounts TaskMounts) (Lease, string, error) {
	if err := mounts.Validate(namespace, task); err != nil {
		return Lease{}, "", err
	}
	return s.resolve(ctx, id, namespace, task, barrier, wait, &mounts)
}

func (s *Store) resolve(ctx context.Context, id, namespace, task string, barrier *Barrier, wait time.Duration, mounts *TaskMounts) (Lease, string, error) {
	if err := ValidateID(id); err != nil {
		return Lease{}, "", err
	}
	if !uidPattern.MatchString(task) || namespace == "" || strings.ContainsAny(namespace, "/\x00") {
		return Lease{}, "", fmt.Errorf("invalid task identity")
	}
	if wait <= 0 || wait > 30*time.Second {
		wait = 30 * time.Second
	}
	if barrier != nil && (barrier.ID != id || !uidPattern.MatchString(barrier.InventoryUID) || barrier.Generation < 1) {
		return Lease{}, "", failure("InvalidRuntimeGeneration", id)
	}
	bounded, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	waitReason := "RuntimeImageInstalling"
	if barrier != nil {
		waitReason = "RuntimeImageUpdating"
	}
	timeout := func() error { return failure(waitReason, id) }
	deadline := time.Now().Add(wait)
	for {
		f, err := s.lock(bounded, id)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return Lease{}, "", timeout()
			}
			return Lease{}, "", err
		}
		lease, root, retryReason, err := s.resolveLocked(id, namespace, task, barrier, mounts)
		unlock(f)
		if retryReason == "" || err != nil {
			return lease, root, err
		}
		waitReason = retryReason
		if time.Now().After(deadline) {
			return Lease{}, "", timeout()
		}
		select {
		case <-bounded.Done():
			if ctx.Err() != nil {
				return Lease{}, "", ctx.Err()
			}
			return Lease{}, "", timeout()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (s *Store) resolveLocked(id, namespace, task string, barrier *Barrier, mounts *TaskMounts) (Lease, string, string, error) {
	a, err := s.auth(id, time.Now())
	if err != nil {
		return Lease{}, "", "", err
	}
	if barrier != nil && barrier.InventoryUID != a.InventoryUID {
		return Lease{}, "", "", failure("RuntimeImageEpochMismatch", id)
	}
	m, root, err := s.current(id, a)
	if os.IsNotExist(err) {
		if a.Install != "OnDemand" {
			return Lease{}, "", "", failure("RuntimeImageNotInstalled", id)
		}
		if err := s.trusted(filepath.Join(s.Root, ".requests"), true); err != nil {
			return Lease{}, "", "", err
		}
		request := filepath.Join(s.Root, ".requests", id)
		f, err := os.OpenFile(request, os.O_CREATE|os.O_EXCL|os.O_WRONLY|unix.O_NOFOLLOW, 0600)
		if err == nil {
			err = f.Sync()
			_ = f.Close()
		} else if os.IsExist(err) {
			err = s.trusted(request, false)
		}
		return Lease{}, "", "RuntimeImageInstalling", err
	}
	if err != nil {
		return Lease{}, "", "", err
	}
	if barrier != nil && m.Generation < barrier.Generation {
		return Lease{}, "", "RuntimeImageUpdating", nil
	}
	lease := Lease{TaskID: task, Namespace: namespace, Metadata: m, AcquiredAt: time.Now().UTC(), TaskMounts: mounts}
	dir := filepath.Join(s.Root, ".tasks", task)
	if err := s.trusted(filepath.Join(s.Root, ".tasks"), true); err != nil {
		return Lease{}, "", "", err
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return Lease{}, "", "", err
	}
	if err := s.trusted(dir, true); err != nil {
		return Lease{}, "", "", err
	}
	var existing Lease
	name := filepath.Join(dir, "lease.json")
	if err := s.read(name, &existing); err == nil {
		if existing.Namespace != namespace || existing.Metadata.InventoryUID != m.InventoryUID || existing.Metadata.Generation != m.Generation {
			return Lease{}, "", "", fmt.Errorf("task lease already exists")
		}
		if mounts != nil && (existing.TaskMounts == nil || *existing.TaskMounts != *mounts) {
			return Lease{}, "", "", fmt.Errorf("task lease mount provenance cannot be replaced")
		}
		lease = existing
	} else if !os.IsNotExist(err) {
		return Lease{}, "", "", err
	} else if err := atomicJSON(name, lease); err != nil {
		return Lease{}, "", "", err
	}
	if err := atomicJSON(filepath.Join(s.Root, id, "usage.json"), usage{}); err != nil {
		return Lease{}, "", "", err
	}
	return lease, root, "", nil
}

func (s *Store) SaveInitial(state InitialState) error {
	if err := s.Check(); err != nil {
		return err
	}
	if err := s.trusted(filepath.Join(s.Root, ".tasks"), true); err != nil {
		return err
	}
	if !uidPattern.MatchString(state.Lease.TaskID) {
		return fmt.Errorf("invalid task")
	}
	if err := CheckProcess(&state.Process); err != nil {
		return err
	}
	dir := filepath.Join(s.Root, ".tasks", state.Lease.TaskID)
	if err := s.trusted(dir, true); err != nil {
		return err
	}
	return atomicJSON(filepath.Join(dir, "initial.json"), state)
}
func (s *Store) LoadInitial(task string) (InitialState, error) {
	var state InitialState
	if !uidPattern.MatchString(task) {
		return state, failure("ExecStateUnavailable", "invalid task")
	}
	if err := s.Check(); err != nil {
		return state, err
	}
	for _, dir := range []string{filepath.Join(s.Root, ".tasks"), filepath.Join(s.Root, ".tasks", task)} {
		if err := s.trusted(dir, true); err != nil {
			return state, failure("ExecStateUnavailable", err.Error())
		}
	}
	if err := s.read(filepath.Join(s.Root, ".tasks", task, "initial.json"), &state); err != nil {
		return state, failure("ExecStateUnavailable", err.Error())
	}
	if state.Lease.TaskID != task {
		return state, failure("ExecStateUnavailable", "task mismatch")
	}
	if err := state.Lease.Metadata.Validate(); err != nil {
		return state, failure("ExecStateUnavailable", err.Error())
	}
	if err := CheckProcess(&state.Process); err != nil {
		return state, failure("ExecStateUnavailable", err.Error())
	}
	return state, nil
}

// Evidence must establish absence of tasks, execs AND mounts, including other
// mount namespaces. An error or incomplete evidence always retains the lease.
type Evidence interface {
	Gone(context.Context, Lease) (bool, error)
}
type MountEvidence interface {
	MountsGone(context.Context, string) (bool, error)
}

func (s *Store) Release(ctx context.Context, task string, evidence Evidence) error {
	if evidence == nil || !uidPattern.MatchString(task) {
		return fmt.Errorf("complete task/mount evidence required")
	}
	taskGuard, err := s.AcquireTaskGuard(ctx, task)
	if err != nil {
		return err
	}
	defer taskGuard.Close()
	if err := s.Check(); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Join(s.Root, ".tasks"), filepath.Join(s.Root, ".tasks", task)} {
		if err := s.trusted(dir, true); err != nil {
			return err
		}
	}
	var lease Lease
	name := filepath.Join(s.Root, ".tasks", task, "lease.json")
	if err := s.read(name, &lease); err != nil {
		return err
	}
	f, err := s.lock(ctx, lease.Metadata.ID)
	if err != nil {
		return err
	}
	defer unlock(f)
	gone, err := evidence.Gone(ctx, lease)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("task or mounts remain")
	}
	if err := os.RemoveAll(filepath.Dir(name)); err != nil {
		return err
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, ".tasks"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var other Lease
		if err := s.read(filepath.Join(s.Root, ".tasks", entry.Name(), "lease.json"), &other); err != nil {
			return err
		}
		if other.Metadata.ID == lease.Metadata.ID {
			return nil
		}
	}
	return atomicJSON(filepath.Join(s.Root, lease.Metadata.ID, "usage.json"), usage{IdleSince: time.Now().UTC()})
}

func (s *Store) Requests() ([]string, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, ".requests")
	if err := s.trusted(dir, true); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var result []string
	for _, entry := range entries {
		if ValidateID(entry.Name()) != nil {
			return nil, fmt.Errorf("invalid install marker")
		}
		p := filepath.Join(dir, entry.Name())
		if err := s.trusted(p, false); err != nil {
			return nil, err
		}
		st, err := os.Stat(p)
		if err != nil || st.Size() != 0 {
			return nil, fmt.Errorf("nonempty install marker")
		}
		result = append(result, entry.Name())
	}
	return result, nil
}
func (s *Store) RemoveRequest(id string) error {
	if err := ValidateID(id); err != nil {
		return err
	}
	if err := s.Check(); err != nil {
		return err
	}
	if err := s.trusted(filepath.Join(s.Root, ".requests"), true); err != nil {
		return err
	}
	err := os.Remove(filepath.Join(s.Root, ".requests", id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Current reads installed metadata without acquiring a task lease. It is used
// only by the fenced provisioner, not as an alternate Create authority.
func (s *Store) Current(ctx context.Context, id string) (Metadata, string, error) {
	f, err := s.lock(ctx, id)
	if err != nil {
		return Metadata{}, "", err
	}
	defer unlock(f)
	a, err := s.auth(id, time.Now())
	if err != nil {
		return Metadata{}, "", err
	}
	return s.current(id, a)
}

func (s *Store) Authorizations() ([]Authorization, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	var result []Authorization
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if err := ValidateID(entry.Name()); err != nil {
			return nil, err
		}
		if err := s.trusted(filepath.Join(s.Root, entry.Name()), true); err != nil {
			return nil, err
		}
		var a Authorization
		if err := s.read(filepath.Join(s.Root, entry.Name(), "authorization.json"), &a); err != nil {
			return nil, err
		}
		if a.ID != entry.Name() {
			return nil, fmt.Errorf("authorization path mismatch")
		}
		result = append(result, a)
	}
	return result, nil
}

func (s *Store) Revoke(ctx context.Context, id, profile string) error {
	f, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock(f)
	var a Authorization
	name := filepath.Join(s.Root, id, "authorization.json")
	if err := s.read(name, &a); err != nil {
		return err
	}
	if a.ProfileUID != profile {
		return fmt.Errorf("foreign authorization cannot be revoked")
	}
	a.Revoked = true
	return atomicJSON(name, a)
}

func (s *Store) AcquireWriter(ctx context.Context) (io.Closer, error) {
	if err := s.Check(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Root, ".writer-lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := s.trusted(filepath.Join(s.Root, ".writer-lock"), false); err != nil {
		f.Close()
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
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
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type writerLock struct{ *os.File }

func (w writerLock) Close() error { unlock(w.File); return nil }

func (s *Store) ValidateLease(ctx context.Context, l Lease) error {
	if !uidPattern.MatchString(l.TaskID) || l.Namespace == "" {
		return fmt.Errorf("invalid leased task identity")
	}
	if err := l.Metadata.Validate(); err != nil {
		return err
	}
	f, err := s.lock(ctx, l.Metadata.ID)
	if err != nil {
		return err
	}
	defer unlock(f)
	a, err := s.auth(l.Metadata.ID, time.Now())
	if err != nil {
		return err
	}
	if a.InventoryUID != l.Metadata.InventoryUID {
		return failure("RuntimeImageEpochMismatch", l.Metadata.ID)
	}
	if l.Metadata.Generation > a.Generation || (l.Metadata.Generation == a.Generation && l.Metadata.SourceDigest != a.Digest) {
		return fmt.Errorf("leased runtime no longer agrees with authorized target")
	}
	var durable Lease
	if err := s.read(filepath.Join(s.Root, ".tasks", l.TaskID, "lease.json"), &durable); err != nil {
		return err
	}
	if durable.TaskID != l.TaskID || durable.Namespace != l.Namespace || durable.Metadata.InventoryUID != l.Metadata.InventoryUID || durable.Metadata.Generation != l.Metadata.Generation || durable.Metadata.SourceDigest != l.Metadata.SourceDigest {
		return fmt.Errorf("durable runtime lease changed")
	}
	return nil
}
