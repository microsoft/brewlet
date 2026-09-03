// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestValidateDigestRejectsNonCanonicalDigests(t *testing.T) {
	hex64 := strings.Repeat("a", 64)
	cases := []struct {
		name   string
		digest string
	}{
		{"parent traversal", "sha256:../../../../../.."},
		{"short traversal", "sha256:.."},
		{"absolute path", "sha256:/etc/shadow"},
		{"embedded separator", "sha256:" + strings.Repeat("a", 32) + "/" + strings.Repeat("a", 31)},
		{"embedded null byte", "sha256:" + strings.Repeat("a", 63) + "\x00"},
		{"one hex digit short", "sha256:" + strings.Repeat("a", 63)},
		{"one hex digit long", "sha256:" + strings.Repeat("a", 65)},
		{"uppercase hex", "sha256:" + strings.ToUpper(hex64)},
		{"mixed-case hex", "sha256:A" + strings.Repeat("a", 63)},
		{"non-hex character", "sha256:g" + strings.Repeat("a", 63)},
		{"unsupported algorithm sha512", "sha512:" + hex64},
		{"unsupported algorithm md5", "md5:" + strings.Repeat("a", 32)},
		{"missing algorithm prefix", hex64},
		{"empty digest", ""},
		{"empty hex component", "sha256:"},
		{"leading whitespace", " sha256:" + hex64},
		{"trailing whitespace", "sha256:" + hex64 + " "},
		{"repeated prefix", "sha256:sha256:" + hex64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateDigest(tc.digest); err == nil {
				t.Fatalf("ValidateDigest(%q) = nil, want an error", tc.digest)
			}
			if _, err := DigestHex(tc.digest); err == nil {
				t.Fatalf("DigestHex(%q) = nil error, want an error", tc.digest)
			}
			if got, err := BlobPathIn(t.TempDir(), tc.digest); err == nil {
				t.Fatalf("BlobPathIn(%q) = %q, want an error and no path", tc.digest, got)
			}
		})
	}
}

func TestValidateDigestAcceptsCanonicalDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0123456789abcdef", 4)
	if err := ValidateDigest(digest); err != nil {
		t.Fatalf("ValidateDigest(%q) = %v, want nil", digest, err)
	}
	hex, err := DigestHex(digest)
	if err != nil {
		t.Fatalf("DigestHex: %v", err)
	}
	if hex != digest[len("sha256:"):] {
		t.Errorf("DigestHex = %q, want %q", hex, digest[len("sha256:"):])
	}
}

func TestBlobPathInResolvesUnderTheContentStore(t *testing.T) {
	root := t.TempDir()
	got, err := BlobPathIn(root, validDigest)
	if err != nil {
		t.Fatalf("BlobPathIn: %v", err)
	}
	want := filepath.Join(root, "blobs", "sha256", validDigest[len("sha256:"):])
	if got != want {
		t.Errorf("BlobPathIn = %q, want %q", got, want)
	}
}

// TestBlobPathInNeverEscapesContentStore is the property that makes the root
// shim safe: no descriptor digest, however hostile, may resolve to a host path
// outside the store's blob directory.
func TestBlobPathInNeverEscapesContentStore(t *testing.T) {
	root := t.TempDir()
	hostile := []string{
		"sha256:../../../../../..",
		"sha256:..",
		"sha256:../" + strings.Repeat("a", 61),
		"sha256:/",
		"sha256:" + strings.Repeat("../", 21) + "a",
		"../../../../etc",
		"sha256:.",
	}
	for _, digest := range hostile {
		got, err := BlobPathIn(root, digest)
		if err == nil {
			t.Errorf("BlobPathIn(%q) = %q, want an error", digest, got)
			continue
		}
		if got != "" {
			t.Errorf("BlobPathIn(%q) returned path %q alongside an error", digest, got)
		}
	}
}

func FuzzBlobPathInStaysUnderContentStore(f *testing.F) {
	f.Add("sha256:" + strings.Repeat("a", 64))
	f.Add("sha256:../../../../../..")
	f.Add("sha256:")
	f.Add("")
	f.Add("sha256:/etc/shadow")
	f.Add("sha512:" + strings.Repeat("a", 128))

	root := f.TempDir()
	blobs := filepath.Join(root, "blobs", "sha256") + string(os.PathSeparator)
	f.Fuzz(func(t *testing.T, digest string) {
		got, err := BlobPathIn(root, digest)
		if err != nil {
			if got != "" {
				t.Fatalf("BlobPathIn(%q) returned path %q alongside error %v", digest, got, err)
			}
			return
		}
		if !strings.HasPrefix(got, blobs) {
			t.Fatalf("BlobPathIn(%q) = %q, which escapes %q", digest, got, blobs)
		}
		if got != filepath.Clean(got) {
			t.Fatalf("BlobPathIn(%q) = %q, which is not a cleaned path", digest, got)
		}
	})
}

func TestVerifyBytesRejectsMismatchedContent(t *testing.T) {
	raw := []byte("PK\x03\x04 jar")
	desc := Descriptor{Digest: digestOf(raw), Size: int64(len(raw))}
	if err := VerifyBytes(desc, raw); err != nil {
		t.Fatalf("VerifyBytes on matching content: %v", err)
	}
	if err := VerifyBytes(desc, []byte("PK\x03\x04 tampered")); err == nil {
		t.Error("VerifyBytes accepted content that does not match the descriptor digest")
	}
	if err := VerifyBytes(Descriptor{Digest: "sha256:../../.."}, raw); err == nil {
		t.Error("VerifyBytes accepted a non-canonical descriptor digest")
	}
}

// TestVerifyFileRejectsMismatchedContent covers the blobs that are bind-mounted
// rather than read: their bytes are only ever checked here.
func TestVerifyFileRejectsMismatchedContent(t *testing.T) {
	raw := []byte("PK\x03\x04 jar payload")
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	desc := Descriptor{Digest: digestOf(raw), Size: int64(len(raw))}
	if err := VerifyFile(desc, path); err != nil {
		t.Fatalf("VerifyFile on matching content: %v", err)
	}
	if err := VerifyFile(Descriptor{Digest: digestOf([]byte("other"))}, path); err == nil {
		t.Error("VerifyFile accepted content that does not match the descriptor digest")
	}
	if err := VerifyFile(Descriptor{Digest: desc.Digest, Size: desc.Size + 1}, path); err == nil {
		t.Error("VerifyFile accepted a size that does not match the descriptor")
	}
	if err := VerifyFile(Descriptor{Digest: "sha256:.."}, path); err == nil {
		t.Error("VerifyFile accepted a non-canonical descriptor digest")
	}
}
