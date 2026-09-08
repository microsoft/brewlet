// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const legacySchedulingGate = "node.brewlet.sh/migration"

type migrationEvidenceError struct{ message string }

func (e *migrationEvidenceError) Error() string { return e.message }

func (r *NodeProfileReconciler) migrationStatus(ctx context.Context, profile *nodev1alpha1.NodeProfile, err error) (ctrl.Result, error) {
	reason := nodev1alpha1.ReasonOwnershipMigration
	var evidence *migrationEvidenceError
	if errors.As(err, &evidence) {
		reason = nodev1alpha1.ReasonCleanupBlocked
	}
	return r.ownershipBlocked(ctx, profile, reason, err)
}

// This is a cluster-wide read barrier, never authority to mutate resources
// outside the configured namespace or adopt a worker by its name/labels.
func (r *NodeProfileReconciler) profileWriterBarrier(ctx context.Context, profile *nodev1alpha1.NodeProfile, includeUnfenced bool) error {
	var daemonSets appsv1.DaemonSetList
	if err := r.apiReader().List(ctx, &daemonSets); err != nil {
		return fmt.Errorf("listing cluster-wide legacy DaemonSets before host ownership: %w", err)
	}
	for i := range daemonSets.Items {
		ds := &daemonSets.Items[i]
		if ds.Namespace != r.Config.Namespace && metav1.IsControlledBy(ds, profile) {
			return &migrationEvidenceError{message: fmt.Sprintf("profile writer DaemonSet %s/%s is outside the operator namespace; its original owner must finish teardown before provisioning or cleanup", ds.Namespace, ds.Name)}
		}
		if includeUnfenced && (profileWriter(ds.Labels) || profileWriter(ds.Spec.Template.Labels) || ds.Name == brewlet.ProvisionerName) && !claimFenced(ds) {
			return fmt.Errorf("legacy DaemonSet %s/%s must finish migration before node claims can activate; foreign namespace workers must be drained by their owner", ds.Namespace, ds.Name)
		}
	}
	var pods corev1.PodList
	if err := r.apiReader().List(ctx, &pods); err != nil {
		return fmt.Errorf("listing cluster-wide legacy pods before host ownership: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		uid, _ := legacyLiteralEnv(&pod.Spec, "BREWLET_PROFILE_UID")
		if pod.Namespace != r.Config.Namespace && profileWriter(pod.Labels) && uid != "" && uid == string(profile.UID) {
			return &migrationEvidenceError{message: fmt.Sprintf("profile writer pod %s/%s is outside the operator namespace; its original owner must finish teardown before provisioning or cleanup", pod.Namespace, pod.Name)}
		}
		if includeUnfenced && profileWriter(pod.Labels) && !claimFencedPod(&pod.Spec) {
			return fmt.Errorf("legacy writer pod %s/%s must terminate before node claims can activate", pod.Namespace, pod.Name)
		}
	}
	return nil
}

func legacyLiteralEnv(spec *corev1.PodSpec, name string) (string, bool) {
	var value string
	count := 0
	for _, container := range spec.Containers {
		if container.Name != "provisioner" {
			continue
		}
		for _, env := range container.Env {
			if env.Name == name {
				if env.ValueFrom != nil {
					return "", false
				}
				value = env.Value
				count++
			}
		}
	}
	return value, count == 1
}

func legacyPolicy(spec *corev1.PodSpec, profile *nodev1alpha1.NodeProfile) (string, string, error) {
	uid, uidOK := legacyLiteralEnv(spec, "BREWLET_PROFILE_UID")
	name, nameOK := legacyLiteralEnv(spec, "BREWLET_PROFILE_NAME")
	generation, generationOK := legacyLiteralEnv(spec, "BREWLET_PROFILE_GENERATION")
	number, err := strconv.ParseInt(generation, 10, 64)
	if !uidOK || !nameOK || !generationOK || uid != string(profile.UID) || name != profile.Name ||
		err != nil || number <= 0 || number > profile.Generation {
		return "", "", fmt.Errorf("legacy worker lacks verifiable profile UID/generation provenance")
	}
	mode, ok := legacyLiteralEnv(spec, "BREWLET_CONTAINERD_RESTART")
	if !ok || (mode != nodev1alpha1.ContainerdRestartNone && mode != nodev1alpha1.ContainerdRestartSIGHUP && mode != nodev1alpha1.ContainerdRestartValidated) {
		return "", "", fmt.Errorf("legacy worker has unknown cleanup restart policy")
	}
	return mode, generation, nil
}

func mergeLegacyMode(prior, next string) string {
	if prior == "" || next == "" {
		return ""
	}
	if prior == nodev1alpha1.ContainerdRestartValidated || next == nodev1alpha1.ContainerdRestartValidated {
		return nodev1alpha1.ContainerdRestartValidated
	}
	if prior == nodev1alpha1.ContainerdRestartSIGHUP || next == nodev1alpha1.ContainerdRestartSIGHUP {
		return nodev1alpha1.ContainerdRestartSIGHUP
	}
	return nodev1alpha1.ContainerdRestartNone
}

// Unknown evidence is sticky. A newer template or name-only advertisement
// cannot silently turn an unverified old obligation into verified authority.
func recordLegacyTarget(profile *nodev1alpha1.NodeProfile, target nodev1alpha1.NodeTarget) {
	for i := range profile.Status.Targets {
		prior := &profile.Status.Targets[i]
		if prior.Name != target.Name {
			continue
		}
		if prior.UID != target.UID {
			prior.ContainerdRestart = ""
		} else {
			prior.ContainerdRestart = mergeLegacyMode(prior.ContainerdRestart, target.ContainerdRestart)
		}
		return
	}
	profile.Status.Targets = append(profile.Status.Targets, target)
}

func migrationPodNames(spec *corev1.PodSpec, nodes []corev1.Node) ([]string, bool) {
	if spec.NodeName != "" {
		return []string{spec.NodeName}, true
	}
	var names []string
	bounded := false
	if spec.Affinity != nil && spec.Affinity.NodeAffinity != nil &&
		spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
		bounded = len(terms) > 0
		for _, term := range terms {
			restricted := false
			for _, requirement := range term.MatchFields {
				if requirement.Key == "metadata.name" && requirement.Operator == corev1.NodeSelectorOpIn && len(requirement.Values) > 0 {
					restricted = true
					for _, name := range requirement.Values {
						if !slices.Contains(names, name) {
							names = append(names, name)
						}
					}
				}
			}
			bounded = bounded && restricted
		}
	}
	if !bounded {
		for i := range nodes {
			if legacyTemplateMatches(spec, &nodes[i]) && !slices.Contains(names, nodes[i].Name) {
				names = append(names, nodes[i].Name)
			}
		}
	}
	return names, bounded
}

func migrationFencePresent(ds *appsv1.DaemonSet) bool {
	return ds.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType &&
		slices.ContainsFunc(ds.Spec.Template.Spec.SchedulingGates, func(gate corev1.PodSchedulingGate) bool {
			return gate.Name == legacySchedulingGate
		})
}

func legacyPodOwner(owner *metav1.OwnerReference, profile *nodev1alpha1.NodeProfile) bool {
	return owner != nil && owner.APIVersion == appsv1.SchemeGroupVersion.String() && owner.Kind == "DaemonSet" &&
		(owner.Name == brewlet.ProfileDaemonSetName(profile.Name) || owner.Name == brewlet.CleanupDaemonSetName(profile.Name))
}

func legacyPodNodeUID(pod *corev1.Pod, node *corev1.Node) types.UID {
	// Pod bindings store names, not Node UIDs. A Node created after the pod (or
	// in the same timestamp tick) may be a replacement; never infer its UID.
	if node.CreationTimestamp.IsZero() || pod.CreationTimestamp.IsZero() ||
		!node.CreationTimestamp.Before(&pod.CreationTimestamp) {
		return ""
	}
	return node.UID
}

func (r *NodeProfileReconciler) fenceLegacyScheduling(ctx context.Context, profile *nodev1alpha1.NodeProfile, ds *appsv1.DaemonSet) (bool, error) {
	if ds.Namespace != r.Config.Namespace || !metav1.IsControlledBy(ds, profile) {
		return false, fmt.Errorf("refusing to fence a legacy DaemonSet without exact namespace/controller UID authority")
	}
	if !ds.DeletionTimestamp.IsZero() {
		return false, fmt.Errorf("legacy DaemonSet %s is already deleting before its scheduling fence was observed; preserve and verify its original inventory", ds.Name)
	}
	if migrationFencePresent(ds) {
		return ds.Status.ObservedGeneration >= ds.Generation, nil
	}
	base := ds.DeepCopy()
	// Changing selectors would immediately evict old pods, even with OnDelete.
	// A template scheduling gate stops future bindings without erasing old pods.
	ds.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{Type: appsv1.OnDeleteDaemonSetStrategyType}
	if !slices.ContainsFunc(ds.Spec.Template.Spec.SchedulingGates, func(gate corev1.PodSchedulingGate) bool { return gate.Name == legacySchedulingGate }) {
		ds.Spec.Template.Spec.SchedulingGates = append(ds.Spec.Template.Spec.SchedulingGates, corev1.PodSchedulingGate{Name: legacySchedulingGate})
	}
	if err := r.Patch(ctx, ds, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	var fresh appsv1.DaemonSet
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(ds), &fresh); err != nil {
		return false, err
	}
	if fresh.UID != ds.UID || !metav1.IsControlledBy(&fresh, profile) || !migrationFencePresent(&fresh) {
		return false, fmt.Errorf("legacy scheduling fence was not durably preserved; refusing worker teardown")
	}
	// Inventory must be taken AFTER the DaemonSet controller acknowledges this
	// fence, not merely after the API server accepts the template.
	return false, nil
}

func (r *NodeProfileReconciler) initializeOwnership(ctx context.Context, profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) error {
	if profile.Status.OwnershipInitialized && !profile.Status.Migrating {
		return r.profileWriterBarrier(ctx, profile, false)
	}
	base := profile.DeepCopy()
	var sets appsv1.DaemonSetList
	if err := r.apiReader().List(ctx, &sets); err != nil {
		return err
	}
	var freshNodes corev1.NodeList
	if err := r.apiReader().List(ctx, &freshNodes); err != nil {
		return err
	}
	nodes = freshNodes.Items
	var pods corev1.PodList
	if err := r.apiReader().List(ctx, &pods); err != nil {
		return err
	}
	owned := map[types.UID]*appsv1.DaemonSet{}
	legacy := map[types.UID]bool{}
	var blocked error
	for i := range sets.Items {
		ds := &sets.Items[i]
		canonical := ds.Name == brewlet.ProfileDaemonSetName(profile.Name) || ds.Name == brewlet.CleanupDaemonSetName(profile.Name)
		if ds.Namespace != r.Config.Namespace {
			if !metav1.IsControlledBy(ds, profile) {
				continue
			}
			if !canonical {
				return fmt.Errorf("foreign DaemonSet %s/%s has noncanonical ownership; its original operator must repair it", ds.Namespace, ds.Name)
			}
			owned[ds.UID] = ds
			legacy[ds.UID] = true
			blocked = fmt.Errorf("profile-owned legacy DaemonSet %s/%s is outside the operator namespace; its original owner must finish worker teardown before ownership can migrate", ds.Namespace, ds.Name)
			continue
		}
		if !canonical && ds.Labels[brewlet.LabelNodeProfile] != profile.Name && !metav1.IsControlledBy(ds, profile) {
			continue
		}
		if !canonical || !metav1.IsControlledBy(ds, profile) {
			return fmt.Errorf("legacy DaemonSet %s lacks canonical name and exact controller UID authority", ds.Name)
		}
		owned[ds.UID] = ds
		if !claimFenced(ds) || slices.Contains(profile.Status.MigrationDaemonSetUIDs, ds.UID) {
			legacy[ds.UID] = true
		}
	}
	var relevantPods []*corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		uid, _ := legacyLiteralEnv(&pod.Spec, "BREWLET_PROFILE_UID")
		knownOwner := legacyPodOwner(owner, profile) &&
			(owned[owner.UID] != nil || slices.Contains(profile.Status.MigrationDaemonSetUIDs, owner.UID))
		if pod.Labels[brewlet.LabelNodeProfile] != profile.Name && !knownOwner && uid != string(profile.UID) {
			continue
		}
		if pod.Namespace != r.Config.Namespace {
			profile.Status.Migrating = true
			blocked = fmt.Errorf("legacy pod %s/%s is outside the operator namespace; its original owner must finish worker teardown", pod.Namespace, pod.Name)
		} else if claimFencedPod(&pod.Spec) {
			continue
		}
		relevantPods = append(relevantPods, pod)
		profile.Status.Migrating = true
		if knownOwner && owned[owner.UID] != nil {
			legacy[owner.UID] = true
		}
	}
	for i := range sets.Items {
		ds := &sets.Items[i]
		if !legacy[ds.UID] {
			continue
		}
		profile.Status.Migrating = true
		if !slices.Contains(profile.Status.MigrationDaemonSetUIDs, ds.UID) {
			profile.Status.MigrationDaemonSetUIDs = append(profile.Status.MigrationDaemonSetUIDs, ds.UID)
		}
		mode, _, err := legacyPolicy(&ds.Spec.Template.Spec, profile)
		if err != nil {
			blocked = fmt.Errorf("DaemonSet %s: %w", ds.Name, err)
		}
		for j := range nodes {
			if legacyTemplateMatches(&ds.Spec.Template.Spec, &nodes[j]) {
				recordLegacyTarget(profile, nodev1alpha1.NodeTarget{Name: nodes[j].Name, UID: nodes[j].UID, ContainerdRestart: mode})
			}
		}
	}
	// A matching live pod can attest an advertisement's profile incarnation
	// and revision. A name-only advertisement cannot provide that authority.
	provenAdvertisements := map[string]string{}
	for _, pod := range relevantPods {
		owner := metav1.GetControllerOf(pod)
		verifiedOwner := legacyPodOwner(owner, profile) && slices.Contains(profile.Status.MigrationDaemonSetUIDs, owner.UID)
		mode, generation, err := legacyPolicy(&pod.Spec, profile)
		if !verifiedOwner || err != nil {
			mode = ""
			blocked = fmt.Errorf("legacy pod %s lacks verified controller UID or cleanup policy; restore its original provenance before migration", pod.Name)
		}
		names, bounded := migrationPodNames(&pod.Spec, nodes)
		if !bounded {
			blocked = fmt.Errorf("pending legacy pod %s has no finite metadata.name affinity; preserve its evidence and restore a verifiable node binding", pod.Name)
			mode = ""
		}
		for _, name := range names {
			target := nodev1alpha1.NodeTarget{Name: name, ContainerdRestart: mode}
			for j := range nodes {
				if nodes[j].Name == name && verifiedOwner && err == nil {
					target.UID = legacyPodNodeUID(pod, &nodes[j])
					if target.UID != "" && pod.Spec.NodeName != "" {
						provenAdvertisements[name] = generation
					}
				}
			}
			recordLegacyTarget(profile, target)
		}
	}
	for i := range nodes {
		node := &nodes[i]
		if node.Annotations[brewlet.AnnotationProfile] != profile.Name {
			continue
		}
		known := false
		for _, target := range base.Status.Targets {
			known = known || (target.Name == node.Name && target.UID == node.UID && target.ContainerdRestart != "")
		}
		if known || (provenAdvertisements[node.Name] != "" &&
			provenAdvertisements[node.Name] == node.Annotations[brewlet.AnnotationProfileGeneration]) {
			continue
		}
		profile.Status.Migrating = true
		recordLegacyTarget(profile, nodev1alpha1.NodeTarget{Name: node.Name})
		blocked = fmt.Errorf("node %s advertises legacy host state but its profile UID/revision cleanup policy is unverified; restore original ownership and cleanup-policy records", node.Name)
	}
	profile.Status.OwnershipInitialized = true
	if profile.Status.Migrating {
		if profile.Status.ProvisioningSpec == nil {
			profile.Status.ProvisioningSpec = profile.Spec.DeepCopy()
			profile.Status.ProvisioningGeneration = profile.Generation
		}
		for i := range sets.Items {
			if legacy[sets.Items[i].UID] {
				profile.Status.ProvisioningSpec.Tolerations = provisioningSnapshot(profile.Status.ProvisioningSpec,
					&nodev1alpha1.NodeProfileSpec{Tolerations: sets.Items[i].Spec.Template.Spec.Tolerations}).Tolerations
			}
		}
		for _, pod := range relevantPods {
			profile.Status.ProvisioningSpec.Tolerations = provisioningSnapshot(profile.Status.ProvisioningSpec,
				&nodev1alpha1.NodeProfileSpec{Tolerations: pod.Spec.Tolerations}).Tolerations
		}
	}
	if !reflect.DeepEqual(base.Status, profile.Status) {
		if err := r.persistOwnershipStatus(ctx, profile); err != nil {
			return err
		}
	}
	if !profile.Status.Migrating {
		return nil
	}
	fenced := true
	for i := range sets.Items {
		ds := &sets.Items[i]
		if !legacy[ds.UID] {
			continue
		}
		if ds.Namespace != r.Config.Namespace {
			continue
		}
		// Once our durably inventoried, observed fence starts foreground
		// teardown, keep waiting without trying to recreate or refence it.
		if !ds.DeletionTimestamp.IsZero() && migrationFencePresent(ds) && ds.Status.ObservedGeneration >= ds.Generation {
			continue
		}
		ready, err := r.fenceLegacyScheduling(ctx, profile, ds)
		if err != nil {
			return err
		}
		fenced = fenced && ready
	}
	if blocked != nil {
		return &migrationEvidenceError{message: blocked.Error()}
	}
	if len(profile.Status.Targets) == 0 {
		return &migrationEvidenceError{message: "legacy host history has no verifiable target inventory; restore its original node/cleanup-policy records"}
	}
	for _, target := range profile.Status.Targets {
		if target.UID == "" || target.ContainerdRestart == "" {
			return &migrationEvidenceError{message: fmt.Sprintf("legacy target %s has unverified Node UID or cleanup policy; retain its evidence and restore original ownership records", target.Name)}
		}
	}
	if !fenced {
		return fmt.Errorf("legacy scheduling fence saved; waiting for DaemonSet observedGeneration before final inventory and worker teardown")
	}
	provisioner, err := r.deleteProfileDaemonSetIfExists(ctx, profile)
	if err != nil {
		return err
	}
	cleanup, err := r.deleteCleanupDaemonSetIfExists(ctx, profile)
	if err != nil {
		return err
	}
	if provisioner || cleanup || len(relevantPods) > 0 {
		return fmt.Errorf("legacy target snapshot saved; waiting for all unfenced profile workers to terminate")
	}
	for _, target := range profile.Status.Targets {
		if err := r.claimTarget(ctx, profile, target, true); err != nil {
			return err
		}
	}
	for i := range profile.Status.Targets {
		profile.Status.Targets[i].Claimed = true
	}
	profile.Status.Migrating = false
	profile.Status.MigrationDaemonSetUIDs = nil
	return r.persistOwnershipStatus(ctx, profile)
}
