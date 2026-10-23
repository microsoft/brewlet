package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type restartWorkload struct {
	kind          string
	object        client.Object
	template      *corev1.PodTemplateSpec
	selector      *metav1.LabelSelector
	deferred      string
	onDelete      bool
	complete      bool
	nativeFailure string
}

func describeWorkload(obj client.Object) (restartWorkload, error) {
	w := restartWorkload{object: obj}
	switch o := obj.(type) {
	case *appsv1.Deployment:
		w.kind = "Deployment"
		w.template = &o.Spec.Template
		w.selector = o.Spec.Selector
		if o.Spec.Paused {
			w.deferred = "Deployment is paused; resume it to replace old-generation pods"
		}
		replicas := int32(1)
		if o.Spec.Replicas != nil {
			replicas = *o.Spec.Replicas
		}
		observed := o.Status.ObservedGeneration >= o.Generation
		w.complete = observed && o.Status.UpdatedReplicas == replicas && o.Status.Replicas == replicas && o.Status.AvailableReplicas == replicas
		if observed {
			for _, c := range o.Status.Conditions {
				if c.Type == appsv1.DeploymentProgressing && c.Status == corev1.ConditionFalse && c.Reason == "ProgressDeadlineExceeded" {
					w.nativeFailure = c.Message
					if w.nativeFailure == "" {
						w.nativeFailure = "ProgressDeadlineExceeded"
					}
				}
			}
		}
	case *appsv1.StatefulSet:
		w.kind = "StatefulSet"
		w.template = &o.Spec.Template
		w.selector = o.Spec.Selector
		w.onDelete = o.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType
		if o.Spec.UpdateStrategy.RollingUpdate != nil && o.Spec.UpdateStrategy.RollingUpdate.Partition != nil && *o.Spec.UpdateStrategy.RollingUpdate.Partition > 0 {
			w.deferred = "StatefulSet partition prevents replacement of all old-generation pods"
		}
		replicas := int32(1)
		if o.Spec.Replicas != nil {
			replicas = *o.Spec.Replicas
		}
		w.complete = o.Status.ObservedGeneration >= o.Generation && o.Status.UpdatedReplicas == replicas && o.Status.ReadyReplicas == replicas && o.Status.CurrentRevision == o.Status.UpdateRevision
	case *appsv1.DaemonSet:
		w.kind = "DaemonSet"
		w.template = &o.Spec.Template
		w.selector = o.Spec.Selector
		w.onDelete = o.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType
		want := o.Status.DesiredNumberScheduled
		w.complete = o.Status.ObservedGeneration >= o.Generation && o.Status.UpdatedNumberScheduled == want && o.Status.NumberAvailable == want && o.Status.NumberReady == want && o.Status.NumberUnavailable == 0
	default:
		return w, fmt.Errorf("unsupported restart workload %T", obj)
	}
	return w, nil
}

// listRestartWorkloads lists candidate workloads cluster-wide, or only in the
// configured namespaces when restarts are namespace-scoped (§7.6 Enablement).
func listRestartWorkloads(ctx context.Context, r client.Reader, namespaces []string) ([]restartWorkload, error) {
	scopes := [][]client.ListOption{nil}
	if len(namespaces) > 0 {
		scopes = nil
		for _, ns := range namespaces {
			scopes = append(scopes, []client.ListOption{client.InNamespace(ns)})
		}
	}
	out := []restartWorkload{}
	for _, opts := range scopes {
		var deployments appsv1.DeploymentList
		if err := r.List(ctx, &deployments, opts...); err != nil {
			return nil, err
		}
		for i := range deployments.Items {
			w, _ := describeWorkload(deployments.Items[i].DeepCopy())
			out = append(out, w)
		}
		var statefulsets appsv1.StatefulSetList
		if err := r.List(ctx, &statefulsets, opts...); err != nil {
			return nil, err
		}
		for i := range statefulsets.Items {
			w, _ := describeWorkload(statefulsets.Items[i].DeepCopy())
			out = append(out, w)
		}
		var daemonsets appsv1.DaemonSetList
		if err := r.List(ctx, &daemonsets, opts...); err != nil {
			return nil, err
		}
		for i := range daemonsets.Items {
			w, _ := describeWorkload(daemonsets.Items[i].DeepCopy())
			out = append(out, w)
		}
	}
	return out, nil
}

// ParseRestartNamespaces validates a comma-separated restart namespace
// allowlist. An empty value means cluster-wide.
func ParseRestartNamespaces(raw string) ([]string, error) {
	out := []string{}
	for _, ns := range strings.Split(raw, ",") {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return nil, fmt.Errorf("invalid restart namespace %q: %s", ns, strings.Join(errs, "; "))
		}
		if !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out, nil
}
func refreshWorkload(ctx context.Context, r client.Reader, w restartWorkload) (restartWorkload, error) {
	var o client.Object
	switch w.kind {
	case "Deployment":
		o = new(appsv1.Deployment)
	case "StatefulSet":
		o = new(appsv1.StatefulSet)
	case "DaemonSet":
		o = new(appsv1.DaemonSet)
	default:
		return w, fmt.Errorf("unsupported kind")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(w.object), o); err != nil {
		return w, err
	}
	if o.GetUID() != w.object.GetUID() {
		return w, fmt.Errorf("workload UID changed; refusing stale restart")
	}
	return describeWorkload(o)
}
func workloadPods(ctx context.Context, r client.Reader, w restartWorkload) ([]corev1.Pod, error) {
	// UID-bound controller ownership, including a Deployment's ReplicaSet hop,
	// is the authority. Label drift must not hide an old owned running pod.
	_, err := metav1.LabelSelectorAsSelector(w.selector)
	if err != nil {
		return nil, err
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(w.object.GetNamespace())); err != nil {
		return nil, err
	}
	rsUIDs := map[types.UID]bool{}
	if w.kind == "Deployment" {
		var replicasets appsv1.ReplicaSetList
		if err := r.List(ctx, &replicasets, client.InNamespace(w.object.GetNamespace())); err != nil {
			return nil, err
		}
		for _, rs := range replicasets.Items {
			if owner := metav1.GetControllerOf(&rs); owner != nil && owner.UID == w.object.GetUID() && owner.Kind == "Deployment" {
				rsUIDs[rs.UID] = true
			}
		}
	}
	out := []corev1.Pod{}
	for _, p := range pods.Items {
		owner := metav1.GetControllerOf(&p)
		if owner == nil {
			continue
		}
		owned := owner.UID == w.object.GetUID() && owner.Kind == w.kind
		if w.kind == "Deployment" {
			owned = owner.Kind == "ReplicaSet" && rsUIDs[owner.UID]
		}
		if owned {
			out = append(out, p)
		}
	}
	return out, nil
}

// stalePods reports whether any running pod still runs, or may still run, an
// older runtime generation (§7.6 Selection). Genuine lease bindings decide
// first; a pod is treated as current only when a fresh, complete observation
// of its node postdates its latest container start and lists no older binding
// for it. Without such evidence the start-time rule applies, so uncertainty
// fails toward restarting.
func stalePods(pods []corev1.Pod, im *api.RuntimeImage, now time.Time) bool {
	for _, p := range pods {
		if podOnOlderGeneration(p, im, now) {
			return true
		}
	}
	return false
}

func podOnOlderGeneration(p corev1.Pod, im *api.RuntimeImage, now time.Time) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, o := range im.Status.TaskObservations {
		if slices.ContainsFunc(o.Bindings, func(b api.RuntimeTaskBinding) bool { return b.PodUID == p.UID }) {
			return true
		}
	}
	if leaseObservedCurrent(p, im, now) {
		return false
	}
	completed := im.Status.RotationCompletedAt
	return completed != nil && p.Status.StartTime != nil && p.Status.StartTime.Before(completed)
}

func leaseObservedCurrent(p corev1.Pod, im *api.RuntimeImage, now time.Time) bool {
	if p.Spec.NodeName == "" || p.Status.StartTime == nil {
		return false
	}
	started := p.Status.StartTime.Time
	for _, s := range p.Status.ContainerStatuses {
		if s.State.Running != nil && s.State.Running.StartedAt.After(started) {
			started = s.State.Running.StartedAt.Time
		}
	}
	for _, o := range im.Status.TaskObservations {
		if o.NodeName != p.Spec.NodeName || !o.Current || o.Reason != "CompleteTaskAndLeaseObservation" {
			continue
		}
		if o.ObservedAt.After(now.Add(observationFutureSkew)) || now.Sub(o.ObservedAt) > observationMaxAge || !o.ObservedAt.After(started) {
			continue
		}
		return true
	}
	return false
}
func neverStartedFailure(t *corev1.ContainerStateTerminated) string {
	if t != nil && t.Reason == "StartError" && t.StartedAt.Time.Unix() <= 0 {
		return t.Message
	}
	return ""
}

func restartBlockers(pods []corev1.Pod, im *api.RuntimeImage) []api.RestartBlockedPod {
	blocked := []api.RestartBlockedPod{}
	for _, p := range pods {
		reason := ""
		statuses := append(append(append([]corev1.ContainerStatus{}, p.Status.ContainerStatuses...), p.Status.InitContainerStatuses...), p.Status.EphemeralContainerStatuses...)
		for _, s := range statuses {
			if s.State.Running != nil {
				continue
			}
			message := neverStartedFailure(s.State.Terminated)
			if s.State.Waiting != nil {
				// Kubelet retains Create failures in lastState during restart backoff.
				message += " " + s.State.Waiting.Reason + " " + s.State.Waiting.Message
				message += " " + neverStartedFailure(s.LastTerminationState.Terminated)
			}
			for _, code := range []string{"RuntimeImageInstalling", "RuntimeImageUpdating", "RuntimeImageEpochMismatch", "RuntimeImageAuthorizationExpired", "RuntimeImageRevoked"} {
				if strings.Contains(message, code) {
					reason = code
					break
				}
			}
		}
		if reason == "" && p.Status.Phase != corev1.PodRunning {
			for _, n := range im.Status.Rotation.ExcludedNodes {
				if n.Name == p.Spec.NodeName && p.Spec.NodeName != "" {
					reason = "NodeExcluded"
					break
				}
			}
		}
		if reason != "" {
			blocked = append(blocked, api.RestartBlockedPod{Name: p.Name, Namespace: p.Namespace, UID: p.UID, Node: p.Spec.NodeName, Reason: reason, MinimumGeneration: im.Status.Generation})
		}
	}
	return blocked
}
