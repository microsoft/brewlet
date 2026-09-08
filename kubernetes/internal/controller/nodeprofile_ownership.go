// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *NodeProfileReconciler) persistOwnershipStatus(ctx context.Context, profile *nodev1alpha1.NodeProfile) error {
	identity := profile.ObjectMeta.DeepCopy()
	expected := profile.Status.DeepCopy()
	want, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	if err := r.Status().Update(ctx, profile); err != nil {
		return err
	}
	// Decoding an update response into profile may leave omitted/pruned fields
	// populated. Only a fresh zero-valued object proves what the API persisted.
	var persisted nodev1alpha1.NodeProfile
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(profile), &persisted); err != nil {
		return fmt.Errorf("confirming durable NodeProfile checkpoint: %w", err)
	}
	if persisted.UID != identity.UID || persisted.Generation != identity.Generation ||
		!reflect.DeepEqual(persisted.DeletionTimestamp, identity.DeletionTimestamp) {
		return apierrors.NewConflict(nodev1alpha1.GroupVersion.WithResource("nodeprofiles").GroupResource(), profile.Name,
			fmt.Errorf("profile identity, generation, or deletion state changed while confirming its checkpoint"))
	}
	got, err := json.Marshal(persisted.Status)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, got) {
		return fmt.Errorf("NodeProfile checkpoint was not durably preserved (status fields may have been pruned); apply the current NodeProfile CRD before upgrading the operator (Helm does not upgrade existing CRDs)")
	}
	persisted.DeepCopyInto(profile)
	return nil
}

func desiredTargets(profile *nodev1alpha1.NodeProfile, profiles []nodev1alpha1.NodeProfile, nodes []corev1.Node) []nodev1alpha1.NodeTarget {
	var targets []nodev1alpha1.NodeTarget
	key := resolvePoolKey(profile, nodes)
	for i := range nodes {
		node := &nodes[i]
		if !profileClaimsNode(profile, key, nil, node) {
			continue
		}
		reserved := false
		if isDefaultProfile(profile) {
			for j := range profiles {
				other := &profiles[j]
				if other.Name != profile.Name && !isDefaultProfile(other) &&
					nodeInPool(node, resolvePoolKey(other, nodes), other.Spec.NodePool.Names) {
					reserved = true
					break
				}
			}
		}
		if !reserved {
			targets = append(targets, nodev1alpha1.NodeTarget{Name: node.Name, UID: node.UID})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	return targets
}

func sameTarget(a, b nodev1alpha1.NodeTarget) bool { return a.Name == b.Name && a.UID == b.UID }

func includesTarget(targets []nodev1alpha1.NodeTarget, target nodev1alpha1.NodeTarget) bool {
	for _, t := range targets {
		if sameTarget(t, target) {
			return true
		}
	}
	return false
}

func nodeClaimedBy(node *corev1.Node, profile *nodev1alpha1.NodeProfile, target nodev1alpha1.NodeTarget) bool {
	return target.UID != "" && node.UID == target.UID &&
		node.Labels[brewlet.LabelNodeOwner] == string(profile.UID) &&
		node.Labels[brewlet.LabelNodeIdentity] == string(target.UID) &&
		node.Annotations[brewlet.AnnotationNodeOwner] == profile.Name
}

// HasNodeProfileCleanupObligations is deliberately conservative for admission:
// only the reconciler can prove an unclaimed intent has no corresponding node
// claim or writer, then remove that intent before finalization.
func HasNodeProfileCleanupObligations(profile *nodev1alpha1.NodeProfile) bool {
	condition := meta.FindStatusCondition(profile.Status.Conditions, nodev1alpha1.ConditionReady)
	return len(profile.Status.Targets) > 0 || profile.Status.Retirement != nil ||
		hasProvisioningHistory(profile) || profile.Status.Migrating ||
		(condition != nil && condition.Reason == nodev1alpha1.ReasonCleanupBlocked)
}

func hasProvisioningHistory(profile *nodev1alpha1.NodeProfile) bool {
	return profile.Status.ProvisioningSpec != nil || profile.Status.ProvisioningGeneration != 0
}

func invalidProfileHasHostOwnership(profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) bool {
	if hasProvisioningHistory(profile) {
		return true
	}
	for _, target := range profile.Status.Targets {
		if target.Claimed {
			return true
		}
	}
	if profile.Status.Retirement != nil || (profile.Status.Migrating && len(profile.Status.Targets) > 0) {
		return true
	}
	for _, node := range nodes {
		// Even a damaged identity fence must not be mistaken for no ownership.
		if profile.UID != "" && node.Labels[brewlet.LabelNodeOwner] == string(profile.UID) {
			return true
		}
	}
	return false
}

func unrecordedNodeClaim(profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) error {
	if hasProvisioningHistory(profile) && len(profile.Status.Targets) == 0 {
		return fmt.Errorf("profile has saved provisioning history but no durable targets; restore its original target/cleanup-policy records before proceeding")
	}
	for _, node := range nodes {
		if profile.UID == "" || node.Labels[brewlet.LabelNodeOwner] != string(profile.UID) {
			continue
		}
		recorded := false
		for _, target := range profile.Status.Targets {
			recorded = recorded || (target.Name == node.Name && string(target.UID) == node.Labels[brewlet.LabelNodeIdentity])
		}
		if !recorded {
			return fmt.Errorf("node %s has this profile's ownership claim but no matching durable target; restore its original target/cleanup-policy record before proceeding", node.Name)
		}
	}
	return nil
}

func (r *NodeProfileReconciler) ownershipBlocked(ctx context.Context, profile *nodev1alpha1.NodeProfile, reason string, cause error) (ctrl.Result, error) {
	base := profile.DeepCopy()
	profile.Status.ObservedGeneration = profile.Generation
	profile.Status.ReadyNodes = 0
	meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type: nodev1alpha1.ConditionReady, Status: metav1.ConditionFalse, Reason: reason,
		Message: cause.Error(), ObservedGeneration: profile.Generation,
	})
	if !equalStatus(&base.Status, &profile.Status) {
		if err := r.persistOwnershipStatus(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *NodeProfileReconciler) reconcileTargets(ctx context.Context, profile *nodev1alpha1.NodeProfile, profiles []nodev1alpha1.NodeProfile, nodes []corev1.Node) (bool, ctrl.Result, error) {
	if err := r.initializeOwnership(ctx, profile, nodes); err != nil {
		result, err := r.migrationStatus(ctx, profile, err)
		return true, result, err
	}
	if err := unrecordedNodeClaim(profile, nodes); err != nil {
		result, err := r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonCleanupBlocked, err)
		return true, result, err
	}
	if profile.Status.Retirement != nil {
		result, err := r.reconcileRetirement(ctx, profile)
		return true, result, err
	}
	wanted := desiredTargets(profile, profiles, nodes)
	base := profile.DeepCopy()
	var departing, retained []nodev1alpha1.NodeTarget
	for _, target := range profile.Status.Targets {
		// Recover a claim committed just before a lost status acknowledgement.
		for i := range nodes {
			if nodes[i].Name == target.Name && nodeClaimedBy(&nodes[i], profile, target) {
				target.Claimed = true
			}
		}
		if !includesTarget(wanted, target) {
			if target.Claimed {
				departing = append(departing, target)
				retained = append(retained, target)
			}
		} else {
			retained = append(retained, target)
		}
	}
	profile.Status.Targets = retained
	if len(departing) > 0 {
		spec := profile.Spec.DeepCopy()
		if profile.Status.ProvisioningSpec != nil {
			spec = profile.Status.ProvisioningSpec.DeepCopy()
		}
		profile.Status.Retirement = &nodev1alpha1.NodeRetirement{
			Targets: departing, Generation: profile.Generation, Spec: *spec,
			Phase: nodev1alpha1.RetirementCleaning,
		}
		meta.RemoveStatusCondition(&profile.Status.Conditions, nodev1alpha1.ConditionCleanupComplete)
		if err := r.persistOwnershipStatus(ctx, profile); err != nil {
			return true, ctrl.Result{}, err
		}
		result, err := r.reconcileRetirement(ctx, profile)
		return true, result, err
	}
	for _, target := range wanted {
		if !includesTarget(profile.Status.Targets, target) {
			profile.Status.Targets = append(profile.Status.Targets, target)
		}
	}
	// This ledger precedes the node CAS and any template that can start writers.
	if !reflect.DeepEqual(base.Status, profile.Status) {
		if err := r.persistOwnershipStatus(ctx, profile); err != nil {
			return true, ctrl.Result{}, err
		}
	}
	for i := range profile.Status.Targets {
		if err := r.claimTarget(ctx, profile, profile.Status.Targets[i], false); err != nil {
			result, err := r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonOwnershipConflict, err)
			return true, result, err
		}
	}
	base = profile.DeepCopy()
	for i := range profile.Status.Targets {
		profile.Status.Targets[i].Claimed = true
		priorMode := profile.Status.Targets[i].ContainerdRestart
		mode := containerdRestart(profile)
		if priorMode != "" && mode == nodev1alpha1.ContainerdRestartNone {
			mode = priorMode
		}
		profile.Status.Targets[i].ContainerdRestart = mode
	}
	if len(profile.Status.Targets) > 0 {
		profile.Status.ProvisioningSpec = provisioningSnapshot(profile.Status.ProvisioningSpec, &profile.Spec)
		profile.Status.ProvisioningGeneration = profile.Generation
	}
	if !reflect.DeepEqual(base.Status, profile.Status) {
		if err := r.persistOwnershipStatus(ctx, profile); err != nil {
			return true, ctrl.Result{}, err
		}
	}
	return false, ctrl.Result{}, nil
}

func provisioningSnapshot(prior, current *nodev1alpha1.NodeProfileSpec) *nodev1alpha1.NodeProfileSpec {
	snapshot := current.DeepCopy()
	if prior != nil {
		// A later label-only rollout cannot erase the obligation to reverse
		// containerd mutations already authorized by an earlier writer.
		if current.Rollout.ContainerdRestart == nodev1alpha1.ContainerdRestartNone &&
			prior.Rollout.ContainerdRestart != nodev1alpha1.ContainerdRestartNone {
			snapshot.Rollout.ContainerdRestart = prior.Rollout.ContainerdRestart
		}
		for _, toleration := range prior.Tolerations {
			if !slices.ContainsFunc(snapshot.Tolerations, func(t corev1.Toleration) bool { return reflect.DeepEqual(t, toleration) }) {
				snapshot.Tolerations = append(snapshot.Tolerations, *toleration.DeepCopy())
			}
		}
	}
	return snapshot
}

func (r *NodeProfileReconciler) claimTarget(ctx context.Context, profile *nodev1alpha1.NodeProfile, target nodev1alpha1.NodeTarget, migrating bool) error {
	if err := r.profileWriterBarrier(ctx, profile, true); err != nil {
		return err
	}
	var node corev1.Node
	if err := r.apiReader().Get(ctx, types.NamespacedName{Name: target.Name}, &node); err != nil {
		return fmt.Errorf("target %s (%s) unavailable; restore its identity before proceeding: %w", target.Name, target.UID, err)
	}
	if node.UID != target.UID || target.UID == "" {
		return fmt.Errorf("target %s Node UID changed from %s to %s; refusing host access", target.Name, target.UID, node.UID)
	}
	if nodeClaimedBy(&node, profile, target) {
		return nil
	}
	if owner := node.Labels[brewlet.LabelNodeOwner]; owner != "" {
		return fmt.Errorf("node %s is claimed by profile UID %s; waiting for that owner's cleanup and worker teardown", node.Name, owner)
	}
	if node.Labels[brewlet.LabelNodeIdentity] != "" || node.Annotations[brewlet.AnnotationNodeOwner] != "" {
		return fmt.Errorf("node %s has an incomplete ownership fence; recover its original owner before proceeding", node.Name)
	}
	if target.Claimed {
		return fmt.Errorf("node %s lost its persisted ownership fence; restore the original claim before cleanup", node.Name)
	}
	var profiles nodev1alpha1.NodeProfileList
	if err := r.apiReader().List(ctx, &profiles); err != nil {
		return err
	}
	for _, other := range profiles.Items {
		if other.UID == profile.UID {
			continue
		}
		for _, prior := range other.Status.Targets {
			if prior.Name == node.Name && (prior.Claimed || other.Status.Migrating) {
				return fmt.Errorf("node name %s is still recorded by profile %s (%s); its prior identity must finish cleanup first", node.Name, other.Name, other.UID)
			}
		}
	}
	advertised := node.Annotations[brewlet.AnnotationProfile]
	if (advertised != "" || node.Labels[brewlet.LabelRuntimeReady] != "") &&
		(!migrating || advertised != profile.Name) {
		return fmt.Errorf("node %s has unfenced runtime state from %q; drain and migrate its existing owner first", node.Name, advertised)
	}
	var pods corev1.PodList
	if err := r.apiReader().List(ctx, &pods); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == node.Name && profileWriter(pod.Labels) {
			return fmt.Errorf("node %s still has writer pod %s; waiting for termination before claiming", node.Name, pod.Name)
		}
	}
	base := node.DeepCopy()
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Labels[brewlet.LabelNodeOwner] = string(profile.UID)
	node.Labels[brewlet.LabelNodeIdentity] = string(target.UID)
	node.Annotations[brewlet.AnnotationNodeOwner] = profile.Name
	return r.Patch(ctx, &node, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func profileWriter(labels map[string]string) bool {
	return labels["app"] == brewlet.ProvisionerAppLabel || labels["app"] == "brewlet-cleanup"
}

func claimFenced(ds *appsv1.DaemonSet) bool {
	return claimFencedPod(&ds.Spec.Template.Spec)
}

func claimFencedPod(spec *corev1.PodSpec) bool {
	for _, c := range spec.Containers {
		if c.Name == "provisioner" {
			for _, e := range c.Env {
				if e.Name == "BREWLET_REQUIRE_NODE_CLAIM" && e.Value == "true" {
					return true
				}
			}
		}
	}
	return false
}

func legacyTemplateMatches(spec *corev1.PodSpec, node *corev1.Node) bool {
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
			default:
				// Unknown legacy constraints must not undercount potential targets.
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

func (r *NodeProfileReconciler) validateTargetClaims(ctx context.Context, profile *nodev1alpha1.NodeProfile, targets []nodev1alpha1.NodeTarget) ([]corev1.Node, error) {
	var nodes []corev1.Node
	for _, target := range targets {
		if !target.Claimed {
			continue
		}
		var node corev1.Node
		if err := r.apiReader().Get(ctx, types.NamespacedName{Name: target.Name}, &node); apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("cleanup blocked for %s (%s): original Node object was deleted; recreating its name cannot restore its UID. There is no supported in-place recovery after Node deletion; complete retirement before deleting Node objects: %w", target.Name, target.UID, err)
		} else if err != nil {
			return nil, fmt.Errorf("cleanup blocked for %s (%s): cannot read original Node; restore API access without replacing its recorded UID: %w", target.Name, target.UID, err)
		}
		if node.UID != target.UID {
			return nil, fmt.Errorf("cleanup blocked for %s (%s): Node UID changed to %s; a replacement cannot prove cleanup of the original host. There is no supported in-place recovery after Node replacement; complete retirement before deleting Node objects", target.Name, target.UID, node.UID)
		}
		if !nodeClaimedBy(&node, profile, target) {
			return nil, fmt.Errorf("cleanup blocked for %s (%s): node identity or owner changed; refusing another owner's host", target.Name, target.UID)
		}
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status != corev1.ConditionTrue {
				return nil, fmt.Errorf("cleanup blocked for node %s: NodeReady=%s; restore node connectivity before retiring its runtime", node.Name, condition.Status)
			}
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

func (r *NodeProfileReconciler) releaseTargetClaims(ctx context.Context, profile *nodev1alpha1.NodeProfile, targets []nodev1alpha1.NodeTarget) error {
	for _, target := range targets {
		var node corev1.Node
		if err := r.apiReader().Get(ctx, types.NamespacedName{Name: target.Name}, &node); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if !nodeClaimedBy(&node, profile, target) {
			continue
		}
		base := node.DeepCopy()
		removeNodeAdvertisements(&node)
		delete(node.Labels, brewlet.LabelNodeOwner)
		delete(node.Labels, brewlet.LabelNodeIdentity)
		delete(node.Annotations, brewlet.AnnotationNodeOwner)
		if err := r.Patch(ctx, &node, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

func (r *NodeProfileReconciler) reconcileRetirement(ctx context.Context, profile *nodev1alpha1.NodeProfile) (ctrl.Result, error) {
	retirement := profile.Status.Retirement
	provisioner, err := r.profileProvisionerRemains(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if provisioner {
		if retirement.Phase != nodev1alpha1.RetirementCleaning {
			retirement.Phase = nodev1alpha1.RetirementCleaning
			if err := r.persistOwnershipStatus(ctx, profile); err != nil {
				return ctrl.Result{}, err
			}
		}
		if _, err := r.deleteProfileDaemonSetIfExists(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.deleteCleanupDaemonSet(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
		return r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonRetargeting, fmt.Errorf("retiring %d nodes: waiting for old provisioners to terminate", len(retirement.Targets)))
	}
	if retirement.Phase == nodev1alpha1.RetirementCleaning {
		nodes, err := r.validateTargetClaims(ctx, profile, retirement.Targets)
		if err != nil {
			return r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonCleanupBlocked,
				fmt.Errorf("profile-wide provisioning and upgrades are paused, including retained and new nodes; retained-node claims and runtime advertisements are preserved: %w", err))
		}
		execution := profile.DeepCopy()
		execution.Generation = retirement.Generation
		execution.Spec = *retirement.Spec.DeepCopy()
		execution.Spec.Tolerations = provisioningSnapshot(&retirement.Spec, &profile.Spec).Tolerations
		execution.Status.Targets = append([]nodev1alpha1.NodeTarget(nil), retirement.Targets...)
		done, err := r.ensureCleanupComplete(ctx, execution, "", nil, nodes)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !done {
			return r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonRetargeting, fmt.Errorf("waiting for explicit cleanup completion on all %d retiring nodes", len(retirement.Targets)))
		}
		retirement.Phase = nodev1alpha1.RetirementTeardown
		if err := r.persistOwnershipStatus(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
		retirement = profile.Status.Retirement
	}
	cleanup, err := r.deleteCleanupDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	pods, err := r.profilePodsRemain(ctx, profile.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	provisioner, err = r.profileProvisionerRemains(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if provisioner {
		retirement.Phase = nodev1alpha1.RetirementCleaning
		if err := r.persistOwnershipStatus(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
		return r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonRetargeting, fmt.Errorf("provisioner reappeared during retirement teardown; cleanup must run again"))
	}
	if cleanup || pods {
		return r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonRetargeting, fmt.Errorf("retiring cleanup completed; retaining claims until all workers terminate"))
	}
	if err := r.releaseTargetClaims(ctx, profile, retirement.Targets); err != nil {
		return ctrl.Result{}, err
	}
	var retained []nodev1alpha1.NodeTarget
	for _, target := range profile.Status.Targets {
		if !includesTarget(retirement.Targets, target) {
			retained = append(retained, target)
		}
	}
	profile.Status.Targets = retained
	profile.Status.Retirement = nil
	if len(retained) == 0 {
		profile.Status.ProvisioningSpec = nil
		profile.Status.ProvisioningGeneration = 0
	}
	if err := r.persistOwnershipStatus(ctx, profile); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}
