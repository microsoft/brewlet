// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync/atomic"

	"github.com/microsoft/brewlet/internal/artifact"
)

// Progress exposes live upload counters, safe for concurrent reads.
type Progress struct {
	TotalBytes    atomic.Int64
	UploadedBytes atomic.Int64
}

// Status renders the counters, e.g. "3.2/12.0 MiB (26%)".
func (p *Progress) Status() string {
	total, done := p.TotalBytes.Load(), p.UploadedBytes.Load()
	if total <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/%s (%d%%)", formatBytes(done), formatBytes(total), done*100/total)
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// PushOptions configures PushLayout.
type PushOptions struct {
	// Logf reports per-blob decisions; may be nil.
	Logf func(format string, args ...any)
	// Progress receives byte counters; may be nil.
	Progress *Progress
}

// PushResult describes the pushed root object.
type PushResult struct {
	Digest        string
	Size          int64
	MediaType     string
	BlobsUploaded int
	BlobsSkipped  int
}

type node struct {
	MediaType string                `json:"mediaType"`
	Manifests []artifact.Descriptor `json:"manifests"`
	Config    *artifact.Descriptor  `json:"config"`
	Layers    []artifact.Descriptor `json:"layers"`
}

type plan struct {
	store     artifact.Store
	blobs     []artifact.Descriptor
	manifests []artifact.Descriptor // children first, root last
	seen      map[string]bool
}

func (p *plan) walk(desc artifact.Descriptor, depth int) error {
	if depth > 4 {
		return fmt.Errorf("OCI index nesting too deep at %s", desc.Digest)
	}
	raw, err := p.store.ReadBlob(desc.Digest)
	if err != nil {
		return fmt.Errorf("read %s: %w", desc.Digest, err)
	}
	var n node
	if err := json.Unmarshal(raw, &n); err != nil {
		return fmt.Errorf("decode %s: %w", desc.Digest, err)
	}
	if desc.MediaType == "" {
		desc.MediaType = n.MediaType
	}
	if desc.MediaType == "" {
		return fmt.Errorf("manifest %s has no media type", desc.Digest)
	}
	for _, child := range n.Manifests {
		if err := p.walk(child, depth+1); err != nil {
			return err
		}
	}
	if n.Config != nil {
		p.addBlob(*n.Config)
	}
	for _, l := range n.Layers {
		p.addBlob(l)
	}
	if !p.seen["m"+desc.Digest] {
		p.seen["m"+desc.Digest] = true
		p.manifests = append(p.manifests, desc)
	}
	return nil
}

func (p *plan) addBlob(d artifact.Descriptor) {
	if d.Digest == "" || p.seen["b"+d.Digest] {
		return
	}
	p.seen["b"+d.Digest] = true
	p.blobs = append(p.blobs, d)
}

// PushLayout copies the object tagged localRef in store, with every manifest
// and blob it references, to the client's repository and tags it tag. Blobs
// the registry already has are skipped; child manifests are pushed by digest
// before the tagged root so the registry never sees a dangling reference.
func PushLayout(ctx context.Context, store artifact.Store, localRef string, c *Client, tag string, opts PushOptions) (PushResult, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	prog := opts.Progress
	if prog == nil {
		prog = &Progress{}
	}
	root, err := store.DescriptorByRef(localRef)
	if err != nil {
		return PushResult{}, err
	}
	p := &plan{store: store, seen: map[string]bool{}}
	if err := p.walk(root, 0); err != nil {
		return PushResult{}, err
	}
	rootDesc := p.manifests[len(p.manifests)-1]

	var missing []artifact.Descriptor
	var result PushResult
	for _, b := range p.blobs {
		exists, err := c.BlobExists(ctx, b.Digest)
		if err != nil {
			return PushResult{}, err
		}
		if exists {
			result.BlobsSkipped++
			logf("  %s already exists in %s (skipping upload)", shortDigest(b.Digest), c.Registry())
			continue
		}
		missing = append(missing, b)
		prog.TotalBytes.Add(b.Size)
	}
	for _, b := range missing {
		path, err := store.BlobPath(b.Digest)
		if err != nil {
			return PushResult{}, err
		}
		base := prog.UploadedBytes.Load()
		open := func() (io.ReadCloser, error) {
			prog.UploadedBytes.Store(base)
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			return &countingReader{ReadCloser: f, n: &prog.UploadedBytes}, nil
		}
		if err := c.UploadBlob(ctx, b.Digest, b.Size, open); err != nil {
			return PushResult{}, err
		}
		prog.UploadedBytes.Store(base + b.Size)
		result.BlobsUploaded++
		logf("  uploaded %s (%s)", shortDigest(b.Digest), formatBytes(b.Size))
	}
	for _, m := range p.manifests {
		raw, err := store.ReadBlob(m.Digest)
		if err != nil {
			return PushResult{}, err
		}
		ref := m.Digest
		if m.Digest == rootDesc.Digest {
			ref = tag
		}
		if err := c.PutManifest(ctx, ref, raw, m.MediaType); err != nil {
			return PushResult{}, err
		}
	}
	result.Digest, result.Size, result.MediaType = rootDesc.Digest, rootDesc.Size, rootDesc.MediaType
	return result, nil
}

func shortDigest(d string) string {
	if len(d) > len("sha256:")+12 {
		return d[:len("sha256:")+12]
	}
	return d
}

type countingReader struct {
	io.ReadCloser
	n *atomic.Int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.n.Add(int64(n))
	return n, err
}
