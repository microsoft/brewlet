package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	digest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"
)

func (s Store) referrerIndex(subject digest.Digest) (string, error) {
	if subject.Validate() != nil || subject.Algorithm() != digest.SHA256 {
		return "", fmt.Errorf("sha256 subject required")
	}
	return filepath.Join(s.Dir, "referrers", "sha256", subject.Encoded(), "index.json"), nil
}
func readReferrerIndex(file string) ([]v1.Descriptor, error) {
	b, e := os.ReadFile(file)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var index v1.Index
	if e = StrictJSON(b, &index); e != nil {
		return nil, e
	}
	if index.SchemaVersion != 2 || index.MediaType != v1.MediaTypeImageIndex {
		return nil, fmt.Errorf("invalid referrer index")
	}
	return index.Manifests, nil
}
func (s Store) Referrers(subject v1.Descriptor) ([]v1.Descriptor, error) {
	file, e := s.referrerIndex(subject.Digest)
	if e != nil {
		return nil, e
	}
	sidecar, e := readReferrerIndex(file)
	if e != nil {
		return nil, e
	}
	root, e := readReferrerIndex(filepath.Join(s.Dir, "index.json"))
	if e != nil {
		return nil, e
	}
	var result []v1.Descriptor
	seen := map[digest.Digest]v1.Descriptor{}
	for _, descriptor := range append(sidecar, root...) {
		if descriptor.Annotations[AnnotationReferrerSubject] != subject.Digest.String() {
			continue
		}
		if prior, ok := seen[descriptor.Digest]; ok {
			if !sameSubject(prior, descriptor) || prior.ArtifactType != descriptor.ArtifactType {
				return nil, fmt.Errorf("conflicting referrer descriptor")
			}
			continue
		}
		seen[descriptor.Digest] = descriptor
		result = append(result, descriptor)
	}
	slices.SortFunc(result, func(a, b v1.Descriptor) int {
		if a.Digest < b.Digest {
			return -1
		}
		if a.Digest > b.Digest {
			return 1
		}
		return 0
	})
	return result, nil
}
func (s Store) RecordReferrer(subject, descriptor v1.Descriptor) error {
	file, e := s.referrerIndex(subject.Digest)
	if e != nil {
		return e
	}
	if descriptor.Digest.Validate() != nil || descriptor.Size < 0 || descriptor.MediaType != v1.MediaTypeImageManifest {
		return fmt.Errorf("invalid referrer descriptor")
	}
	dir := filepath.Dir(file)
	if e = os.MkdirAll(dir, 0755); e != nil {
		return e
	}
	lock, e := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX); e != nil {
		return e
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	entries, e := readReferrerIndex(file)
	if e != nil {
		return e
	}
	for _, prior := range entries {
		if prior.Digest == descriptor.Digest {
			if !sameSubject(prior, descriptor) || prior.ArtifactType != descriptor.ArtifactType {
				return fmt.Errorf("conflicting referrer descriptor")
			}
			return nil
		}
	}
	descriptor.Annotations = cloneAnnotations(descriptor.Annotations)
	descriptor.Annotations[AnnotationReferrerSubject] = subject.Digest.String()
	entries = append(entries, descriptor)
	slices.SortFunc(entries, func(a, b v1.Descriptor) int {
		if a.Digest < b.Digest {
			return -1
		}
		if a.Digest > b.Digest {
			return 1
		}
		return 0
	})
	b, e := json.Marshal(v1.Index{Versioned: specs.Versioned{SchemaVersion: 2}, MediaType: v1.MediaTypeImageIndex, Manifests: entries})
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".index-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(f.Name(), file)
}
func cloneAnnotations(src map[string]string) map[string]string {
	out := make(map[string]string, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	return out
}
