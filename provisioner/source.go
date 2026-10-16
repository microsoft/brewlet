package provisioner

import (
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/microsoft/brewlet/kubernetes/policy"
	digest "github.com/opencontainers/go-digest"
)

func SourceReference(source string, mirrors map[string]string, allowed []string) (string, error) {
	if err := policy.Source(source); err != nil {
		return "", err
	}
	if err := policy.MirrorPolicy(mirrors, allowed); err != nil {
		return "", err
	}
	parsed, err := reference.ParseNormalizedNamed(source)
	if err != nil {
		return "", err
	}
	canonical, ok := parsed.(reference.Canonical)
	if !ok || canonical.String() != source || canonical.Digest().Algorithm() != digest.SHA256 || len(canonical.Digest().Encoded()) != 64 {
		return "", fmt.Errorf("source must be a canonical sha256 digest reference")
	}
	if _, tagged := parsed.(reference.Tagged); tagged {
		return "", fmt.Errorf("source tags forbidden")
	}
	allow := map[string]bool{}
	for _, host := range allowed {
		if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/@ \t\r\n") || strings.Contains(host, "://") || allow[host] {
			return "", fmt.Errorf("invalid or duplicate mirror allowlist host")
		}
		allow[host] = true
	}
	for upstream, target := range mirrors {
		if upstream == "" || strings.TrimSpace(upstream) != upstream || strings.ContainsAny(upstream, "/@ \t\r\n") {
			return "", fmt.Errorf("invalid source mirror host")
		}
		test, err := reference.ParseNormalizedNamed(target + "/probe")
		if err != nil || test.String() != target+"/probe" {
			return "", fmt.Errorf("invalid mirror repository prefix")
		}
		if !allow[reference.Domain(test)] || reference.Domain(test) == upstream {
			return "", fmt.Errorf("unapproved or self mirror")
		}
	}
	target, ok := mirrors[reference.Domain(parsed)]
	if !ok {
		return source, nil
	}
	return target + "/" + reference.Path(parsed) + "@" + canonical.Digest().String(), nil
}
