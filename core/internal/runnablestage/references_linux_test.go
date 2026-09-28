// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package runnablestage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type fakeMetadata struct {
	nss     []string
	targets map[string][]ocispec.Descriptor
	infos   map[string][]content.Info
	blobs   map[string]map[digest.Digest][]byte
	fail    string
	visited []string
}

func (m *fakeMetadata) namespaces(context.Context) ([]string, error) {
	if m.fail == "namespaces" {
		return nil, os.ErrPermission
	}
	return m.nss, nil
}
func (m *fakeMetadata) images(_ context.Context, ns string) ([]ocispec.Descriptor, error) {
	m.visited = append(m.visited, "images:"+ns)
	if m.fail == "images:"+ns {
		return nil, os.ErrPermission
	}
	return m.targets[ns], nil
}
func (m *fakeMetadata) contents(_ context.Context, ns string, fn func(content.Info) error) error {
	m.visited = append(m.visited, "content:"+ns)
	if m.fail == "content:"+ns {
		return os.ErrPermission
	}
	for _, info := range m.infos[ns] {
		if err := fn(info); err != nil {
			return err
		}
	}
	return nil
}
func (m *fakeMetadata) read(_ context.Context, ns string, d ocispec.Descriptor) ([]byte, error) {
	if raw, ok := m.blobs[ns][d.Digest]; ok {
		return raw, nil
	}
	return nil, os.ErrNotExist
}

func descriptor(char string) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageManifest, Digest: digest.Digest("sha256:" + strings.Repeat(char, 64))}
}

func indexDescriptor(t *testing.T, children ...ocispec.Descriptor) (ocispec.Descriptor, []byte) {
	t.Helper()
	raw, err := json.Marshal(ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: ocispec.MediaTypeImageIndex, Manifests: children,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: digest.FromBytes(raw), Size: int64(len(raw))}, raw
}

func TestReferencesAllNamespacesAndIndexes(t *testing.T) {
	child := descriptor("a")
	nested, nestedRaw := indexDescriptor(t, child)
	index, raw := indexDescriptor(t, nested, descriptor("b"))
	store := &fakeMetadata{
		nss: []string{"default", "k8s.io", "other"},
		targets: map[string][]ocispec.Descriptor{
			"default": {descriptor("c")},
			"k8s.io":  {index},
		},
		infos: map[string][]content.Info{
			"other": {{Digest: descriptor("d").Digest, Labels: map[string]string{"containerd.io/gc.ref.content.child": descriptor("e").Digest.String()}}},
		},
		blobs: map[string]map[digest.Digest][]byte{"k8s.io": {index.Digest: raw, nested.Digest: nestedRaw}},
	}
	refs, err := references(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []ocispec.Descriptor{child, descriptor("b"), descriptor("c"), descriptor("d"), descriptor("e"), index, nested} {
		if !refs[d.Digest.Encoded()] {
			t.Errorf("missing reference %s", d.Digest)
		}
	}
	if len(store.visited) != 6 {
		t.Fatalf("did not query both services in every namespace: %v", store.visited)
	}
}

func TestReferencesFailClosed(t *testing.T) {
	for _, failure := range []string{"namespaces", "images:two", "content:two", "index-missing", "index-malformed", "index-corrupt", "child-index-missing", "unknown-type", "invalid-digest", "invalid-label"} {
		t.Run(failure, func(t *testing.T) {
			index, raw := indexDescriptor(t, descriptor("a"))
			store := &fakeMetadata{
				nss:     []string{"one", "two"},
				targets: map[string][]ocispec.Descriptor{"one": {descriptor("b")}, "two": {index}},
				blobs:   map[string]map[digest.Digest][]byte{"two": {index.Digest: raw}},
			}
			switch failure {
			case "index-missing":
				store.blobs = nil
			case "index-malformed":
				raw = []byte(`{"schemaVersion":2}`)
				index.Digest = digest.FromBytes(raw)
				store.targets["two"] = []ocispec.Descriptor{index}
				store.blobs["two"][index.Digest] = raw
			case "index-corrupt":
				store.blobs["two"][index.Digest] = []byte(`{}`)
			case "child-index-missing":
				child := descriptor("a")
				child.MediaType = ocispec.MediaTypeImageIndex
				index, raw = indexDescriptor(t, child)
				store.targets["two"] = []ocispec.Descriptor{index}
				store.blobs["two"][index.Digest] = raw
			case "unknown-type":
				store.targets["two"][0].MediaType = "application/unknown"
			case "invalid-digest":
				store.targets["two"][0].Digest = "sha256:../bad"
			case "invalid-label":
				store.infos = map[string][]content.Info{"two": {{Digest: descriptor("c").Digest, Labels: map[string]string{"containerd.io/gc.ref.content.child": "broken"}}}}
			default:
				store.fail = failure
			}
			if refs, err := references(context.Background(), store); err == nil || refs != nil {
				t.Fatalf("returned partial references on error: %v, %v", refs, err)
			}
		})
	}
}

func TestLateNamespaceFailurePreventsAllDeletion(t *testing.T) {
	root := t.TempDir()
	path := createStage(t, root, strings.Repeat("a", 64), 2*DefaultMinAge)
	store := &fakeMetadata{nss: []string{"one", "two"}, fail: "content:two"}
	deps := fakeDependencies(t, root)
	deps.references = func(ctx context.Context, _ string) (map[string]bool, error) { return references(ctx, store) }
	if _, err := reap(context.Background(), "", Options{Root: root, MinAge: DefaultMinAge}, deps); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("expected namespace failure, got %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(fmt.Errorf("deleted before completing namespace scan: %w", err))
	}
}
