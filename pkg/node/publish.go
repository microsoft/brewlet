package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Publish installs an already extracted, validated staging directory containing
// rootfs. The caller must hold the exclusive provisioner writer lock and must
// perform its fresh cluster fence immediately before this call.
func (s *Store) Publish(ctx context.Context, staging string, m Metadata, fence func(context.Context) error) error {
	if fence == nil {
		return fmt.Errorf("fresh publication fence required")
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("runtime publication requires Linux capability validation")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	f, err := s.lock(ctx, m.ID)
	if err != nil {
		return err
	}
	defer unlock(f)
	a, err := s.auth(m.ID, time.Now())
	if err != nil {
		return err
	}
	if a.InventoryUID != m.InventoryUID || a.Generation != m.Generation || a.Digest != m.SourceDigest {
		return fmt.Errorf("runtime target changed before publication")
	}
	if err := ValidateRoot(filepath.Join(staging, "rootfs"), &m); err != nil {
		return err
	}
	epoch := filepath.Join(s.Root, m.ID, m.InventoryUID)
	if err := os.Mkdir(epoch, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := s.trusted(epoch, true); err != nil {
		return err
	}
	if err := atomicJSON(filepath.Join(staging, "metadata.json"), m); err != nil {
		return err
	}
	if err := atomicFile(filepath.Join(staging, "rootfs", ".brewlet-source"), []byte(m.SourceDigest+"\n")); err != nil {
		return err
	}
	if err := syncDirectories(staging); err != nil {
		return err
	}
	if err := fence(ctx); err != nil {
		return err
	}
	fresh, err := s.auth(m.ID, time.Now())
	if err != nil {
		return err
	}
	if fresh.InventoryUID != m.InventoryUID || fresh.Generation != m.Generation || fresh.Digest != m.SourceDigest {
		return fmt.Errorf("runtime authorization changed before publication")
	}
	final := filepath.Join(epoch, strconv.FormatInt(m.Generation, 10))
	if _, err := os.Lstat(final); err == nil {
		if err := s.trusted(final, true); err != nil {
			return err
		}
		var published Metadata
		if err := s.read(filepath.Join(final, "metadata.json"), &published); err != nil {
			return err
		}
		candidate := m
		candidate.ValidatedAt = published.ValidatedAt
		candidateJSON, err := json.Marshal(candidate)
		if err != nil {
			return err
		}
		publishedJSON, err := json.Marshal(published)
		if err != nil {
			return err
		}
		if !bytes.Equal(candidateJSON, publishedJSON) {
			return fmt.Errorf("immutable generation identity collision")
		}
		if err := ValidateRoot(filepath.Join(final, "rootfs"), &published); err != nil {
			return err
		}
		marker := filepath.Join(final, "rootfs", ".brewlet-source")
		if err := s.trusted(marker, false); err != nil {
			return err
		}
		source, err := os.ReadFile(marker)
		if err != nil {
			return err
		}
		if string(source) != m.SourceDigest+"\n" {
			return fmt.Errorf("immutable generation source receipt mismatch")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		if err := os.Rename(staging, final); err != nil {
			return err
		}
	}
	epochDir, err := os.Open(epoch)
	if err != nil {
		return err
	}
	err = epochDir.Sync()
	epochDir.Close()
	if err != nil {
		return err
	}
	if err := fence(ctx); err != nil {
		return err
	}
	if _, err := s.auth(m.ID, time.Now()); err != nil {
		return err
	}
	pointer := filepath.Join(s.Root, m.ID, ".next-current")
	previous, previousErr := os.Readlink(filepath.Join(s.Root, m.ID, "current"))
	if previousErr != nil && !os.IsNotExist(previousErr) {
		return previousErr
	}
	if info, err := os.Lstat(pointer); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("unrecognized pending current pointer")
		}
		if err := os.Remove(pointer); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(filepath.Join(m.InventoryUID, strconv.FormatInt(m.Generation, 10)), pointer); err != nil {
		return err
	}

	if err := os.Rename(pointer, filepath.Join(s.Root, m.ID, "current")); err != nil {
		return err
	}
	if previousErr == nil && previous != filepath.Join(m.InventoryUID, strconv.FormatInt(m.Generation, 10)) {
		parts := strings.Split(previous, "/")
		if len(parts) != 2 || !uidPattern.MatchString(parts[0]) {
			return fmt.Errorf("unsafe previous current pointer")
		}
		generation, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || generation < 1 {
			return fmt.Errorf("unsafe previous generation")
		}
		if err := fence(ctx); err != nil {
			return err
		}
		if err := s.markRetired(m.ID, parts[0], generation); err != nil {
			return err
		}
	}
	dir, err := os.Open(filepath.Join(s.Root, m.ID))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func syncDirectories(root string) error {
	var dirs []string
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		f, err := os.Open(dirs[i])
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Collect is deliberately conservative: every durable lease survives restart,
// and complete cross-namespace task/mount evidence is mandatory even when the
// lease directory is empty. Unknown or malformed records prevent deletion.
func (s *Store) Collect(ctx context.Context, id string, grace time.Duration, evidence Evidence, fence func(context.Context) error) error {
	if evidence == nil || grace <= 0 || fence == nil {
		return fmt.Errorf("positive grace and complete evidence required")
	}
	f, err := s.lock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock(f)
	entries, err := os.ReadDir(filepath.Join(s.Root, ".tasks"))
	if err != nil {
		return err
	}
	leased := map[string]bool{}
	for _, entry := range entries {
		if !uidPattern.MatchString(entry.Name()) {
			return fmt.Errorf("unrecognized durable task")
		}
		var lease Lease
		if err := s.read(filepath.Join(s.Root, ".tasks", entry.Name(), "lease.json"), &lease); err != nil {
			return err
		}
		if lease.Metadata.ID == id {
			leased[filepath.Join(lease.Metadata.InventoryUID, strconv.FormatInt(lease.Metadata.Generation, 10))] = true
		}
	}
	base := filepath.Join(s.Root, id)
	if err := s.trusted(base, true); err != nil {
		return err
	}
	current, err := os.Readlink(filepath.Join(base, "current"))
	if err != nil {
		return err
	}
	parts := strings.Split(current, "/")
	if len(parts) != 2 || !uidPattern.MatchString(parts[0]) {
		return fmt.Errorf("unsafe current pointer during collection")
	}
	gen, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || gen < 1 || strconv.FormatInt(gen, 10) != parts[1] {
		return fmt.Errorf("unsafe current generation during collection")
	}
	epochs, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	for _, epoch := range epochs {
		if epoch.Name() == ".retirements" {
			continue
		}
		if !epoch.IsDir() || !uidPattern.MatchString(epoch.Name()) {
			continue
		}
		generations, err := os.ReadDir(filepath.Join(base, epoch.Name()))
		if err != nil {
			return err
		}
		for _, generation := range generations {
			gen, err := strconv.ParseInt(generation.Name(), 10, 64)
			if err != nil || gen <= 0 {
				return fmt.Errorf("unrecognized generation")
			}
			rel := filepath.Join(epoch.Name(), generation.Name())
			if rel == current || leased[rel] {
				continue
			}
			dir := filepath.Join(base, rel)
			var m Metadata
			if err := s.read(filepath.Join(dir, "metadata.json"), &m); err != nil {
				return err
			}
			if err := m.Validate(); err != nil {
				return err
			}
			if m.ID != id || m.InventoryUID != epoch.Name() || m.Generation != gen {
				return fmt.Errorf("generation metadata mismatch")
			}
			var retired retirement
			retirementErr := s.read(s.retirementName(id, m.InventoryUID, m.Generation), &retired)
			if os.IsNotExist(retirementErr) {
				if err := fence(ctx); err != nil {
					return err
				}
				if err := s.markRetired(id, m.InventoryUID, m.Generation); err != nil {
					return err
				}
				continue
			} else if retirementErr != nil {
				return retirementErr
			}
			if retired.InventoryUID != m.InventoryUID || retired.Generation != m.Generation || retired.RetiredAt.IsZero() {
				return fmt.Errorf("invalid retirement receipt")
			}
			if time.Since(retired.RetiredAt) < grace {
				continue
			}
			gone, err := evidence.Gone(ctx, Lease{Metadata: m})
			if err != nil {
				return err
			}
			if !gone {
				continue
			}
			if err := fence(ctx); err != nil {
				return err
			}
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}
