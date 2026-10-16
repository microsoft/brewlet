//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/node"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
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
