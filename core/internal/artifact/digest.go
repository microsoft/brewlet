// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifact

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Digest handling is a trust boundary: an OCI manifest, its descriptors, and any
// digest carried on a pod annotation are all attacker-controlled input, yet they
// are used to build host filesystem paths that the root shim bind-mounts into a
// container. Every digest is therefore required to be a canonical
// "sha256:<64 lowercase hex>" string before it can name a path or a blob, and
// every resolved path is independently confined to the store's blob directory.

const (
	digestPrefix    = "sha256:"
	digestHexLength = sha256.Size * 2
)

// ValidateDigest reports whether digest is a canonical SHA-256 OCI digest.
// Anything else — another algorithm, wrong length, uppercase hex, a path
// traversal sequence, or an empty component — is rejected rather than
// normalized, because a normalized digest would still name the wrong blob.
func ValidateDigest(digest string) error {
	hex, ok := strings.CutPrefix(digest, digestPrefix)
	if !ok || len(hex) != digestHexLength {
		return fmt.Errorf("invalid digest %q: must be canonical %s<%d lowercase hex>", digest, digestPrefix, digestHexLength)
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("invalid digest %q: must be canonical %s<%d lowercase hex>", digest, digestPrefix, digestHexLength)
		}
	}
	return nil
}

// DigestHex returns the validated hex component of a canonical SHA-256 digest.
// It is the only supported way to turn a digest into a path element.
func DigestHex(digest string) (string, error) {
	if err := ValidateDigest(digest); err != nil {
		return "", err
	}
	return digest[len(digestPrefix):], nil
}

// BlobPathIn maps a digest to its on-disk path in an OCI-style content store
// (<root>/blobs/sha256/<hex>). The digest is validated first, and the result is
// then independently confined to the blob directory so that no future change to
// the digest grammar can produce a path outside the store.
func BlobPathIn(root, digest string) (string, error) {
	hex, err := DigestHex(digest)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "blobs", "sha256")
	path := filepath.Join(dir, hex)
	rel, err := filepath.Rel(dir, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("blob path for digest %q escapes content store %s", digest, dir)
	}
	return path, nil
}

// VerifyBytes checks raw against the size and digest its descriptor declares.
// The size is only advisory (descriptors written by older tooling may omit it),
// but the digest is always verified so an absent size cannot disable the check.
// A negative size is a malformed descriptor rather than an absent one.
func VerifyBytes(desc Descriptor, raw []byte) error {
	if err := ValidateDigest(desc.Digest); err != nil {
		return err
	}
	if desc.Size < 0 {
		return fmt.Errorf("blob %s declares a negative size %d", desc.Digest, desc.Size)
	}
	if desc.Size > 0 && int64(len(raw)) != desc.Size {
		return fmt.Errorf("blob %s size mismatch: got %d, descriptor requires %d", desc.Digest, len(raw), desc.Size)
	}
	if got := digestOf(raw); got != desc.Digest {
		return fmt.Errorf("blob digest mismatch: got %s, descriptor requires %s", got, desc.Digest)
	}
	return nil
}

// VerifyFile checks the file at path against its descriptor without reading it
// into memory. It is used for payload blobs that are bind-mounted straight out
// of the content store — those bytes are executed by the workload, so they are
// verified even though they are never read by Brewlet itself.
func VerifyFile(desc Descriptor, path string) error {
	if err := ValidateDigest(desc.Digest); err != nil {
		return err
	}
	if desc.Size < 0 {
		return fmt.Errorf("blob %s declares a negative size %d", desc.Digest, desc.Size)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if desc.Size > 0 {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() != desc.Size {
			return fmt.Errorf("blob %s size mismatch: got %d, descriptor requires %d", desc.Digest, info.Size(), desc.Size)
		}
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := fmt.Sprintf("%s%x", digestPrefix, h.Sum(nil)); got != desc.Digest {
		return fmt.Errorf("blob digest mismatch: got %s, descriptor requires %s", got, desc.Digest)
	}
	return nil
}
