// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifact

import (
	"archive/tar"
	"bytes"
	"testing"
)

func dependencyTarFixture(t *testing.T, content []byte) ([]byte, DependencyLock) {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	if err := writer.WriteHeader(&tar.Header{Name: "a.jar", Size: int64(len(content)), Mode: 0o644, Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes(), DependencyLock{
		SchemaVersion: 1,
		Artifacts: []DependencyLockEntry{{
			GroupID: "g", ArtifactID: "a", Version: "1", Type: "jar", Scope: "runtime",
			FileName: "a.jar", SHA256: hexDigest(content),
		}},
	}
}

func TestDependencyTarRequiresCompleteFraming(t *testing.T) {
	valid, lock := dependencyTarFixture(t, []byte("a"))
	cases := []struct {
		name   string
		change func([]byte) []byte
	}{
		{"checksum", func(raw []byte) []byte { raw[100] ^= 1; return raw }},
		{"partial header", func(raw []byte) []byte { return raw[:511] }},
		{"partial padding", func(raw []byte) []byte { return raw[:513] }},
		{"nonzero padding", func(raw []byte) []byte { raw[513] = 1; return raw }},
		{"no end blocks", func(raw []byte) []byte { return raw[:1024] }},
		{"one end block", func(raw []byte) []byte { return raw[:1536] }},
		{"nonzero second end block", func(raw []byte) []byte { raw[len(raw)-1] = 1; return raw }},
		{"data after end blocks", func(raw []byte) []byte { return append(raw, append([]byte{1}, make([]byte, 511)...)...) }},
		{"partial trailing block", func(raw []byte) []byte { return append(raw, 0) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			raw := test.change(bytes.Clone(valid))
			if err := validateDependencyTar(raw, lock); err == nil {
				t.Fatal("malformed archive accepted despite a matching locked file")
			}
		})
	}
	for _, raw := range [][]byte{valid, append(bytes.Clone(valid), make([]byte, 512)...)} {
		if err := validateDependencyTar(raw, lock); err != nil {
			t.Fatalf("valid archive rejected: %v", err)
		}
	}
}

func TestDependencyTarDoesNotMistakeZeroPayloadForEndMarkers(t *testing.T) {
	raw, lock := dependencyTarFixture(t, make([]byte, 1536))
	if err := validateDependencyTar(raw, lock); err != nil {
		t.Fatal(err)
	}
	if err := validateDependencyTar(raw[:len(raw)-1024], lock); err == nil {
		t.Fatal("zero-filled payload was mistaken for archive terminators")
	}
}

func TestDependencyTarSourceIsCanonicalizedBeforePublication(t *testing.T) {
	_, lock := dependencyTarFixture(t, []byte("a"))
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: "a.jar", Size: 1, Mode: 0o600, Format: tar.FormatGNU}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buffer.Bytes()
	if err := validateDependencyTar(raw, lock); err == nil {
		t.Fatal("non-USTAR bundle layer accepted")
	}
	if err := validateDependencyTarRecords(raw, lock, false); err != nil {
		t.Fatalf("valid source tar rejected before normalization: %v", err)
	}
	canonical, err := canonicalDependencyTar(raw, lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDependencyTar(canonical, lock); err != nil {
		t.Fatalf("normalized USTAR output rejected: %v", err)
	}
}
