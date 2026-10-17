//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/node"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

type cacheEvidence struct {
	gone bool
	err  error
}

func (e cacheEvidence) MountsGone(context.Context, string) (bool, error) { return e.gone, e.err }
func cacheFixture(t *testing.T) (*Worker, string, string) {
	t.Helper()
	w, _ := authorityFixture(t)
	requireRootWorkdir(t, w)
	cache := filepath.Join(w.Store.Root, ".cache")
	key := artifact.PolicyVersion + "-" + strings.Repeat("a", 64)
	entry := filepath.Join(cache, key)
	if err := os.MkdirAll(filepath.Join(entry, "rootfs"), 0755); err != nil {
		t.Fatal(err)
	}
	receipt := artifact.CacheReceipt{Policy: artifact.PolicyVersion, Digest: "sha256:" + strings.Repeat("a", 64), DiffID: "sha256:" + strings.Repeat("b", 64), PublishedAt: time.Now().Add(-2 * time.Hour)}
	b, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "receipt.json"), b, 0400); err != nil {
		t.Fatal(err)
	}
	return w, cache, entry
}
func TestCacheCollectionRetainsMountsAndRequiresFreshFenceUnit(t *testing.T) {
	w, cache, entry := cacheFixture(t)
	ctx := context.Background()
	fence := func(context.Context) error { return nil }
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: false}, fence); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(entry); err != nil {
		t.Fatal("mounted cache removed", err)
	}
	calls := 0
	lost := func(context.Context) error {
		calls++
		if calls > 1 {
			return errors.New("ownership-fence-failed")
		}
		return nil
	}
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, lost); err == nil {
		t.Fatal("lost fence accepted")
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatal("cache removed after fence loss", err)
	}
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatal("unused old cache retained", err)
	}
}
func TestCacheCollectionRetainsActualBindMountOnSeparateFilesystem(t *testing.T) {
	w, cache, entry := cacheFixture(t)
	ctx := context.Background()
	target := filepath.Join(w.Store.Root, "cache-bind")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	err := unix.Mount(filepath.Join(entry, "rootfs"), target, "", unix.MS_BIND, "")
	if errors.Is(err, unix.EPERM) {
		t.Skipf("requires isolated Linux bind mounts and CAP_SYS_ADMIN: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	mounted := true
	defer func() {
		if mounted {
			if err := unix.Unmount(target, 0); err != nil {
				t.Error(err)
			}
		}
	}()
	fence := func(context.Context) error { return nil }
	evidence := node.LinuxEvidence{}
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, evidence, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatal("actual bind mount failed to protect cache on a separate filesystem", err)
	}
	if err := unix.Unmount(target, 0); err != nil {
		t.Fatal(err)
	}
	mounted = false
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, evidence, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatal("unreferenced cache was not reclaimed after actual unmount", err)
	}
}

func TestCacheCollectionWaitsForActualConsumerGuard(t *testing.T) {
	w, cache, entry := cacheFixture(t)
	ctx := context.Background()
	consumer, err := node.AcquireCacheGuard(ctx, cache, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = consumer.Close() })
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	fence := func(context.Context) error { return nil }
	if err := CollectLayerCache(bounded, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, fence); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("collection bypassed an actual in-progress consumer guard: %v", err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatal("collection removed cache while Create could still mount it", err)
	}
	if err := consumer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatal("unused cache remained after the actual consumer barrier drained", err)
	}
}

func TestCacheCollectionUsesDurableReferencesAndBlocksPendingCreateUnit(t *testing.T) {
	w, cache, entry := cacheFixture(t)
	ctx := context.Background()
	m := node.Metadata{ID: "static", InventoryUID: "epoch-one", Generation: 1, SourceDigest: "sha256:" + strings.Repeat("a", 64), ManifestDigest: "sha256:" + strings.Repeat("b", 64), Platform: ocispec.Platform{OS: "linux", Architecture: "arm64"}, Env: []string{"PATH=/bin"}, UID: 1000, GID: 1000, ValidatedAt: time.Now()}
	l := node.Lease{TaskID: "task-one", Namespace: "k8s.io", Metadata: m, AcquiredAt: time.Now()}
	task := filepath.Join(w.Store.Root, ".tasks", l.TaskID)
	if err := os.Mkdir(task, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task, "lease.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	fence := func(context.Context) error { return nil }
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, fence); err == nil {
		t.Fatal("pending initial state did not block GC")
	}
	process := specs.Process{Args: []string{"probe"}, Cwd: "/app", User: specs.User{UID: 1000, GID: 1000}, NoNewPrivileges: true}
	if err := w.Store.SaveInitial(node.InitialState{Lease: l, Process: process, LayerRoots: []string{filepath.Join(entry, "rootfs")}}); err != nil {
		t.Fatal(err)
	}
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatal("durably referenced cache removed", err)
	}
}

func TestOriginalContentCollectionRequiresGraceMountEvidenceAndFreshFenceUnit(t *testing.T) {
	w, cache, _ := cacheFixture(t)
	ctx := context.Background()
	raw := "original protected launch bytes"
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayer, Digest: digest.FromString(raw), Size: int64(len(raw))}
	content := &artifact.ContentCache{Dir: filepath.Join(cache, ".content")}
	reader, err := content.Open(ctx, func(ocispec.Descriptor) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(raw)), nil
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	path := content.References()[0]
	fence := func(context.Context) error { return nil }
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("young original content removed", err)
	}
	receipt, err := artifact.VerifyContentEntry(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	receipt.PublishedAt = time.Now().Add(-2 * time.Hour)
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	receiptPath := filepath.Join(path, "receipt.json")
	if err := os.Chmod(receiptPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, data, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(receiptPath, 0400); err != nil {
		t.Fatal(err)
	}
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: false}, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("mounted original content removed", err)
	}
	calls := 0
	lost := func(context.Context) error {
		calls++
		if calls > 1 {
			return errors.New("ownership-fence-failed")
		}
		return nil
	}
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, lost); err == nil {
		t.Fatal("original content collected without fresh authority")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("original content removed after fence loss", err)
	}
	if err := CollectLayerCache(ctx, w.Store, cache, cache, time.Hour, cacheEvidence{gone: true}, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unreferenced old original content retained", err)
	}
}
