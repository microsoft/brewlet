package controller

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/control"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimecontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type RuntimeReconciler struct {
	client.Client
	Reader               client.Reader
	NodeProgressDeadline time.Duration
	Now                  func() time.Time
	AllowedMirrorHosts   []string
	Restarts             *RestartEngine
}

func runtimeCondition(im *api.RuntimeImage, kind, reason, msg string, status metav1.ConditionStatus) {
	if kind == "RotationComplete" {
		rotationConditions(im, reason, msg, status)
		return
	}
	meta.SetStatusCondition(&im.Status.Conditions, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: msg, ObservedGeneration: im.Generation})
	if kind == "Approved" && status == metav1.ConditionFalse {
		rotationRevoked(im, msg)
	}
}
func (r *RuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	im := new(api.RuntimeImage)
	if err := r.Reader.Get(ctx, req.NamespacedName, im); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if im.UID == "" {
		return ctrl.Result{}, fmt.Errorf("inventory epoch missing")
	}
	if !contains(im.Finalizers, api.CleanupFinalizer) && im.DeletionTimestamp.IsZero() {
		base := im.DeepCopy()
		im.Finalizers = append(im.Finalizers, api.CleanupFinalizer)
		if err := policy.Runtime(im, nil); err == nil {
			if im.Status.Generation != 0 {
				return ctrl.Result{}, fmt.Errorf("unfinalized inventory has existing generation; refusing adoption")
			}
			if err := encodeJournal(im, journalFor(im, 1)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: time.Second}, r.Patch(ctx, im, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	validErr := policy.Runtime(im, nil)
	j, journalErr := readJournal(im)
	if journalErr != nil {
		validErr = journalErr
	}
	c := meta.FindStatusCondition(im.Status.Conditions, "Approved")
	revoked := !im.DeletionTimestamp.IsZero() || validErr != nil || c != nil && c.Status == metav1.ConditionFalse
	if revoked {
		if journalErr == nil && im.Status.Generation < j.Generation {
			im.Status.Generation = j.Generation
			im.Status.Digest = strings.SplitN(j.Source, "@", 2)[1]
			im.Status.SourceImage = j.Source
			u, compat := j.User, j.Compatibility
			im.Status.ApprovedUser = &u
			im.Status.ApprovedCompatibility = &compat
		}
		message := "new starts revoked; local authorization expiry and cleanup must be confirmed"
		if validErr != nil {
			message += ": " + validErr.Error()
		}
		runtimeCondition(im, "Approved", "Revoked", message, metav1.ConditionFalse)
		if err := r.revoke(ctx, im); err != nil {
			runtimeCondition(im, "CleanupComplete", "CleanupBlocked", err.Error(), metav1.ConditionFalse)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, r.Status().Update(ctx, im)
		}
		if !im.DeletionTimestamp.IsZero() {
			if _, err := r.observeTasks(ctx, im); err != nil {
				return ctrl.Result{}, err
			}
			done, err := r.deletionProgress(ctx, im)
			if err != nil {
				runtimeCondition(im, "CleanupComplete", "CleanupBlocked", err.Error(), metav1.ConditionFalse)
				return ctrl.Result{RequeueAfter: 10 * time.Second}, r.Status().Update(ctx, im)
			}
			if !done {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			base := im.DeepCopy()
			im.Finalizers = slices.DeleteFunc(im.Finalizers, func(f string) bool { return f == api.CleanupFinalizer })
			return ctrl.Result{}, r.Patch(ctx, im, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}
		if _, err := r.collectRevocationTargets(ctx, im); err != nil {
			return ctrl.Result{}, err
		}
		if _, err := r.resolveHistoricalTargets(ctx, im); err != nil {
			return ctrl.Result{}, err
		}
		if _, err := r.observeTasks(ctx, im); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, r.revocationCheckpoint(ctx, im)
	}
	digest := strings.SplitN(im.Spec.Source.Image, "@", 2)[1]
	if j.Source != im.Spec.Source.Image {
		if j.Generation == math.MaxInt64 {
			return ctrl.Result{}, fmt.Errorf("runtime generation exhausted; refusing reuse")
		}
		j.Generation++
		j.Source = im.Spec.Source.Image
		return ctrl.Result{RequeueAfter: time.Second}, r.persistJournal(ctx, im, j)
	}
	if im.Status.SourceImage != im.Spec.Source.Image || im.Status.Digest != digest || im.Status.Generation != j.Generation || im.Status.ApprovedUser == nil || im.Status.ApprovedCompatibility == nil {
		im.Status.Generation = j.Generation
		u, compat := im.Spec.User, im.Spec.Compatibility
		im.Status.ApprovedUser = &u
		im.Status.ApprovedCompatibility = &compat
		im.Status.Digest = digest
		im.Status.SourceImage = im.Spec.Source.Image
		im.Status.Rotation = api.RotationStatus{Generation: im.Status.Generation, Digest: digest}
		im.Status.RotationCompletedAt = nil
		runtimeCondition(im, "Approved", "Approved", "administrator-approved immutable source", metav1.ConditionTrue)
		runtimeCondition(im, "RotationComplete", "RotationProgressing", "target persisted; awaiting node observations", metav1.ConditionFalse)
		if _, err := r.collectRevocationTargets(ctx, im); err != nil {
			return ctrl.Result{}, err
		}
		// Target and epoch must be durable before any downstream action.
		if err := r.Status().Update(ctx, im); err != nil {
			return ctrl.Result{}, err
		}
		fresh := new(api.RuntimeImage)
		if err := r.Reader.Get(ctx, req.NamespacedName, fresh); err != nil {
			return ctrl.Result{}, err
		}
		if fresh.UID != im.UID || fresh.Status.Generation != im.Status.Generation || fresh.Status.Digest != digest || !slices.Equal(fresh.Status.RevocationTargets, im.Status.RevocationTargets) {
			return ctrl.Result{}, fmt.Errorf("inventory checkpoint not observed")
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if im.Status.Generation != j.Generation {
		return ctrl.Result{}, fmt.Errorf("status allocation does not match durable journal")
	}
	if c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != im.Generation {
		runtimeCondition(im, "Approved", "Approved", "administrator-approved immutable source", metav1.ConditionTrue)
		return ctrl.Result{RequeueAfter: time.Second}, r.Status().Update(ctx, im)
	}
	if changed, err := r.collectRevocationTargets(ctx, im); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{RequeueAfter: time.Second}, r.revocationCheckpoint(ctx, im)
	}
	if changed, err := r.resolveHistoricalTargets(ctx, im); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{RequeueAfter: time.Second}, r.revocationCheckpoint(ctx, im)
	}
	if _, err := r.observeTasks(ctx, im); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.rotation(ctx, im); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.revocationCheckpoint(ctx, im); err != nil {
		return ctrl.Result{}, err
	}
	if r.Restarts != nil {
		if err := r.Restarts.Reconcile(ctx, im); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}
func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
func (r *RuntimeReconciler) revoke(ctx context.Context, im *api.RuntimeImage) error {
	var ps api.NodeProfileList
	if err := r.Reader.List(ctx, &ps); err != nil {
		return err
	}
	for _, p := range ps.Items {
		for _, t := range p.Status.Targets {
			if !t.Claimed {
				continue
			}
			n := new(corev1.Node)
			err := r.Reader.Get(ctx, client.ObjectKey{Name: t.Name}, n)
			if apierrors.IsNotFound(err) || err == nil && n.UID != t.UID {
				continue
			}
			if err != nil {
				return err
			}
			relevant := offers(p.Spec, im.Name) || p.Status.ProvisioningSpec != nil && offers(*p.Status.ProvisioningSpec, im.Name)
			if p.Status.Retirement != nil && offers(p.Status.Retirement.Spec, im.Name) {
				relevant = true
			}
			if !relevant {
				_, advertised := n.Labels["brewlet.sh/runtime."+im.Name]
				inventory, err := control.ParseInventory(n)
				if err != nil {
					return err
				}
				if !advertised && inventory[im.Name].InventoryUID != im.UID {
					continue
				}
			}
			a := authority(&p, t, "provisioning", p.Status.ProvisioningGeneration)
			if p.Status.Retirement != nil {
				for _, old := range p.Status.Retirement.Targets {
					if old.UID == t.UID {
						a = authority(&p, t, "cleanup", p.Status.Retirement.Generation)
					}
				}
			}
			if _, err := control.WithdrawAdvertisement(ctx, r.Client, r.Reader, a, []string{"brewlet.sh/runtime." + im.Name}); err != nil {
				return err
			}
		}
	}
	return nil
}
func (r *RuntimeReconciler) rotation(ctx context.Context, im *api.RuntimeImage) error {
	var nodes corev1.NodeList
	if err := r.Reader.List(ctx, &nodes); err != nil {
		return err
	}
	var ps api.NodeProfileList
	if err := r.Reader.List(ctx, &ps); err != nil {
		return err
	}
	participants := []api.RotationNode{}
	excluded := []api.RotationNode{}
	complete := true
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	deadline := r.NodeProgressDeadline
	if deadline <= 0 {
		deadline = 10 * time.Minute
	}
	previous := map[types.UID]api.RotationNode{}
	for _, n := range append(append([]api.RotationNode{}, im.Status.Rotation.Participants...), im.Status.Rotation.ExcludedNodes...) {
		previous[n.UID] = n
	}
	for _, n := range nodes.Items {
		var p *api.NodeProfile
		var t api.Target
		mode := ""
		for i := range ps.Items {
			q := &ps.Items[i]
			if string(q.UID) != n.Labels[api.OwnerUID] {
				continue
			}
			for _, target := range q.Status.Targets {
				if target.Name == n.Name && target.UID == n.UID && target.Claimed {
					p = q
					t = target
				}
			}
			for _, e := range q.Spec.RuntimeImages {
				if e.ID == im.Name {
					mode = e.Mode()
				}
			}
		}
		if p == nil || mode == "" {
			continue
		}
		inventory, err := control.ParseInventory(&n)
		if err != nil {
			return fmt.Errorf("invalid node inventory %s: %w", n.Name, err)
		}
		local, advertised := inventory[im.Name]
		// An installed OnDemand runtime remains a participant while retrying.
		// A cold available entry also advertises its approved generation.
		if mode == "OnDemand" && (!advertised || !local.Installed) {
			continue
		}
		obs := api.RotationNode{Name: n.Name, UID: n.UID, ObservedGeneration: local.Generation}
		old, hadPrevious := previous[n.UID]
		obs.AssignedAt = old.AssignedAt
		obs.LastProgressAt = old.LastProgressAt
		if obs.AssignedAt == nil {
			v := metav1.NewTime(now)
			obs.AssignedAt = &v
		}
		if hadPrevious && local.InventoryUID == im.UID && local.Generation > old.ObservedGeneration {
			v := metav1.NewTime(now)
			obs.LastProgressAt = &v
		}
		current := local.InventoryUID == im.UID && local.Generation == im.Status.Generation && local.Digest == im.Status.Digest && local.State == "ready"
		reason := ""
		switch {
		case policy.Profile(p, ps.Items, r.AllowedMirrorHosts) != nil:
			reason = "InvalidProfile"
		case n.DeletionTimestamp != nil:
			reason = "NodeDeleting"
		case n.Spec.Unschedulable:
			reason = "NodeCordoned"
		case control.HasStartupTaint(&n):
			reason = "NodeWideStartupFence"
		case !p.DeletionTimestamp.IsZero() || p.Status.Retirement != nil:
			reason = "ProfileRetiring"
		default:
			ready := false
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					ready = true
				}
			}
			if !ready {
				reason = "NodeNotReady"
			}
		}
		if reason == "" && !current {
			last := obs.AssignedAt
			if obs.LastProgressAt != nil {
				last = obs.LastProgressAt
			}
			if strings.HasPrefix(old.Reason, "RotationStalled") || now.Sub(last.Time) >= deadline {
				reason = "RotationStalled"
			}
		}
		if reason != "" {
			obs.Reason = reason
			if !control.StartupFenced(&n) {
				a := authority(p, t, "provisioning", p.Status.ProvisioningGeneration)
				if p.Status.Retirement != nil {
					a = authority(p, t, "cleanup", p.Status.Retirement.Generation)
				}
				broader, e := control.WithdrawAdvertisement(ctx, r.Client, r.Reader, a, []string{"brewlet.sh/runtime." + im.Name})
				if e != nil {
					complete = false
					obs.Reason += ": UnconfirmedFence: " + e.Error()
				} else if broader {
					obs.Reason = "NodeWideStartupFence: provider refused capability withdrawal"
				}
			}
			excluded = append(excluded, obs)
			continue
		}
		if _, err := control.Check(ctx, r.Reader, authority(p, t, "provisioning", p.Status.ProvisioningGeneration)); err != nil {
			// No authority to mutate a foreign or stale claim. It cannot be silently
			// excluded: its stale advertisement still blocks completion.
			complete = false
			obs.Reason = "OwnershipUnverified: " + err.Error()
			excluded = append(excluded, obs)
			continue
		}
		participants = append(participants, obs)
		if !current || n.Labels["brewlet.sh/runtime."+im.Name] != "ready" {
			complete = false
		}
	}
	for _, t := range im.Status.RevocationTargets {
		if t.Resolved || slices.ContainsFunc(nodes.Items, func(n corev1.Node) bool { return n.UID == t.NodeUID }) {
			continue
		}
		obs := previous[t.NodeUID]
		obs.Name = t.NodeName
		obs.UID = t.NodeUID
		obs.ObservedGeneration = t.ObservedGeneration
		obs.Reason = "NodeMissing: original UID retains an independent cleanup/authorization obligation"
		if obs.AssignedAt == nil {
			v := metav1.NewTime(now)
			obs.AssignedAt = &v
		}
		excluded = append(excluded, obs)
	}
	im.Status.Rotation.Participants = participants
	im.Status.Rotation.ExcludedNodes = excluded
	if complete {
		if im.Status.RotationCompletedAt == nil {
			now := metav1.Now()
			im.Status.RotationCompletedAt = &now
		}
		runtimeCondition(im, "RotationComplete", "ParticipantsCurrent", fmt.Sprintf("epoch %s generation %d digest %s; %d excluded nodes", im.UID, im.Status.Generation, im.Status.Digest, len(excluded)), metav1.ConditionTrue)
	} else {
		runtimeCondition(im, "RotationComplete", "RotationProgressing", "waiting for target validation or confirmed exclusion fencing", metav1.ConditionFalse)
	}
	return nil
}
func (r *RuntimeReconciler) SetupWithManager(m ctrl.Manager) error {
	mapAll := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var ims api.RuntimeImageList
		if r.Reader.List(ctx, &ims) != nil {
			return nil
		}
		out := []reconcile.Request{}
		for _, im := range ims.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: im.Name}})
		}
		return out
	})
	builder := ctrl.NewControllerManagedBy(m).For(&api.RuntimeImage{}).WithOptions(runtimecontroller.Options{MaxConcurrentReconciles: 1}).Watches(&corev1.Node{}, mapAll).Watches(&api.NodeProfile{}, mapAll).Watches(&api.NodeRetirementEvidence{}, mapAll)
	if r.Restarts != nil {
		builder = builder.Watches(&appsv1.Deployment{}, mapAll).Watches(&appsv1.StatefulSet{}, mapAll).Watches(&appsv1.DaemonSet{}, mapAll).Watches(&corev1.Pod{}, mapAll)
	}
	return builder.Complete(r)
}
