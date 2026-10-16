package artifact

import (
	"encoding/json"
	"fmt"
	"os"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// A digest selects immutable CAS metadata even after index.json rotates. Size
// comes from the local regular blob and its digest is verified before its
// explicit OCI media type is used.
func (s Store) targetDigest(value string) (v1.Descriptor, error) {
	descriptor := v1.Descriptor{Digest: digest.Digest(value)}
	file, e := s.blobPath(descriptor.Digest)
	if e != nil {
		return descriptor, e
	}
	info, e := os.Lstat(file)
	if e != nil {
		return descriptor, e
	}
	if !info.Mode().IsRegular() {
		return descriptor, fmt.Errorf("pinned OCI metadata blob must be regular")
	}
	descriptor.Size = info.Size()
	b, e := s.Read(descriptor)
	if e != nil {
		return descriptor, e
	}
	var header struct {
		MediaType    string `json:"mediaType"`
		ArtifactType string `json:"artifactType"`
	}
	if e = json.Unmarshal(b, &header); e != nil {
		return descriptor, e
	}
	if header.MediaType != v1.MediaTypeImageManifest && header.MediaType != v1.MediaTypeImageIndex {
		return descriptor, fail("UnsupportedArtifact", "pinned OCI manifest or index required")
	}
	descriptor.MediaType = header.MediaType
	descriptor.ArtifactType = header.ArtifactType
	return descriptor, nil
}
