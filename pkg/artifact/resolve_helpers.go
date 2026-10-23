package artifact

import (
	"context"
	"fmt"
	"io"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// FetchManifest fetches the manifest or index named by key (a tag or a
// digest) without importing its graph. The returned descriptor carries the
// digest of the exact bytes served, so a tag resolves to a canonical digest;
// digest keys are verified.
func (r *Registry) FetchManifest(ctx context.Context, key string) (v1.Descriptor, []byte, error) {
	return r.fetchManifest(ctx, key)
}

// FetchBlob fetches a small blob such as an image config, bounded by max bytes,
// and verifies its digest.
func (r *Registry) FetchBlob(ctx context.Context, d digest.Digest, max int64) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	resp, err := r.do(ctx, "GET", r.endpoint("blobs", d.String()), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err = status(resp, 200); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("blob %s exceeds %d bytes", d, max)
	}
	if digest.FromBytes(b) != d {
		return nil, fail("DigestMismatch", "blob digest mismatch for %s", d)
	}
	return b, nil
}
