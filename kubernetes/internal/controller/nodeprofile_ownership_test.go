// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/jsonpath"
)

func templateMatchesNode(spec *corev1.PodSpec, node *corev1.Node) bool {
	if spec.NodeName != "" && spec.NodeName != node.Name {
		return false
	}
	for key, value := range spec.NodeSelector {
		if node.Labels[key] != value {
			return false
		}
	}
	if spec.Affinity == nil || spec.Affinity.NodeAffinity == nil ||
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}
	for _, term := range spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		matches := len(term.MatchExpressions)+len(term.MatchFields) > 0
		for _, req := range term.MatchExpressions {
			value, exists := node.Labels[req.Key]
			found := false
			for _, candidate := range req.Values {
				found = found || value == candidate
			}
			switch req.Operator {
			case corev1.NodeSelectorOpIn:
				matches = matches && exists && found
			case corev1.NodeSelectorOpNotIn:
				matches = matches && (!exists || !found)
			case corev1.NodeSelectorOpExists:
				matches = matches && exists
			case corev1.NodeSelectorOpDoesNotExist:
				matches = matches && !exists
			}
		}
		for _, req := range term.MatchFields {
			if req.Key == "metadata.name" && req.Operator == corev1.NodeSelectorOpIn {
				found := false
				for _, candidate := range req.Values {
					found = found || node.Name == candidate
				}
				matches = matches && found
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func TestNodeProfileSelectorsExcludeFutureOverlaps(t *testing.T) {
	for _, tc := range []struct {
		name, aKey, bKey string
		aNames, bNames   []string
		conflict         bool
	}{
		{"two-defaults", "", "", nil, nil, true},
		{"different-keys", "agentpool", "example.com/pool", []string{"a"}, []string{"b"}, true},
		{"auto-explicit", "", "agentpool", []string{"a"}, []string{"b"}, true},
		{"overlapping-auto", "", "", []string{"a"}, []string{"a"}, true},
		{"disjoint-auto", "", "", []string{"a"}, []string{"b"}, false},
		{"disjoint-explicit", "agentpool", "agentpool", []string{"a"}, []string{"b"}, false},
		{"default-named-cross-key", "example.com/irrelevant", "agentpool", nil, []string{"a"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := profileNamed("a", tc.aNames, jdk("temurin", 21))
			b := profileNamed("b", tc.bNames, jdk("temurin", 21))
			a.Spec.NodePool.Key, b.Spec.NodePool.Key = tc.aKey, tc.bKey
			if err := ValidateNoPoolConflicts(&a, []nodev1alpha1.NodeProfile{b}); (err != nil) != tc.conflict {
				t.Fatalf("conflict=%v, want %v: %v", err != nil, tc.conflict, err)
			}
		})
	}
}

func TestNodeProfileDefaultExcludesNamedProfileOwnKey(t *testing.T) {
	a := profileNamed("default", nil, jdk("temurin", 21))
	a.Spec.NodePool.Key = "example.com/default"
	a.Spec.NodePool.IncludeControlPlane = true
	b := profileNamed("named", []string{"workers"}, jdk("temurin", 21))
	b.Spec.NodePool.Key = "agentpool"
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "named", UID: types.UID("uid-named"), Labels: map[string]string{"agentpool": "workers"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fallback", UID: types.UID("uid-fallback")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "named-control-plane", UID: types.UID("uid-cp"), Labels: map[string]string{"agentpool": "workers", "node-role.kubernetes.io/control-plane": ""}}},
	}
	got := desiredTargets(&a, []nodev1alpha1.NodeProfile{a, b}, nodes)
	if len(got) != 1 || got[0].Name != "fallback" {
		t.Fatalf("default targets = %+v, want only fallback", got)
	}
}

func TestNodeProfileClaimAffinityNeverMatchesUnrecordedNodes(t *testing.T) {
	p := profileNamed("owner", []string{"pool"}, jdk("temurin", 21))
	p.UID = types.UID("profile-uid")
	p.Status.Targets = []nodev1alpha1.NodeTarget{{Name: "authorized", UID: types.UID("node-uid"), Claimed: true}}
	ds := buildProfileDaemonSet(testConfig(), &p, "agentpool", nil)
	for _, tc := range []struct {
		name, uid, owner string
		want             bool
	}{
		{"authorized", "node-uid", "profile-uid", true},
		{"autoscaled", "node-uid", "profile-uid", false},
		{"authorized", "replacement-uid", "profile-uid", false},
		{"authorized", "node-uid", "another-owner", false},
	} {
		node := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Labels: map[string]string{
			"agentpool": "pool", brewlet.LabelNodeOwner: tc.owner, brewlet.LabelNodeIdentity: tc.uid,
		}}}
		if got := templateMatchesNode(&ds.Spec.Template.Spec, &node); got != tc.want {
			t.Fatalf("node %+v matched=%v, want %v", node.Labels, got, tc.want)
		}
	}
	p.Status.Targets = nil
	ds = buildProfileDaemonSet(testConfig(), &p, "agentpool", nil)
	if templateMatchesNode(&ds.Spec.Template.Spec, &corev1.Node{}) {
		t.Fatal("empty ledger must schedule no privileged workers")
	}
}

func TestNodeProfileCleanupPolicyRetainsPriorHostMutationObligations(t *testing.T) {
	prior := nodev1alpha1.NodeProfileSpec{
		Rollout:     nodev1alpha1.RolloutSpec{ContainerdRestart: nodev1alpha1.ContainerdRestartValidated},
		Tolerations: []corev1.Toleration{{Key: "old-pool"}},
	}
	current := nodev1alpha1.NodeProfileSpec{
		Rollout:     nodev1alpha1.RolloutSpec{ContainerdRestart: nodev1alpha1.ContainerdRestartNone},
		Tolerations: []corev1.Toleration{{Key: "recovery"}},
	}
	snapshot := provisioningSnapshot(&prior, &current)
	if snapshot.Rollout.ContainerdRestart != nodev1alpha1.ContainerdRestartValidated || len(snapshot.Tolerations) != 2 {
		t.Fatalf("new label-only policy forgot previously mutated hosts: %+v", snapshot)
	}
	if current.Rollout.ContainerdRestart != nodev1alpha1.ContainerdRestartNone || len(current.Tolerations) != 1 {
		t.Fatal("cleanup policy must not mutate the current provisioning request")
	}
	immutable := provisioningSnapshot(&current, &current)
	if immutable.Rollout.ContainerdRestart != nodev1alpha1.ContainerdRestartNone {
		t.Fatal("never-mutated immutable host configuration must remain untouched")
	}
}

func TestWorkerContainerdPolicyRejectsUnsupportedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     []corev1.EnvVar
		wantErr bool
	}{
		{"missing", nil, true},
		{"validated", []corev1.EnvVar{{Name: "BREWLET_CONTAINERD_RESTART", Value: "validated"}}, false},
		{"none", []corev1.EnvVar{{Name: "BREWLET_CONTAINERD_RESTART", Value: "none"}}, false},
		{"sighup", []corev1.EnvVar{{Name: "BREWLET_CONTAINERD_RESTART", Value: "sighup"}}, true},
		{"unknown", []corev1.EnvVar{{Name: "BREWLET_CONTAINERD_RESTART", Value: "reboot"}}, true},
		{"duplicate", []corev1.EnvVar{{Name: "BREWLET_CONTAINERD_RESTART", Value: "validated"}, {Name: "BREWLET_CONTAINERD_RESTART", Value: "none"}}, true},
		{"indirect", []corev1.EnvVar{{Name: "BREWLET_CONTAINERD_RESTART", ValueFrom: &corev1.EnvVarSource{}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := corev1.PodSpec{Containers: []corev1.Container{{Name: "provisioner", Env: tc.env}}}
			if err := validateWorkerContainerdPolicy(&spec); (err != nil) != tc.wantErr {
				t.Fatalf("worker policy error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestStoredContainerdPolicies(t *testing.T) {
	for _, mode := range []string{"", "validated", "none", "sighup", "reboot"} {
		for _, field := range []string{"provisioningSpec", "targets", "retirement.spec", "retirement.targets"} {
			t.Run(field+"/"+mode, func(t *testing.T) {
				p := profileNamed("owner", []string{"pool"}, jdk("temurin", 21))
				p.Spec.Rollout.ContainerdRestart = "none"
				setStoredRestartPolicy(&p, field, mode)
				err := validateStoredContainerdPolicies(&p)
				wantErr := mode == "sighup" || mode == "reboot"
				if (err != nil) != wantErr {
					t.Fatalf("policy %q: %v", mode, err)
				}
				if err != nil && !strings.Contains(err.Error(), field) {
					t.Fatalf("error does not identify stored policy: %v", err)
				}
			})
		}
	}
}

func setStoredRestartPolicy(p *nodev1alpha1.NodeProfile, field, mode string) {
	if len(p.Status.Targets) == 0 {
		p.Status.Targets = []nodev1alpha1.NodeTarget{{Name: "worker", UID: "node-uid", Claimed: true}}
	}
	switch field {
	case "provisioningSpec":
		p.Status.ProvisioningSpec = p.Spec.DeepCopy()
		p.Status.ProvisioningSpec.Rollout.ContainerdRestart = mode
	case "targets":
		p.Status.Targets[0].ContainerdRestart = mode
	case "retirement.spec", "retirement.targets":
		p.Status.Retirement = &nodev1alpha1.NodeRetirement{
			Spec: *p.Spec.DeepCopy(), Targets: append([]nodev1alpha1.NodeTarget(nil), p.Status.Targets...),
			Generation: p.Generation, Phase: nodev1alpha1.RetirementCleaning,
		}
		if field == "retirement.spec" {
			p.Status.Retirement.Spec.Rollout.ContainerdRestart = mode
		} else {
			p.Status.Retirement.Targets[0].ContainerdRestart = mode
		}
	}
}

func TestNodeProfileOwnershipFenceJSONPathMatchesDurableAPI(t *testing.T) {
	script, err := os.ReadFile("../../../provisioner/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	var query string
	for _, line := range strings.Split(string(script), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "local query='") {
			query = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "local query='"), "'")
			query = strings.ReplaceAll(query, `'"$NODE_NAME"'`, "worker")
		}
	}
	if query == "" {
		t.Fatal("provisioner target-ledger JSONPath missing")
	}
	p := profileNamed("owner", []string{"pool"}, jdk("temurin", 21))
	p.UID, p.Generation = "profile-uid", 7
	p.Status.Targets = []nodev1alpha1.NodeTarget{{Name: "worker", UID: "node-uid", Claimed: true, ContainerdRestart: "none"}}
	p.Status.Retirement = &nodev1alpha1.NodeRetirement{
		Targets:    []nodev1alpha1.NodeTarget{{Name: "worker", UID: "node-uid", Claimed: true, ContainerdRestart: "validated"}},
		Generation: 3, Phase: nodev1alpha1.RetirementCleaning,
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]interface{}
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	parser := jsonpath.New("ownership").AllowMissingKeys(true)
	if err := parser.Parse(query); err != nil {
		t.Fatalf("invalid actual provisioner JSONPath: %v", err)
	}
	var result bytes.Buffer
	if err := parser.Execute(&result, object); err != nil {
		t.Fatal(err)
	}
	if got, want := result.String(), "profile-uid|7||node-uid|true|3|Cleaning|node-uid|true|none|validated"; got != want {
		t.Fatalf("fence JSONPath = %q, want %q", got, want)
	}
}
