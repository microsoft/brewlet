package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type ProfileReconciler struct {
	client.Client
	Reader             client.Reader
	Namespace          string
	WorkerImage        string
	AllowedMirrorHosts []string
	NodeMetricsAddress string
	GCGrace            time.Duration
	GCSweepInterval    time.Duration
	Recorder           record.EventRecorder
}

func setCondition(p *api.NodeProfile, kind, reason, msg string, status metav1.ConditionStatus) {
	meta.SetStatusCondition(&p.Status.Conditions, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: msg, ObservedGeneration: p.Generation})
}
func (r *ProfileReconciler) blocked(ctx context.Context, p *api.NodeProfile, reason string, err error) (ctrl.Result, error) {
	setCondition(p, "Ready", reason, err.Error(), metav1.ConditionFalse)
	if r.Recorder != nil {
		r.Recorder.Event(p, corev1.EventTypeWarning, reason, err.Error())
	}
	if e := r.Status().Update(ctx, p); e != nil {
		return ctrl.Result{}, fmt.Errorf("%v; status: %w", err, e)
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}
func (r *ProfileReconciler) save(ctx context.Context, p *api.NodeProfile) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, p); err != nil {
		return ctrl.Result{}, err
	}
	// Do not grant worker authority from an in-memory or potentially pruned ledger.
	fresh := new(api.NodeProfile)
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(p), fresh); err != nil {
		return ctrl.Result{}, err
	}
	want, _ := json.Marshal(p.Status)
	observed, _ := json.Marshal(fresh.Status)
	if fresh.UID != p.UID || !bytes.Equal(want, observed) {
		return ctrl.Result{}, fmt.Errorf("durable ownership checkpoint not observed; matching CRD required")
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}
func authority(p *api.NodeProfile, t api.Target, mode string, g int64) control.Authority {
	return control.Authority{NodeName: t.Name, NodeUID: t.UID, ProfileName: p.Name, ProfileUID: p.UID, Generation: g, TargetUIDs: []types.UID{t.UID}, Mode: mode}
}

func (r *ProfileReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	p := new(api.NodeProfile)
	if err := r.Reader.Get(ctx, req.NamespacedName, p); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if p.UID == "" {
		return ctrl.Result{}, fmt.Errorf("profile missing UID")
	}
	var profiles api.NodeProfileList
	if err := r.Reader.List(ctx, &profiles); err != nil {
		return ctrl.Result{}, err
	}
	var nodes corev1.NodeList
	if err := r.Reader.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}
	// Inventory writers before deleting or modifying anything. Names/labels never
	// confer host authority on an old or unfenced worker.
	writerErr := r.writerConflicts(ctx, p)
	retiredNodes, historyErr := r.quarantineHistory(ctx, profiles.Items, nodes.Items)
	if retiredNodes == nil && historyErr != nil {
		return r.blocked(ctx, p, "CleanupBlocked", historyErr)
	}
	if historyErr != nil {
		setCondition(p, "RetirementPending", "QuarantineBlocked", historyErr.Error(), metav1.ConditionTrue)
	}
	if writerErr != nil {
		return r.blocked(ctx, p, "OwnershipConflict", writerErr)
	}
	if !slices.Contains(p.Finalizers, api.CleanupFinalizer) {
		if !p.DeletionTimestamp.IsZero() {
			return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("deleting profile has no cleanup finalizer; cannot authorize cleanup"))
		}
		base := p.DeepCopy()
		p.Finalizers = append(p.Finalizers, api.CleanupFinalizer)
		return ctrl.Result{RequeueAfter: time.Second}, r.Patch(ctx, p, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	}
	if err := policy.Profile(p, profiles.Items, r.AllowedMirrorHosts); err != nil {
		if e := r.stopWorkers(ctx, p); e != nil {
			return ctrl.Result{}, e
		}
		// Invalid policy cannot create new cleanup authority. Existing proven
		// snapshots may only withdraw advertisements, never mutate the host.
		for _, t := range p.Status.Targets {
			if !t.Claimed {
				continue
			}
			_, e := control.WithdrawAdvertisement(ctx, r.Client, r.Reader, authority(p, t, "provisioning", p.Status.ProvisioningGeneration), capabilityKeys(nodes.Items, t))
			if e != nil {
				return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("invalid profile: %v; withdrawal: %w", err, e))
			}
		}
		return r.blocked(ctx, p, "CleanupBlocked", err)
	}
	if changed, err := r.resolveDetached(ctx, p); err != nil {
		return r.blocked(ctx, p, "CleanupBlocked", err)
	} else if changed {
		return r.save(ctx, p)
	}
	if historyErr == nil {
		if len(p.Status.DetachedRetirements) > 0 {
			setCondition(p, "RetirementPending", "OutstandingExternalObligations", "missing hosts retain frozen independent cleanup obligations", metav1.ConditionTrue)
		} else {
			setCondition(p, "RetirementPending", "NoOutstandingObligations", "no unresolved external retirement obligations", metav1.ConditionFalse)
		}
	}
	if p.Status.Retirement != nil {
		return r.retire(ctx, p)
	}
	if !p.DeletionTimestamp.IsZero() {
		if historyErr != nil {
			return r.blocked(ctx, p, "CleanupBlocked", historyErr)
		}
		if len(p.Status.DetachedRetirements) > 0 {
			return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("detached external retirement obligations remain unresolved"))
		}
		if len(p.Status.Targets) == 0 {
			if busy, err := r.workersRemain(ctx, p); err != nil || busy {
				if err == nil {
					err = fmt.Errorf("workers remain")
				}
				return r.blocked(ctx, p, "CleanupBlocked", err)
			}
			// A missing ledger cannot prove no host mutation happened.
			for _, n := range nodes.Items {
				if n.Labels[api.OwnerUID] == string(p.UID) || n.Annotations[api.OwnerName] == p.Name {
					return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("unrecorded owned Node %s", n.Name))
				}
			}
			if !p.Status.OwnershipInitialized {
				return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("ownership ledger was never initialized; cannot prove unprovisioned"))
			}
			reason := "CleanupSucceeded"
			if p.Status.ExternalRetirementResolved {
				reason = "CleanupResolved"
			}
			c := meta.FindStatusCondition(p.Status.Conditions, "CleanupComplete")
			if c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != p.Generation || c.Reason != reason {
				setCondition(p, "CleanupComplete", reason, "all original obligations are resolved and all workers are gone", metav1.ConditionTrue)
				return r.save(ctx, p)
			}
			base := p.DeepCopy()
			p.Finalizers = slices.DeleteFunc(p.Finalizers, func(s string) bool { return s == api.CleanupFinalizer })
			return ctrl.Result{}, r.Patch(ctx, p, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}
		if p.Status.ProvisioningSpec == nil || p.Status.ProvisioningGeneration <= 0 {
			return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("missing frozen provisioning authority; preserving claims and finalizer"))
		}
		p.Status.Retirement = &api.Retirement{Targets: slices.Clone(p.Status.Targets), Generation: p.Status.ProvisioningGeneration, Spec: *p.Status.ProvisioningSpec, Phase: "Stopping"}
		return r.save(ctx, p)
	}
	desired := selectNodes(p, profiles.Items, nodes.Items)
	desired = slices.DeleteFunc(desired, func(n corev1.Node) bool { return retiredNodes[n.UID] })
	departing := []api.Target{}
	for _, t := range p.Status.Targets {
		if !slices.ContainsFunc(desired, func(n corev1.Node) bool { return n.Name == t.Name && n.UID == t.UID }) {
			departing = append(departing, t)
		}
	}
	if len(departing) > 0 {
		if p.Status.ProvisioningSpec == nil {
			return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("missing frozen provisioning policy"))
		}
		p.Status.Retirement = &api.Retirement{Targets: departing, Generation: p.Status.ProvisioningGeneration, Spec: *p.Status.ProvisioningSpec, Phase: "Stopping"}
		return r.save(ctx, p)
	}
	if !p.Status.OwnershipInitialized || p.Status.ProvisioningGeneration != p.Generation || p.Status.ProvisioningSpec == nil || !reflect.DeepEqual(*p.Status.ProvisioningSpec, p.Spec) {
		if err := r.stopWorkers(ctx, p); err != nil {
			return ctrl.Result{}, err
		}
		if busy, err := r.workersRemain(ctx, p); err != nil || busy {
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		s := p.DeepCopy().Spec
		p.Status.ProvisioningSpec = &s
		p.Status.ProvisioningGeneration = p.Generation
		p.Status.OwnershipInitialized = true
		return r.save(ctx, p)
	}
	for _, n := range desired {
		idx := slices.IndexFunc(p.Status.Targets, func(t api.Target) bool { return t.Name == n.Name && t.UID == n.UID })
		if idx < 0 {
			// History across profiles fences reused machines even after Node rename.
			for _, q := range profiles.Items {
				for _, d := range q.Status.DetachedRetirements {
					for _, t := range d.Targets {
						if sameHost(t, n) {
							return r.blocked(ctx, p, "OwnershipConflict", fmt.Errorf("Node %s has unresolved retired host identity", n.Name))
						}
					}
				}
			}
			p.Status.Targets = append(p.Status.Targets, api.Target{Name: n.Name, UID: n.UID, ContainerdRestart: p.Spec.Rollout.RestartPolicy(), ProviderID: n.Spec.ProviderID, SystemUUID: n.Status.NodeInfo.SystemUUID})
			return r.save(ctx, p)
		}
		t := &p.Status.Targets[idx]
		if n.Labels[api.OwnerUID] == "" {
			hasAd := n.Annotations[api.InventoryAnnotation] != "" || n.Annotations[api.OwnerName] != "" || n.Labels[api.OwnerNodeUID] != ""
			hasCap := false
			for k := range n.Labels {
				if k == api.RuntimeLabel || strings.HasPrefix(k, "brewlet.sh/runtime.") {
					hasCap = true
				}
			}
			if hasAd || hasCap && !control.StartupFenced(&n) {
				return r.blocked(ctx, p, "OwnershipConflict", fmt.Errorf("unmanaged advertisements on Node %s", n.Name))
			}
			if n.Labels == nil {
				n.Labels = map[string]string{}
			}
			if n.Annotations == nil {
				n.Annotations = map[string]string{}
			}
			n.Labels[api.OwnerUID] = string(p.UID)
			n.Labels[api.OwnerNodeUID] = string(n.UID)
			n.Annotations[api.OwnerName] = p.Name
			if err := r.Update(ctx, &n); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if n.Labels[api.OwnerUID] != string(p.UID) || n.Labels[api.OwnerNodeUID] != string(n.UID) || n.Annotations[api.OwnerName] != p.Name {
			return r.blocked(ctx, p, "OwnershipConflict", fmt.Errorf("Node %s has foreign or incomplete claim", n.Name))
		}
		if !t.Claimed {
			t.Claimed = true
			return r.save(ctx, p)
		}
		// Preserve validated restart reversibility across later "none" edits.
		if p.Spec.Rollout.RestartPolicy() == "validated" && t.ContainerdRestart != "validated" {
			t.ContainerdRestart = "validated"
			return r.save(ctx, p)
		}
	}
	// Missing inventory is observable but does not block independently approved IDs.
	unknown := []string{}
	approved := map[string]bool{}
	for _, e := range p.Spec.RuntimeImages {
		im := new(api.RuntimeImage)
		err := r.Reader.Get(ctx, client.ObjectKey{Name: e.ID}, im)
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err != nil || !policy.Approved(im) {
			unknown = append(unknown, e.ID)
		} else {
			approved[e.ID] = true
		}
	}
	if err := r.ensureWorker(ctx, p, false); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.mirrorNodes(ctx, p, approved); err != nil {
		return r.blocked(ctx, p, "OwnershipConflict", err)
	}
	p.Status.AssignedNodes = int32(len(p.Status.Targets))
	p.Status.ReadyNodes = 0
	for _, t := range p.Status.Targets {
		for _, n := range nodes.Items {
			if n.UID == t.UID && n.Name == t.Name && t.Claimed && effectiveNodeReady(&n, p, approved) {
				p.Status.ReadyNodes++
			}
		}
	}
	if len(unknown) > 0 {
		setCondition(p, "Ready", "UnknownRuntimeImage", strings.Join(unknown, ", "), metav1.ConditionFalse)
	} else if len(p.Status.Targets) == 0 {
		setCondition(p, "Ready", "EmptyPool", "no Nodes currently match this profile; approved inventory remains available for future capacity", metav1.ConditionFalse)
	} else {
		ready := p.Status.AssignedNodes > 0 && p.Status.ReadyNodes == p.Status.AssignedNodes
		if ready {
			setCondition(p, "Ready", "AllNodesProvisioned", "all claimed Ready targets report validated baseline and eager roots without startup fence", metav1.ConditionTrue)
		} else {
			setCondition(p, "Ready", "Provisioning", "waiting for independently fenced node workers", metav1.ConditionFalse)
		}
	}
	p.Status.AssignedNodes = int32(len(p.Status.Targets))
	for _, t := range p.Status.Targets {
		for _, n := range nodes.Items {
			if len(unknown) == 0 && n.UID == t.UID && n.Name == t.Name && n.Annotations["brewlet.sh/provision-error"] != "" {
				setCondition(p, "Ready", "NodeFailure", n.Name+": "+n.Annotations["brewlet.sh/provision-error"], metav1.ConditionFalse)
			}
		}
	}
	return r.save(ctx, p)
}

func sameHost(t api.Target, n corev1.Node) bool {
	return t.UID == n.UID || (t.ProviderID != "" && t.ProviderID == n.Spec.ProviderID) || (t.SystemUUID != "" && t.SystemUUID == n.Status.NodeInfo.SystemUUID)
}

var poolKeys = []string{"cloud.google.com/gke-nodepool", "agentpool", "eks.amazonaws.com/nodegroup", "karpenter.sh/nodepool"}

func selectNodes(p *api.NodeProfile, all []api.NodeProfile, nodes []corev1.Node) []corev1.Node {
	key := p.Spec.NodePool.Key
	if key == "" {
		for _, k := range poolKeys {
			if slices.ContainsFunc(nodes, func(n corev1.Node) bool { _, ok := n.Labels[k]; return ok }) {
				key = k
				break
			}
		}
	}
	out := []corev1.Node{}
	for _, n := range nodes {
		if n.DeletionTimestamp != nil {
			continue
		}
		if !p.Spec.NodePool.IncludeControlPlane {
			if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
				continue
			}
			if _, ok := n.Labels["node-role.kubernetes.io/master"]; ok {
				continue
			}
		}
		if len(p.Spec.NodePool.Names) > 0 {
			if key == "" || !slices.Contains(p.Spec.NodePool.Names, n.Labels[key]) {
				continue
			}
		} else {
			matched := false
			for _, q := range all {
				qkey := q.Spec.NodePool.Key
				if qkey == "" {
					qkey = key
				}
				if len(q.Spec.NodePool.Names) > 0 && qkey != "" && slices.Contains(q.Spec.NodePool.Names, n.Labels[qkey]) {
					matched = true
				}
			}
			if matched {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}
func capabilityKeys(nodes []corev1.Node, t api.Target) []string {
	out := []string{}
	for _, n := range nodes {
		if n.Name == t.Name && n.UID == t.UID {
			for k := range n.Labels {
				if k == api.RuntimeLabel || strings.HasPrefix(k, "brewlet.sh/runtime.") {
					out = append(out, k)
				}
			}
		}
	}
	return out
}
func (r *ProfileReconciler) SetupWithManager(m ctrl.Manager) error {
	mapAll := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var ps api.NodeProfileList
		if r.Reader.List(ctx, &ps) != nil {
			return nil
		}
		out := []reconcile.Request{}
		for _, p := range ps.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(m).For(&api.NodeProfile{}).Owns(&appsv1.DaemonSet{}).Watches(&corev1.Node{}, mapAll).Watches(&api.RuntimeImage{}, mapAll).Watches(&api.NodeRetirementEvidence{}, mapAll).Complete(r)
}

func workerEnv(ds *appsv1.DaemonSet, key string) string {
	if len(ds.Spec.Template.Spec.Containers) != 1 {
		return ""
	}
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.Name == key && e.ValueFrom == nil {
			return e.Value
		}
	}
	return ""
}
func associated(name string, labels map[string]string, uid types.UID) bool {
	return labels[api.OwnerUID] == string(uid) || strings.HasPrefix(name, "brewlet-node-provisioner-") || strings.HasPrefix(name, "brewlet-cleanup-")
}
func (r *ProfileReconciler) writerConflicts(ctx context.Context, p *api.NodeProfile) error {
	var ds appsv1.DaemonSetList
	if err := r.Reader.List(ctx, &ds); err != nil {
		return err
	}
	for _, d := range ds.Items {
		if !associated(d.Name, d.Labels, p.UID) {
			continue
		}
		if workerEnv(&d, "BREWLET_PROFILE_UID") == "" {
			return fmt.Errorf("unfenced writer %s/%s", d.Namespace, d.Name)
		}
		if workerEnv(&d, "BREWLET_PROFILE_UID") == string(p.UID) {
			if d.Namespace != r.Namespace || d.Labels[api.OwnerUID] != string(p.UID) || !metav1.IsControlledBy(&d, p) {
				return fmt.Errorf("competing writer %s/%s", d.Namespace, d.Name)
			}
		}
	}
	var pods corev1.PodList
	if err := r.Reader.List(ctx, &pods); err != nil {
		return err
	}
	for _, pod := range pods.Items {
		known := associated(pod.Name, pod.Labels, p.UID)
		for _, c := range append(append([]corev1.Container{}, pod.Spec.Containers...), pod.Spec.InitContainers...) {
			uid := ""
			for _, e := range c.Env {
				if e.Name == "BREWLET_PROFILE_UID" {
					uid = e.Value
				}
			}
			if known && uid == "" {
				return fmt.Errorf("unfenced pod %s/%s", pod.Namespace, pod.Name)
			}
			if uid == string(p.UID) && (pod.Namespace != r.Namespace || pod.Labels[api.OwnerUID] != string(p.UID)) {
				return fmt.Errorf("competing pod %s/%s", pod.Namespace, pod.Name)
			}
		}
	}
	return nil
}
func (r *ProfileReconciler) stopWorkers(ctx context.Context, p *api.NodeProfile) error {
	var all appsv1.DaemonSetList
	if err := r.Reader.List(ctx, &all); err != nil {
		return err
	}
	for _, d := range all.Items {
		if workerEnv(&d, "BREWLET_PROFILE_UID") == string(p.UID) {
			if d.Namespace != r.Namespace || !metav1.IsControlledBy(&d, p) {
				return fmt.Errorf("foreign writer cannot be stopped")
			}
			if err := r.Delete(ctx, &d, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &d.UID, ResourceVersion: &d.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}
func (r *ProfileReconciler) workersRemain(ctx context.Context, p *api.NodeProfile) (bool, error) {
	var ds appsv1.DaemonSetList
	if err := r.Reader.List(ctx, &ds); err != nil {
		return false, err
	}
	for _, d := range ds.Items {
		if workerEnv(&d, "BREWLET_PROFILE_UID") == string(p.UID) {
			return true, nil
		}
	}
	var pods corev1.PodList
	if err := r.Reader.List(ctx, &pods); err != nil {
		return false, err
	}
	for _, pod := range pods.Items {
		for _, c := range append(append([]corev1.Container{}, pod.Spec.Containers...), pod.Spec.InitContainers...) {
			for _, e := range c.Env {
				if e.Name == "BREWLET_PROFILE_UID" && e.Value == string(p.UID) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
func (r *ProfileReconciler) ensureWorker(ctx context.Context, p *api.NodeProfile, cleanup bool) error {
	targets := p.Status.Targets
	spec := p.Spec
	podTolerations := spec.Tolerations
	g := p.Status.ProvisioningGeneration
	mode := "provisioning"
	prefix := "brewlet-node-provisioner-"
	if cleanup {
		targets = p.Status.Retirement.Targets
		spec = p.Status.Retirement.Spec
		g = p.Status.Retirement.Generation
		mode = "cleanup"
		prefix = "brewlet-cleanup-"
		shadow := &api.NodeProfile{ObjectMeta: p.ObjectMeta, Spec: spec}
		if err := policy.Profile(shadow, nil, r.AllowedMirrorHosts); err != nil {
			return fmt.Errorf("frozen cleanup policy invalid: %w", err)
		}
		podTolerations = append([]corev1.Toleration{}, spec.Tolerations...)
		for _, t := range p.Spec.Tolerations {
			if !slices.ContainsFunc(podTolerations, func(old corev1.Toleration) bool { return reflect.DeepEqual(old, t) }) {
				podTolerations = append(podTolerations, t)
			}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	uids := []types.UID{}
	terms := []corev1.NodeSelectorTerm{}
	for _, t := range targets {
		if !t.Claimed || t.RetirementEvidenceUID != "" {
			continue
		}
		uids = append(uids, t.UID)
		terms = append(terms, corev1.NodeSelectorTerm{MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{t.Name}}}, MatchExpressions: []corev1.NodeSelectorRequirement{{Key: api.OwnerUID, Operator: corev1.NodeSelectorOpIn, Values: []string{string(p.UID)}}, {Key: api.OwnerNodeUID, Operator: corev1.NodeSelectorOpIn, Values: []string{string(t.UID)}}}})
	}
	if len(uids) == 0 {
		return nil
	}
	if !cleanup && !spec.NodePool.IncludeControlPlane {
		for i := range terms {
			for _, k := range []string{"node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"} {
				terms[i].MatchExpressions = append(terms[i].MatchExpressions, corev1.NodeSelectorRequirement{Key: k, Operator: corev1.NodeSelectorOpDoesNotExist})
			}
			if !cleanup && len(spec.NodePool.Names) > 0 {
				key := spec.NodePool.Key
				if key == "" {
					var nodes corev1.NodeList
					if err := r.Reader.List(ctx, &nodes); err != nil {
						return err
					}
					for _, k := range poolKeys {
						if slices.ContainsFunc(nodes.Items, func(n corev1.Node) bool { _, ok := n.Labels[k]; return ok }) {
							key = k
							break
						}
					}
				}
				if key == "" {
					return fmt.Errorf("named pool key unresolved; refusing worker authority")
				}
				for i := range terms {
					terms[i].MatchExpressions = append(terms[i].MatchExpressions, corev1.NodeSelectorRequirement{Key: key, Operator: corev1.NodeSelectorOpIn, Values: spec.NodePool.Names})
				}
			}
		}
	}
	b, _ := json.Marshal(uids)
	profileJSON, _ := json.Marshal(spec)
	grace, sweep := r.GCGrace, r.GCSweepInterval
	if grace == 0 {
		grace = time.Hour
	}
	if sweep == 0 {
		sweep = time.Minute
	}
	if grace <= 0 || sweep <= 0 {
		return fmt.Errorf("GC grace and sweep interval must be positive")
	}
	allowed, _ := json.Marshal(r.AllowedMirrorHosts)
	labels := map[string]string{api.OwnerUID: string(p.UID), "app.kubernetes.io/name": "brewlet-worker", "brewlet.sh/mode": mode}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: prefix + p.Name, Namespace: r.Namespace, Labels: labels}}
	desired := appsv1.DaemonSetSpec{
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: corev1.PodSpec{
				ServiceAccountName:       "brewlet-node",
				DeprecatedServiceAccount: "brewlet-node",
				HostPID:                  true, HostNetwork: true,
				DNSPolicy:                     corev1.DNSClusterFirstWithHostNet,
				RestartPolicy:                 corev1.RestartPolicyAlways,
				SchedulerName:                 corev1.DefaultSchedulerName,
				TerminationGracePeriodSeconds: int64Ptr(30),
				EnableServiceLinks:            boolPtr(true),
				SecurityContext:               &corev1.PodSecurityContext{},
				Tolerations:                   podTolerations,
				Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: terms},
				}},
				Containers: []corev1.Container{{
					Name: "provisioner", Image: r.WorkerImage, Command: []string{"/brewlet-node"},
					ImagePullPolicy:          corev1.PullIfNotPresent,
					TerminationMessagePath:   corev1.TerminationMessagePathDefault,
					TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					SecurityContext:          &corev1.SecurityContext{Privileged: boolPtr(true)},
					ReadinessProbe:           &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"/brewlet-node", "--health-check"}}}, PeriodSeconds: 5, TimeoutSeconds: 2, FailureThreshold: 1},
					Env: []corev1.EnvVar{
						{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
						{Name: "BREWLET_PROFILE_NAME", Value: p.Name},
						{Name: "BREWLET_PROFILE_UID", Value: string(p.UID)},
						{Name: "BREWLET_TARGET_UIDS", Value: string(b)},
						{Name: "BREWLET_PROFILE_GENERATION", Value: fmt.Sprint(g)},
						{Name: "BREWLET_MODE", Value: mode},
						{Name: "BREWLET_PROFILE_SPEC", Value: string(profileJSON)},
						{Name: "SOURCE_ALLOWED_MIRROR_HOSTS", Value: string(allowed)},
						{Name: "BREWLET_GC_GRACE", Value: grace.String()},
						{Name: "BREWLET_GC_SWEEP_INTERVAL", Value: sweep.String()},
					},
					VolumeMounts: []corev1.VolumeMount{{
						Name: "host", MountPath: "/host",
						MountPropagation: propagationPtr(corev1.MountPropagationBidirectional),
					}},
				}},
				Volumes: []corev1.Volume{{
					Name:         "host",
					VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/", Type: hostPathPtr(corev1.HostPathDirectory)}},
				}},
			},
		},
	}
	maxUnavailable := spec.Rollout.MaxUnavailable
	if maxUnavailable == 0 {
		maxUnavailable = 1
	}
	unavailable := intstr.FromInt32(maxUnavailable)
	desired.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType, RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: &unavailable}}
	if r.NodeMetricsAddress != "" {
		desired.Template.Spec.Containers[0].Env = append(desired.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "BREWLET_METRICS_ADDRESS", Value: r.NodeMetricsAddress})
	}
	for _, path := range []string{"/var/lib/containerd", "/run/containerd"} {
		name := "containerd-root"
		if path == "/run/containerd" {
			name = "containerd-run"
		}
		desired.Template.Spec.Containers[0].VolumeMounts = append(desired.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: name, MountPath: path, MountPropagation: propagationPtr(corev1.MountPropagationBidirectional)})
		desired.Template.Spec.Volumes = append(desired.Template.Spec.Volumes, corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path, Type: hostPathPtr(corev1.HostPathDirectory)}}})
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ds), ds); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		ds.Spec = desired
		if err := ctrl.SetControllerReference(p, ds, r.Scheme()); err != nil {
			return err
		}
		return r.Create(ctx, ds)
	}
	if !metav1.IsControlledBy(ds, p) {
		return fmt.Errorf("worker name occupied by foreign resource")
	}
	if !reflect.DeepEqual(ds.Spec.Template, desired.Template) || !reflect.DeepEqual(ds.Spec.UpdateStrategy, desired.UpdateStrategy) {
		ds.Spec = desired
		return r.Update(ctx, ds)
	}
	return nil
}
func boolPtr(v bool) *bool                                                      { return &v }
func int64Ptr(v int64) *int64                                                   { return &v }
func propagationPtr(v corev1.MountPropagationMode) *corev1.MountPropagationMode { return &v }
func hostPathPtr(v corev1.HostPathType) *corev1.HostPathType                    { return &v }

func (r *ProfileReconciler) retire(ctx context.Context, p *api.NodeProfile) (ctrl.Result, error) {
	d := p.Status.Retirement
	setCondition(p, "Ready", "CleanupPending", "frozen retirement is waiting for confirmed withdrawal, cleanup and complete worker teardown", metav1.ConditionFalse)
	if d.Spec.Rollout.RestartPolicy() != "validated" && d.Spec.Rollout.RestartPolicy() != "none" {
		return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("unsupported frozen cleanup policy"))
	}
	switch d.Phase {
	case "Stopping":
		for _, t := range d.Targets {
			n := new(corev1.Node)
			err := r.Reader.Get(ctx, client.ObjectKey{Name: t.Name}, n)
			if err != nil && !apierrors.IsNotFound(err) {
				return r.blocked(ctx, p, "CleanupBlocked", err)
			}
			if err == nil && n.UID == t.UID && t.Claimed {
				if _, err := control.WithdrawAdvertisement(ctx, r.Client, r.Reader, authority(p, t, "cleanup", d.Generation), capabilityKeys([]corev1.Node{*n}, t)); err != nil {
					return r.blocked(ctx, p, "CleanupBlocked", err)
				}
			}
		}
		if err := r.stopWorkers(ctx, p); err != nil {
			return ctrl.Result{}, err
		}
		if busy, err := r.workersRemain(ctx, p); err != nil || busy {
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		missing := []api.Target{}
		for _, t := range d.Targets {
			n := new(corev1.Node)
			err := r.Reader.Get(ctx, client.ObjectKey{Name: t.Name}, n)
			if err != nil && !apierrors.IsNotFound(err) {
				return r.blocked(ctx, p, "CleanupBlocked", err)
			}
			if apierrors.IsNotFound(err) || n.UID != t.UID {
				missing = append(missing, t)
			}
		}
		// Copy each loss into independent durable history before removing any
		// active membership. A pruned field makes save/readback fail closed.
		for _, t := range missing {
			found := slices.ContainsFunc(p.Status.DetachedRetirements, func(old api.Retirement) bool {
				return len(old.Targets) == 1 && old.Targets[0].UID == t.UID && old.Generation == d.Generation
			})
			if !found {
				p.Status.DetachedRetirements = append(p.Status.DetachedRetirements, api.Retirement{Targets: []api.Target{t}, Generation: d.Generation, Spec: d.Spec, Phase: "Missing"})
				return r.save(ctx, p)
			}
		}
		if len(missing) > 0 {
			isMissing := func(t api.Target) bool {
				return slices.ContainsFunc(missing, func(old api.Target) bool { return old.Name == t.Name && old.UID == t.UID })
			}
			d.Targets = slices.DeleteFunc(d.Targets, isMissing)
			p.Status.Targets = slices.DeleteFunc(p.Status.Targets, isMissing)
			if len(d.Targets) == 0 {
				p.Status.Retirement = nil
			}
			return r.save(ctx, p)
		}
		d.Phase = "Cleaning"
		return r.save(ctx, p)
	case "Cleaning":
		for _, t := range d.Targets {
			n := new(corev1.Node)
			if err := r.Reader.Get(ctx, client.ObjectKey{Name: t.Name}, n); err != nil || n.UID != t.UID {
				if err != nil && !apierrors.IsNotFound(err) {
					return r.blocked(ctx, p, "CleanupBlocked", err)
				}
				d.Phase = "Stopping"
				return r.save(ctx, p)
			}
			if _, err := control.Check(ctx, r.Reader, authority(p, t, "cleanup", d.Generation)); err != nil {
				return r.blocked(ctx, p, "CleanupBlocked", err)
			}
		}
		if err := r.ensureWorker(ctx, p, true); err != nil {
			return ctrl.Result{}, err
		}
		complete := true
		for _, t := range d.Targets {
			n := new(corev1.Node)
			if err := r.Reader.Get(ctx, client.ObjectKey{Name: t.Name}, n); err != nil {
				return ctrl.Result{}, err
			}
			if n.Annotations["brewlet.sh/cleanup-complete"] != fmt.Sprintf("%s/%d", p.UID, d.Generation) {
				complete = false
			}
			if len(capabilityKeys([]corev1.Node{*n}, t)) > 0 || n.Annotations[api.InventoryAnnotation] != "" && n.Annotations[api.InventoryAnnotation] != "{}" {
				complete = false
			}
		}
		if !complete {
			setCondition(p, "Ready", "CleanupBlocked", "waiting for fenced worker revocation, lease drain, host cleanup and advertisement withdrawal; pinned stale labels require provider repair", metav1.ConditionFalse)
			return r.save(ctx, p)
		}
		d.Phase = "Teardown"
		if !p.DeletionTimestamp.IsZero() {
			setCondition(p, "CleanupComplete", "CleanupSucceeded", "host cleanup completed; worker teardown remains", metav1.ConditionTrue)
		}
		return r.save(ctx, p)
	case "Teardown":
		if err := inventoryTargetsResolved(ctx, r.Reader, p, d.Targets); err != nil {
			return r.blocked(ctx, p, "CleanupBlocked", err)
		}
		if err := r.stopWorkers(ctx, p); err != nil {
			return ctrl.Result{}, err
		}
		if busy, err := r.workersRemain(ctx, p); err != nil || busy {
			if err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		for _, t := range d.Targets {
			n := new(corev1.Node)
			err := r.Reader.Get(ctx, client.ObjectKey{Name: t.Name}, n)
			// Only a durable proven-clean Teardown may survive Node disappearance.
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return ctrl.Result{}, err
			}
			if n.UID != t.UID {
				continue
			}
			if n.Labels[api.OwnerUID] == "" {
				continue
			}
			if err := control.ReleaseClaim(ctx, r.Client, r.Reader, authority(p, t, "cleanup", d.Generation)); err != nil {
				return r.blocked(ctx, p, "CleanupBlocked", err)
			}
		}
		p.Status.Targets = slices.DeleteFunc(p.Status.Targets, func(t api.Target) bool {
			return slices.ContainsFunc(d.Targets, func(old api.Target) bool { return old.Name == t.Name && old.UID == t.UID })
		})
		p.Status.Retirement = nil
		return r.save(ctx, p)
	default:
		return r.blocked(ctx, p, "CleanupBlocked", fmt.Errorf("unsupported retirement phase %q", d.Phase))
	}
}
