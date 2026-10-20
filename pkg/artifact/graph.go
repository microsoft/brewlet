package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func CopyGraph(ctx context.Context, source, destination Store, target v1.Descriptor) error {
	return copyGraph(ctx, source, destination, target, map[digest.Digest]int64{})
}
func copyGraph(ctx context.Context, source, destination Store, target v1.Descriptor, seen map[digest.Digest]int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size, ok := seen[target.Digest]; ok {
		if size != target.Size {
			return fail("DigestMismatch", "conflicting copied descriptor size")
		}
		return nil
	}
	r, e := source.Open(target)
	if e != nil {
		return e
	}
	got, e := destination.Put(target.MediaType, r)
	r.Close()
	if e != nil {
		return e
	}
	if got.Digest != target.Digest || got.Size != target.Size {
		return fail("DigestMismatch", "copy identity mismatch")
	}
	seen[target.Digest] = target.Size
	if target.MediaType == v1.MediaTypeImageIndex {
		b, e := source.Read(target)
		if e != nil {
			return e
		}
		var idx v1.Index
		if e = json.Unmarshal(b, &idx); e != nil {
			return e
		}
		for _, d := range idx.Manifests {
			if e = copyGraph(ctx, source, destination, d, seen); e != nil {
				return e
			}
		}
	} else if target.MediaType == v1.MediaTypeImageManifest {
		b, e := source.Read(target)
		if e != nil {
			return e
		}
		var m v1.Manifest
		if e = json.Unmarshal(b, &m); e != nil {
			return e
		}
		for _, d := range append([]v1.Descriptor{m.Config}, m.Layers...) {
			if e = copyGraph(ctx, source, destination, d, seen); e != nil {
				return e
			}
		}
	}
	if target.MediaType == v1.MediaTypeImageManifest || target.MediaType == v1.MediaTypeImageIndex {
		refs, e := source.Referrers(target)
		if e != nil {
			return e
		}
		for _, ref := range refs {
			if e = copyGraph(ctx, source, destination, ref, seen); e != nil && ref.ArtifactType != MediaTypeAttestation {
				return e
			}
			if e = destination.RecordReferrer(target, ref); e != nil {
				return e
			}
			materials, e := CompositionMaterials(source, target)
			if e != nil {
				return e
			}
			for _, material := range materials {
				if e = copyGraph(ctx, source, destination, material, seen); e != nil {
					return e
				}
			}
		}
	}
	return nil
}

// BuildIndex accepts only explicitly supplied descriptors. Their blobs must
// already be present in the store, and each selected artifact is verified.
func BuildIndex(ctx context.Context, store Store, descriptorFiles []string) (v1.Descriptor, error) {
	if len(descriptorFiles) == 0 {
		return v1.Descriptor{}, fmt.Errorf("index requires --manifest descriptor.json")
	}
	idx := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex}
	seen := map[string]bool{}
	for _, f := range descriptorFiles {
		b, e := os.ReadFile(f)
		if e != nil {
			return v1.Descriptor{}, e
		}
		var d v1.Descriptor
		if e = StrictJSON(b, &d); e != nil {
			return v1.Descriptor{}, e
		}
		if d.Platform == nil || d.MediaType != v1.MediaTypeImageManifest {
			return v1.Descriptor{}, fmt.Errorf("explicit platform manifest descriptor required")
		}
		key := d.Platform.OS + "/" + d.Platform.Architecture + "/" + d.Platform.Variant
		if seen[key] {
			return v1.Descriptor{}, fail("InvalidPlatform", "duplicate index platform")
		}
		seen[key] = true
		if _, e = Resolve(ctx, store.Open, d, *d.Platform); e != nil {
			return v1.Descriptor{}, e
		}
		idx.Manifests = append(idx.Manifests, d)
	}
	return store.PutJSON(v1.MediaTypeImageIndex, idx)
}
