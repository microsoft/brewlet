//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
	"github.com/microsoft/brewlet/pkg/node"
	digest "github.com/opencontainers/go-digest"
)

type stagingRecord struct {
	ProfileUID        string `json:"profileUID"`
	NodeUID           string `json:"nodeUID"`
	ProfileGeneration int64  `json:"profileGeneration"`
	ID                string `json:"id"`
	InventoryUID      string `json:"inventoryUID"`
	Generation        int64  `json:"generation"`
	Digest            string `json:"digest"`
	Snapshotter       string `json:"snapshotter"`
	SnapshotKey       string `json:"snapshotKey"`
}

func (i *ContainerdInstaller) fence(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.Fence == nil {
		return fmt.Errorf("fenced runtime installer required")
	}
	return i.Fence(ctx)
}

func (i *ContainerdInstaller) RecoverStaging(ctx context.Context, root string) error {
	if err := i.fence(ctx); err != nil {
		return err
	}
	base := filepath.Join(root, ".staging")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := node.NewStore(base).Check(); err != nil {
		return err
	}
	for _, entry := range entries {
		stage := filepath.Join(base, entry.Name())
		private := strings.HasPrefix(entry.Name(), ".runtime-pending-")
		if !private && !strings.HasPrefix(entry.Name(), "runtime-") {
			return fmt.Errorf("unmanaged staging entry")
		}
		if err := node.NewStore(stage).Check(); err != nil {
			return err
		}
		name := filepath.Join(stage, "pending.json")
		if err := trustedFile(name); os.IsNotExist(err) || private {
			// Only an already UID-bound activation can own this unpublished
			// workspace. No root absence or filename alone confers authority.
			activation := HostActivation{InventoryRoot: root, ProfileUID: i.ProfileUID, NodeUID: i.NodeUID}
			if _, err := activation.load(); err != nil {
				return err
			}
			if i.MountEvidence == nil {
				return fmt.Errorf("complete unpublished-stage mount evidence required")
			}
			gone, err := i.MountEvidence.MountsGone(ctx, stage)
			if err != nil {
				return err
			}
			if !gone {
				return fmt.Errorf("unpublished staging data remains mounted")
			}
			if err := i.fence(ctx); err != nil {
				return err
			}
			if err := os.RemoveAll(stage); err != nil {
				return err
			}
			continue
		} else if err != nil {
			return fmt.Errorf("staging ownership evidence unavailable: %w", err)
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		var record stagingRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return err
		}
		if record.ProfileUID != i.ProfileUID || record.NodeUID != i.NodeUID || record.ProfileGeneration <= 0 || record.ProfileGeneration > i.ProfileGeneration || record.Snapshotter != i.Snapshotter || !strings.HasPrefix(record.SnapshotKey, "brewlet-copy-") {
			return fmt.Errorf("ownership-fence-failed: foreign or invalid staged runtime")
		}
		if err := node.ValidateID(record.ID); err != nil {
			return err
		}
		sourceDigest := digest.Digest(record.Digest)
		if record.InventoryUID == "" || record.Generation < 1 || sourceDigest.Validate() != nil || sourceDigest.Algorithm() != digest.SHA256 || !regexp.MustCompile(`^brewlet-copy-[0-9]+-[0-9]+$`).MatchString(record.SnapshotKey) {
			return fmt.Errorf("invalid staged runtime identity")
		}
		if err := i.fence(ctx); err != nil {
			return err
		}
		source := filepath.Join(stage, "source")
		if info, err := os.Lstat(source); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("unrecognized staging source")
			}
			if err := mount.UnmountAll(source, 0); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if i.MountEvidence == nil {
			return fmt.Errorf("complete staged-runtime mount evidence required")
		}
		gone, err := i.MountEvidence.MountsGone(ctx, stage)
		if err != nil {
			return err
		}
		if !gone {
			return fmt.Errorf("staged runtime remains mounted")
		}
		if err := i.fence(ctx); err != nil {
			return err
		}
		store := i.Client.SnapshotService(record.Snapshotter)
		info, err := store.Stat(ctx, record.SnapshotKey)
		if err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		if err == nil {
			if info.Kind != snapshots.KindView || info.Labels["brewlet.sh/profile-uid"] != record.ProfileUID || info.Labels["brewlet.sh/node-uid"] != record.NodeUID || info.Labels["brewlet.sh/runtime-image"] != record.ID || info.Labels["brewlet.sh/inventory-uid"] != record.InventoryUID {
				return fmt.Errorf("ownership-fence-failed: foreign staged snapshot")
			}
			if err := i.fence(ctx); err != nil {
				return err
			}
			if err := store.Remove(ctx, record.SnapshotKey); err != nil {
				return err
			}
		}
		if err := i.fence(ctx); err != nil {
			return err
		}
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
	}
	return syncDir(base)
}
