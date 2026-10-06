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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (r *NodeProfileReconciler) checkWriterOwnership(ctx context.Context, profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) error {
	for _, node := range nodes {
		if node.Annotations[brewlet.AnnotationProfile] == profile.Name &&
			node.Labels[brewlet.LabelNodeOwner] == "" {
			return fmt.Errorf("node %s advertises profile %s without UID-bound ownership; refusing to adopt it", node.Name, profile.Name)
		}
	}
	return r.profileWriterBarrier(ctx, profile)
}

func (r *NodeProfileReconciler) initializeOwnership(ctx context.Context, profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) error {
	if err := r.checkWriterOwnership(ctx, profile, nodes); err != nil {
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
		if isOwned {
			if err := validateWorkerContainerdPolicy(&ds.Spec.Template.Spec); err != nil {
				return fmt.Errorf("DaemonSet %s/%s: %w", ds.Namespace, ds.Name, err)
			}
		}
		if isOwned && ds.Namespace != r.Config.Namespace {
			conflict = fmt.Errorf("profile writer DaemonSet %s/%s is outside the operator namespace; its original owner must finish teardown before provisioning or cleanup", ds.Namespace, ds.Name)
		} else if relevant && (!canonical || !isOwned) {
			conflict = fmt.Errorf("DaemonSet %s/%s lacks canonical name and exact controller UID authority; its original owner must finish teardown", ds.Namespace, ds.Name)
		} else if writer && !claimFenced(ds) {
			conflict = fmt.Errorf("standalone DaemonSet %s/%s lacks a profile identity and must be removed by its owner before node ownership can proceed; automatic adoption is unsupported", ds.Namespace, ds.Name)
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
		if relevant {
			if err := validateWorkerContainerdPolicy(&pod.Spec); err != nil {
				return fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
			}
		}
		if relevant && pod.Namespace != r.Config.Namespace {
			conflict = fmt.Errorf("profile writer pod %s/%s is outside the operator namespace; its original owner must finish teardown before provisioning or cleanup", pod.Namespace, pod.Name)
		} else if profileWriter(pod.Labels) && !claimFencedPod(&pod.Spec) {
			conflict = fmt.Errorf("standalone writer pod %s/%s lacks a profile identity and must be removed by its owner; automatic adoption is unsupported", pod.Namespace, pod.Name)
		}
	}
	return conflict
}

func validateWorkerContainerdPolicy(spec *corev1.PodSpec) error {
	mode, ok := literalProvisionerEnv(spec, "BREWLET_CONTAINERD_RESTART")
	if !ok {
		return fmt.Errorf("worker has unverifiable cleanup restart policy; recreate it from the current operator")
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
