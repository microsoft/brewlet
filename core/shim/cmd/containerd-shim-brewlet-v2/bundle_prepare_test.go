// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"testing"

	kcruntime "github.com/microsoft/brewlet/internal/runtime"
)

func TestImageConfigProcessIdentity(t *testing.T) {
	uid := uint32(0)
	gid := uint32(1234)
	cases := []struct {
		name string
		cfg  imageConfig
		want kcruntime.ProcessIdentity
	}{
		{
			name: "secure defaults",
			want: kcruntime.DefaultProcessIdentity(),
		},
		{
			name: "trusted explicit values",
			cfg:  imageConfig{ProcessUID: &uid, ProcessGID: &gid},
			want: kcruntime.ProcessIdentity{UID: uid, GID: gid},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.processIdentity(); got != tc.want {
				t.Fatalf("process identity = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// decodeJVMArgs is the node-side half of the brewlet.sh/jvm-args contract: the
// operator stamps a JSON array so argument boundaries survive delivery, and the
// shim appends the decoded args to the launcher argv ahead of the entrypoint.
func TestDecodeJVMArgs(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{"absent annotation", "", nil, false},
		{"whitespace only", "   ", nil, false},
		{"empty array", "[]", []string{}, false},
		{"single arg", `["-Xmx1g"]`, []string{"-Xmx1g"}, false},
		{"order preserved", `["-Xms1g","-Xmx1g"]`, []string{"-Xms1g", "-Xmx1g"}, false},
		{
			// The reason the wire form is a JSON array rather than a
			// whitespace-joined string.
			"arg containing spaces stays one element",
			`["-XX:OnOutOfMemoryError=kill -9 %p"]`,
			[]string{`-XX:OnOutOfMemoryError=kill -9 %p`},
			false,
		},
		{"surrounding whitespace tolerated", "  [\"-Xmx1g\"]  ", []string{"-Xmx1g"}, false},

		// A malformed annotation must fail the launch, not silently drop the
		// platform team's tuning.
		{"not JSON", "-Xmx1g", nil, true},
		{"JSON string not array", `"-Xmx1g"`, nil, true},
		{"array of non-strings", `[1,2]`, nil, true},
		{"object", `{"a":"b"}`, nil, true},
		{"truncated", `["-Xmx1g"`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeJVMArgs(tc.raw)
			if (err != nil) != tc.wantErr {
				t.Fatalf("decodeJVMArgs(%q) error = %v, wantErr %v", tc.raw, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("decodeJVMArgs(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("decodeJVMArgs(%q) = %q, want %q", tc.raw, got, tc.want)
				}
			}
		})
	}
}
