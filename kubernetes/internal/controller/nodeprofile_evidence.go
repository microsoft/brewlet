// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func externallyRetired(t nodev1alpha1.NodeTarget) bool {
	return t.RetirementEvidenceName != "" || t.RetirementEvidenceUID != ""
}

func cleanupExecutionTargets(targets []nodev1alpha1.NodeTarget) []nodev1alpha1.NodeTarget {
	var result []nodev1alpha1.NodeTarget
	for _, t := range targets {
		if !externallyRetired(t) {
			result = append(result, t)
		}
	}
	return result
}

func evidenceObligation(p *nodev1alpha1.NodeProfile, target nodev1alpha1.NodeTarget) nodev1alpha1.NodeRetirement {
	target.RetirementEvidenceName, target.RetirementEvidenceUID = "", ""
	spec, generation := p.Spec.DeepCopy(), p.Generation
	if p.Status.ProvisioningSpec != nil {
		spec, generation = p.Status.ProvisioningSpec.DeepCopy(), p.Status.ProvisioningGeneration
	}
	if p.Status.Retirement != nil {
		spec, generation = p.Status.Retirement.Spec.DeepCopy(), p.Status.Retirement.Generation
	}
	return nodev1alpha1.NodeRetirement{
		Targets: []nodev1alpha1.NodeTarget{target}, Generation: generation, Spec: *spec,
		Phase: "ExternallyRetired",
	}
}

func validateEvidence(e *nodev1alpha1.NodeRetirementEvidence, p *nodev1alpha1.NodeProfile, target nodev1alpha1.NodeTarget) error {
	s := e.Spec
	if e.UID == "" || !e.DeletionTimestamp.IsZero() || len(e.OwnerReferences) != 0 {
		return fmt.Errorf("evidence must have a durable UID, no deletion timestamp, and no owner references")
	}
	if p.UID == "" || target.UID == "" || !target.Claimed ||
		s.ProfileName != p.Name || s.ProfileUID != p.UID || s.NodeName != target.Name || s.NodeUID != target.UID {
		return fmt.Errorf("evidence does not match the original claimed profile and Node identities")
	}
	for _, value := range []struct {
		name, value string
		limit       int
	}{
		{"providerID", s.ProviderID, 2048}, {"instanceID", s.InstanceID, 1024},
		{"evidenceRef", s.EvidenceRef, 2048}, {"identityBinding", s.IdentityBinding, 4096},
	} {
		if strings.TrimSpace(value.value) == "" || len(value.value) > value.limit {
			return fmt.Errorf("evidence %s must be nonempty and at most %d bytes", value.name, value.limit)
		}
	}
	if !s.PermanentlyDecommissioned || s.RetiredAt.IsZero() || s.RetiredAt.After(time.Now()) ||
		(!e.CreationTimestamp.IsZero() && s.RetiredAt.After(e.CreationTimestamp.Time)) {
		return fmt.Errorf("evidence must attest permanent decommissioning completed before submission, not shutdown or deallocation")
	}
	if (target.ProviderID != "" && target.ProviderID != s.ProviderID) ||
		(target.SystemUUID != "" && !strings.EqualFold(target.SystemUUID, s.SystemUUID)) {
		return fmt.Errorf("evidence contradicts the original target's recorded host identity")
	}
	return nil
}

func (r *NodeProfileReconciler) persistEvidence(ctx context.Context, e *nodev1alpha1.NodeRetirementEvidence) error {
	expected := e.DeepCopy()
	if err := r.Status().Update(ctx, e); err != nil {
		return err
	}
	var fresh nodev1alpha1.NodeRetirementEvidence
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(e), &fresh); err != nil {
		return err
	}
	want, err := json.Marshal(expected.Status)
	if err != nil {
		return err
	}
	got, err := json.Marshal(fresh.Status)
	if err != nil {
		return err
	}
	if fresh.UID != expected.UID || !fresh.DeletionTimestamp.IsZero() ||
		!reflect.DeepEqual(fresh.Spec, expected.Spec) || len(fresh.OwnerReferences) != 0 || string(want) != string(got) {
		return fmt.Errorf("retirement evidence checkpoint was not durably preserved; check concurrent changes and apply the current CRDs")
	}
	*e = fresh
	return nil
}

func (r *NodeProfileReconciler) originalHostAbsent(ctx context.Context, target nodev1alpha1.NodeTarget) error {
	var node corev1.Node
	err := r.apiReader().Get(ctx, types.NamespacedName{Name: target.Name}, &node)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading original Node: %w", err)
	}
	if err == nil && node.UID == target.UID {
		return fmt.Errorf("original Node %s (%s) still exists; restore connectivity for ordinary cleanup", target.Name, target.UID)
	}
	if target.SystemUUID != "" {
		var nodes corev1.NodeList
		if err := r.apiReader().List(ctx, &nodes); err != nil {
			return fmt.Errorf("checking whether the original host re-registered: %w", err)
		}
		for _, live := range nodes.Items {
			if strings.EqualFold(live.Status.NodeInfo.SystemUUID, target.SystemUUID) {
				return fmt.Errorf("original host systemUUID is still registered on Node %s (%s); refusing external retirement", live.Name, live.UID)
			}
		}
	}
	return nil
}

func (r *NodeProfileReconciler) evidenceHostAbsent(ctx context.Context, target nodev1alpha1.NodeTarget, e *nodev1alpha1.NodeRetirementEvidence) error {
	if target.SystemUUID == "" {
		target.SystemUUID = e.Spec.SystemUUID
	}
	return r.originalHostAbsent(ctx, target)
}

// Labels alone cannot prove teardown: an owned writer may have lost its labels.
func (r *NodeProfileReconciler) recoveryWorkersGone(ctx context.Context, p *nodev1alpha1.NodeProfile) error {
	if err := r.profileWriterBarrier(ctx, p); err != nil {
		return err
	}
	var sets appsv1.DaemonSetList
	if err := r.apiReader().List(ctx, &sets); err != nil {
		return err
	}
	for _, ds := range sets.Items {
		uid, _ := literalProvisionerEnv(&ds.Spec.Template.Spec, "BREWLET_PROFILE_UID")
		if metav1.IsControlledBy(&ds, p) || uid == string(p.UID) {
			return fmt.Errorf("waiting for writer DaemonSet %s/%s to terminate", ds.Namespace, ds.Name)
		}
	}
	var pods corev1.PodList
	if err := r.apiReader().List(ctx, &pods); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		uid, _ := literalProvisionerEnv(&pod.Spec, "BREWLET_PROFILE_UID")
		owner := metav1.GetControllerOf(&pod)
		namedOwner := owner != nil && owner.Kind == "DaemonSet" && owner.APIVersion == appsv1.SchemeGroupVersion.String() &&
			(owner.Name == brewlet.ProfileDaemonSetName(p.Name) || owner.Name == brewlet.CleanupDaemonSetName(p.Name))
		targetWriter := false
		for _, target := range p.Status.Targets {
			targetWriter = targetWriter || (pod.Spec.NodeName == target.Name && profileWriter(pod.Labels))
		}
		if uid == string(p.UID) || namedOwner || targetWriter || pod.Labels[brewlet.LabelNodeProfile] == p.Name {
			return fmt.Errorf("waiting for writer pod %s/%s to terminate", pod.Namespace, pod.Name)
		}
	}
	return nil
}

func (r *NodeProfileReconciler) recordedEvidence(ctx context.Context, p *nodev1alpha1.NodeProfile, target nodev1alpha1.NodeTarget) (*nodev1alpha1.NodeRetirementEvidence, error) {
	var e nodev1alpha1.NodeRetirementEvidence
	if target.RetirementEvidenceName == "" || target.RetirementEvidenceUID == "" {
		return nil, fmt.Errorf("incomplete external-retirement receipt for %s", target.Name)
	}
	if err := r.apiReader().Get(ctx, types.NamespacedName{Name: target.RetirementEvidenceName}, &e); err != nil {
		return nil, fmt.Errorf("reading retained retirement evidence: %w", err)
	}
	if e.UID != target.RetirementEvidenceUID {
		return nil, fmt.Errorf("retirement evidence UID changed; refusing a replacement record")
	}
	if err := validateEvidence(&e, p, target); err != nil {
		return nil, err
	}
	obligation := evidenceObligation(p, target)
	if (e.Status.Phase != nodev1alpha1.EvidenceAccepted && e.Status.Phase != nodev1alpha1.EvidenceResolved) ||
		e.Status.AcceptedAt == nil || !reflect.DeepEqual(e.Status.Obligation, &obligation) {
		return nil, fmt.Errorf("retirement evidence has no matching durable obligation snapshot")
	}
	if err := r.evidenceHostAbsent(ctx, target, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *NodeProfileReconciler) acceptTargetEvidence(ctx context.Context, p *nodev1alpha1.NodeProfile, target nodev1alpha1.NodeTarget) error {
	var records nodev1alpha1.NodeRetirementEvidenceList
	if err := r.apiReader().List(ctx, &records); err != nil {
		return fmt.Errorf("listing retirement evidence: %w", err)
	}
	sort.Slice(records.Items, func(i, j int) bool {
		a, b := records.Items[i], records.Items[j]
		if (a.Status.Phase == nodev1alpha1.EvidenceAccepted) != (b.Status.Phase == nodev1alpha1.EvidenceAccepted) {
			return a.Status.Phase == nodev1alpha1.EvidenceAccepted
		}
		return a.Name < b.Name
	})
	var chosen *nodev1alpha1.NodeRetirementEvidence
	for i := range records.Items {
		e := &records.Items[i]
		if e.Spec.ProfileUID != p.UID || e.Spec.ProfileName != p.Name || e.Spec.NodeName != target.Name || e.Spec.NodeUID != target.UID {
			continue
		}
		if err := validateEvidence(e, p, target); err != nil {
			if e.Status.Phase == nodev1alpha1.EvidenceAccepted || e.Status.Phase == nodev1alpha1.EvidenceResolved {
				return err
			}
			if e.Status.Phase != nodev1alpha1.EvidenceBlocked || e.Status.Message != err.Error() {
				e.Status.Phase, e.Status.Message = nodev1alpha1.EvidenceBlocked, err.Error()
				if writeErr := r.persistEvidence(ctx, e); writeErr != nil {
					return writeErr
				}
			}
			continue
		}
		if e.Status.Phase == nodev1alpha1.EvidenceResolved {
			continue
		}
		chosen = e
		break
	}
	if chosen == nil {
		return fmt.Errorf("cleanup blocked for %s (%s): original Node is missing or replaced; no valid NodeRetirementEvidence attests its permanent retirement. Recreating a Node cannot restore its UID; submit authorized identity-bound evidence only after verifying external decommissioning", target.Name, target.UID)
	}
	if err := r.evidenceHostAbsent(ctx, target, chosen); err != nil {
		return err
	}
	if err := r.profileWriterBarrier(ctx, p); err != nil {
		return err
	}
	cleanup, err := r.deleteCleanupDaemonSetIfExists(ctx, p)
	if err != nil {
		return err
	}
	pods, err := r.profilePodsRemain(ctx, p.Name)
	if err != nil {
		return err
	}
	provisioner, err := r.profileProvisionerRemains(ctx, p)
	if err != nil {
		return err
	}
	if cleanup || pods || provisioner {
		return fmt.Errorf("waiting for all profile workers to terminate before accepting external retirement")
	}
	if err := r.recoveryWorkersGone(ctx, p); err != nil {
		return err
	}
	obligation := evidenceObligation(p, target)
	if chosen.Status.Phase == nodev1alpha1.EvidenceAccepted {
		if chosen.Status.AcceptedAt == nil || !reflect.DeepEqual(chosen.Status.Obligation, &obligation) {
			return fmt.Errorf("accepted evidence obligation changed; refusing to overwrite retained history")
		}
	} else {
		now := metav1.Now()
		chosen.Status = nodev1alpha1.RetirementEvidenceStatus{
			Phase: nodev1alpha1.EvidenceAccepted, AcceptedAt: &now, Obligation: &obligation,
			Message: "Administrator attests permanent external retirement; host cleanup was not executed",
		}
		if err := r.persistEvidence(ctx, chosen); err != nil {
			return err
		}
	}
	if err := r.recoveryWorkersGone(ctx, p); err != nil {
		return err
	}
	if err := r.evidenceHostAbsent(ctx, target, chosen); err != nil {
		return err
	}
	setReceipt := func(targets []nodev1alpha1.NodeTarget) {
		for i := range targets {
			if sameTarget(targets[i], target) {
				targets[i].RetirementEvidenceName = chosen.Name
				targets[i].RetirementEvidenceUID = chosen.UID
			}
		}
	}
	setReceipt(p.Status.Targets)
	if p.Status.Retirement != nil {
		setReceipt(p.Status.Retirement.Targets)
	}
	return r.persistOwnershipStatus(ctx, p)
}

// The ordinary claim validator stays fail-closed. Only durable, UID-bound
// evidence can select the separate external-retirement disposition.
func (r *NodeProfileReconciler) resolveCleanupTargets(ctx context.Context, p *nodev1alpha1.NodeProfile, targets []nodev1alpha1.NodeTarget) ([]corev1.Node, error) {
	var nodes []corev1.Node
	for _, target := range append([]nodev1alpha1.NodeTarget(nil), targets...) {
		if externallyRetired(target) {
			if _, err := r.recordedEvidence(ctx, p, target); err != nil {
				return nil, err
			}
			continue
		}
		live, err := r.validateTargetClaims(ctx, p, []nodev1alpha1.NodeTarget{target})
		if err == nil {
			nodes = append(nodes, live...)
			continue
		}
		if absentErr := r.originalHostAbsent(ctx, target); absentErr != nil {
			return nil, err
		}
		if err := r.acceptTargetEvidence(ctx, p, target); err != nil {
			return nil, err
		}
	}
	return nodes, nil
}

func (r *NodeProfileReconciler) finishExternalRetirements(ctx context.Context, p *nodev1alpha1.NodeProfile, targets []nodev1alpha1.NodeTarget) error {
	if err := r.profileWriterBarrier(ctx, p); err != nil {
		return err
	}
	hasEvidence := false
	for _, target := range targets {
		if !externallyRetired(target) {
			continue
		}
		hasEvidence = true
		if err := r.recoveryWorkersGone(ctx, p); err != nil {
			return err
		}
		e, err := r.recordedEvidence(ctx, p, target)
		if err != nil {
			return err
		}
		if e.Status.Phase != nodev1alpha1.EvidenceResolved {
			now := metav1.Now()
			e.Status.Phase, e.Status.ResolvedAt = nodev1alpha1.EvidenceResolved, &now
			e.Status.Message = "External retirement resolved after worker teardown; original cleanup obligation retained, not executed"
			if err := r.persistEvidence(ctx, e); err != nil {
				return err
			}
		}
	}
	if hasEvidence {
		if err := r.recoveryWorkersGone(ctx, p); err != nil {
			return err
		}
		// Reject a concurrent spec/deletion/ledger change before active claims
		// can be released, even if evidence resolution itself succeeded.
		return r.persistOwnershipStatus(ctx, p)
	}
	return nil
}

func (r *NodeProfileReconciler) evidenceToProfile(_ context.Context, obj client.Object) []reconcile.Request {
	e, ok := obj.(*nodev1alpha1.NodeRetirementEvidence)
	if !ok || e.Spec.ProfileName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: e.Spec.ProfileName}}}
}
