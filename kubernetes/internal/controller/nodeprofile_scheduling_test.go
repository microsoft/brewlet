// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
)

// These are the remediation tests for SECURITY-REVIEW.md finding 10 — "the
// privileged provisioner defaults to every node". The provisioner runs
// privileged with hostPID and host mounts, so where it is allowed to land is a
// security boundary, not a scheduling preference.

const (
	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
	legacyMasterLabel = "node-role.kubernetes.io/master"
)

// exclusionKeys returns the keys a DoesNotExist requirement is asserted on.
func exclusionKeys(aff *corev1.Affinity) map[string]bool {
	keys := map[string]bool{}
	if aff == nil || aff.NodeAffinity == nil {
		return keys
	}
	selector := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	if selector == nil {
		return keys
	}
	for _, term := range selector.NodeSelectorTerms {
		for _, req := range term.MatchExpressions {
			if req.Operator == corev1.NodeSelectorOpDoesNotExist {
				keys[req.Key] = true
			}
		}
	}
	return keys
}

func assertExcludesControlPlane(t *testing.T, name string, aff *corev1.Affinity) {
	t.Helper()
	keys := exclusionKeys(aff)
	for _, want := range brewlet.ControlPlaneRoleLabels {
		if !keys[want] {
			t.Errorf("%s: affinity does not require %s to be absent (%+v)", name, want, aff)
		}
	}
}

func TestProvisionerTolerationsAreExplicitOptIn(t *testing.T) {
	cfg := testConfig()
	p := profileNamed("general", []string{"general"}, jdk("temurin", 21))

	// A blanket `Exists` toleration would defeat the control-plane taint (and
	// every other taint a platform team relies on), so the default is none.
	ds := buildProfileDaemonSet(cfg, &p, "agentpool", nil)
	if got := ds.Spec.Template.Spec.Tolerations; len(got) != 0 {
		t.Fatalf("default tolerations = %+v, want none", got)
	}
	cleanup := buildCleanupDaemonSet(cfg, &p, "agentpool", nil)
	if got := cleanup.Spec.Template.Spec.Tolerations; len(got) != 0 {
		t.Fatalf("cleanup tolerations = %+v, want none", got)
	}

	// What the administrator names, and nothing more, is tolerated.
	p.Spec.Tolerations = []corev1.Toleration{{
		Key:      "workload",
		Operator: corev1.TolerationOpEqual,
		Value:    "java",
		Effect:   corev1.TaintEffectNoSchedule,
	}}
	ds = buildProfileDaemonSet(cfg, &p, "agentpool", nil)
	got := ds.Spec.Template.Spec.Tolerations
	if len(got) != 1 {
		t.Fatalf("tolerations = %+v, want exactly the declared one", got)
	}
	if got[0].Key != "workload" || got[0].Value != "java" || got[0].Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("toleration = %+v, want the declared workload=java:NoSchedule", got[0])
	}

	// Mutating the rendered pod spec must not write back into the profile.
	got[0].Key = "mutated"
	if p.Spec.Tolerations[0].Key != "workload" {
		t.Fatal("rendered tolerations alias the profile spec")
	}
}

func TestProfileAffinityExcludesControlPlane(t *testing.T) {
	named := profileNamed("general", []string{"general"}, jdk("temurin", 21))
	catchAll := profileNamed("default", nil, jdk("temurin", 21))

	assertExcludesControlPlane(t, "named pool",
		profileAffinity(&named, "agentpool", nil))
	assertExcludesControlPlane(t, "named pool with unresolved key",
		profileAffinity(&named, "", nil))
	assertExcludesControlPlane(t, "catch-all with sibling pools",
		profileAffinity(&catchAll, "agentpool", []string{"batch"}))
	// The lone catch-all used to produce a nil affinity — "every node".
	assertExcludesControlPlane(t, "lone catch-all",
		profileAffinity(&catchAll, "", nil))

	// The pool requirement still comes first and is unchanged.
	term := profileAffinity(&named, "agentpool", nil).
		NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
	if term.Key != "agentpool" || term.Operator != corev1.NodeSelectorOpIn || term.Values[0] != "general" {
		t.Fatalf("pool requirement = %+v, want In agentpool [general]", term)
	}

	// Opting in is the only way back onto a control-plane node.
	named.Spec.NodePool.IncludeControlPlane = true
	if keys := exclusionKeys(profileAffinity(&named, "agentpool", nil)); len(keys) != 0 {
		t.Fatalf("includeControlPlane still excluded %v", keys)
	}
	catchAll.Spec.NodePool.IncludeControlPlane = true
	if aff := profileAffinity(&catchAll, "", nil); aff != nil {
		t.Fatalf("opted-in lone catch-all affinity = %+v, want nil (every node)", aff)
	}
}

func TestProfileClaimsNodeSkipsControlPlane(t *testing.T) {
	catchAll := profileNamed("default", nil, jdk("temurin", 21))
	named := profileNamed("general", []string{"general"}, jdk("temurin", 21))

	worker := labeledNode("worker", map[string]string{"agentpool": "general"})
	// kind and Docker Desktop single-node clusters label the control plane but
	// leave it untainted, so the label — not the taint — is what excludes it.
	cp := labeledNode("cp", map[string]string{"agentpool": "general", controlPlaneLabel: ""})
	legacy := labeledNode("legacy", map[string]string{"agentpool": "general", legacyMasterLabel: ""})

	if !profileClaimsNode(&catchAll, "agentpool", nil, &worker) {
		t.Error("catch-all must still claim a worker node")
	}
	if profileClaimsNode(&catchAll, "agentpool", nil, &cp) {
		t.Error("catch-all claimed a control-plane node")
	}
	if profileClaimsNode(&catchAll, "agentpool", nil, &legacy) {
		t.Error("catch-all claimed a legacy master node")
	}
	if profileClaimsNode(&named, "agentpool", nil, &cp) {
		t.Error("named pool claimed a control-plane node")
	}

	catchAll.Spec.NodePool.IncludeControlPlane = true
	if !profileClaimsNode(&catchAll, "agentpool", nil, &cp) {
		t.Error("includeControlPlane must claim the control-plane node")
	}

	// Membership and placement must agree, or a profile counts nodes it can
	// never provision and never reaches Ready.
	r := &NodeProfileReconciler{}
	excluded := profileNamed("general", []string{"general"}, jdk("temurin", 21))
	if r.nodeAssigned(&excluded, "agentpool", nil, &cp) {
		t.Error("status accounting assigned an unschedulable control-plane node")
	}
}

func TestProvisionerHostMountsMinimizeWrite(t *testing.T) {
	cfg := testConfig()
	p := profileNamed("general", []string{"general"}, jdk("temurin", 21))
	spec := buildProfileDaemonSet(cfg, &p, "agentpool", nil).Spec.Template.Spec

	// The metrics exporter only reads what the provisioner wrote, except for
	// the one directory holding the telemetry socket it must bind.
	exporter := spec.Containers[1]
	if len(exporter.VolumeMounts) != 2 {
		t.Fatalf("metrics-exporter mounts = %+v, want the read-only tree plus the socket directory", exporter.VolumeMounts)
	}
	// The read-only parent must be listed first so the nested socket mount
	// lands on a path that already exists inside it.
	if exporter.VolumeMounts[0].MountPath != "/opt/brewlet" || !exporter.VolumeMounts[0].ReadOnly {
		t.Errorf("first exporter mount = %+v, want a read-only /opt/brewlet", exporter.VolumeMounts[0])
	}
	if exporter.VolumeMounts[1].MountPath != "/opt/brewlet/metrics" || exporter.VolumeMounts[1].ReadOnly {
		t.Errorf("second exporter mount = %+v, want a writable /opt/brewlet/metrics", exporter.VolumeMounts[1])
	}
	if exporter.SecurityContext == nil ||
		exporter.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*exporter.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("metrics-exporter must run with a read-only root filesystem")
	}
	if exporter.SecurityContext == nil || exporter.SecurityContext.Capabilities == nil ||
		len(exporter.SecurityContext.Capabilities.Drop) != 1 ||
		exporter.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("metrics-exporter must drop all capabilities")
	}

	// Host paths the provisioner mutates must already exist, so a typo or a
	// non-containerd node fails the pod instead of creating root-owned paths.
	wantTypes := map[string]corev1.HostPathType{
		"/opt/brewlet":                    corev1.HostPathDirectoryOrCreate,
		"/opt/brewlet/metrics":            corev1.HostPathDirectoryOrCreate,
		"/etc/containerd":                 corev1.HostPathDirectory,
		"/usr/local/bin":                  corev1.HostPathDirectory,
		"/run/containerd/containerd.sock": corev1.HostPathSocket,
	}
	for _, vol := range spec.Volumes {
		if vol.HostPath == nil {
			t.Fatalf("volume %q is not a hostPath", vol.Name)
		}
		want, ok := wantTypes[vol.HostPath.Path]
		if !ok {
			t.Fatalf("unexpected host mount %q", vol.HostPath.Path)
		}
		if vol.HostPath.Type == nil || *vol.HostPath.Type != want {
			t.Errorf("hostPath %q type = %v, want %v", vol.HostPath.Path, vol.HostPath.Type, want)
		}
	}

	// The privileged provisioner already writes the whole tree through its own
	// read-write mount, so the narrowly scoped socket volume stays exporter-only.
	for _, mount := range spec.Containers[0].VolumeMounts {
		if mount.MountPath == "/opt/brewlet/metrics" {
			t.Error("provisioner must not carry the exporter's socket mount")
		}
	}
}

func TestMetricsSocketVolumeOnlyWithExporter(t *testing.T) {
	hasSocketVolume := func(spec corev1.PodSpec) bool {
		for _, vol := range spec.Volumes {
			if vol.HostPath != nil && vol.HostPath.Path == "/opt/brewlet/metrics" {
				return true
			}
		}
		return false
	}

	cfg := testConfig()
	p := profileNamed("general", []string{"general"}, jdk("temurin", 21))
	if !hasSocketVolume(buildProfileDaemonSet(cfg, &p, "agentpool", nil).Spec.Template.Spec) {
		t.Error("the exporter sidecar needs a writable telemetry socket volume")
	}

	// Without the sidecar nothing binds the socket, so kubelet must not create
	// the directory on the node either.
	if spec := buildCleanupDaemonSet(cfg, &p, "agentpool", nil).Spec.Template.Spec; hasSocketVolume(spec) {
		t.Error("cleanup DaemonSet must not mount the telemetry socket directory")
	}
	cfg.MetricsEnabled = false
	if spec := buildProfileDaemonSet(cfg, &p, "agentpool", nil).Spec.Template.Spec; hasSocketVolume(spec) {
		t.Error("metrics-disabled DaemonSet must not mount the telemetry socket directory")
	}
}
