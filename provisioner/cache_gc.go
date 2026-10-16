package provisioner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/node"
)

var cacheKey = regexp.MustCompile(`^v[0-9]+-[0-9]+-[0-9a-f]{64}$`)

// CollectLayerCache requires the exclusive start/mount guard, durable task
// references, immutable publication receipts, and complete host mount evidence.
// Pending creates with unavailable initial state block all cache collection.
func CollectLayerCache(ctx context.Context, store *node.Store, directory, hostPath string, grace time.Duration, evidence node.MountEvidence, fence func(context.Context) error) error {
	if grace <= 0 || store == nil || evidence == nil || fence == nil {
		return fmt.Errorf("positive cache grace, trusted state, mount evidence and fresh fence required")
	}
	if !filepath.IsAbs(directory) || !filepath.IsAbs(hostPath) || filepath.Clean(hostPath) != hostPath {
		return fmt.Errorf("canonical cache paths required")
	}
	if err := fence(ctx); err != nil {
		return err
	}
	guard, err := node.AcquireCacheGuard(ctx, directory, true)
	if err != nil {
		return err
	}
	defer guard.Close()
	leases, err := store.Leases()
	if err != nil {
		return err
	}
	references := map[string]bool{}
	for _, lease := range leases {
		state, err := store.LoadInitial(lease.TaskID)
		if err != nil {
			return err
		}
		if len(state.LayerRoots) == 0 {
			return fmt.Errorf("cache references unavailable for durable task")
		}
		for _, root := range state.LayerRoots {
			if filepath.Clean(root) != root || !strings.HasPrefix(root, hostPath+"/") {
				return fmt.Errorf("unrecognized durable cache reference")
			}
			references[root] = true
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var candidates []string
	for _, entry := range entries {
		if !cacheKey.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		if err := node.NewStore(path).Check(); err != nil {
			return err
		}
		if err := trustedFile(filepath.Join(path, "receipt.json")); err != nil {
			return err
		}
		root, receipt, err := artifact.VerifyCachedEntry(path)
		if err != nil {
			return err
		}
		if err := node.NewStore(root).Check(); err != nil {
			return err
		}
		hostRoot := filepath.Join(hostPath, entry.Name(), "rootfs")
		if references[hostRoot] || time.Since(receipt.PublishedAt) < grace {
			continue
		}
		gone, err := evidence.MountsGone(ctx, hostRoot)
		if err != nil {
			return err
		}
		if !gone {
			continue
		}
		candidates = append(candidates, path)
	}
	for _, candidate := range candidates {
		if err := fence(ctx); err != nil {
			return err
		}
		if err := os.RemoveAll(candidate); err != nil {
			return err
		}
	}
	return nil
}
