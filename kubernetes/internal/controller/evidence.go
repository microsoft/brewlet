package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/control"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type EvidenceReconciler struct {
	client.Client
	Reader client.Reader
}

func (r *EvidenceReconciler) checkpoint(ctx context.Context, e *api.NodeRetirementEvidence) error {
	if err := r.Status().Update(ctx, e); err != nil {
		return err
	}
	fresh := new(api.NodeRetirementEvidence)
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(e), fresh); err != nil {
		return err
	}
	a, _ := json.Marshal(e.Status)
	b, _ := json.Marshal(fresh.Status)
	if fresh.UID != e.UID || !bytes.Equal(a, b) {
		return fmt.Errorf("retirement evidence checkpoint not observed; matching CRD required")
	}
	return nil
}
func (r *EvidenceReconciler) blocked(ctx context.Context, e *api.NodeRetirementEvidence, err error) (ctrl.Result, error) {
	meta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{Type: "Resolved", Status: metav1.ConditionFalse, Reason: "RetirementBlocked", Message: err.Error(), ObservedGeneration: e.Generation})
	if writeErr := r.checkpoint(ctx, e); writeErr != nil {
		return ctrl.Result{}, fmt.Errorf("%v; checkpoint: %w", err, writeErr)
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}
func evidenceMatches(e *api.NodeRetirementEvidence, p *api.NodeProfile, d *api.Retirement, t api.Target) error {
	if e.Spec.ProfileUID != p.UID || e.Spec.ProfileName != p.Name || e.Spec.NodeUID != t.UID || e.Spec.NodeName != t.Name || !t.Claimed || d.Phase != "Missing" || d.Generation <= 0 {
		return fmt.Errorf("foreign or unverifiable retirement obligation")
	}
	if t.ProviderID != "" && e.Spec.ProviderID != t.ProviderID || t.SystemUUID != "" && e.Spec.SystemUUID != t.SystemUUID {
		return fmt.Errorf("external evidence does not match frozen physical host identity")
	}
	if t.ProviderID == "" && t.SystemUUID == "" && e.Spec.LegacyMapping == "" {
		return fmt.Errorf("legacy target requires an explicit administrative mapping from external records")
	}
	if t.ContainerdRestart != "validated" && t.ContainerdRestart != "none" || d.Spec.Rollout.RestartPolicy() != "validated" && d.Spec.Rollout.RestartPolicy() != "none" {
		return fmt.Errorf("unsupported frozen cleanup policy")
	}
	return nil
}
func (r *EvidenceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	e := new(api.NodeRetirementEvidence)
	if err := r.Reader.Get(ctx, req.NamespacedName, e); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if err := policy.Evidence(e, nil); err != nil {
		return r.blocked(ctx, e, err)
	}
	if e.UID == "" {
		return ctrl.Result{}, fmt.Errorf("evidence UID missing")
	}
	if !contains(e.Finalizers, api.EvidenceFinalizer) {
		if !e.DeletionTimestamp.IsZero() {
			return r.blocked(ctx, e, fmt.Errorf("deleting evidence was never retained; cannot accept it"))
		}
		base := e.DeepCopy()
		e.Finalizers = append(e.Finalizers, api.EvidenceFinalizer)
		return ctrl.Result{RequeueAfter: time.Second}, r.Patch(ctx, e, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	if e.Status.ResolvedAt != nil {
		if e.Status.AcceptedAt == nil || e.Status.OriginalTarget == nil || e.Status.CleanupSpec == nil {
			return r.blocked(ctx, e, fmt.Errorf("resolved evidence is incomplete; preserving record"))
		}
		if !e.DeletionTimestamp.IsZero() && e.Status.AcceptedAt == nil {
			return r.blocked(ctx, e, fmt.Errorf("deleting unaccepted evidence cannot become authority"))
		}
		var nodes corev1.NodeList
		if err := r.Reader.List(ctx, &nodes); err != nil {
			return ctrl.Result{}, err
		}
		identity := control.RetiredIdentity{ProfileName: e.Spec.ProfileName, ProfileUID: e.Spec.ProfileUID, Target: *e.Status.OriginalTarget, EvidenceName: e.Name, EvidenceUID: e.UID}
		for _, n := range nodes.Items {
			if control.IdentityMatches(identity.Target, &n) {
				if _, err := control.WithdrawRetiredIdentity(ctx, r.Client, r.Reader, n.Name, n.UID, identity, capabilityKeys([]corev1.Node{n}, api.Target{Name: n.Name, UID: n.UID})); err != nil {
					return r.blocked(ctx, e, err)
				}
			}
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	p := new(api.NodeProfile)
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: e.Spec.ProfileName}, p); err != nil {
		return r.blocked(ctx, e, fmt.Errorf("original profile ledger unavailable: %w", err))
	}
	if p.UID != e.Spec.ProfileUID || !p.Status.OwnershipInitialized {
		return r.blocked(ctx, e, fmt.Errorf("original profile UID/ownership ledger mismatch"))
	}
	idx := slices.IndexFunc(p.Status.DetachedRetirements, func(d api.Retirement) bool {
		return len(d.Targets) == 1 && d.Targets[0].UID == e.Spec.NodeUID && d.Targets[0].Name == e.Spec.NodeName
	})
	if idx < 0 {
		return r.blocked(ctx, e, fmt.Errorf("no matching detached original obligation; names alone confer no authority"))
	}
	d := &p.Status.DetachedRetirements[idx]
	t := d.Targets[0]
	if err := evidenceMatches(e, p, d, t); err != nil {
		return r.blocked(ctx, e, err)
	}
	var nodes corev1.NodeList
	if err := r.Reader.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}
	for _, n := range nodes.Items {
		if n.UID == t.UID {
			return r.blocked(ctx, e, fmt.Errorf("original Node UID still exists; external retirement is ineligible"))
		}
	}
	if busy, err := control.WorkersMayAuthorize(ctx, r.Reader, p.UID, t.UID); err != nil || busy {
		if err == nil {
			err = fmt.Errorf("workers can still authorize original Node UID")
		}
		return r.blocked(ctx, e, err)
	}
	if (t.RetirementEvidenceName != "" || t.RetirementEvidenceUID != "") && (t.RetirementEvidenceName != e.Name || t.RetirementEvidenceUID != e.UID) {
		return r.blocked(ctx, e, fmt.Errorf("foreign/recreated evidence receipt; original UID-bound receipt retained"))
	}
	var all api.NodeRetirementEvidenceList
	if err := r.Reader.List(ctx, &all); err != nil {
		return ctrl.Result{}, err
	}
	for _, q := range all.Items {
		if q.UID != e.UID && q.Spec.ProfileUID == e.Spec.ProfileUID && q.Spec.NodeUID == e.Spec.NodeUID && q.Status.AcceptedAt != nil {
			return r.blocked(ctx, e, fmt.Errorf("another immutable evidence UID already accepted this obligation"))
		}
	}
	if e.Status.AcceptedAt == nil {
		original := t
		original.RetirementEvidenceName = ""
		original.RetirementEvidenceUID = ""
		e.Status.OriginalTarget = &original
		e.Status.CleanupGeneration = d.Generation
		spec := p.DeepCopy().Status.DetachedRetirements[idx].Spec
		e.Status.CleanupSpec = &spec
		now := metav1.Now()
		e.Status.AcceptedAt = &now
		meta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "ExternalDispositionAccepted", Message: "administrator attests permanent external decommission; no host cleanup is claimed", ObservedGeneration: e.Generation})
		return ctrl.Result{RequeueAfter: time.Second}, r.checkpoint(ctx, e)
	}
	if e.Status.OriginalTarget == nil || e.Status.OriginalTarget.UID != t.UID || e.Status.CleanupGeneration != d.Generation || e.Status.CleanupSpec == nil {
		return r.blocked(ctx, e, fmt.Errorf("accepted evidence lost frozen cleanup snapshot"))
	}
	original := t
	original.RetirementEvidenceName = ""
	original.RetirementEvidenceUID = ""
	if !reflect.DeepEqual(original, *e.Status.OriginalTarget) || !reflect.DeepEqual(d.Spec, *e.Status.CleanupSpec) {
		return r.blocked(ctx, e, fmt.Errorf("accepted evidence frozen target/policy no longer matches original obligation"))
	}
	if t.RetirementEvidenceUID == "" && t.RetirementEvidenceName == "" {
		d.Targets[0].RetirementEvidenceName = e.Name
		d.Targets[0].RetirementEvidenceUID = e.UID
		temp := &ProfileReconciler{Client: r.Client, Reader: r.Reader}
		return temp.save(ctx, p)
	}
	if t.RetirementEvidenceUID != e.UID || t.RetirementEvidenceName != e.Name {
		return r.blocked(ctx, e, fmt.Errorf("foreign/recreated evidence receipt; original receipt retained"))
	}
	// The receipt above survived a separate reconcile/API read. Acceptance and
	// resolution remain independent durable checkpoints outside profile lifetime.
	now := metav1.Now()
	e.Status.ResolvedAt = &now
	meta.SetStatusCondition(&e.Status.Conditions, metav1.Condition{Type: "Resolved", Status: metav1.ConditionTrue, Reason: "ExternalDispositionResolved", Message: "external disposition resolved the obligation; cleanup was not executed", ObservedGeneration: e.Generation})
	return ctrl.Result{RequeueAfter: time.Second}, r.checkpoint(ctx, e)
}
func (r *EvidenceReconciler) SetupWithManager(m ctrl.Manager) error {
	mapAll := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var es api.NodeRetirementEvidenceList
		if r.Reader.List(ctx, &es) != nil {
			return nil
		}
		out := []reconcile.Request{}
		for _, e := range es.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: e.Name}})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(m).For(&api.NodeRetirementEvidence{}).Watches(&corev1.Node{}, mapAll).Watches(&api.NodeProfile{}, mapAll).Complete(r)
}
