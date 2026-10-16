package artifact

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"time"

	digest "github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

type CacheReceipt struct {
	Policy      string    `json:"policy"`
	Digest      string    `json:"digest"`
	DiffID      string    `json:"diffId"`
	PublishedAt time.Time `json:"publishedAt"`
}

// LayerCache stores immutable per-compressed-digest overlay lowers. Its trusted,
// root-owned parent must live on persistent storage. The node owns cache leases
// and must not collect entries with live mounts or durable task references.
type LayerCache struct{ Dir string }

func (c LayerCache) Materialize(ctx context.Context, source LayerSource) (string, error) {
	if source.Descriptor.Digest.Validate() != nil || source.DiffID.Validate() != nil {
		return "", fmt.Errorf("verified layer digest and diff ID required")
	}
	if source.Descriptor.Annotations["brewlet.sh/layer"] == "launch" {
		return "", fail("UnsupportedArtifact", "launch is never an overlay lower")
	}
	if e := os.MkdirAll(c.Dir, 0700); e != nil {
		return "", e
	}
	key := PolicyVersion + "-" + source.Descriptor.Digest.Encoded()
	lock, e := os.OpenFile(filepath.Join(c.Dir, key+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return "", e
	}
	defer lock.Close()
	for {
		e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if e == nil {
			break
		}
		if !errors.Is(e, unix.EWOULDBLOCK) {
			return "", e
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	destination := filepath.Join(c.Dir, key)
	want := CacheReceipt{Policy: PolicyVersion, Digest: source.Descriptor.Digest.String(), DiffID: source.DiffID.String()}
	if _, e := os.Lstat(destination); e == nil {
		root, got, e := VerifyCachedEntry(destination)
		if e != nil || got.Policy != want.Policy || got.Digest != want.Digest || got.DiffID != want.DiffID {
			return "", fmt.Errorf("untrusted cache receipt")
		}
		return root, nil
	} else if !os.IsNotExist(e) {
		return "", e
	}
	stage, e := os.MkdirTemp(c.Dir, ".layer-stage-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(stage)
	rootfs := filepath.Join(stage, "rootfs")
	if e = MaterializeLayers(ctx, []LayerSource{source}, rootfs); e != nil {
		return "", e
	}
	info, e := WalkLayer(ctx, source, nil)
	if e != nil {
		return "", e
	}
	layer, e := compileLayer(info)
	if e != nil {
		return "", e
	}
	root, e := os.OpenRoot(rootfs)
	if e != nil {
		return "", e
	}
	defer root.Close()
	for _, entry := range layer.entries {
		target, wh, opaque := whiteout(entry.Path)
		if !wh {
			continue
		}
		parent, e := resolveLayerPath(root, path.Dir(target), 0)
		if e != nil {
			return "", e
		}
		target = path.Join(parent, path.Base(target))
		if _, e = archivePath(target); e != nil {
			return "", e
		}
		if e = root.MkdirAll(path.Dir(target), 0755); e != nil {
			return "", e
		}
		if opaque {
			if e = root.MkdirAll(target, 0755); e != nil {
				return "", e
			}
			if e = overlayOpaque(filepath.Join(rootfs, filepath.FromSlash(target))); e != nil {
				return "", e
			}
		} else {
			fi, e := root.Lstat(target)
			if e == nil {
				if fi.IsDir() {
					if e = overlayOpaque(filepath.Join(rootfs, filepath.FromSlash(target))); e != nil {
						return "", e
					}
				}
				continue
			}
			if !os.IsNotExist(e) {
				return "", e
			}
			if e = overlayWhiteout(filepath.Join(rootfs, filepath.FromSlash(target))); e != nil {
				return "", e
			}
		}
	}
	for i := len(layer.entries) - 1; i >= 0; i-- {
		entry := layer.entries[i]
		if entry.Type == tar.TypeDir {
			if node, ok := layer.nodes[entry.Path]; ok && node.Type == tar.TypeDir {
				if e = root.Chtimes(entry.Path, entry.ModTime, entry.ModTime); e != nil {
					return "", e
				}
			}
		}
	}
	want.PublishedAt = time.Now().UTC()
	b, e := json.Marshal(want)
	if e != nil {
		return "", e
	}
	if e = os.WriteFile(filepath.Join(stage, "receipt.json"), b, 0400); e != nil {
		return "", e
	}
	if _, e = os.Lstat(destination); !os.IsNotExist(e) {
		return "", fmt.Errorf("cache destination exists without valid receipt")
	}
	if e = os.Rename(stage, destination); e != nil {
		return "", e
	}
	return filepath.Join(destination, "rootfs"), nil
}

// VerifyCachedEntry checks a canonical published entry without following a
// symlink for its directory, receipt, or rootfs. The caller supplies a trusted
// root-owned parent and holds its GC/start barrier for the entire decision.
// Missing or malformed receipts are uncertainty, never permission to collect.
func VerifyCachedEntry(directory string) (string, CacheReceipt, error) {
	var receipt CacheReceipt
	fi, e := os.Lstat(directory)
	if e != nil {
		return "", receipt, e
	}
	if !fi.IsDir() {
		return "", receipt, fmt.Errorf("cache entry is not a directory")
	}
	root, e := os.OpenRoot(directory)
	if e != nil {
		return "", receipt, e
	}
	defer root.Close()
	fi, e = root.Lstat("receipt.json")
	if e != nil {
		return "", receipt, e
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0222 != 0 {
		return "", receipt, fmt.Errorf("cache receipt is not regular")
	}
	b, e := root.ReadFile("receipt.json")
	if e != nil {
		return "", receipt, e
	}
	if e = StrictJSON(b, &receipt); e != nil {
		return "", receipt, e
	}
	layerDigest, diffID := digest.Digest(receipt.Digest), digest.Digest(receipt.DiffID)
	if !regexp.MustCompile(`^v[0-9]+-[0-9]+$`).MatchString(receipt.Policy) || layerDigest.Validate() != nil || layerDigest.Algorithm() != digest.SHA256 || diffID.Validate() != nil || diffID.Algorithm() != digest.SHA256 || receipt.PublishedAt.IsZero() || filepath.Base(directory) != receipt.Policy+"-"+layerDigest.Encoded() {
		return "", receipt, fmt.Errorf("invalid cache receipt identity/publication time")
	}
	fi, e = root.Lstat("rootfs")
	if e != nil {
		return "", receipt, e
	}
	if !fi.IsDir() {
		return "", receipt, fmt.Errorf("cache rootfs is not a directory")
	}
	return filepath.Join(directory, "rootfs"), receipt, nil
}
