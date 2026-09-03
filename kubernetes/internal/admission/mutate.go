// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"strconv"
	"strings"

	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
)

// MutationResult reports what MutatePod did.
type MutationResult struct {
	// Applies is true when the pod targets the brewlet RuntimeClass and was
	// therefore considered. When false the pod is left untouched.
	Applies bool
	// DenyReason / DenyMessage are set (from CheckFleet) when the pod requested a
	// JDK/launcher no ready node provides. When DenyReason is empty the mutation
	// succeeded and the (possibly modified) pod should be admitted.
	DenyReason  string
	DenyMessage string
	// ArtifactRef / ArtifactDigest are the image-derived informational values
	// written onto the pod for logging and compatibility.
	ArtifactRef    string
	ArtifactDigest string
}

// IsBrewletPod reports whether a pod is routed to the brewlet runtime.
func IsBrewletPod(pod *corev1.Pod) bool {
	return pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName == brewlet.RuntimeClassName
}

// MutatePod applies the admission/scheduling seam to a brewlet pod in place:
//
//  1. overwrites the artifact-container/ref/digest compatibility hints from the
//     selected container image. The shim derives executable identity from
//     containerd rather than trusting these Pod annotations;
//  2. validates explicit JDK/launcher/architecture/AppCDS requests against the
//     ready fleet, returning the corresponding denial when unsatisfiable;
//  3. injects nodeAffinity so the scheduler only lands the pod on nodes that
//     advertise all requested capabilities.
//
// Non-brewlet pods are left untouched (Applies=false). The function is pure over
// (pod, fleet): it mutates only the passed pod and is unit-tested without a
// cluster.
func MutatePod(pod *corev1.Pod, fleet []NodeCapability) MutationResult {
	if !IsBrewletPod(pod) {
		return MutationResult{}
	}
	res := MutationResult{Applies: true}

	// 1. Replace tenant-supplied identity hints with values derived from the pod
	// image. Record the selected container so other tasks in a multi-container
	// pod ignore these Pod-wide hints. A tag-based image must also clear any
	// stale digest hint.
	container := podArtifactContainer(pod)
	if container != nil {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[brewlet.AnnotationArtifactContainer] = container.Name
		ref := container.Image
		pod.Annotations[brewlet.AnnotationArtifactRef] = ref
		res.ArtifactRef = ref
		if digest := refDigest(ref); digest != "" {
			pod.Annotations[brewlet.AnnotationArtifactDigest] = digest
			res.ArtifactDigest = digest
		} else {
			delete(pod.Annotations, brewlet.AnnotationArtifactDigest)
		}
	}

	// 2. Validate runtime capabilities and AppCDS policy against the ready fleet.
	jdk := strings.TrimSpace(pod.Annotations[brewlet.AnnotationRequestedJDK])
	launcher := strings.TrimSpace(pod.Annotations[brewlet.AnnotationRequestedLauncher])
	arch := splitArch(pod.Annotations[brewlet.AnnotationRequestedArch])
	appCDSRegeneration := strings.EqualFold(strings.TrimSpace(pod.Annotations[brewlet.AnnotationCDSRegenerate]), "true")
	if fr := CheckFleet(fleet, jdk, launcher, arch, appCDSRegeneration); !fr.Compatible {
		res.DenyReason = fr.DenyReason
		res.DenyMessage = fr.Message
		return res
	}

	// 3. Steer scheduling onto capable nodes.
	injectNodeAffinity(pod, jdk, launcher, arch, appCDSRegeneration)
	return res
}

// splitArch parses the comma-separated brewlet.sh/arch annotation into a
// trimmed, non-empty token slice. An empty annotation yields nil (arch-neutral).
func splitArch(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// podArtifactContainer returns the regular container selected for the Pod-wide
// compatibility hints, or the first regular container when none is selected.
func podArtifactContainer(pod *corev1.Pod) *corev1.Container {
	if len(pod.Spec.Containers) == 0 {
		return nil
	}
	if name := pod.Annotations[brewlet.AnnotationArtifactContainer]; name != "" {
		for i := range pod.Spec.Containers {
			if pod.Spec.Containers[i].Name == name {
				return &pod.Spec.Containers[i]
			}
		}
	}
	return &pod.Spec.Containers[0]
}

// refDigest returns the "sha256:…" manifest digest of a digest-pinned reference
// (repo@sha256:…), or "" for a tag-based reference.
func refDigest(ref string) string {
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		return ""
	}
	digest := ref[at+1:]
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return ""
	}
	for _, r := range strings.TrimPrefix(digest, "sha256:") {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return ""
		}
	}
	return digest
}

// requiredCapabilityLabels returns the node-label selector requirements a pod
// needs, derived from its explicit runtime and AppCDS policy requests. JDK,
// launcher, and AppCDS map to presence matches (Operator: Exists) against
// provisioner-emitted capability labels; arch maps to an In match against the
// standard kubernetes.io/arch label. An empty request contributes nothing.
func requiredCapabilityLabels(jdk, launcher string, arch []string, appCDSRegeneration bool) []corev1.NodeSelectorRequirement {
	var reqs []corev1.NodeSelectorRequirement
	if jdk != "" {
		key := brewlet.LabelJDKPrefix + jdk
		if feature, ok := bareFeature(jdk); ok {
			key = brewlet.LabelJDKFeaturePrefix + strconv.Itoa(feature)
		}
		reqs = append(reqs, corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpExists})
	}
	if launcher != "" && launcher != brewlet.VanillaLauncher {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key: brewlet.LabelLauncherPrefix + launcher, Operator: corev1.NodeSelectorOpExists,
		})
	}
	if len(arch) > 0 {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key: brewlet.LabelArch, Operator: corev1.NodeSelectorOpIn, Values: arch,
		})
	}
	if appCDSRegeneration {
		reqs = append(reqs, corev1.NodeSelectorRequirement{
			Key: brewlet.LabelAppCDSRegeneration, Operator: corev1.NodeSelectorOpExists,
		})
	}
	return reqs
}

// injectNodeAffinity ANDs the pod's capability requirements into its
// requiredDuringSchedulingIgnoredDuringExecution nodeAffinity. Because
// matchExpressions within a term are ANDed while terms are ORed, the
// requirements are appended to every existing term (and a fresh term is created
// when the pod has none), preserving any operator/user-supplied affinity.
func injectNodeAffinity(pod *corev1.Pod, jdk, launcher string, arch []string, appCDSRegeneration bool) {
	reqs := requiredCapabilityLabels(jdk, launcher, arch, appCDSRegeneration)
	if len(reqs) == 0 {
		return
	}
	if pod.Spec.Affinity == nil {
		pod.Spec.Affinity = &corev1.Affinity{}
	}
	if pod.Spec.Affinity.NodeAffinity == nil {
		pod.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	na := pod.Spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
	}
	sel := na.RequiredDuringSchedulingIgnoredDuringExecution
	if len(sel.NodeSelectorTerms) == 0 {
		sel.NodeSelectorTerms = []corev1.NodeSelectorTerm{{}}
	}
	for i := range sel.NodeSelectorTerms {
		sel.NodeSelectorTerms[i].MatchExpressions = append(sel.NodeSelectorTerms[i].MatchExpressions, reqs...)
	}
}
