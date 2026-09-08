// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/dump"
)

const cleanupTemplateAnnotation = "brewlet.sh/cleanup-template"

// profileLabels are the pod/selector labels for a profile's managed DaemonSet:
// the shared provisioner app label (so the node-state reflection and the
// admission fleet check still match) plus a per-profile label recording the
// owner.
func profileLabels(profile string) map[string]string {
	return map[string]string{
		"app":                    brewlet.ProvisionerAppLabel,
		brewlet.LabelNodeProfile: profile,
	}
}

// resolvePoolKey returns the node label key a profile's pool is matched on. An
// explicit spec.nodePool.key wins; otherwise the operator auto-detects the
// provider key by probing the known keys across the fleet (§5.1). It returns ""
// when no provider pool label is present anywhere (bare-metal / kubeadm), which
// the caller treats as "every node".
func resolvePoolKey(profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) string {
	if k := profile.Spec.NodePool.Key; k != "" {
		return k
	}
	for _, key := range brewlet.ProviderPoolKeys {
		for i := range nodes {
			if _, ok := nodes[i].Labels[key]; ok {
				return key
			}
		}
	}
	return ""
}

// isDefaultProfile reports whether a profile is the empty-pool catch-all (no
// pool names) that owns every node not claimed by a named-pool profile (§5.6).
func isDefaultProfile(profile *nodev1alpha1.NodeProfile) bool {
	return len(profile.Spec.NodePool.Names) == 0
}

// nodeIsControlPlane reports whether a node carries a control-plane role label.
func nodeIsControlPlane(node *corev1.Node) bool {
	for _, key := range brewlet.ControlPlaneRoleLabels {
		if _, ok := node.Labels[key]; ok {
			return true
		}
	}
	return false
}

// profileExcludesNodeRole reports whether a node is off-limits to a profile
// because it is a control-plane node the profile has not explicitly opted into.
// It is the scheduling rule in controlPlaneExclusions expressed for the
// controllers' own node bookkeeping, so status counts, the cleanup wait, and
// node advertisement agree with where the DaemonSet can actually land.
func profileExcludesNodeRole(profile *nodev1alpha1.NodeProfile, node *corev1.Node) bool {
	return !profile.Spec.NodePool.IncludeControlPlane && nodeIsControlPlane(node)
}

// profileClaimsNode reports whether a node belongs to the given profile: it must
// be in the profile's pool (or unclaimed by any named pool, for the catch-all
// default) and must not be a control-plane node the profile has not opted into.
// Both reconcilers share it so membership never disagrees with placement.
func profileClaimsNode(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, node *corev1.Node) bool {
	if profileExcludesNodeRole(profile, node) {
		return false
	}
	if !isDefaultProfile(profile) {
		return nodeInPool(node, resolvedKey, profile.Spec.NodePool.Names)
	}
	// Catch-all default: every node not claimed by a named pool.
	if resolvedKey != "" && len(otherPools) > 0 && nodeInPool(node, resolvedKey, otherPools) {
		return false
	}
	return true
}

// nodeInPool reports whether a node belongs to one of the given pool names on
// the resolved key.
func nodeInPool(node *corev1.Node, key string, names []string) bool {
	if key == "" || len(names) == 0 {
		return false
	}
	val, ok := node.Labels[key]
	if !ok {
		return false
	}
	for _, n := range names {
		if val == n {
			return true
		}
	}
	return false
}

// profileAffinity computes the nodeAffinity term for a profile's DaemonSet:
//   - named pool:      <resolvedKey> In [names]
//   - catch-all default with sibling named pools: <resolvedKey> NotIn [namedPools]
//   - catch-all default with no named pools / no pool key: no pool requirement
//
// Every term additionally carries the control-plane exclusions, so the
// privileged provisioner never lands on a control-plane node — tainted or not —
// unless the profile sets nodePool.includeControlPlane. The exclusions are the
// reason a lone catch-all default still produces an affinity.
//
// otherPools is the set of pool names claimed by OTHER (named) profiles; it is
// what excludes the catch-all default from named pools (§5.6).
func profileAffinity(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string) *corev1.Affinity {
	var reqs []corev1.NodeSelectorRequirement
	names := profile.Spec.NodePool.Names

	switch {
	case len(names) > 0:
		key := resolvedKey
		if key == "" {
			// Could not resolve a pool key for a named-pool profile: land the
			// DaemonSet nowhere (the reconciler surfaces EmptyPool) rather than
			// emit an invalid empty-key selector or provision every node.
			key = brewlet.ProviderPoolKeys[0]
		}
		reqs = append(reqs, poolRequirement(key, corev1.NodeSelectorOpIn, names))
	case resolvedKey != "" && len(otherPools) > 0:
		// Catch-all default: everything the named profiles did not claim.
		reqs = append(reqs, poolRequirement(resolvedKey, corev1.NodeSelectorOpNotIn, otherPools))
	}

	reqs = append(reqs, controlPlaneExclusions(profile)...)
	if len(reqs) == 0 {
		return nil
	}
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: reqs}},
			},
		},
	}
}

// controlPlaneExclusions requires every control-plane role label to be absent
// from the node, unless the profile explicitly opted control-plane nodes in.
func controlPlaneExclusions(profile *nodev1alpha1.NodeProfile) []corev1.NodeSelectorRequirement {
	if profile.Spec.NodePool.IncludeControlPlane {
		return nil
	}
	reqs := make([]corev1.NodeSelectorRequirement, 0, len(brewlet.ControlPlaneRoleLabels))
	for _, key := range brewlet.ControlPlaneRoleLabels {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key:      key,
			Operator: corev1.NodeSelectorOpDoesNotExist,
		})
	}
	return reqs
}

func poolRequirement(key string, op corev1.NodeSelectorOperator, values []string) corev1.NodeSelectorRequirement {
	vals := append([]string(nil), values...)
	sort.Strings(vals)
	return corev1.NodeSelectorRequirement{Key: key, Operator: op, Values: vals}
}

// profileTolerations returns the taints a profile's provisioner DaemonSet is
// allowed to tolerate. There is deliberately no blanket `Exists` toleration: an
// administrator must name every taint they want the privileged provisioner to
// schedule through, so control-plane and other reserved nodes stay off-limits
// by default. Returns nil (not an empty slice) when the profile names none, so
// the rendered pod spec omits the field.
func profileTolerations(profile *nodev1alpha1.NodeProfile) []corev1.Toleration {
	if len(profile.Spec.Tolerations) == 0 {
		return nil
	}
	tolerations := make([]corev1.Toleration, len(profile.Spec.Tolerations))
	copy(tolerations, profile.Spec.Tolerations)
	return tolerations
}

// jdkTokens renders a profile's JDK inventory as the comma-separated
// "<dist>-<feature>" list the provisioner env (JDKS) expects.
func jdkTokens(profile *nodev1alpha1.NodeProfile) string {
	toks := make([]string, 0, len(profile.Spec.JDKs))
	for _, j := range profile.Spec.JDKs {
		toks = append(toks, j.Token())
	}
	return strings.Join(toks, ",")
}

func jdkSourceEnv(profile *nodev1alpha1.NodeProfile) []corev1.EnvVar {
	sources := make([]corev1.EnvVar, 0, 1+len(profile.Spec.JDKs)*3)
	sources = append(sources, corev1.EnvVar{Name: "JDK_SOURCE_COUNT", Value: strconv.Itoa(len(profile.Spec.JDKs))})
	for i, j := range profile.Spec.JDKs {
		prefix := fmt.Sprintf("JDK_SOURCE_%d_", i)
		sources = append(sources,
			corev1.EnvVar{Name: prefix + "TOKEN", Value: j.Token()},
			corev1.EnvVar{Name: prefix + "IMAGE", Value: j.Source.Image},
			corev1.EnvVar{Name: prefix + "JAVA_HOME", Value: j.Source.JavaHome},
		)
	}
	return sources
}

func launcherSourceEnv(profile *nodev1alpha1.NodeProfile) []corev1.EnvVar {
	sources := make([]corev1.EnvVar, 0, 1+len(profile.Spec.Launchers)*3)
	sources = append(sources, corev1.EnvVar{Name: "LAUNCHER_SOURCE_COUNT", Value: strconv.Itoa(len(profile.Spec.Launchers))})
	for i, launcher := range profile.Spec.Launchers {
		prefix := fmt.Sprintf("LAUNCHER_SOURCE_%d_", i)
		sources = append(sources,
			corev1.EnvVar{Name: prefix + "NAME", Value: launcher.Name},
			corev1.EnvVar{Name: prefix + "IMAGE", Value: launcher.Source.Image},
			corev1.EnvVar{Name: prefix + "PATH", Value: launcher.Source.Path},
		)
	}
	return sources
}

// mirrorEnv encodes a profile's registry mirrors as a deterministic
// comma-separated "host=mirror" list for the provisioner's MIRRORS env (§5.6).
// Returns "" when no mirrors are configured.
func mirrorEnv(profile *nodev1alpha1.NodeProfile) string {
	if profile.Spec.Registry == nil || len(profile.Spec.Registry.Mirrors) == 0 {
		return ""
	}
	hosts := make([]string, 0, len(profile.Spec.Registry.Mirrors))
	for h := range profile.Spec.Registry.Mirrors {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	pairs := make([]string, 0, len(hosts))
	for _, h := range hosts {
		pairs = append(pairs, h+"="+profile.Spec.Registry.Mirrors[h])
	}
	return strings.Join(pairs, ",")
}

// containerdRestart returns the effective containerd-restart mode for a profile,
// defaulting to "validated" (proposal 0002).
func containerdRestart(profile *nodev1alpha1.NodeProfile) string {
	if m := profile.Spec.Rollout.ContainerdRestart; m != "" {
		return m
	}
	return nodev1alpha1.ContainerdRestartValidated
}

func appCDSRegenerationEnabled(profile *nodev1alpha1.NodeProfile) bool {
	return profile.Spec.AppCDS != nil && profile.Spec.AppCDS.RegenerationEnabled
}

// buildProfileDaemonSet returns the provisioner DaemonSet for one profile — the
// generalized buildDaemonSet: pod nodeAffinity comes from the profile's pool and
// indexed runtime sources and MIRRORS env come from its inventory (§5.2). Pool disjointness is
// what enforces single ownership; there is no per-node assignment label.
func buildProfileDaemonSet(cfg Config, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string) *appsv1.DaemonSet {
	privileged := true
	hostPathSocket := corev1.HostPathSocket
	hostPathDir := corev1.HostPathDirectory
	hostPathDirOrCreate := corev1.HostPathDirectoryOrCreate
	lbls := profileLabels(profile.Name)
	metricsPort := cfg.MetricsPort
	if metricsPort <= 0 {
		metricsPort = 9090
	}

	env := []corev1.EnvVar{
		{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"},
		}},
		{Name: "BREWLET_APP_CDS_REGENERATION_ENABLED", Value: strconv.FormatBool(appCDSRegenerationEnabled(profile))},
		{Name: "BREWLET_CONTAINERD_RESTART", Value: containerdRestart(profile)},
		{Name: "BREWLET_PROFILE_NAME", Value: profile.Name},
		{Name: "BREWLET_PROFILE_UID", Value: string(profile.UID)},
		{Name: "BREWLET_PROFILE_GENERATION", Value: strconv.FormatInt(profile.Generation, 10)},
		{Name: "SOURCE_ALLOWED_MIRROR_HOSTS", Value: strings.Join(cfg.AllowedSourceMirrorHosts, ",")},
	}
	env = append(env, jdkSourceEnv(profile)...)
	env = append(env, launcherSourceEnv(profile)...)
	if m := mirrorEnv(profile); m != "" {
		env = append(env, corev1.EnvVar{Name: "MIRRORS", Value: m})
	}
	if v := profile.Spec.Rollout.Validate; v != nil && !*v {
		env = append(env, corev1.EnvVar{Name: "BREWLET_VALIDATE", Value: "false"})
	}

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      brewlet.ProfileDaemonSetName(profile.Name),
			Namespace: cfg.Namespace,
			Labels:    lbls,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: lbls},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
					ServiceAccountName: brewlet.ProvisionerName,
					HostPID:            true,
					// Only the taints the profile deliberately names. The
					// DaemonSet controller still adds the standard
					// node-condition tolerations, so normal rollouts are
					// unaffected while control-plane and other taints are
					// respected.
					Tolerations: profileTolerations(profile),
					Affinity:    profileAffinity(profile, resolvedKey, otherPools),
					Containers: []corev1.Container{
						{
							Name:            "provisioner",
							Image:           cfg.ProvisionerImage,
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
							Env:             env,
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"/usr/bin/test", "-f", "/tmp/brewlet-complete"}},
								},
								PeriodSeconds:    2,
								TimeoutSeconds:   1,
								SuccessThreshold: 1,
								FailureThreshold: 1,
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "host-opt", MountPath: "/opt/brewlet"},
								{Name: "containerd-conf", MountPath: "/etc/containerd"},
								{Name: "host-bin", MountPath: "/host/usr/local/bin"},
								{Name: "containerd-sock", MountPath: "/run/containerd/containerd.sock"},
							},
						},
						{
							Name:    "metrics-exporter",
							Image:   cfg.ProvisionerImage,
							Command: []string{"/opt/brewlet-dist/brewlet-metrics-exporter"},
							Args: []string{
								"--root=/opt/brewlet",
								fmt.Sprintf("--listen-address=:%d", metricsPort),
							},
							Ports: []corev1.ContainerPort{{
								Name:          "metrics",
								ContainerPort: int32(metricsPort),
								Protocol:      corev1.ProtocolTCP,
							}},
							SecurityContext: &corev1.SecurityContext{
								AllowPrivilegeEscalation: boolPtr(false),
								ReadOnlyRootFilesystem:   boolPtr(true),
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
							},
							VolumeMounts: []corev1.VolumeMount{
								// The exporter only reads the inventory the
								// provisioner wrote; it never mutates the host.
								// The one exception is the telemetry socket it
								// must bind, so that single subdirectory is
								// mounted read-write on top of the read-only
								// tree. The parent is listed first so the
								// nested bind mount lands on an existing path.
								{Name: "host-opt", MountPath: "/opt/brewlet", ReadOnly: true},
								{Name: metricsSocketVolume, MountPath: "/opt/brewlet/metrics"},
							},
						},
					},
					Volumes: []corev1.Volume{
						// /opt/brewlet is brewlet-owned and created on first
						// provision; the remaining host paths must already
						// exist, so a typo or a non-containerd node fails the
						// pod instead of creating root-owned directories.
						hostPathVolume("host-opt", "/opt/brewlet", &hostPathDirOrCreate),
						// Only the exporter's telemetry socket directory is
						// writable, and only when the sidecar is scheduled;
						// dropMetricsExporter removes it otherwise.
						hostPathVolume(metricsSocketVolume, "/opt/brewlet/metrics", &hostPathDirOrCreate),
						hostPathVolume("containerd-conf", "/etc/containerd", &hostPathDir),
						hostPathVolume("host-bin", "/usr/local/bin", &hostPathDir),
						hostPathVolume("containerd-sock", "/run/containerd/containerd.sock", &hostPathSocket),
					},
				},
			},
		},
	}
	if mu := profile.Spec.Rollout.MaxUnavailable; mu != nil {
		ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{
			Type:          appsv1.RollingUpdateDaemonSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: mu},
		}
	}
	if !cfg.MetricsEnabled {
		dropMetricsExporter(ds)
	}
	return ds
}

// metricsSocketVolume is the only host path the metrics exporter may write. It
// is scoped to the directory holding the shim telemetry socket so the exporter
// keeps a read-only view of the rest of the runtime tree (§ SECURITY-REVIEW
// finding 9).
const metricsSocketVolume = "host-opt-metrics"

// dropMetricsExporter removes the exporter sidecar and the writable telemetry
// socket volume it alone needs, so a DaemonSet without the sidecar never asks
// kubelet to create /opt/brewlet/metrics on the node.
func dropMetricsExporter(ds *appsv1.DaemonSet) {
	spec := &ds.Spec.Template.Spec
	spec.Containers = spec.Containers[:1]
	kept := spec.Volumes[:0]
	for _, vol := range spec.Volumes {
		if vol.Name == metricsSocketVolume {
			continue
		}
		kept = append(kept, vol)
	}
	spec.Volumes = kept
}

// buildCleanupDaemonSet returns the short-lived brewlet-cleanup DaemonSet the
// operator launches to reverse host state for a deleted profile (§5.6). It runs
// the same provisioner image in BREWLET_MODE=cleanup, scoped to the profile's
// pool, and (following the kata-cleanup pattern) restores containerd config,
// removes the shim, and drops the runtime + capability labels.
func buildCleanupDaemonSet(cfg Config, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string) *appsv1.DaemonSet {
	ds := buildProfileDaemonSet(cfg, profile, resolvedKey, otherPools)
	ds.Name = brewlet.CleanupDaemonSetName(profile.Name)
	lbls := ds.Spec.Selector.MatchLabels
	lbls["app"] = "brewlet-cleanup"
	ds.Labels = lbls
	ds.Spec.Template.ObjectMeta.Labels = lbls
	ds.Spec.Template.Spec.Containers[0].Env = append(
		ds.Spec.Template.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BREWLET_MODE", Value: "cleanup"},
	)
	// Cleanup never scrapes metrics, so it carries neither the exporter nor its
	// writable telemetry socket mount.
	dropMetricsExporter(ds)
	// DaemonSet readiness can still describe old pods during a rollout. Stamp
	// the desired template so cleanup completion can verify each current pod.
	revision := sha256.Sum256([]byte(dump.ForHash(ds.Spec.Template)))
	ds.Spec.Template.Annotations = map[string]string{
		cleanupTemplateAnnotation: fmt.Sprintf("%x", revision),
	}
	return ds
}
