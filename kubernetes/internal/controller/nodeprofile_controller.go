// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"
	"brewlet-operator/internal/observability"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// NodeProfileReconciler reconciles NodeProfiles (https://github.com/microsoft/brewlet/tree/main/specs,
// 0001): it ensures the cluster-singleton brewlet RuntimeClass and one
// provisioner DaemonSet per profile (nodeAffinity selected on the resolved node
// pool), reflects assigned/ready node counts on status, and — via the
// node.brewlet.sh/cleanup finalizer — runs host cleanup before letting owner-ref
// GC drop the managed DaemonSet.
type NodeProfileReconciler struct {
	client.Client
	APIReader client.Reader
	Recorder  record.EventRecorder
	Config    Config
}

// Reconcile is invoked for NodeProfile events (and, via a Node watch, when the
// fleet changes so pool membership/readiness is re-evaluated).
func (r *NodeProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var profile nodev1alpha1.NodeProfile
	if err := r.Get(ctx, req.NamespacedName, &profile); err != nil {
		if apierrors.IsNotFound(err) {
			observability.DeleteNodeProfile(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing nodes: %w", err)
	}
	var profiles nodev1alpha1.NodeProfileList
	if err := r.List(ctx, &profiles); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing profiles: %w", err)
	}

	resolvedKey := resolvePoolKey(&profile, nodes.Items)
	otherPools := namedPoolsExcept(profiles.Items, profile.Name)
	policy := NodeProfilePolicy{AllowedSourceMirrorHosts: r.Config.AllowedSourceMirrorHosts}
	validationErr := policy.Validate(&profile)
	if validationErr == nil {
		validationErr = ValidateNoPoolConflicts(&profile, profiles.Items)
	}

	// Deletion: run cleanup behind the finalizer before owner-ref GC (§5.6).
	if !profile.DeletionTimestamp.IsZero() {
		if validationErr != nil {
			return r.reconcileDeleteInvalid(ctx, &profile, nodes.Items, validationErr)
		}
		return r.reconcileDelete(ctx, &profile, resolvedKey, otherPools, nodes.Items)
	}

	if validationErr != nil {
		return r.reconcileInvalidProfile(ctx, &profile, resolvedKey, otherPools, nodes.Items, validationErr)
	}

	// Only profiles that pass policy validation can own host state. Avoid adding
	// a cleanup finalizer to a never-valid profile, which could otherwise target
	// nodes owned by the profile it conflicts with when the invalid object is
	// deleted.
	if controllerutil.AddFinalizer(&profile, brewlet.FinalizerCleanup) {
		if err := r.Update(ctx, &profile); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
	}

	if err := r.ensureRuntimeClass(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring RuntimeClass: %w", err)
	}
	if err := r.ensureProfileDaemonSet(ctx, &profile, resolvedKey, otherPools); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring provisioner DaemonSet: %w", err)
	}

	return ctrl.Result{}, r.updateStatus(ctx, &profile, resolvedKey, otherPools, nodes.Items)
}

func (r *NodeProfileReconciler) reconcileDeleteInvalid(
	ctx context.Context,
	profile *nodev1alpha1.NodeProfile,
	nodes []corev1.Node,
	validationErr error,
) (ctrl.Result, error) {
	profileDeletionStarted, err := r.deleteProfileDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	cleanupDeletionStarted, err := r.deleteCleanupDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.withdrawProfileNodeAdvertisements(ctx, profile.Name, nodes); err != nil {
		return ctrl.Result{}, err
	}
	if !controllerutil.ContainsFinalizer(profile, brewlet.FinalizerCleanup) {
		return ctrl.Result{}, nil
	}
	podsRemain, err := r.profilePodsRemain(ctx, profile.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	if profileDeletionStarted || cleanupDeletionStarted || podsRemain {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	var freshNodes corev1.NodeList
	if err := r.apiReader().List(ctx, &freshNodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing nodes before invalid profile finalization: %w", err)
	}
	if err := r.withdrawProfileNodeAdvertisements(ctx, profile.Name, freshNodes.Items); err != nil {
		return ctrl.Result{}, err
	}

	// The current pool selector failed validation and may now overlap another
	// profile. Never run privileged host cleanup against an untrusted selector.
	// The old provisioner and cleanup pods are gone and readiness was withdrawn
	// again after they stopped, so removing the finalizer cannot leave a pod that
	// republishes stale capabilities. This avoids damaging a valid profile's
	// nodes at the cost of leaving the previously trusted host installation for
	// the platform team to clean explicitly.
	r.Recorder.Eventf(
		profile,
		corev1.EventTypeWarning,
		nodev1alpha1.ReasonInvalidProfile,
		"skipping host cleanup for invalid profile during deletion: %s",
		validationErr,
	)
	controllerutil.RemoveFinalizer(profile, brewlet.FinalizerCleanup)
	if err := r.Update(ctx, profile); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing invalid profile finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *NodeProfileReconciler) reconcileInvalidProfile(
	ctx context.Context,
	profile *nodev1alpha1.NodeProfile,
	resolvedKey string,
	otherPools []string,
	nodes []corev1.Node,
	validationErr error,
) (ctrl.Result, error) {
	deletionStarted, err := r.deleteProfileDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.withdrawProfileNodeAdvertisements(ctx, profile.Name, nodes); err != nil {
		return ctrl.Result{}, err
	}
	podsRemain, err := r.profilePodsRemain(ctx, profile.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	result := ctrl.Result{}
	if deletionStarted || podsRemain {
		result.RequeueAfter = time.Second
	} else {
		var freshNodes corev1.NodeList
		if err := r.apiReader().List(ctx, &freshNodes); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing nodes after invalid profile pod termination: %w", err)
		}
		if err := r.withdrawProfileNodeAdvertisements(ctx, profile.Name, freshNodes.Items); err != nil {
			return ctrl.Result{}, err
		}
	}
	assigned, _ := r.poolCounts(profile, resolvedKey, otherPools, nodes)
	base := profile.DeepCopy()
	profile.Status.ObservedGeneration = profile.Generation
	profile.Status.ResolvedPoolKey = resolvedKey
	profile.Status.AssignedNodes = assigned
	profile.Status.ReadyNodes = 0
	meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:               nodev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             nodev1alpha1.ReasonInvalidProfile,
		Message:            validationErr.Error(),
		ObservedGeneration: profile.Generation,
	})
	observability.SetNodeProfile(profile.Name, assigned, 0, nodev1alpha1.ReasonInvalidProfile, false)
	if equalStatus(&base.Status, &profile.Status) {
		return result, nil
	}
	r.Recorder.Eventf(profile, corev1.EventTypeWarning, nodev1alpha1.ReasonInvalidProfile, "%s", validationErr)
	if err := r.Status().Update(ctx, profile); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating invalid NodeProfile status: %w", err)
	}
	return result, nil
}

func (r *NodeProfileReconciler) deleteProfileDaemonSet(ctx context.Context, profile *nodev1alpha1.NodeProfile) error {
	_, err := r.deleteProfileDaemonSetIfExists(ctx, profile)
	return err
}

func (r *NodeProfileReconciler) deleteProfileDaemonSetIfExists(
	ctx context.Context,
	profile *nodev1alpha1.NodeProfile,
) (bool, error) {
	return r.deleteDaemonSetIfExists(
		ctx,
		brewlet.ProfileDaemonSetName(profile.Name),
		"invalid profile",
	)
}

func (r *NodeProfileReconciler) withdrawProfileNodeAdvertisements(ctx context.Context, profileName string, nodes []corev1.Node) error {
	for i := range nodes {
		node := &nodes[i]
		if node.Annotations[brewlet.AnnotationProfile] != profileName {
			continue
		}
		base := node.DeepCopy()
		for key := range node.Labels {
			if key == brewlet.LabelRuntimeReady ||
				strings.HasPrefix(key, brewlet.LabelJDKPrefix) ||
				strings.HasPrefix(key, brewlet.LabelJDKFeaturePrefix) ||
				strings.HasPrefix(key, brewlet.LabelLauncherPrefix) {
				delete(node.Labels, key)
			}
		}
		delete(node.Annotations, brewlet.AnnotationJDKs)
		delete(node.Annotations, brewlet.AnnotationJDKsInfo)
		delete(node.Annotations, brewlet.AnnotationLaunchers)
		delete(node.Annotations, brewlet.AnnotationProfile)
		delete(node.Annotations, brewlet.AnnotationProfileGeneration)
		delete(node.Annotations, brewlet.AnnotationProvisionError)
		if err := r.Patch(ctx, node, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("withdrawing invalid profile %q from node %q: %w", profileName, node.Name, err)
		}
	}
	return nil
}

// reconcileDelete launches the cleanup DaemonSet, and only removes the finalizer
// (unblocking owner-ref GC of the managed provisioner DaemonSet) once cleanup
// has completed on every assigned node.
func (r *NodeProfileReconciler) reconcileDelete(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(profile, brewlet.FinalizerCleanup) {
		return ctrl.Result{}, nil
	}

	assigned, _ := r.poolCounts(profile, resolvedKey, otherPools, nodes)

	done, err := r.ensureCleanupComplete(ctx, profile, resolvedKey, otherPools, assigned)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		r.setDeleting(ctx, profile)
		logger.Info("cleanup in progress; holding finalizer", "profile", profile.Name)
		return ctrl.Result{Requeue: true}, nil
	}

	// Cleanup finished: tear down the cleanup DaemonSet and release the finalizer.
	if err := r.deleteCleanupDaemonSet(ctx, profile); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(profile, brewlet.FinalizerCleanup)
	if err := r.Update(ctx, profile); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

// ensureCleanupComplete makes sure the cleanup DaemonSet exists and reports
// whether it has finished on every assigned node. With no assigned nodes there
// is nothing to clean, so it completes immediately.
func (r *NodeProfileReconciler) ensureCleanupComplete(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, assigned int32) (bool, error) {
	if assigned == 0 {
		return true, nil
	}
	desired := buildCleanupDaemonSet(r.Config, profile, resolvedKey, otherPools)
	ds := &appsv1.DaemonSet{}
	ds.Name = desired.Name
	ds.Namespace = desired.Namespace
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		if err := controllerutil.SetControllerReference(profile, ds, r.Scheme()); err != nil {
			return err
		}
		ds.Labels = desired.Labels
		ds.Spec = desired.Spec
		return nil
	}); err != nil {
		return false, fmt.Errorf("ensuring cleanup DaemonSet: %w", err)
	}
	// Complete once the cleanup DaemonSet has run to ready on all its nodes.
	if ds.Status.DesiredNumberScheduled > 0 && ds.Status.NumberReady >= ds.Status.DesiredNumberScheduled {
		return true, nil
	}
	return false, nil
}

func (r *NodeProfileReconciler) deleteCleanupDaemonSet(ctx context.Context, profile *nodev1alpha1.NodeProfile) error {
	_, err := r.deleteCleanupDaemonSetIfExists(ctx, profile)
	return err
}

func (r *NodeProfileReconciler) deleteCleanupDaemonSetIfExists(
	ctx context.Context,
	profile *nodev1alpha1.NodeProfile,
) (bool, error) {
	return r.deleteDaemonSetIfExists(
		ctx,
		brewlet.CleanupDaemonSetName(profile.Name),
		"cleanup",
	)
}

func (r *NodeProfileReconciler) deleteDaemonSetIfExists(
	ctx context.Context,
	name string,
	kind string,
) (bool, error) {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: r.Config.Namespace,
	}}
	if err := r.Delete(ctx, ds, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting %s DaemonSet: %w", kind, err)
	} else if apierrors.IsNotFound(err) {
		return false, nil
	}
	return true, nil
}

func (r *NodeProfileReconciler) profilePodsRemain(ctx context.Context, profileName string) (bool, error) {
	var pods corev1.PodList
	if err := r.apiReader().List(
		ctx,
		&pods,
		client.InNamespace(r.Config.Namespace),
		client.MatchingLabels{brewlet.LabelNodeProfile: profileName},
	); err != nil {
		return false, fmt.Errorf("listing pods for invalid profile %q: %w", profileName, err)
	}
	return len(pods.Items) > 0, nil
}

func (r *NodeProfileReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *NodeProfileReconciler) ensureRuntimeClass(ctx context.Context) error {
	desired := buildRuntimeClass()
	rc := &nodev1.RuntimeClass{}
	rc.Name = desired.Name
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, rc, func() error {
		rc.Handler = desired.Handler
		rc.Scheduling = desired.Scheduling
		rc.Overhead = desired.Overhead
		if rc.Labels == nil {
			rc.Labels = map[string]string{}
		}
		for k, v := range desired.Labels {
			rc.Labels[k] = v
		}
		return nil
	})
	return err
}

func (r *NodeProfileReconciler) ensureProfileDaemonSet(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string) error {
	desired := buildProfileDaemonSet(r.Config, profile, resolvedKey, otherPools)
	ds := &appsv1.DaemonSet{}
	ds.Name = desired.Name
	ds.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		if err := controllerutil.SetControllerReference(profile, ds, r.Scheme()); err != nil {
			return err
		}
		ds.Labels = desired.Labels
		ds.Spec = desired.Spec
		return nil
	})
	return err
}

// poolCounts returns the number of nodes assigned to a profile and how many of
// those advertise the brewlet runtime. The catch-all default owns every node not
// claimed by a named pool (§5.6).
func (r *NodeProfileReconciler) poolCounts(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) (assigned, ready int32) {
	for i := range nodes {
		node := &nodes[i]
		if !r.nodeAssigned(profile, resolvedKey, otherPools, node) {
			continue
		}
		assigned++
		appliedRevision := node.Annotations[brewlet.AnnotationProfile] == profile.Name &&
			node.Annotations[brewlet.AnnotationProfileGeneration] == strconv.FormatInt(profile.Generation, 10)
		if node.Labels[brewlet.LabelRuntimeReady] == brewlet.ValueReady &&
			(profile.Generation == 0 || appliedRevision) {
			ready++
		}
	}
	return assigned, ready
}

// nodeAssigned reports whether a node belongs to the given profile.
func (r *NodeProfileReconciler) nodeAssigned(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, node *corev1.Node) bool {
	if !isDefaultProfile(profile) {
		return nodeInPool(node, resolvedKey, profile.Spec.NodePool.Names)
	}
	// Catch-all default: every node not claimed by a named pool.
	if resolvedKey != "" && len(otherPools) > 0 && nodeInPool(node, resolvedKey, otherPools) {
		return false
	}
	return true
}

// updateStatus recomputes assigned/ready counts and the Ready condition, and
// emits NodeUnmatched when a named pool resolves to zero nodes (§14).
func (r *NodeProfileReconciler) updateStatus(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) error {
	assigned, ready := r.poolCounts(profile, resolvedKey, otherPools, nodes)
	failed, failReason := r.assignedNodeFailure(profile, resolvedKey, otherPools, nodes)

	base := profile.DeepCopy()
	profile.Status.ObservedGeneration = profile.Generation
	profile.Status.ResolvedPoolKey = resolvedKey
	profile.Status.AssignedNodes = assigned
	profile.Status.ReadyNodes = ready

	cond := metav1.Condition{Type: nodev1alpha1.ConditionReady, ObservedGeneration: profile.Generation}
	switch {
	case !isDefaultProfile(profile) && assigned == 0:
		cond.Status = metav1.ConditionFalse
		cond.Reason = nodev1alpha1.ReasonEmptyPool
		cond.Message = fmt.Sprintf("no nodes match pool(s) %v on key %q", profile.Spec.NodePool.Names, resolvedKey)
		r.Recorder.Eventf(profile, corev1.EventTypeWarning, brewlet.ReasonNodeUnmatched, cond.Message)
	case failed:
		cond.Status = metav1.ConditionFalse
		cond.Reason = nodev1alpha1.ReasonNodeFailure
		cond.Message = failReason
	case ready >= assigned:
		cond.Status = metav1.ConditionTrue
		cond.Reason = nodev1alpha1.ReasonAllNodesProvisioned
		cond.Message = fmt.Sprintf("%d/%d assigned nodes provisioned", ready, assigned)
	default:
		cond.Status = metav1.ConditionFalse
		cond.Reason = nodev1alpha1.ReasonProvisioning
		cond.Message = fmt.Sprintf("%d/%d assigned nodes provisioned", ready, assigned)
	}
	meta.SetStatusCondition(&profile.Status.Conditions, cond)
	observability.SetNodeProfile(profile.Name, assigned, ready, cond.Reason, cond.Status == metav1.ConditionTrue)

	if equalStatus(&base.Status, &profile.Status) {
		return nil
	}
	return r.Status().Update(ctx, profile)
}

// assignedNodeFailure reports whether any assigned node carries a
// provision-error annotation (proposal 0002), propagating it as the profile's
// Degraded reason (§5.5).
func (r *NodeProfileReconciler) assignedNodeFailure(profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) (bool, string) {
	for i := range nodes {
		node := &nodes[i]
		if !r.nodeAssigned(profile, resolvedKey, otherPools, node) {
			continue
		}
		if e := node.Annotations[brewlet.AnnotationProvisionError]; e != "" {
			return true, fmt.Sprintf("node %s: %s", node.Name, e)
		}
	}
	return false, ""
}

// setDeleting best-effort marks the profile Ready=False/CleanupPending while its
// cleanup runs, so `kubectl get nodeprofile` shows the teardown in flight.
func (r *NodeProfileReconciler) setDeleting(ctx context.Context, profile *nodev1alpha1.NodeProfile) {
	base := profile.DeepCopy()
	meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:               nodev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             nodev1alpha1.ReasonCleanupPending,
		Message:            "running host cleanup before deletion",
		ObservedGeneration: profile.Generation,
	})
	if equalStatus(&base.Status, &profile.Status) {
		return
	}
	_ = r.Status().Update(ctx, profile)
}

// namedPoolsExcept returns the union of pool names claimed by every profile
// except the named one.
func namedPoolsExcept(profiles []nodev1alpha1.NodeProfile, except string) []string {
	seen := map[string]struct{}{}
	var out []string
	for i := range profiles {
		p := &profiles[i]
		if p.Name == except {
			continue
		}
		for _, name := range p.Spec.NodePool.Names {
			if _, ok := seen[name]; !ok {
				seen[name] = struct{}{}
				out = append(out, name)
			}
		}
	}
	return out
}

// equalStatus compares the fields updateStatus manages (ignoring condition
// timestamps) to avoid a hot status-write loop.
func equalStatus(a, b *nodev1alpha1.NodeProfileStatus) bool {
	if a.ObservedGeneration != b.ObservedGeneration ||
		a.ResolvedPoolKey != b.ResolvedPoolKey ||
		a.AssignedNodes != b.AssignedNodes ||
		a.ReadyNodes != b.ReadyNodes ||
		len(a.Conditions) != len(b.Conditions) {
		return false
	}
	for i := range a.Conditions {
		ac, bc := a.Conditions[i], b.Conditions[i]
		if ac.Type != bc.Type || ac.Status != bc.Status ||
			ac.Reason != bc.Reason || ac.Message != bc.Message ||
			ac.ObservedGeneration != bc.ObservedGeneration {
			return false
		}
	}
	return true
}

// SetupWithManager wires the controller to reconcile NodeProfiles and re-run when
// the node fleet changes (so pool membership/readiness stays current).
func (r *NodeProfileReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nodev1alpha1.NodeProfile{}).
		Owns(&appsv1.DaemonSet{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeToProfiles)).
		Named("brewlet-nodeprofile").
		Complete(r)
}

// nodeToProfiles enqueues every NodeProfile when a node changes: pool membership
// and readiness are cluster-wide inputs to each profile's status.
func (r *NodeProfileReconciler) nodeToProfiles(ctx context.Context, _ client.Object) []reconcile.Request {
	var profiles nodev1alpha1.NodeProfileList
	if err := r.List(ctx, &profiles); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(profiles.Items))
	for i := range profiles.Items {
		reqs = append(reqs, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: profiles.Items[i].Name},
		})
	}
	return reqs
}
