package artifact

import (
	"archive/tar"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLayerCacheConcurrentDigestReuse(t *testing.T) {
	dir := testDir(t)
	store := Store{Dir: filepath.Join(dir, "store")}
	src := source(t, store, "zstd", []*tar.Header{fileHeader("app/server", "bytes", 0755)}, []string{"bytes"})
	cache := LayerCache{Dir: filepath.Join(dir, "cache")}
	var wg sync.WaitGroup
	results := make(chan string, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); p, e := cache.Materialize(context.Background(), src); results <- p; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		must(t, e)
	}
	root := ""
	for p := range results {
		if root != "" && root != p {
			t.Fatal("digest cached twice")
		}
		root = p
	}
	b, e := os.ReadFile(filepath.Join(root, "app/server"))
	must(t, e)
	if string(b) != "bytes" {
		t.Fatal("invalid cache bytes")
	}
	fi, e := os.Stat(root)
	must(t, e)
	if fi.Mode().Perm() != 0755 {
		t.Fatal("overlay root is not traversable by nonroot")
	}
}

func TestVerifyCachedEntryReceipt(t *testing.T) {
	dir := testDir(t)
	store := Store{Dir: filepath.Join(dir, "store")}
	src := source(t, store, "gzip", []*tar.Header{fileHeader("app/file", "bytes", 0644)}, []string{"bytes"})
	cache := LayerCache{Dir: filepath.Join(dir, "cache")}
	before := time.Now().UTC()
	root, e := cache.Materialize(context.Background(), src)
	must(t, e)
	entry := filepath.Dir(root)
	verified, receipt, e := VerifyCachedEntry(entry)
	must(t, e)
	if verified != root || receipt.Policy != PolicyVersion || receipt.Digest != src.Descriptor.Digest.String() || receipt.DiffID != src.DiffID.String() || receipt.PublishedAt.Before(before) || receipt.PublishedAt.After(time.Now().UTC()) {
		t.Fatal("receipt identity/time mismatch")
	}
	_, e = cache.Materialize(context.Background(), src)
	must(t, e)
	_, again, e := VerifyCachedEntry(entry)
	must(t, e)
	if again.PublishedAt != receipt.PublishedAt {
		t.Fatal("cache reuse changed immutable publication time")
	}
	for name, mutate := range map[string]func(*CacheReceipt){
		"zero-time":    func(r *CacheReceipt) { r.PublishedAt = time.Time{} },
		"wrong-digest": func(r *CacheReceipt) { r.Digest = "not-a-digest" },
		"wrong-diff":   func(r *CacheReceipt) { r.DiffID = "not-a-digest" },
		"policy-path":  func(r *CacheReceipt) { r.Policy = "../v2" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := receipt
			mutate(&bad)
			b, e := json.Marshal(bad)
			must(t, e)
			must(t, os.Chmod(filepath.Join(entry, "receipt.json"), 0600))
			must(t, os.WriteFile(filepath.Join(entry, "receipt.json"), b, 0600))
			must(t, os.Chmod(filepath.Join(entry, "receipt.json"), 0400))
			if _, _, e = VerifyCachedEntry(entry); e == nil {
				t.Fatal("untrusted receipt accepted")
			}
		})
	}
	b, e := json.Marshal(receipt)
	must(t, e)
	must(t, os.Chmod(filepath.Join(entry, "receipt.json"), 0600))
	must(t, os.WriteFile(filepath.Join(entry, "receipt.json"), b, 0400))
	must(t, os.Chmod(filepath.Join(entry, "receipt.json"), 0400))
	alias := filepath.Join(dir, "alias")
	must(t, os.Symlink(entry, alias))
	if _, _, e = VerifyCachedEntry(alias); e == nil {
		t.Fatal("symlink cache entry accepted")
	}
	must(t, os.Remove(filepath.Join(entry, "receipt.json")))
	must(t, os.WriteFile(filepath.Join(dir, "external-receipt"), b, 0400))
	must(t, os.Symlink(filepath.Join(dir, "external-receipt"), filepath.Join(entry, "receipt.json")))
	if _, _, e = VerifyCachedEntry(entry); e == nil {
		t.Fatal("symlink receipt accepted")
	}
}
