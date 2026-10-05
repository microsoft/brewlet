// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

type preClaimStateError struct {
	evidence  string
	persisted bool
}

func (e *preClaimStateError) Error() string {
	if e.persisted {
		return e.evidence
	}
	return e.evidence + "; automatic pre-claim migration is unsupported: preserve workers, finalizers, and ownership evidence; restore the original release's compatible components to finish host cleanup and worker teardown before reinstalling"
}

// HasUnsupportedPreClaimState recognizes evidence, not authority to migrate or clean hosts.
func HasUnsupportedPreClaimState(profile *nodev1alpha1.NodeProfile) bool {
	for _, target := range profile.Status.Targets {
		if target.UID == "" {
			return true
		}
	}
	condition := meta.FindStatusCondition(profile.Status.Conditions, nodev1alpha1.ConditionReady)
	return profile.Status.Migrating || len(profile.Status.MigrationDaemonSetUIDs) != 0 ||
		(condition != nil && (condition.Reason == nodev1alpha1.ReasonUnsupportedPreClaimState || condition.Reason == "OwnershipMigration"))
}

func (r *NodeProfileReconciler) compatibilityStatus(ctx context.Context, profile *nodev1alpha1.NodeProfile, err error) (ctrl.Result, error) {
	return r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonCleanupBlocked, err)
}

func (r *NodeProfileReconciler) checkOwnershipCompatibility(ctx context.Context, profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) error {
	if HasUnsupportedPreClaimState(profile) {
		if condition := meta.FindStatusCondition(profile.Status.Conditions, nodev1alpha1.ConditionReady); condition != nil &&
			condition.Reason == nodev1alpha1.ReasonUnsupportedPreClaimState {
			return &preClaimStateError{evidence: condition.Message, persisted: true}
		}
		for _, target := range profile.Status.Targets {
			if target.UID == "" {
				return &preClaimStateError{evidence: fmt.Sprintf("NodeProfile %s records target %s without a verified Node UID", profile.Name, target.Name)}
			}
		}
		return &preClaimStateError{evidence: fmt.Sprintf("NodeProfile %s (%s) retains unresolved pre-claim evidence", profile.Name, profile.UID)}
	}
	for _, node := range nodes {
		if node.Annotations[brewlet.AnnotationProfile] == profile.Name &&
			node.Labels[brewlet.LabelNodeOwner] == "" {
			return &preClaimStateError{evidence: fmt.Sprintf("node %s advertises profile %s without UID-bound ownership", node.Name, profile.Name)}
		}
	}
	return r.profileWriterBarrier(ctx, profile)
}

func (r *NodeProfileReconciler) initializeOwnership(ctx context.Context, profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) error {
	if err := r.checkOwnershipCompatibility(ctx, profile, nodes); err != nil {
		return err
	}
	if profile.Status.OwnershipInitialized {
		return nil
	}
	profile.Status.OwnershipInitialized = true
	return r.persistOwnershipStatus(ctx, profile)
}

// Inspection can establish a conflict, never authority to adopt or mutate a worker.
func (r *NodeProfileReconciler) profileWriterBarrier(ctx context.Context, profile *nodev1alpha1.NodeProfile) error {
	var sets appsv1.DaemonSetList
	if err := r.apiReader().List(ctx, &sets); err != nil {
		return fmt.Errorf("listing cluster-wide DaemonSets before host ownership: %w", err)
	}
	owned := make(map[types.UID]bool)
	var conflict error
	for i := range sets.Items {
		ds := &sets.Items[i]
		isOwned := metav1.IsControlledBy(ds, profile)
		owned[ds.UID] = isOwned
		canonical := ds.Name == brewlet.ProfileDaemonSetName(profile.Name) || ds.Name == brewlet.CleanupDaemonSetName(profile.Name)
		uid, _ := literalProvisionerEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_UID")
		relevant := isOwned || canonical || (uid != "" && uid == string(profile.UID)) ||
			ds.Labels[brewlet.LabelNodeProfile] == profile.Name || ds.Spec.Template.Labels[brewlet.LabelNodeProfile] == profile.Name
		writer := relevant || profileWriter(ds.Labels) || profileWriter(ds.Spec.Template.Labels) || ds.Name == brewlet.ProvisionerName
		if relevant && !claimFenced(ds) {
			return &preClaimStateError{evidence: fmt.Sprintf("pre-claim DaemonSet %s/%s (%s) conflicts with profile %s", ds.Namespace, ds.Name, ds.UID, profile.Name)}
		}
		if isOwned && claimFenced(ds) {
			if err := validateWorkerContainerdPolicy(&ds.Spec.Template.Spec); err != nil {
				return fmt.Errorf("DaemonSet %s/%s: %w", ds.Namespace, ds.Name, err)
			}
		}
		if isOwned && ds.Namespace != r.Config.Namespace {
			conflict = fmt.Errorf("profile writer DaemonSet %s/%s is outside the operator namespace; its original owner must finish teardown before provisioning or cleanup", ds.Namespace, ds.Name)
		} else if relevant && (!canonical || !isOwned) {
			conflict = fmt.Errorf("DaemonSet %s/%s lacks canonical name and exact controller UID authority; its original owner must finish teardown", ds.Namespace, ds.Name)
		} else if writer && !claimFenced(ds) {
			conflict = fmt.Errorf("pre-claim or standalone DaemonSet %s/%s must be safely deprovisioned by its original owner before node ownership can proceed; automatic adoption is unsupported", ds.Namespace, ds.Name)
		}
	}
	var pods corev1.PodList
	if err := r.apiReader().List(ctx, &pods); err != nil {
		return fmt.Errorf("listing cluster-wide pods before host ownership: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		uid, _ := literalProvisionerEnv(&pod.Spec, "BREWLET_PROFILE_UID")
		owner := metav1.GetControllerOf(pod)
		knownOwner := owner != nil && owned[owner.UID]
		namedOwner := owner != nil && owner.APIVersion == appsv1.SchemeGroupVersion.String() && owner.Kind == "DaemonSet" &&
			(owner.Name == brewlet.ProfileDaemonSetName(profile.Name) || owner.Name == brewlet.CleanupDaemonSetName(profile.Name))
		relevant := knownOwner || namedOwner || (uid != "" && uid == string(profile.UID)) || pod.Labels[brewlet.LabelNodeProfile] == profile.Name
		if relevant && !claimFencedPod(&pod.Spec) {
			return &preClaimStateError{evidence: fmt.Sprintf("pre-claim pod %s/%s (%s) conflicts with profile %s", pod.Namespace, pod.Name, pod.UID, profile.Name)}
		}
		if relevant && claimFencedPod(&pod.Spec) {
			if err := validateWorkerContainerdPolicy(&pod.Spec); err != nil {
				return fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}
		if relevant && pod.Namespace != r.Config.Namespace {
			conflict = fmt.Errorf("profile writer pod %s/%s is outside the operator namespace; its original owner must finish teardown before provisioning or cleanup", pod.Namespace, pod.Name)
		} else if profileWriter(pod.Labels) && !claimFencedPod(&pod.Spec) {
			conflict = fmt.Errorf("pre-claim writer pod %s/%s must be safely deprovisioned by its original owner; automatic adoption is unsupported", pod.Namespace, pod.Name)
		}
	}
	if conflict != nil {
		return conflict
	}
	var profiles nodev1alpha1.NodeProfileList
	if err := r.apiReader().List(ctx, &profiles); err != nil {
		return fmt.Errorf("listing profiles before host ownership: %w", err)
	}
	for i := range profiles.Items {
		other := &profiles.Items[i]
		if HasUnsupportedPreClaimState(other) {
			if other.UID == profile.UID {
				return &preClaimStateError{evidence: fmt.Sprintf("NodeProfile %s retains unresolved pre-claim evidence", other.Name)}
			}
			return fmt.Errorf("NodeProfile %s (%s) retains unresolved pre-claim host obligations; its original release must finish cleanup before new host ownership can proceed", other.Name, other.UID)
		}
	}
	return nil
}

func validateWorkerContainerdPolicy(spec *corev1.PodSpec) error {
	mode, ok := literalProvisionerEnv(spec, "BREWLET_CONTAINERD_RESTART")
	if !ok {
		return fmt.Errorf("worker has unverifiable cleanup restart policy; preserve its evidence and recover with compatible components")
	}
	return validateContainerdRestart("worker BREWLET_CONTAINERD_RESTART", mode)
}

func literalProvisionerEnv(spec *corev1.PodSpec, name string) (string, bool) {
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
