package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

// ContentReceipt binds retained ORIGINAL bytes to their declaring descriptor.
// It is never a reconstructed manifest, config, launch document or layer.
type ContentReceipt struct {
	Policy      string             `json:"policy"`
	Descriptor  ocispec.Descriptor `json:"descriptor"`
	PublishedAt time.Time          `json:"publishedAt"`
}
type ContentCache struct {
	Dir        string
	IsMissing  func(error) bool
	references map[string]bool
	mu         sync.Mutex
}

func (c *ContentCache) entry(d digest.Digest) (string, error) {
	if d.Validate() != nil || d.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("canonical sha256 content identity required")
	}
	return filepath.Join(c.Dir, PolicyVersion+"-"+d.Encoded()), nil
}
func cacheOwned(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || info.IsDir() != directory || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("untrusted retained content path %s", path)
	}
	return nil
}
func syncCacheDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (c *ContentCache) reference(entry string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.references == nil {
		c.references = map[string]bool{}
	}
	c.references[entry] = true
}
func (c *ContentCache) References() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]string, 0, len(c.references))
	for entry := range c.references {
		result = append(result, entry)
	}
	sort.Strings(result)
	return result
}
func (c *ContentCache) missing(err error) bool {
	if c.IsMissing != nil {
		return c.IsMissing(err)
	}
	return os.IsNotExist(err)
}

// Open always checks the source first. Only a genuinely absent source permits
// retained bytes; corrupt/present, inaccessible and rejected content fails.
// Compressed payloads are copied/hashed as streams, never loaded into memory.
func (c *ContentCache) Open(ctx context.Context, source BlobOpener, d ocispec.Descriptor) (io.ReadCloser, error) {
	entry, err := c.entry(d.Digest)
	if err != nil {
		return nil, err
	}
	if d.Size < 0 {
		return nil, fmt.Errorf("invalid retained content size")
	}
	if err := os.MkdirAll(c.Dir, 0700); err != nil {
		return nil, err
	}
	if err := cacheOwned(c.Dir, true); err != nil {
		return nil, err
	}
	name := entry + ".lock"
	lock, err := os.OpenFile(name, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := cacheOwned(name, false); err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EAGAIN && err != unix.EWOULDBLOCK {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	_, statErr := os.Lstat(entry)
	exists := statErr == nil
	if statErr != nil && !os.IsNotExist(statErr) {
		return nil, statErr
	}
	if exists {
		receipt, err := VerifyContentEntry(ctx, entry)
		if err != nil {
			return nil, err
		}
		if receipt.Policy != PolicyVersion || receipt.Descriptor.Digest != d.Digest || receipt.Descriptor.Size != d.Size || receipt.Descriptor.MediaType != d.MediaType {
			return nil, fmt.Errorf("retained content descriptor mismatch")
		}
	}
	reader, sourceErr := source(d)
	if sourceErr != nil {
		if !c.missing(sourceErr) {
			return nil, sourceErr
		}
		if !exists {
			return nil, sourceErr
		}
		c.reference(entry)
		return os.OpenFile(filepath.Join(entry, "blob"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	}
	defer reader.Close()
	if exists {
		if err := verifyOriginalStream(ctx, reader, io.Discard, d); err != nil {
			return nil, err
		}
		c.reference(entry)
		return os.OpenFile(filepath.Join(entry, "blob"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	}
	stage, err := os.MkdirTemp(c.Dir, ".content-stage-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	blob, err := os.OpenFile(filepath.Join(stage, "blob"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
	if err != nil {
		return nil, err
	}
	copyErr := verifyOriginalStream(ctx, reader, blob, d)
	if copyErr == nil {
		copyErr = blob.Sync()
	}
	closeErr := blob.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	receipt := ContentReceipt{Policy: PolicyVersion, Descriptor: d, PublishedAt: time.Now().UTC()}
	data, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(stage, "receipt.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr = file.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := syncCacheDirectory(stage); err != nil {
		return nil, err
	}
	if err := os.Rename(stage, entry); err != nil {
		return nil, err
	}
	if err := syncCacheDirectory(c.Dir); err != nil {
		return nil, err
	}
	c.reference(entry)
	return os.OpenFile(filepath.Join(entry, "blob"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
}

func verifyOriginalStream(ctx context.Context, reader io.Reader, writer io.Writer, d ocispec.Descriptor) error {
	h := digest.SHA256.Digester()
	buffer := make([]byte, 128<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > d.Size {
				return fail("DigestMismatch", "retained content size differs")
			}
			if _, writeErr := io.MultiWriter(writer, h.Hash()).Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if total != d.Size || h.Digest() != d.Digest {
		return fail("DigestMismatch", "retained original content identity differs")
	}
	return nil
}

func VerifyContentEntry(ctx context.Context, entry string) (ContentReceipt, error) {
	var receipt ContentReceipt
	if err := cacheOwned(entry, true); err != nil {
		return receipt, err
	}
	for _, name := range []string{"receipt.json", "blob"} {
		path := filepath.Join(entry, name)
		if err := cacheOwned(path, false); err != nil {
			return receipt, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return receipt, err
		}
		if info.Mode().Perm()&0222 != 0 {
			return receipt, fmt.Errorf("retained content is mutable")
		}
	}
	file, err := os.OpenFile(filepath.Join(entry, "receipt.json"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return receipt, err
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	file.Close()
	if err != nil {
		return receipt, err
	}
	if len(data) > 1<<20 {
		return receipt, fmt.Errorf("retained receipt too large")
	}
	if err := StrictJSON(data, &receipt); err != nil {
		return receipt, err
	}
	d := receipt.Descriptor
	if receipt.Policy != PolicyVersion || d.Digest.Validate() != nil || d.Digest.Algorithm() != digest.SHA256 || d.Size < 0 || d.MediaType == "" || receipt.PublishedAt.IsZero() || filepath.Base(entry) != receipt.Policy+"-"+d.Digest.Encoded() {
		return receipt, fmt.Errorf("invalid retained content receipt")
	}
	blob, err := os.OpenFile(filepath.Join(entry, "blob"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return receipt, err
	}
	defer blob.Close()
	if err := verifyOriginalStream(ctx, blob, io.Discard, d); err != nil {
		return receipt, err
	}
	return receipt, nil
}

// Target supplies the ORIGINAL target descriptor only when the caller has
// independently established genuine absence of the protected CRI target.
func (c *ContentCache) Target(ctx context.Context, target digest.Digest) (ocispec.Descriptor, error) {
	entry, err := c.entry(target)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	receipt, err := VerifyContentEntry(ctx, entry)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	c.reference(entry)
	return receipt.Descriptor, nil
}
