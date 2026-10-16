package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestRetainedOriginalContentConcurrentPublicationAndAbsenceOnlyFallback(t *testing.T) {
	dir, err := os.MkdirTemp(".", ".retained-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	data := []byte("exact ORIGINAL content")
	d := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: digest.FromBytes(data), Size: int64(len(data))}
	cache := &ContentCache{Dir: filepath.Join(dir, "content")}
	source := func(ocispec.Descriptor) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			reader, err := cache.Open(context.Background(), source, d)
			if err != nil {
				failures <- err
				return
			}
			defer reader.Close()
			actual, err := io.ReadAll(reader)
			if err != nil {
				failures <- err
			} else if !bytes.Equal(actual, data) {
				failures <- errors.New("original bytes changed")
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if len(cache.References()) != 1 {
		t.Fatal(cache.References())
	}
	missing := func(ocispec.Descriptor) (io.ReadCloser, error) { return nil, os.ErrNotExist }
	reader, err := cache.Open(context.Background(), missing, d)
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	bad := d
	bad.Size++
	if _, err := cache.Open(context.Background(), missing, bad); err == nil {
		t.Fatal("mismatched declaring descriptor reused")
	}
	rejected := func(ocispec.Descriptor) (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF }
	if _, err := cache.Open(context.Background(), rejected, d); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("nonabsence error masked", err)
	}
	entry := filepath.Join(cache.Dir, PolicyVersion+"-"+d.Digest.Encoded())
	if err := os.Chmod(filepath.Join(entry, "receipt.json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Open(context.Background(), missing, d); err == nil {
		t.Fatal("mutable receipt accepted")
	}
}
