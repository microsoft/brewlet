// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func detachedObligation(p *nodev1alpha1.NodeProfile, target nodev1alpha1.NodeTarget) *nodev1alpha1.NodeRetirement {
	for i := range p.Status.DetachedRetirements {
		if includesTarget(p.Status.DetachedRetirements[i].Targets, target) {
			return &p.Status.DetachedRetirements[i]
		}
	}
	return nil
}

// Absence detaches scheduling membership, not the obligation. Stop old workers
// before committing the move, and read it back before any new claim is issued.
func (r *NodeProfileReconciler) detachMissingTargets(ctx context.Context, p *nodev1alpha1.NodeProfile) error {
	var missing []nodev1alpha1.NodeTarget
	for _, target := range p.Status.Retirement.Targets {
		var node corev1.Node
		err := r.apiReader().Get(ctx, types.NamespacedName{Name: target.Name}, &node)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if target.Claimed && (apierrors.IsNotFound(err) || node.UID != target.UID) {
			missing = append(missing, target)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if _, err := r.deleteCleanupDaemonSetIfExists(ctx, p); err != nil {
		return err
	}
	if err := r.recoveryWorkersGone(ctx, p); err != nil {
		return err
	}
	for _, target := range missing {
		record := p.Status.Retirement.DeepCopy()
		record.Targets = []nodev1alpha1.NodeTarget{target}
		record.Phase = nodev1alpha1.RetirementMissing
		if prior := detachedObligation(p, target); prior != nil {
			if !reflect.DeepEqual(prior, record) {
				return fmt.Errorf("detached obligation changed for %s (%s)", target.Name, target.UID)
			}
			continue
		}
		p.Status.DetachedRetirements = append(p.Status.DetachedRetirements, *record)
	}
	// A CRD that prunes the new field must leave the old ledger intact.
	if err := r.persistOwnershipStatus(ctx, p); err != nil {
		return err
	}
	withoutMissing := func(targets []nodev1alpha1.NodeTarget) []nodev1alpha1.NodeTarget {
		var kept []nodev1alpha1.NodeTarget
		for _, target := range targets {
			if !includesTarget(missing, target) {
				kept = append(kept, target)
			}
		}
		return kept
	}
	p.Status.Targets = withoutMissing(p.Status.Targets)
	p.Status.Retirement.Targets = withoutMissing(p.Status.Retirement.Targets)
	if len(p.Status.Retirement.Targets) == 0 {
		p.Status.Retirement = nil
	}
	if len(p.Status.Targets) == 0 {
		p.Status.ProvisioningSpec = nil
		p.Status.ProvisioningGeneration = 0
	}
	setRetirementPending(p, fmt.Sprintf("%d missing-host obligations require authorized retirement evidence; active provisioning remains independent", len(p.Status.DetachedRetirements)))
	return r.persistOwnershipStatus(ctx, p)
}

func setRetirementPending(p *nodev1alpha1.NodeProfile, message string) {
	status, reason := metav1.ConditionTrue, nodev1alpha1.ReasonCleanupBlocked
	if len(p.Status.DetachedRetirements) == 0 {
		status, reason, message = metav1.ConditionFalse, nodev1alpha1.ReasonCleanupResolved, "No detached cleanup obligations remain"
	}
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{
		Type: nodev1alpha1.ConditionRetirementPending, Status: status, Reason: reason,
		Message: message, ObservedGeneration: p.Generation,
	})
}

// Failures here retain the history and surface independently of active readiness.
// No mutation from a failed checkpoint may be used for subsequent provisioning.
func (r *NodeProfileReconciler) reconcileDetachedRetirements(ctx context.Context, p *nodev1alpha1.NodeProfile) error {
	var blocked error
	for _, record := range append([]nodev1alpha1.NodeRetirement(nil), p.Status.DetachedRetirements...) {
		target := record.Targets[0]
		current := detachedObligation(p, target)
		if current == nil || len(current.Targets) != 1 {
			return fmt.Errorf("detached obligation changed concurrently for %s (%s)", target.Name, target.UID)
		}
		target = current.Targets[0]
		if _, err := r.resolveCleanupTargets(ctx, p, []nodev1alpha1.NodeTarget{target}); err != nil {
			blocked = errors.Join(blocked, err)
			var fresh nodev1alpha1.NodeProfile
			if err := r.apiReader().Get(ctx, types.NamespacedName{Name: p.Name}, &fresh); err != nil {
				return err
			}
			if fresh.UID != p.UID || fresh.Generation != p.Generation || !fresh.DeletionTimestamp.Equal(p.DeletionTimestamp) {
				return fmt.Errorf("profile lifecycle changed during detached retirement resolution")
			}
			*p = fresh
			continue
		}
		current = detachedObligation(p, target)
		if err := r.finishExternalRetirements(ctx, p, current.Targets); err != nil {
			return err
		}
		for i := range p.Status.DetachedRetirements {
			if includesTarget(p.Status.DetachedRetirements[i].Targets, target) {
				p.Status.DetachedRetirements = append(p.Status.DetachedRetirements[:i], p.Status.DetachedRetirements[i+1:]...)
				break
			}
		}
		setRetirementPending(p, fmt.Sprintf("%d missing-host cleanup obligations remain", len(p.Status.DetachedRetirements)))
		if err := r.persistOwnershipStatus(ctx, p); err != nil {
			return err
		}
	}
	if blocked != nil {
		base := p.Status.DeepCopy()
		setRetirementPending(p, blocked.Error())
		if equalStatus(base, &p.Status) {
			return nil
		}
		return r.persistOwnershipStatus(ctx, p)
	}
	return nil
}

type retiredHostError struct{ message string }

func (e *retiredHostError) Error() string { return e.message }

func sameHost(target nodev1alpha1.NodeTarget, node *corev1.Node) bool {
	return target.UID == node.UID ||
		(target.ProviderID != "" && target.ProviderID == node.Spec.ProviderID) ||
		(target.SystemUUID != "" && strings.EqualFold(target.SystemUUID, node.Status.NodeInfo.SystemUUID))
}

// Include resolved, independent evidence so deleting a profile cannot make a
// previously retired host eligible for silent adoption by a new profile.
func (r *NodeProfileReconciler) retiredHostConflict(ctx context.Context, node *corev1.Node) error {
	var profiles nodev1alpha1.NodeProfileList
	if err := r.apiReader().List(ctx, &profiles); err != nil {
		return err
	}
	for _, p := range profiles.Items {
		for _, record := range p.Status.DetachedRetirements {
			for _, target := range record.Targets {
				if sameHost(target, node) {
					return &retiredHostError{fmt.Sprintf("node %s conflicts with detached host %s (%s) of profile %s (%s); restore and resolve the original cleanup obligation before host reuse", node.Name, target.Name, target.UID, p.Name, p.UID)}
				}
			}
		}
	}
	var evidence nodev1alpha1.NodeRetirementEvidenceList
	if err := r.apiReader().List(ctx, &evidence); err != nil {
		return err
	}
	for _, e := range evidence.Items {
		if e.Status.Phase != nodev1alpha1.EvidenceAccepted && e.Status.Phase != nodev1alpha1.EvidenceResolved {
			continue
		}
		target := nodev1alpha1.NodeTarget{UID: e.Spec.NodeUID, ProviderID: e.Spec.ProviderID, SystemUUID: e.Spec.SystemUUID}
		if sameHost(target, node) {
			return &retiredHostError{fmt.Sprintf("node %s conflicts with retained retirement evidence %s (%s); refusing host reuse", node.Name, e.Name, e.UID)}
		}
	}
	return nil
}

func (r *NodeProfileReconciler) excludeRetiredHosts(ctx context.Context, p *nodev1alpha1.NodeProfile, wanted []nodev1alpha1.NodeTarget) ([]nodev1alpha1.NodeTarget, error) {
	var eligible []nodev1alpha1.NodeTarget
	for _, target := range wanted {
		var node corev1.Node
		if err := r.apiReader().Get(ctx, types.NamespacedName{Name: target.Name}, &node); err != nil {
			return nil, err
		}
		if err := r.retiredHostConflict(ctx, &node); err != nil {
			// API failures must not masquerade as a known host conflict.
			var conflict *retiredHostError
			if !errors.As(err, &conflict) {
				return nil, err
			}
			r.Recorder.Eventf(p, corev1.EventTypeWarning, nodev1alpha1.ReasonOwnershipConflict, "%s", err)
			if node.Labels[brewlet.LabelNodeOwner] == string(p.UID) &&
				node.Annotations[brewlet.AnnotationNodeOwner] == p.Name &&
				node.Annotations[brewlet.AnnotationProfile] == p.Name {
				base := node.DeepCopy()
				removeNodeAdvertisements(&node)
				if err := r.Patch(ctx, &node, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					return nil, err
				}
			}
			continue
		}
		eligible = append(eligible, target)
	}
	return eligible, nil
}

func workerExcludesTarget(spec *corev1.PodSpec, target nodev1alpha1.NodeTarget) bool {
	uids, ok := literalProvisionerEnv(spec, "BREWLET_TARGET_UIDS")
	if !ok {
		return false
	}
	for _, uid := range strings.Fields(uids) {
		if uid == string(target.UID) {
			return false
		}
	}
	return true
}
