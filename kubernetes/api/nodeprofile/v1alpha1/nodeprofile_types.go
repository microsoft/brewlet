// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// NodeProfileSpec binds a node pool to a JDK/launcher inventory (§5.6). Selecting
// a pool IS the opt-in: every node in the pool — including nodes the autoscaler
// adds later — is provisioned. The operator reconciles one provisioner DaemonSet
// per profile.
type NodeProfileSpec struct {
	// NodePool selects which node pool(s) this profile provisions. An empty
	// NodePool (no names) means "every node" — the bare-metal / default-profile
	// catch-all fallback (§5.6).
	NodePool NodePoolRef `json:"nodePool,omitempty"`
	// JDKs is the declarative JDK inventory to install on the pool's nodes. It
	// replaces the global JDKS string. At least one entry is required.
	JDKs []JDKRef `json:"jdks"`
	// Launchers is the optional launcher-layer inventory. The vanilla "java"
	// launcher comes from each JDK and must not be listed.
	Launchers []LauncherRef `json:"launchers,omitempty"`
	// AppCDS controls node-side AppCDS behavior. Regeneration is disabled when
	// this field is omitted.
	AppCDS *AppCDSSpec `json:"appCDS,omitempty"`
	// Registry is an optional per-profile registry override for air-gapped /
	// mirrored clusters (§5.6).
	Registry *RegistrySpec `json:"registry,omitempty"`
	// Rollout carries the DaemonSet + host-reconfig rollout policy. The
	// reconfig/validate mechanics themselves are defined in proposal 0002; this
	// profile only selects them.
	Rollout RolloutSpec `json:"rollout,omitempty"`
}

// AppCDSSpec controls node-side AppCDS policy for a profile.
type AppCDSSpec struct {
	// RegenerationEnabled authorizes node-side AppCDS archive regeneration.
	// It defaults to false.
	RegenerationEnabled bool `json:"regenerationEnabled,omitempty"`
}

// NodePoolRef identifies the node pool(s) a profile provisions.
type NodePoolRef struct {
	// Names are the pool name(s) this profile provisions (matched on the resolved
	// pool key). Empty means "every node" (bare-metal / default-profile fallback).
	Names []string `json:"names,omitempty"`
	// Key is the node label carrying the pool name. Empty means the operator
	// auto-detects the provider key (gke-nodepool / agentpool / nodegroup /
	// karpenter). Set it explicitly on bare-metal / non-standard clusters.
	Key string `json:"key,omitempty"`
}

// JDKRef is one JDK root to install.
type JDKRef struct {
	// Distribution is the stable, lowercase distribution identifier used in the
	// on-node inventory token, such as "temurin", "microsoft", or "zulu".
	Distribution string `json:"distribution"`
	// Feature is the JDK feature version (e.g. 21).
	Feature int32 `json:"feature"`
	// Source is the administrator-provided image and JDK root. Brewlet does not
	// resolve distribution names through a built-in catalog.
	Source JDKSource `json:"source"`
}

// JDKSource describes an administrator-provided JDK source.
type JDKSource struct {
	// Image is a fully qualified, tagless OCI image reference pinned by SHA-256
	// digest and containing the JDK root.
	Image string `json:"image"`
	// JavaHome is the absolute path to the JDK root inside Image.
	JavaHome string `json:"javaHome"`
}

// LauncherRef is one optional launcher binary to install.
type LauncherRef struct {
	// Name is the stable lowercase launcher identifier advertised on the node.
	Name string `json:"name"`
	// Source is the administrator-provided image and binary path.
	Source LauncherSource `json:"source"`
}

// LauncherSource describes an administrator-provided launcher source.
type LauncherSource struct {
	// Image is a fully qualified, tagless OCI image reference pinned by SHA-256
	// digest and containing the launcher binary.
	Image string `json:"image"`
	// Path is the absolute path to the launcher binary inside Image.
	Path string `json:"path"`
}

// Token renders the JDK as the on-node "<distribution>-<feature>" inventory token
// (e.g. "temurin-21"), the form the provisioner env and node labels use.
func (j JDKRef) Token() string {
	return j.Distribution + "-" + itoa(j.Feature)
}

// RegistrySpec carries the air-gap registry mirror configuration (§5.6).
type RegistrySpec struct {
	// Mirrors maps an upstream host (e.g. "mcr.microsoft.com", "docker.io") to
	// an operator-approved mirror host/path for digest-pinned copy-from-image
	// pulls. Auth to the mirror is a node/containerd concern, not carried here.
	Mirrors map[string]string `json:"mirrors,omitempty"`
}

// RolloutSpec is the managed DaemonSet + host-reconfig rollout policy.
type RolloutSpec struct {
	// MaxUnavailable bounds the DaemonSet rolling update (defaults to 1).
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`
	// Validate gates readiness on the 0002 smoke test before marking a node
	// ready (defaults to true).
	Validate *bool `json:"validate,omitempty"`
	// ContainerdRestart selects how the provisioner restarts containerd after a
	// config change: "validated", "sighup", or "none" (label-only /
	// immutable-image mode). Empty defaults to "validated" (proposal 0002).
	ContainerdRestart string `json:"containerdRestart,omitempty"`
}

// Valid values for RolloutSpec.ContainerdRestart.
const (
	ContainerdRestartValidated = "validated"
	ContainerdRestartSIGHUP    = "sighup"
	ContainerdRestartNone      = "none"
)

// NodeProfileStatus reflects the reconciled state of a profile (§5.6).
type NodeProfileStatus struct {
	// ObservedGeneration is the .metadata.generation the operator last acted on.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ResolvedPoolKey is the node label key the operator matched the pool on
	// (auto-detected or the spec override). Empty for an every-node profile.
	ResolvedPoolKey string `json:"resolvedPoolKey,omitempty"`
	// AssignedNodes is the number of nodes in the selected pool(s).
	AssignedNodes int32 `json:"assignedNodes"`
	// ReadyNodes is the number of assigned nodes advertising the brewlet runtime.
	ReadyNodes int32 `json:"readyNodes"`
	// Conditions carries the Ready condition (AllNodesProvisioned) / Degraded
	// (EmptyPool, InvalidProfile, or a reconfig failure propagated from the
	// per-node brewlet.sh/provision-error).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Condition types and reasons surfaced on NodeProfile status.
const (
	// ConditionReady is True once every assigned node is provisioned.
	ConditionReady = "Ready"

	// ReasonAllNodesProvisioned — all assigned nodes advertise the runtime.
	ReasonAllNodesProvisioned = "AllNodesProvisioned"
	// ReasonProvisioning — assigned nodes are still being provisioned.
	ReasonProvisioning = "Provisioning"
	// ReasonEmptyPool — a named pool resolved to zero nodes (typo / wrong key).
	ReasonEmptyPool = "EmptyPool"
	// ReasonNodeFailure — one or more assigned nodes reported provision-error.
	ReasonNodeFailure = "NodeFailure"
	// ReasonInvalidProfile — source trust policy rejected the stored profile.
	ReasonInvalidProfile = "InvalidProfile"
	// ReasonCleanupPending — the profile is being deleted; cleanup is running.
	ReasonCleanupPending = "CleanupPending"
)

// NodeProfile binds a node pool to a JDK/launcher inventory (§5.6). Cluster-scoped.
type NodeProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeProfileSpec   `json:"spec,omitempty"`
	Status NodeProfileStatus `json:"status,omitempty"`
}

// NodeProfileList is a list of NodeProfiles.
type NodeProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeProfile `json:"items"`
}

// itoa renders a non-negative int32 without pulling in strconv at call sites.
func itoa(v int32) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
