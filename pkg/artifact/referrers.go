package artifact

import (
	"bytes"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	MediaTypeCycloneDX        = "application/vnd.cyclonedx+json"
	AnnotationReferrerSubject = "brewlet.sh/referrer-subject"
)

func sameSubject(a, b v1.Descriptor) bool {
	return a.MediaType == b.MediaType && a.Digest == b.Digest && a.Size == b.Size
}

func publishReferrer(store Store, subject v1.Descriptor, artifactType, documentType string, data []byte) (v1.Descriptor, error) {
	config, e := store.Put(v1.MediaTypeEmptyJSON, bytes.NewReader([]byte("{}")))
	if e != nil {
		return v1.Descriptor{}, e
	}
	layer, e := store.Put(documentType, bytes.NewReader(data))
	if e != nil {
		return v1.Descriptor{}, e
	}
	m := v1.Manifest{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageManifest, ArtifactType: artifactType, Subject: &subject, Config: config, Layers: []v1.Descriptor{layer}}
	d, e := store.PutJSON(v1.MediaTypeImageManifest, m)
	if e != nil {
		return d, e
	}
	d.ArtifactType = artifactType
	e = store.RecordReferrer(subject, d)
	return d, e
}
