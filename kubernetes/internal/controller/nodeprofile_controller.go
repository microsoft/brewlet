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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
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
	// The persisted teardown checkpoint and deletion timestamp must not lag:
	// a stale profile could recreate writers after cleanup has completed.
	if err := r.apiReader().Get(ctx, req.NamespacedName, &profile); err != nil {
		if apierrors.IsNotFound(err) {
			observability.DeleteNodeProfile(req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var nodes corev1.NodeList
	if err := r.apiReader().List(ctx, &nodes); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing nodes: %w", err)
	}
	var profiles nodev1alpha1.NodeProfileList
	if err := r.apiReader().List(ctx, &profiles); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing profiles: %w", err)
	}
	// Previously authorized legacy workers must be inventoried before even an
	// invalid update can stop them and erase the evidence of touched hosts.
	if controllerutil.ContainsFinalizer(&profile, brewlet.FinalizerCleanup) &&
		(!profile.Status.OwnershipInitialized || profile.Status.Migrating) {
		if err := r.initializeOwnership(ctx, &profile, nodes.Items); err != nil {
			return r.migrationStatus(ctx, &profile, err)
		}
	}

	resolvedKey := resolvePoolKey(&profile, nodes.Items)
	otherPools := namedPoolsExcept(profiles.Items, profile.Name)
	policy := NodeProfilePolicy{AllowedSourceMirrorHosts: r.Config.AllowedSourceMirrorHosts}
	validationErr := policy.Validate(&profile)
	if validationErr == nil {
		validationErr = ValidateNoPoolConflicts(&profile, profiles.Items)
	}
	if validationErr == nil && profile.Status.OwnershipInitialized {
		if err := unrecordedNodeClaim(&profile, nodes.Items); err != nil {
			return r.ownershipBlocked(ctx, &profile, nodev1alpha1.ReasonCleanupBlocked, err)
		}
	}

	// Deletion: run cleanup behind the finalizer before owner-ref GC (§5.6).
	if !profile.DeletionTimestamp.IsZero() {
		if validationErr != nil {
			return r.reconcileDeleteInvalid(ctx, &profile, nodes.Items, validationErr)
		}
		if controllerutil.ContainsFinalizer(&profile, brewlet.FinalizerCleanup) {
			if err := r.initializeOwnership(ctx, &profile, nodes.Items); err != nil {
				return r.migrationStatus(ctx, &profile, err)
			}
			if profile.Status.Retirement != nil {
				return r.reconcileRetirement(ctx, &profile)
			}
		}
		return r.reconcileDelete(ctx, &profile, otherPools)
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
	if handled, result, err := r.reconcileTargets(ctx, &profile, profiles.Items, nodes.Items); handled || err != nil {
		return result, err
	}

	if err := r.ensureRuntimeClass(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring RuntimeClass: %w", err)
	}
	if err := r.ensureProfileDaemonSet(ctx, &profile, resolvedKey, otherPools); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring provisioner DaemonSet: %w", err)
	}

	if err := r.apiReader().List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.updateStatus(ctx, &profile, resolvedKey, otherPools, nodes.Items)
}

func (r *NodeProfileReconciler) reconcileDeleteInvalid(
	ctx context.Context,
	profile *nodev1alpha1.NodeProfile,
	nodes []corev1.Node,
	validationErr error,
) (ctrl.Result, error) {
	writers, err := r.profileProvisionerRemains(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if writers {
		invalidated := meta.FindStatusCondition(profile.Status.Conditions, nodev1alpha1.ConditionCleanupComplete) != nil
		meta.RemoveStatusCondition(&profile.Status.Conditions, nodev1alpha1.ConditionCleanupComplete)
		if profile.Status.Retirement != nil && profile.Status.Retirement.Phase != nodev1alpha1.RetirementCleaning {
			profile.Status.Retirement.Phase = nodev1alpha1.RetirementCleaning
			invalidated = true
		}
		if invalidated {
			if err := r.persistOwnershipStatus(ctx, profile); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	blocked := fmt.Errorf("invalid deleting profile retains node ownership; repair its spec/source policy or pool conflict so host cleanup can run: %w", validationErr)
	if invalidProfileHasHostOwnership(profile, nodes) {
		if _, err := r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonCleanupBlocked, blocked); err != nil {
			return ctrl.Result{}, err
		}
	}
	profileDeletionStarted, err := r.deleteProfileDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	cleanupDeletionStarted, err := r.deleteCleanupDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.withdrawProfileNodeAdvertisements(ctx, profile, nodes); err != nil {
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
	if err := r.withdrawProfileNodeAdvertisements(ctx, profile, freshNodes.Items); err != nil {
		return ctrl.Result{}, err
	}
	if invalidProfileHasHostOwnership(profile, freshNodes.Items) {
		return r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonCleanupBlocked, blocked)
	}
	// No claimed host, retirement, or remaining writer exists. Clear abandoned
	// pre-claim intentions durably so admission can permit this narrow exception.
	if HasNodeProfileCleanupObligations(profile) {
		profile.Status.Targets = nil
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type: nodev1alpha1.ConditionReady, Status: metav1.ConditionFalse,
			Reason: nodev1alpha1.ReasonInvalidProfile, ObservedGeneration: profile.Generation,
			Message: "unprovisioned invalid profile has no host ownership; finalizing without cleanup",
		})
		if err := r.persistOwnershipStatus(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
	}
	r.Recorder.Eventf(
		profile,
		corev1.EventTypeWarning,
		nodev1alpha1.ReasonInvalidProfile,
		"finalizing unprovisioned invalid profile without host cleanup: %s",
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
	if retirement := profile.Status.Retirement; retirement != nil && retirement.Phase == nodev1alpha1.RetirementTeardown {
		writers, err := r.profileProvisionerRemains(ctx, profile)
		if err != nil {
			return ctrl.Result{}, err
		}
		if writers {
			retirement.Phase = nodev1alpha1.RetirementCleaning
			if err := r.persistOwnershipStatus(ctx, profile); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	deletionStarted, err := r.deleteProfileDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	cleanupDeletionStarted, err := r.deleteCleanupDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.withdrawProfileNodeAdvertisements(ctx, profile, nodes); err != nil {
		return ctrl.Result{}, err
	}
	podsRemain, err := r.profilePodsRemain(ctx, profile.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	result := ctrl.Result{}
	if deletionStarted || cleanupDeletionStarted || podsRemain {
		result.RequeueAfter = time.Second
	} else {
		var freshNodes corev1.NodeList
		if err := r.apiReader().List(ctx, &freshNodes); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing nodes after invalid profile pod termination: %w", err)
		}
		if err := r.withdrawProfileNodeAdvertisements(ctx, profile, freshNodes.Items); err != nil {
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
	if err := r.persistOwnershipStatus(ctx, profile); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating invalid NodeProfile status: %w", err)
	}
	return result, nil
}

func (r *NodeProfileReconciler) deleteProfileDaemonSetIfExists(
	ctx context.Context,
	profile *nodev1alpha1.NodeProfile,
) (bool, error) {
	return r.deleteDaemonSetIfExists(
		ctx,
		profile,
		brewlet.ProfileDaemonSetName(profile.Name),
		"provisioner",
	)
}

func (r *NodeProfileReconciler) withdrawProfileNodeAdvertisements(ctx context.Context, profile *nodev1alpha1.NodeProfile, nodes []corev1.Node) error {
	for i := range nodes {
		node := &nodes[i]
		if node.Annotations[brewlet.AnnotationProfile] != profile.Name {
			continue
		}
		if owner := node.Labels[brewlet.LabelNodeOwner]; owner != "" && owner != string(profile.UID) {
			continue
		}
		if !profile.Status.OwnershipInitialized || !r.nodeAssigned(profile, "", nil, node) {
			continue
		}
		base := node.DeepCopy()
		removeNodeAdvertisements(node)
		if err := r.Patch(ctx, node, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return fmt.Errorf("withdrawing invalid profile %q from node %q: %w", profile.Name, node.Name, err)
		}
	}
	return nil
}

func removeNodeAdvertisements(node *corev1.Node) {
	for key := range node.Labels {
		if key == brewlet.LabelRuntimeReady || key == "brewlet.sh/appcds-regeneration" ||
			strings.HasPrefix(key, brewlet.LabelJDKPrefix) ||
			strings.HasPrefix(key, brewlet.LabelJDKFeaturePrefix) ||
			strings.HasPrefix(key, brewlet.LabelLauncherPrefix) {
			delete(node.Labels, key)
		}
	}
	for _, key := range []string{brewlet.AnnotationJDKs, brewlet.AnnotationJDKsInfo, brewlet.AnnotationLaunchers,
		brewlet.AnnotationProfile, brewlet.AnnotationProfileGeneration, brewlet.AnnotationProvisionError,
		brewlet.AnnotationProvisionErrorMessage, brewlet.AnnotationProvisionState} {
		delete(node.Annotations, key)
	}
}

// reconcileDelete stops provisioning before cleanup can mutate the same host
// paths, and holds the finalizer until cleanup has completed on every node and
// every profile writer has terminated.
func (r *NodeProfileReconciler) reconcileDelete(ctx context.Context, profile *nodev1alpha1.NodeProfile, otherPools []string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(profile, brewlet.FinalizerCleanup) {
		return ctrl.Result{}, nil
	}

	provisionerRemains, err := r.profileProvisionerRemains(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if provisionerRemains {
		// Invalidate the checkpoint before stopping reappeared writers. If the
		// operator crashes after their deletion, their mutations still require
		// a fresh cleanup rather than resuming the previous teardown.
		if err := r.setDeleting(ctx, profile, false); err != nil {
			return ctrl.Result{}, err
		}
		if _, err := r.deleteProfileDaemonSetIfExists(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
		// An older operator may already have started cleanup concurrently.
		// Stop that DaemonSet too; its replacement must wait for all writers.
		if err := r.deleteCleanupDaemonSet(ctx, profile); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if cleanupCompleted(profile) {
		return r.reconcileCleanupTeardown(ctx, profile)
	}
	if err := r.setDeleting(ctx, profile, false); err != nil {
		return ctrl.Result{}, err
	}

	// The durable ledger, not today's pool labels, defines what must be undone.
	targetNodes, err := r.validateTargetClaims(ctx, profile, profile.Status.Targets)
	if err != nil {
		return r.ownershipBlocked(ctx, profile, nodev1alpha1.ReasonCleanupBlocked, err)
	}
	execution := profile.DeepCopy()
	if profile.Status.ProvisioningSpec != nil {
		execution.Spec = *profile.Status.ProvisioningSpec.DeepCopy()
		execution.Spec.Tolerations = provisioningSnapshot(profile.Status.ProvisioningSpec, &profile.Spec).Tolerations
	}
	done, err := r.ensureCleanupComplete(ctx, execution, "", nil, targetNodes)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		logger.Info("cleanup in progress; holding finalizer", "profile", profile.Name)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Persist completion before deleting any of its evidence. A restarted
	// reconciler can then resume teardown without recreating cleanup workers.
	if err := r.setDeleting(ctx, profile, true); err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileCleanupTeardown(ctx, profile)
}

func (r *NodeProfileReconciler) reconcileCleanupTeardown(ctx context.Context, profile *nodev1alpha1.NodeProfile) (ctrl.Result, error) {
	cleanupRemains, err := r.deleteCleanupDaemonSetIfExists(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	podsRemain, err := r.profilePodsRemain(ctx, profile.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	provisionerRemains, err := r.profileProvisionerRemains(ctx, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	if provisionerRemains {
		if err := r.setDeleting(ctx, profile, false); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if cleanupRemains || podsRemain {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err := r.releaseTargetClaims(ctx, profile, profile.Status.Targets); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(profile, brewlet.FinalizerCleanup)
	if err := r.Update(ctx, profile); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *NodeProfileReconciler) profileProvisionerRemains(ctx context.Context, profile *nodev1alpha1.NodeProfile) (bool, error) {
	var ds appsv1.DaemonSet
	if err := r.apiReader().Get(ctx, types.NamespacedName{
		Namespace: r.Config.Namespace, Name: brewlet.ProfileDaemonSetName(profile.Name),
	}, &ds); err == nil {
		return true, nil
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("reading provisioner before cleanup: %w", err)
	}
	return r.profilePodsRemain(ctx, profile.Name, brewlet.ProvisionerAppLabel)
}

func cleanupCompleted(profile *nodev1alpha1.NodeProfile) bool {
	// Status belongs to the UID-bearing object fetched through APIReader, not
	// a name-keyed cache entry or the previous incarnation of this profile.
	condition := meta.FindStatusCondition(profile.Status.Conditions, nodev1alpha1.ConditionCleanupComplete)
	return condition != nil && condition.Status == metav1.ConditionTrue &&
		condition.Reason == nodev1alpha1.ReasonCleanupSucceeded &&
		condition.ObservedGeneration == profile.Generation
}

// ensureCleanupComplete makes sure the cleanup DaemonSet exists and reports
// whether its current template has finished on every assigned node.
func (r *NodeProfileReconciler) ensureCleanupComplete(ctx context.Context, profile *nodev1alpha1.NodeProfile, resolvedKey string, otherPools []string, nodes []corev1.Node) (bool, error) {
	assigned := make(map[string]bool)
	for i := range nodes {
		if r.nodeAssigned(profile, resolvedKey, otherPools, &nodes[i]) {
			assigned[nodes[i].Name] = false
		}
	}
	if len(assigned) == 0 {
		remains, err := r.deleteCleanupDaemonSetIfExists(ctx, profile)
		if err != nil {
			return false, err
		}
		podsRemain, err := r.profilePodsRemain(ctx, profile.Name)
		return !remains && !podsRemain, err
	}
	desired := buildCleanupDaemonSet(r.Config, profile, resolvedKey, otherPools)
	ds := &appsv1.DaemonSet{}
	ds.Name = desired.Name
	ds.Namespace = desired.Namespace
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(ds), ds); apierrors.IsNotFound(err) {
		podsRemain, err := r.profilePodsRemain(ctx, profile.Name)
		if err != nil || podsRemain {
			return false, err
		}
	} else if err != nil {
		return false, err
	} else if !metav1.IsControlledBy(ds, profile) {
		return false, fmt.Errorf("cleanup DaemonSet %q is not controlled by NodeProfile %q (%s)", ds.Name, profile.Name, profile.UID)
	} else if !ds.DeletionTimestamp.IsZero() {
		return false, nil
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, ds, func() error {
		if ds.ResourceVersion != "" && !metav1.IsControlledBy(ds, profile) {
			return fmt.Errorf("refusing to replace cleanup DaemonSet with another controller UID")
		}
		if err := controllerutil.SetControllerReference(profile, ds, r.Scheme()); err != nil {
			return err
		}
		ds.Labels = desired.Labels
		ds.Spec = desired.Spec
		return nil
	}); err != nil {
		return false, fmt.Errorf("ensuring cleanup DaemonSet: %w", err)
	}
	// Use fresh status and pods: an informer can lag a template update or pod
	// termination, and aggregate readiness can belong to the previous revision.
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	count := int32(len(assigned))
	revision := desired.Spec.Template.Annotations[cleanupTemplateAnnotation]
	if !ds.DeletionTimestamp.IsZero() ||
		ds.Spec.Template.Annotations[cleanupTemplateAnnotation] != revision ||
		ds.Status.ObservedGeneration != ds.Generation ||
		ds.Status.DesiredNumberScheduled != count ||
		ds.Status.CurrentNumberScheduled != count ||
		ds.Status.UpdatedNumberScheduled != count ||
		ds.Status.NumberReady != count ||
		ds.Status.NumberAvailable != count ||
		ds.Status.NumberUnavailable != 0 ||
		ds.Status.NumberMisscheduled != 0 {
		return false, nil
	}
	var pods corev1.PodList
	if err := r.apiReader().List(ctx, &pods,
		client.InNamespace(ds.Namespace),
		client.MatchingLabels(ds.Spec.Selector.MatchLabels),
	); err != nil {
		return false, fmt.Errorf("listing cleanup pods: %w", err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !metav1.IsControlledBy(pod, ds) {
			continue
		}
		if _, ok := assigned[pod.Spec.NodeName]; !ok ||
			!pod.DeletionTimestamp.IsZero() ||
			pod.Annotations[cleanupTemplateAnnotation] != revision ||
			!cleanupPodReady(pod) {
			return false, nil
		}
		assigned[pod.Spec.NodeName] = true
	}
	for _, complete := range assigned {
		if !complete {
			return false, nil
		}
	}
	return true, nil
}

func cleanupPodReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "provisioner" && status.Ready && status.State.Running != nil {
					return true
				}
			}
		}
	}
	return false
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
		profile,
		brewlet.CleanupDaemonSetName(profile.Name),
		"cleanup",
	)
}

func (r *NodeProfileReconciler) deleteDaemonSetIfExists(
	ctx context.Context,
	profile *nodev1alpha1.NodeProfile,
	name string,
	kind string,
) (bool, error) {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: r.Config.Namespace,
	}}
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(ds, profile) {
		return false, fmt.Errorf("refusing to delete %s DaemonSet %q not controlled by NodeProfile %q (%s)", kind, name, profile.Name, profile.UID)
	}
	if !ds.DeletionTimestamp.IsZero() {
		return true, nil
	}
	uid, version := ds.UID, ds.ResourceVersion
	if err := r.Delete(ctx, ds,
		client.PropagationPolicy(metav1.DeletePropagationForeground),
		client.Preconditions{UID: &uid, ResourceVersion: &version},
	); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting %s DaemonSet: %w", kind, err)
	} else if apierrors.IsNotFound(err) {
		return false, nil
	}
	return true, nil
}

func (r *NodeProfileReconciler) profilePodsRemain(ctx context.Context, profileName string, app ...string) (bool, error) {
	var pods corev1.PodList
	labels := client.MatchingLabels{brewlet.LabelNodeProfile: profileName}
	if len(app) > 0 {
		labels["app"] = app[0]
	}
	if err := r.apiReader().List(
		ctx,
		&pods,
		client.InNamespace(r.Config.Namespace),
		labels,
	); err != nil {
		return false, fmt.Errorf("listing pods for profile %q: %w", profileName, err)
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
		if ds.ResourceVersion != "" && !metav1.IsControlledBy(ds, profile) {
			return fmt.Errorf("refusing to replace provisioner DaemonSet with another controller UID")
		}
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
	if profile.Status.OwnershipInitialized {
		for _, target := range profile.Status.Targets {
			if target.Claimed && target.Name == node.Name && nodeClaimedBy(node, profile, target) {
				return true
			}
		}
		return false
	}
	return profileClaimsNode(profile, resolvedKey, otherPools, node)
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
		r.Recorder.Eventf(profile, corev1.EventTypeWarning, brewlet.ReasonNodeUnmatched, "%s", cond.Message)
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
	return r.persistOwnershipStatus(ctx, profile)
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
		if code := node.Annotations[brewlet.AnnotationProvisionError]; code != "" {
			return true, fmt.Sprintf("node %s: %s", node.Name,
				brewlet.FormatProvisionError(code, node.Annotations[brewlet.AnnotationProvisionErrorMessage]))
		}
	}
	return false, ""
}

// setDeleting persists the cleanup phase. Completion must be acknowledged by
// the API before teardown may remove the DaemonSet or its completion evidence.
func (r *NodeProfileReconciler) setDeleting(ctx context.Context, profile *nodev1alpha1.NodeProfile, complete bool) error {
	base := profile.DeepCopy()
	reason, message := nodev1alpha1.ReasonCleanupPending, "waiting for provisioners to stop and host cleanup to complete"
	if complete {
		reason, message = nodev1alpha1.ReasonCleanupTeardown, "host cleanup complete; waiting for all profile workers to terminate"
		meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
			Type: nodev1alpha1.ConditionCleanupComplete, Status: metav1.ConditionTrue,
			Reason: nodev1alpha1.ReasonCleanupSucceeded, ObservedGeneration: profile.Generation,
			Message: "host cleanup completed on every assigned node",
		})
	} else {
		meta.RemoveStatusCondition(&profile.Status.Conditions, nodev1alpha1.ConditionCleanupComplete)
	}
	profile.Status.ObservedGeneration = profile.Generation
	meta.SetStatusCondition(&profile.Status.Conditions, metav1.Condition{
		Type:               nodev1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: profile.Generation,
	})
	if equalStatus(&base.Status, &profile.Status) {
		return nil
	}
	if err := r.persistOwnershipStatus(ctx, profile); err != nil {
		return fmt.Errorf("persisting profile cleanup phase: %w", err)
	}
	return nil
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
		Watches(&nodev1alpha1.NodeProfile{}, handler.EnqueueRequestsFromMapFunc(r.nodeToProfiles),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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
