package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	digest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func ReferrerTag(subject v1.Descriptor, referrer v1.Descriptor) string {
	typ := digest.FromString(referrer.ArtifactType).Encoded()
	return "sha256-" + subject.Digest.Encoded() + "." + typ[:12] + "." + referrer.Digest.Encoded()[:24]
}

var paginationLink = regexp.MustCompile(`<([^>]+)>([^,]*)`)
var nextRelation = regexp.MustCompile(`(?:^|;)\s*rel\s*=\s*"?next"?(?:\s*;|\s*$)`)

func nextPage(current *url.URL, header string) (*url.URL, error) {
	for _, match := range paginationLink.FindAllStringSubmatch(header, -1) {
		if nextRelation.MatchString(match[2]) {
			relative, e := url.Parse(match[1])
			if e != nil {
				return nil, e
			}
			return current.ResolveReference(relative), nil
		}
	}
	return nil, nil
}
func (r *Registry) Referrers(ctx context.Context, subject v1.Descriptor) ([]v1.Descriptor, error) {
	if subject.Digest.Validate() != nil || subject.Digest.Algorithm() != digest.SHA256 {
		return nil, fmt.Errorf("sha256 subject required")
	}
	page := r.endpoint("referrers", subject.Digest.String())
	var candidates []v1.Descriptor
	invalidAttestation := false
	visited := map[string]bool{}
	for page != nil {
		if visited[page.String()] || len(visited) >= 1000 {
			return nil, fmt.Errorf("referrer pagination cycle/limit")
		}
		visited[page.String()] = true
		resp, e := r.do(ctx, "GET", page, nil, "")
		if e != nil {
			return nil, e
		}
		if resp.StatusCode == 404 || resp.StatusCode == 405 {
			resp.Body.Close()
			if len(visited) != 1 {
				return nil, fmt.Errorf("native referrers disappeared during pagination")
			}
			return r.fallbackReferrers(ctx, subject)
		}
		if e = status(resp, 200); e != nil {
			resp.Body.Close()
			return nil, e
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
		header := resp.Header.Get("Link")
		resp.Body.Close()
		if e != nil {
			return nil, e
		}
		if len(b) > 16<<20 {
			return nil, fmt.Errorf("referrer page too large")
		}
		var index v1.Index
		if e = StrictJSON(b, &index); e != nil {
			return nil, e
		}
		if index.SchemaVersion != 2 || index.MediaType != v1.MediaTypeImageIndex {
			return nil, fmt.Errorf("invalid native referrer index")
		}
		for _, d := range index.Manifests {
			if d.MediaType != v1.MediaTypeImageManifest || d.Digest.Validate() != nil || d.Size < 0 {
				if d.ArtifactType == MediaTypeAttestation {
					invalidAttestation = true
					continue
				}
				return nil, fmt.Errorf("invalid native referrer descriptor")
			}
			candidates = append(candidates, d)
		}
		page, e = nextPage(page, header)
		if e != nil {
			return nil, e
		}
	}
	if invalidAttestation && !hasAttestation(candidates) {
		return nil, fmt.Errorf("provenance discovered without a usable candidate descriptor")
	}
	return uniqueReferrers(candidates)
}
func (r *Registry) fallbackReferrers(ctx context.Context, subject v1.Descriptor) ([]v1.Descriptor, error) {
	page := r.endpoint("tags", "list")
	q := page.Query()
	q.Set("n", "100")
	page.RawQuery = q.Encode()
	prefix := "sha256-" + subject.Digest.Encoded() + "."
	known := map[string]string{digest.FromString(MediaTypeCycloneDX).Encoded()[:12]: MediaTypeCycloneDX, digest.FromString(MediaTypeAttestation).Encoded()[:12]: MediaTypeAttestation}
	var candidates []v1.Descriptor
	invalidAttestation := false
	visited := map[string]bool{}
	for page != nil {
		if visited[page.String()] || len(visited) >= 1000 {
			return nil, fmt.Errorf("tag pagination cycle/limit")
		}
		visited[page.String()] = true
		resp, e := r.do(ctx, "GET", page, nil, "")
		if e != nil {
			return nil, e
		}
		if e = status(resp, 200); e != nil {
			resp.Body.Close()
			return nil, e
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, 16<<20+1))
		header := resp.Header.Get("Link")
		resp.Body.Close()
		if e != nil {
			return nil, e
		}
		if len(b) > 16<<20 {
			return nil, fmt.Errorf("tag page too large")
		}
		var list struct {
			Name string   `json:"name"`
			Tags []string `json:"tags"`
		}
		if e = json.Unmarshal(b, &list); e != nil {
			return nil, e
		}
		for _, tag := range list.Tags {
			if !strings.HasPrefix(tag, prefix) {
				continue
			}
			suffix := strings.Split(strings.TrimPrefix(tag, prefix), ".")
			if len(suffix) != 2 {
				continue
			}
			typ, ok := known[suffix[0]]
			if !ok || len(suffix[1]) != 24 {
				continue
			}
			descriptor, _, e := r.fetchManifest(ctx, tag)
			if e != nil {
				if typ == MediaTypeAttestation {
					invalidAttestation = true
					continue
				}
				return nil, e
			}
			descriptor.ArtifactType = typ
			if descriptor.MediaType != v1.MediaTypeImageManifest || !strings.HasPrefix(descriptor.Digest.Encoded(), suffix[1]) {
				if typ == MediaTypeAttestation {
					invalidAttestation = true
					continue
				}
				return nil, fmt.Errorf("fallback referrer digest tag mismatch")
			}
			candidates = append(candidates, descriptor)
		}
		page, e = nextPage(page, header)
		if e != nil {
			return nil, e
		}
	}
	if invalidAttestation && !hasAttestation(candidates) {
		return nil, fmt.Errorf("provenance discovered without a usable candidate descriptor")
	}
	return uniqueReferrers(candidates)
}
func hasAttestation(candidates []v1.Descriptor) bool {
	for _, d := range candidates {
		if d.ArtifactType == MediaTypeAttestation {
			return true
		}
	}
	return false
}
func uniqueReferrers(input []v1.Descriptor) ([]v1.Descriptor, error) {
	seen := map[digest.Digest]v1.Descriptor{}
	var result []v1.Descriptor
	for _, d := range input {
		if prior, ok := seen[d.Digest]; ok {
			if !sameSubject(prior, d) || prior.ArtifactType != d.ArtifactType {
				return nil, fmt.Errorf("conflicting discovered referrer descriptor")
			}
			continue
		}
		seen[d.Digest] = d
		result = append(result, d)
	}
	return result, nil
}
