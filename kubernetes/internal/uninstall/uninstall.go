// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package uninstall gates Helm removal of the operator on NodeProfile cleanup.
// It never removes finalizers or deletes workers; the running operator owns
// teardown. Callers must supply an uncached API client and stop concurrent
// profile management while uninstalling.
package uninstall

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const DefaultTimeout = 240 * time.Second

type Options struct {
	ReleaseName      string
	ReleaseNamespace string
	// Namespace identifies the operator; worker inventory is cluster-wide.
	Namespace string
	Timeout   time.Duration
	// PollInterval defaults to one second. It does not change the timeout.
	PollInterval time.Duration
}

func (o Options) Validate() error {
	if len(o.ReleaseName) > 53 || len(validation.IsDNS1123Subdomain(o.ReleaseName)) != 0 {
		return fmt.Errorf("--uninstall-release-name must be a nonempty Helm release name (DNS subdomain, at most 53 characters)")
	}
	for _, field := range []struct{ name, value string }{
		{"uninstall-release-namespace", o.ReleaseNamespace},
		{"namespace", o.Namespace},
	} {
		if len(validation.IsDNS1123Label(field.value)) != 0 {
			return fmt.Errorf("--%s must be a nonempty Kubernetes namespace name", field.name)
		}
	}
	if o.Timeout <= 0 {
		return fmt.Errorf("--uninstall-timeout must be greater than zero")
	}
	if o.PollInterval < 0 {
		return fmt.Errorf("uninstall poll interval must not be negative")
	}
	return nil
}

// Run deletes only this release's profiles, with UID and resourceVersion
// preconditions, then waits for profiles and workers to disappear. Any error
// must fail the pre-delete hook so Helm keeps the operator and its RBAC.
// Standalone workers and known workers outside the operator namespace require
// separate deprovisioning and block before profile deletion.
// The client must be direct (client.New), not a manager's cached client.
func Run(ctx context.Context, c client.Client, o Options) error {
	if err := o.Validate(); err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("uninstall requires a direct Kubernetes client")
	}
	if o.PollInterval == 0 {
		o.PollInterval = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	r := runner{
		client: c, options: o,
		profiles: make(map[string]types.UID), workers: make(map[types.UID]bool),
	}
	pending := "checking NodeProfiles and workers"
	empty := false
	for {
		if err := ctx.Err(); err != nil {
			return incomplete(err, pending)
		}
		done, detail, err := r.poll(ctx)
		if err != nil {
			return incomplete(err, pending)
		}
		pending = detail
		if err := ctx.Err(); err != nil {
			return incomplete(err, pending)
		}
		// Require two fresh, complete empty scans separated by a poll interval.
		// This catches creates racing a scan, but is not a cluster-wide lock.
		if done && empty {
			return nil
		}
		empty = done
		timer := time.NewTimer(o.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return incomplete(ctx.Err(), pending)
		case <-timer.C:
		}
	}
}

func incomplete(err error, pending string) error {
	return fmt.Errorf("uninstall cleanup incomplete (%s): %w; keep the operator and its RBAC installed, resolve cleanup or concurrent profile changes, and retry Helm uninstall", pending, err)
}

type runner struct {
	client   client.Client
	options  Options
	profiles map[string]types.UID
	workers  map[types.UID]bool
}

func (r *runner) listProfiles(ctx context.Context) ([]nodev1alpha1.NodeProfile, error) {
	var profiles nodev1alpha1.NodeProfileList
	if err := r.client.List(ctx, &profiles); err != nil {
		return nil, fmt.Errorf("listing cluster-wide NodeProfiles: %w", err)
	}
	var foreign []string
	for i := range profiles.Items {
		p := &profiles.Items[i]
		if p.Labels["app.kubernetes.io/managed-by"] != "Helm" ||
			p.Annotations["meta.helm.sh/release-name"] != r.options.ReleaseName ||
			p.Annotations["meta.helm.sh/release-namespace"] != r.options.ReleaseNamespace {
			foreign = append(foreign, p.Name)
		}
	}
	if len(foreign) > 0 {
		sort.Strings(foreign)
		return nil, fmt.Errorf("NodeProfiles not owned by Helm release %s/%s remain: %s; this operator manages all profiles: drain and remove those profiles through their owner before uninstalling; do not remove finalizers",
			r.options.ReleaseNamespace, r.options.ReleaseName, strings.Join(foreign, ", "))
	}
	for i := range profiles.Items {
		p := &profiles.Items[i]
		if p.UID == "" || p.ResourceVersion == "" {
			return nil, fmt.Errorf("NodeProfile %q has no UID or resourceVersion; refusing an unguarded deletion", p.Name)
		}
		if uid, seen := r.profiles[p.Name]; seen && uid != p.UID {
			return nil, fmt.Errorf("NodeProfile %q was replaced (UID %s became %s); stop concurrent profile changes before retrying", p.Name, uid, p.UID)
		}
		r.profiles[p.Name] = p.UID
	}
	return profiles.Items, nil
}

func (r *runner) poll(ctx context.Context) (bool, string, error) {
	profiles, err := r.listProfiles(ctx)
	if err != nil {
		return false, "", err
	}
	workers, err := r.listWorkers(ctx)
	if err != nil {
		return false, "", err
	}
	var pending []string
	for i := range profiles {
		p := &profiles[i]
		pending = append(pending, pendingProfile(p))
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		// An ownership transfer, finalizer change, status update, or replacement
		// between List and Delete must be rejected by the API server.
		err := r.client.Delete(ctx, p, client.Preconditions{
			UID: &p.UID, ResourceVersion: &p.ResourceVersion,
		})
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			// Re-list and re-check every profile's ownership before retrying.
			return false, "rechecking profiles after a concurrent change", nil
		}
		if err != nil {
			return false, "", fmt.Errorf("deleting NodeProfile %q: %w", p.Name, err)
		}
	}
	pending = append(pending, workers...)
	if len(pending) != 0 {
		sort.Strings(pending)
		return false, strings.Join(pending, ", "), nil
	}
	// Worker reads are not atomic with the first profile list. Check again
	// before treating this scan as empty, without ever using an informer cache.
	profiles, err = r.listProfiles(ctx)
	if err != nil {
		return false, "", err
	}
	if len(profiles) != 0 {
		return false, "new NodeProfiles appeared during worker inspection", nil
	}
	return true, "confirming no NodeProfiles or workers remain", nil
}

func pendingProfile(p *nodev1alpha1.NodeProfile) string {
	detail := fmt.Sprintf("NodeProfile %s (finalizers: %v)", p.Name, p.Finalizers)
	// Ready may describe desired-spec validation while deletion cleans a
	// previously recorded inventory. Surface it for repair, but leave cleanup
	// safety and completion to the controller's finalizer, not a status gate.
	if ready := meta.FindStatusCondition(p.Status.Conditions, nodev1alpha1.ConditionReady); ready != nil {
		detail += fmt.Sprintf("; Ready=%s/%s (observed generation %d, current %d): %s",
			ready.Status, ready.Reason, ready.ObservedGeneration, p.Generation, ready.Message)
	}
	return detail
}

func (r *runner) listWorkers(ctx context.Context) ([]string, error) {
	var daemonSets appsv1.DaemonSetList
	if err := r.client.List(ctx, &daemonSets); err != nil {
		return nil, fmt.Errorf("listing cluster-wide DaemonSets for operator namespace %q: %w", r.options.Namespace, err)
	}
	var pending, standalone, outside []string
	record := func(kind string, obj client.Object, legacy bool) {
		description := kind + " " + obj.GetNamespace() + "/" + obj.GetName()
		if obj.GetNamespace() != r.options.Namespace {
			outside = append(outside, description)
		} else if legacy {
			standalone = append(standalone, description)
		} else {
			pending = append(pending, description)
		}
	}
	for i := range daemonSets.Items {
		ds := &daemonSets.Items[i]
		if standaloneDaemonSet(ds) {
			record("DaemonSet", ds, true)
			continue
		}
		if r.workers[ds.UID] || workerDaemonSet(ds) {
			if ds.UID != "" {
				r.workers[ds.UID] = true
			}
			record("DaemonSet", ds, false)
		}
	}
	var pods corev1.PodList
	if err := r.client.List(ctx, &pods); err != nil {
		return nil, fmt.Errorf("listing cluster-wide pods for operator namespace %q: %w", r.options.Namespace, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if standalonePod(pod) {
			record("Pod", pod, true)
			continue
		}
		if workerPod(pod, r.workers) {
			// Terminating and completed pods still exist and must block success.
			record("Pod", pod, false)
		}
	}
	if len(outside) != 0 {
		sort.Strings(outside)
		return nil, fmt.Errorf("provisioning or cleanup workers outside configured operator namespace %q remain: %s; restore their responsible operator or deprovision those installations separately and finish worker teardown before retrying Helm uninstall; this runner will not delete or adopt them",
			r.options.Namespace, strings.Join(outside, ", "))
	}
	if len(standalone) != 0 {
		sort.Strings(standalone)
		return nil, fmt.Errorf("standalone provisioning workers remain: %s; drain workloads and deprovision the standalone installation separately, then remove its workers before retrying Helm uninstall; this runner will not delete or adopt them",
			strings.Join(standalone, ", "))
	}
	return pending, nil
}

func standaloneDaemonSet(ds *appsv1.DaemonSet) bool {
	return ds.Name == brewlet.ProvisionerName && (ds.Labels["app"] == brewlet.ProvisionerAppLabel ||
		ds.Spec.Template.Labels["app"] == brewlet.ProvisionerAppLabel)
}

func standalonePod(pod *corev1.Pod) bool {
	if pod.Labels["app"] != brewlet.ProvisionerAppLabel {
		return false
	}
	if ref := metav1.GetControllerOf(pod); ref != nil {
		return controllerRef(ref, appsv1.GroupName, "DaemonSet") && ref.Name == brewlet.ProvisionerName
	}
	// Orphan propagation can remove the DS owner reference. Kubernetes keeps
	// the canonical GenerateName on those pods; an app label or name prefix
	// alone is not enough to identify an unrelated workload as a legacy worker.
	prefix := brewlet.ProvisionerName + "-"
	return pod.GenerateName == prefix && strings.HasPrefix(pod.Name, prefix)
}

func controllerRef(ref *metav1.OwnerReference, group, kind string) bool {
	if ref == nil || ref.UID == "" || ref.Name == "" || ref.Kind != kind {
		return false
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	return err == nil && gv.Group == group
}

func workerName(labels map[string]string) string {
	profile := labels[brewlet.LabelNodeProfile]
	if profile == "" {
		return ""
	}
	switch labels["app"] {
	case brewlet.ProvisionerAppLabel:
		return brewlet.ProfileDaemonSetName(profile)
	case "brewlet-cleanup":
		return brewlet.CleanupDaemonSetName(profile)
	default:
		return ""
	}
}

func workerDaemonSet(ds *appsv1.DaemonSet) bool {
	ref := metav1.GetControllerOf(ds)
	if controllerRef(ref, nodev1alpha1.GroupVersion.Group, "NodeProfile") {
		return true
	}
	// Recognize workers orphaned by forced profile deletion, without treating
	// an unrelated DaemonSet sharing only a name prefix or app label as ours.
	return ref == nil && ds.Name != "" && (workerName(ds.Labels) == ds.Name ||
		workerName(ds.Spec.Template.Labels) == ds.Name)
}

func workerPod(pod *corev1.Pod, workers map[types.UID]bool) bool {
	ref := metav1.GetControllerOf(pod)
	if controllerRef(ref, nodev1alpha1.GroupVersion.Group, "NodeProfile") {
		return true
	}
	if ref == nil {
		return workerName(pod.Labels) != ""
	}
	if !controllerRef(ref, appsv1.GroupName, "DaemonSet") {
		// In particular, Helm hook Job pods are not teardown workers.
		return false
	}
	return workers[ref.UID] || (workerName(pod.Labels) != "" && workerName(pod.Labels) == ref.Name)
}
