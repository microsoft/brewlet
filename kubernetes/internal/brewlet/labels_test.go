// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package brewlet

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

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
		{"free-form message without a reason code", "", "some older free-form failure", "some older free-form failure"},
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

func TestRuntimeReadyRequiresStartupTaintRelease(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelRuntimeReady: ValueReady}}}
	if !RuntimeReady(node) || HasStartupTaint(node) {
		t.Fatal("labelled node without startup taint must be ready")
	}
	node.Spec.Taints = []corev1.Taint{{Key: StartupTaintKey, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}}
	if RuntimeReady(node) || !HasStartupTaint(node) {
		t.Fatal("node still carrying the startup taint must not be ready")
	}
	node.Spec.Taints = nil
	node.Labels = nil
	if RuntimeReady(node) {
		t.Fatal("unlabelled node must not be ready")
	}
}
