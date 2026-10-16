package provisioner

import (
	"context"
	"fmt"
	"slices"
	"sort"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
)

var runtimePoolKeys = []string{"cloud.google.com/gke-nodepool", "agentpool", "eks.amazonaws.com/nodegroup", "karpenter.sh/nodepool"}

func resolvedRuntimePoolKey(profile api.NodeProfile, nodes []corev1.Node) string {
	if profile.Spec.NodePool.Key != "" {
		return profile.Spec.NodePool.Key
	}
	for _, key := range runtimePoolKeys {
		for _, node := range nodes {
			if _, present := node.Labels[key]; present {
				return key
			}
		}
	}
	return ""
}
func poolOffersNode(profile api.NodeProfile, profiles []api.NodeProfile, nodes []corev1.Node, node corev1.Node) bool {
	if !node.DeletionTimestamp.IsZero() {
		return false
	}
	if !profile.Spec.NodePool.IncludeControlPlane {
		if _, exists := node.Labels["node-role.kubernetes.io/control-plane"]; exists {
			return false
		}
		if _, exists := node.Labels["node-role.kubernetes.io/master"]; exists {
			return false
		}
	}
	key := resolvedRuntimePoolKey(profile, nodes)
	if len(profile.Spec.NodePool.Names) > 0 {
		return key != "" && slices.Contains(profile.Spec.NodePool.Names, node.Labels[key])
	}
	for _, named := range profiles {
		namedKey := named.Spec.NodePool.Key
		if namedKey == "" {
			namedKey = key
		}
		if len(named.Spec.NodePool.Names) > 0 && namedKey != "" && slices.Contains(named.Spec.NodePool.Names, node.Labels[namedKey]) {
			return false
		}
	}
	return true
}
func nodeRuntimePlatform(node corev1.Node) (ocispec.Platform, error) {
	os, architecture := node.Status.NodeInfo.OperatingSystem, node.Status.NodeInfo.Architecture
	labelOS, labelArch := node.Labels[corev1.LabelOSStable], node.Labels[corev1.LabelArchStable]
	if os == "" {
		os = labelOS
	}
	if architecture == "" {
		architecture = labelArch
	}
	actual, err := NormalizeRuntimePlatform(ocispec.Platform{OS: os, Architecture: architecture})
	if err != nil {
		return actual, fmt.Errorf("NoCompatibleRuntime: Node %s has unknown/unsupported platform: %w", node.Name, err)
	}
	if labelOS != "" || labelArch != "" {
		if labelOS == "" {
			labelOS = actual.OS
		}
		if labelArch == "" {
			labelArch = actual.Architecture
		}
		labelled, err := NormalizeRuntimePlatform(ocispec.Platform{OS: labelOS, Architecture: labelArch})
		if err != nil || runtimePlatformKey(labelled) != runtimePlatformKey(actual) {
			return actual, fmt.Errorf("NoCompatibleRuntime: Node %s platform status and labels disagree", node.Name)
		}
	}
	return actual, nil
}
func knownOfferedPlatforms(id string, profiles []api.NodeProfile, nodes []corev1.Node, current corev1.Node, allowedMirrors []string) ([]ocispec.Platform, error) {
	selected := map[string]corev1.Node{string(current.UID): current}
	for _, profile := range profiles {
		if !profile.DeletionTimestamp.IsZero() || policy.Profile(&profile, profiles, allowedMirrors) != nil {
			continue
		}
		offers := false
		for _, entry := range profile.Spec.RuntimeImages {
			offers = offers || entry.ID == id
		}
		if !offers {
			continue
		}
		for _, node := range nodes {
			if poolOffersNode(profile, profiles, nodes, node) {
				selected[string(node.UID)] = node
			}
		}
	}
	platforms := map[string]ocispec.Platform{}
	for _, node := range selected {
		if node.UID == "" {
			return nil, fmt.Errorf("NoCompatibleRuntime: offered Node UID is unknown")
		}
		p, err := nodeRuntimePlatform(node)
		if err != nil {
			return nil, err
		}
		platforms[runtimePlatformKey(p)] = p
	}
	keys := make([]string, 0, len(platforms))
	for key := range platforms {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]ocispec.Platform, 0, len(keys))
	for _, key := range keys {
		result = append(result, platforms[key])
	}
	return result, nil
}
func (w *Worker) requiredPlatforms(ctx context.Context, id string) ([]ocispec.Platform, error) {
	s, err := w.check(ctx)
	if err != nil {
		return nil, err
	}
	var profiles api.NodeProfileList
	if err := w.Reader.List(ctx, &profiles); err != nil {
		return nil, fmt.Errorf("runtime offered-pool profile lookup unverified: %w", err)
	}
	var nodes corev1.NodeList
	if err := w.Reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("runtime offered-pool Node lookup unverified: %w", err)
	}
	return knownOfferedPlatforms(id, profiles.Items, nodes.Items, *s.Node, w.AllowedMirrorHosts)
}
