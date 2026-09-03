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
