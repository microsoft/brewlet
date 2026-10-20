package artifact

import (
	"context"
	"testing"

	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestCompositionReceiptBindsThisInvocationEvenWhenOutputBytesAreReused(t *testing.T) {
	f := newReleaseFixture(t)
	first, firstEvidence, err := Compose(context.Background(), f.store, f.options())
	must(t, err)
	index, err := f.store.PutJSON(v1.MediaTypeImageIndex, v1.Index{Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{f.app}})
	must(t, err)
	options := f.options()
	options.Application = index
	second, secondEvidence, err := Compose(context.Background(), f.store, options)
	must(t, err)
	if first.Digest != second.Digest || secondEvidence.Predicate.ApplicationRoot.Digest != index.Digest ||
		secondEvidence.Attestation.Digest == firstEvidence.Attestation.Digest {
		t.Fatal("composition receipt selected a different invocation's provenance")
	}
}

func TestCompositionRejectsAmbiguousOrMismatchedIndexSelection(t *testing.T) {
	for name, mutate := range map[string]func(*v1.Index){
		"ambiguous":           func(index *v1.Index) { index.Manifests = append(index.Manifests, index.Manifests[0]) },
		"descriptor-platform": func(index *v1.Index) { index.Manifests[0].Platform = &v1.Platform{OS: "linux", Architecture: "amd64"} },
		"descriptor-size":     func(index *v1.Index) { index.Manifests[0].Size++ },
		"missing-platform":    func(index *v1.Index) { index.Manifests[0].Platform = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := newReleaseFixture(t)
			index := v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: []v1.Descriptor{f.app}}
			mutate(&index)
			target, err := f.store.PutJSON(v1.MediaTypeImageIndex, index)
			must(t, err)
			options := f.options()
			options.Application = target
			if _, _, err = Compose(context.Background(), f.store, options); err == nil {
				t.Fatal("invalid index membership accepted")
			}
		})
	}
}
