package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// VerifyGraph checks the identity of every referenced OCI blob, including local
// referrers, without applying Brewlet launch, rootfs, or layer-set policy. Layers
// are hashed as streams and are never decompressed or rewritten.
func VerifyGraph(ctx context.Context, store Store, target v1.Descriptor) error {
	if target.MediaType != v1.MediaTypeImageManifest && target.MediaType != v1.MediaTypeImageIndex {
		return fail("UnsupportedArtifact", "OCI manifest or index required")
	}
	sizes := map[digest.Digest]int64{}
	seen := map[string]bool{}
	var walk func(v1.Descriptor) error
	walk = func(d v1.Descriptor) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if d.MediaType == "" || d.Digest.Validate() != nil || d.Digest.Algorithm() != digest.SHA256 || d.Size < 0 || d.Size == math.MaxInt64 {
			return fmt.Errorf("invalid OCI graph descriptor")
		}
		if prior, ok := sizes[d.Digest]; ok && prior != d.Size {
			return fail("DigestMismatch", "conflicting graph descriptor size")
		}
		sizes[d.Digest] = d.Size
		key := d.Digest.String() + " " + d.MediaType
		if seen[key] {
			return nil
		}
		seen[key] = true
		if d.MediaType == v1.MediaTypeImageManifest || d.MediaType == v1.MediaTypeImageIndex {
			data, e := store.Read(d)
			if e != nil {
				return e
			}
			var object map[string]json.RawMessage
			if e = StrictJSON(data, &object); e != nil {
				return e
			}
			if d.MediaType == v1.MediaTypeImageIndex {
				var index v1.Index
				if e = json.Unmarshal(data, &index); e != nil {
					return e
				}
				if index.SchemaVersion != 2 || index.MediaType != "" && index.MediaType != d.MediaType {
					return fmt.Errorf("invalid OCI index envelope")
				}
				for _, child := range index.Manifests {
					if child.MediaType != v1.MediaTypeImageManifest && child.MediaType != v1.MediaTypeImageIndex {
						return fmt.Errorf("unsupported OCI index child media type")
					}
					if e = walk(child); e != nil {
						return e
					}
				}
			} else {
				var manifest v1.Manifest
				if e = json.Unmarshal(data, &manifest); e != nil {
					return e
				}
				if manifest.SchemaVersion != 2 || manifest.MediaType != "" && manifest.MediaType != d.MediaType {
					return fmt.Errorf("invalid OCI manifest envelope")
				}
				if e = walk(manifest.Config); e != nil {
					return e
				}
				for _, layer := range manifest.Layers {
					if e = walk(layer); e != nil {
						return e
					}
				}
			}
			referrers, e := store.Referrers(d)
			if e != nil {
				return e
			}
			for _, ref := range referrers {
				if e = walk(ref); e != nil {
					return e
				}
			}
			materials, e := CompositionMaterials(store, d)
			if e != nil {
				return e
			}
			for _, material := range materials {
				if e = walk(material); e != nil {
					return e
				}
			}
			return nil
		}
		reader, e := store.Open(d)
		if e != nil {
			return e
		}
		defer reader.Close()
		hash := digest.SHA256.Digester()
		n, e := io.Copy(hash.Hash(), io.LimitReader(contextReader{ctx, reader}, d.Size+1))
		if e != nil {
			return e
		}
		if n != d.Size || hash.Digest() != d.Digest {
			return fail("DigestMismatch", "OCI blob identity mismatch")
		}
		return nil
	}
	return walk(target)
}
