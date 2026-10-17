package provisioner

import (
	"fmt"

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
	target, ok := mirrors[reference.Domain(parsed)]
	if !ok {
		return source, nil
	}
	return target + "/" + reference.Path(parsed) + "@" + canonical.Digest().String(), nil
}
