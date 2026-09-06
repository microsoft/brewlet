// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package brewlet

import "testing"

// brewlet.sh/provision-error carries a stable reason CODE and
// brewlet.sh/provision-error-message the human detail (§14.1). Rendering them
// for an event or status message must keep the code first and greppable, and
// must never emit a dangling separator when one side is absent.
func TestFormatProvisionError(t *testing.T) {
	cases := []struct {
		name    string
		code    string
		message string
		want    string
	}{
		{"both", "cgroup-v2-required", "node is cgroup v1 only", "cgroup-v2-required: node is cgroup v1 only"},
		{"code only", "rollback-failed", "", "rollback-failed"},
		{"code only, blank message", "rollback-failed", "   ", "rollback-failed"},
		// A node annotated by an older provisioner has no message annotation.
		{"legacy prose in the code slot", "", "some older free-form failure", "some older free-form failure"},
		{"neither", "", "", ""},
		{"whitespace trimmed", "  jdk-install-failed  ", "  copy failed  ", "jdk-install-failed: copy failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatProvisionError(tc.code, tc.message); got != tc.want {
				t.Errorf("FormatProvisionError(%q, %q) = %q, want %q", tc.code, tc.message, got, tc.want)
			}
		})
	}
}
