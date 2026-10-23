package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const restartOptInAnnotation = "brewlet.sh/restart-on-rotation"

type k8sProfileOffer struct {
	Profile string `json:"profile"`
	Install string `json:"install"`
	IdleTTL string `json:"idleTTL,omitempty"`
	Helm    bool   `json:"helmManaged,omitempty"`
}

type k8sNodeState struct {
	Name       string `json:"name"`
	Profile    string `json:"profile"`
	Platform   string `json:"platform"`
	Install    string `json:"install"`
	Label      string `json:"label,omitempty"` // brewlet.sh/runtime.<id> value
	State      string `json:"state,omitempty"` // node inventory annotation state
	Generation int64  `json:"generation,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Fenced     bool   `json:"startupFenced,omitempty"`
}

type k8sPodRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Node      string `json:"node,omitempty"`
	Phase     string `json:"phase"`
	Owner     string `json:"owner,omitempty"`
}

type k8sWorkloadRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	OptedIn   bool   `json:"restartOnRotation"`
	Action    string `json:"action,omitempty"`
}

type k8sBindingRef struct {
	Node       string `json:"node"`
	Pod        string `json:"pod"`
	Container  string `json:"container"`
	Generation int64  `json:"generation"`
	State      string `json:"state,omitempty"`
}

func helmManaged(labels map[string]string) bool {
	return labels["app.kubernetes.io/managed-by"] == "Helm"
}

func listProfiles(ctx context.Context, c client.Client) ([]api.NodeProfile, error) {
	var ps api.NodeProfileList
	if err := c.List(ctx, &ps); err != nil {
		return nil, fmt.Errorf("list NodeProfiles: %w", err)
	}
	sort.Slice(ps.Items, func(i, j int) bool { return ps.Items[i].Name < ps.Items[j].Name })
	return ps.Items, nil
}

func profileOffers(profiles []api.NodeProfile, id string) []k8sProfileOffer {
	var out []k8sProfileOffer
	for _, p := range profiles {
		for _, e := range p.Spec.RuntimeImages {
			if e.ID == id {
				out = append(out, k8sProfileOffer{Profile: p.Name, Install: e.Mode(), IdleTTL: e.IdleTTL, Helm: helmManaged(p.Labels)})
			}
		}
	}
	return out
}

func nodePlatform(n *corev1.Node) k8sPlatform {
	p := k8sPlatform{OS: n.Labels["kubernetes.io/os"], Arch: n.Labels["kubernetes.io/arch"]}
	if p.OS == "" {
		p.OS = n.Status.NodeInfo.OperatingSystem
	}
	if p.Arch == "" {
		p.Arch = n.Status.NodeInfo.Architecture
	}
	if p.OS == "" {
		p.OS = "linux"
	}
	return p
}

// profileNodes returns the claimed target Nodes of the given profiles.
func profileNodes(ctx context.Context, c client.Client, profiles []api.NodeProfile, include func(api.NodeProfile) bool) (map[string]*corev1.Node, map[string]string, error) {
	nodes := map[string]*corev1.Node{}
	owner := map[string]string{}
	for _, p := range profiles {
		if include != nil && !include(p) {
			continue
		}
		for _, t := range p.Status.Targets {
			if !t.Claimed {
				continue
			}
			n := new(corev1.Node)
			if err := c.Get(ctx, client.ObjectKey{Name: t.Name}, n); err != nil {
				if client.IgnoreNotFound(err) == nil {
					continue
				}
				return nil, nil, fmt.Errorf("get Node %s: %w", t.Name, err)
			}
			if n.UID != t.UID && t.UID != "" {
				continue
			}
			nodes[n.Name] = n
			owner[n.Name] = p.Name
		}
	}
	return nodes, owner, nil
}

func platformsOf(nodes map[string]*corev1.Node) []k8sPlatform {
	seen := map[string]bool{}
	var out []k8sPlatform
	for _, n := range nodes {
		p := nodePlatform(n)
		if !seen[p.String()] {
			seen[p.String()] = true
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

func mergePlatforms(a, b []k8sPlatform) []k8sPlatform {
	seen := map[string]bool{}
	var out []k8sPlatform
	for _, p := range append(append([]k8sPlatform{}, a...), b...) {
		if !seen[p.String()] {
			seen[p.String()] = true
			out = append(out, p)
		}
	}
	return out
}

// runtimeNodeStates reports per-node state for id on nodes of profiles that
// offer it, from the capability label and the brewlet.sh/runtime-images
// inventory annotation (SPEC §9.1).
func runtimeNodeStates(ctx context.Context, c client.Client, profiles []api.NodeProfile, id string) ([]k8sNodeState, error) {
	modes := map[string]string{}
	for _, o := range profileOffers(profiles, id) {
		modes[o.Profile] = o.Install
	}
	nodes, owner, err := profileNodes(ctx, c, profiles, func(p api.NodeProfile) bool { _, ok := modes[p.Name]; return ok })
	if err != nil {
		return nil, err
	}
	out := []k8sNodeState{}
	for name, n := range nodes {
		s := k8sNodeState{Name: name, Profile: owner[name], Platform: nodePlatform(n).String(), Install: modes[owner[name]], Label: n.Labels["brewlet.sh/runtime."+id], Fenced: control.StartupFenced(n)}
		if inv, err := control.ParseInventory(n); err == nil {
			if r, ok := inv[id]; ok {
				s.State, s.Generation, s.Digest = r.State, r.Generation, r.Digest
			}
		} else {
			s.State = "invalid-annotation"
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// runtimePods lists live pods selecting id through brewlet.sh/runtime-image (SPEC §8.1).
func runtimePods(ctx context.Context, c client.Client, id string) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := c.List(ctx, &pods); err != nil {
		return nil, fmt.Errorf("list Pods: %w", err)
	}
	var out []corev1.Pod
	for _, p := range pods.Items {
		if p.Annotations[api.RuntimeAnnotation] != id || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Namespace+"/"+out[i].Name < out[j].Namespace+"/"+out[j].Name
	})
	return out, nil
}

func podRefs(pods []corev1.Pod) []k8sPodRef {
	out := []k8sPodRef{}
	for _, p := range pods {
		r := k8sPodRef{Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, Phase: string(p.Status.Phase)}
		if o := metaOwner(p.OwnerReferences); o != "" {
			r.Owner = o
		}
		out = append(out, r)
	}
	return out
}

func metaOwner(refs []metav1.OwnerReference) string {
	for _, r := range refs {
		if r.Controller != nil && *r.Controller {
			return r.Kind + "/" + r.Name
		}
	}
	return ""
}

type k8sWorkload struct {
	kind     string
	obj      client.Object
	template *corev1.PodTemplateSpec
	onDelete bool
	deferred string
}

// runtimeWorkloads lists Deployments, StatefulSets and DaemonSets whose pod
// template selects id with runtimeClassName brewlet.
func runtimeWorkloads(ctx context.Context, c client.Client, id string) ([]k8sWorkload, error) {
	var out []k8sWorkload
	var ds appsv1.DeploymentList
	if err := c.List(ctx, &ds); err != nil {
		return nil, fmt.Errorf("list Deployments: %w", err)
	}
	for i := range ds.Items {
		d := &ds.Items[i]
		w := k8sWorkload{kind: "Deployment", obj: d, template: &d.Spec.Template}
		if d.Spec.Paused {
			w.deferred = "Deployment is paused"
		}
		out = append(out, w)
	}
	var ss appsv1.StatefulSetList
	if err := c.List(ctx, &ss); err != nil {
		return nil, fmt.Errorf("list StatefulSets: %w", err)
	}
	for i := range ss.Items {
		s := &ss.Items[i]
		w := k8sWorkload{kind: "StatefulSet", obj: s, template: &s.Spec.Template, onDelete: s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType}
		if r := s.Spec.UpdateStrategy.RollingUpdate; r != nil && r.Partition != nil && *r.Partition > 0 {
			w.deferred = "StatefulSet partition prevents a full rollout"
		}
		out = append(out, w)
	}
	var dss appsv1.DaemonSetList
	if err := c.List(ctx, &dss); err != nil {
		return nil, fmt.Errorf("list DaemonSets: %w", err)
	}
	for i := range dss.Items {
		d := &dss.Items[i]
		out = append(out, k8sWorkload{kind: "DaemonSet", obj: d, template: &d.Spec.Template, onDelete: d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType})
	}
	filtered := out[:0]
	for _, w := range out {
		rc := w.template.Spec.RuntimeClassName
		if w.template.Annotations[api.RuntimeAnnotation] == id && rc != nil && *rc == "brewlet" {
			filtered = append(filtered, w)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		return a.obj.GetNamespace()+"/"+a.kind+"/"+a.obj.GetName() < b.obj.GetNamespace()+"/"+b.kind+"/"+b.obj.GetName()
	})
	return filtered, nil
}

// restartSettings describes the operator's rotation-restart configuration as
// read from the brewlet-operator Deployment args (chart value
// operator.rotationRestarts.*). enabled is nil when it cannot be determined.
type restartSettings struct {
	enabled    *bool
	namespaces []string
}

func readRestartSettings(ctx context.Context, c client.Client, namespace string) restartSettings {
	var d appsv1.Deployment
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "brewlet-operator"}, &d); err != nil {
		return restartSettings{}
	}
	enabled := false
	s := restartSettings{enabled: &enabled}
	for _, ctr := range d.Spec.Template.Spec.Containers {
		for _, a := range append(append([]string{}, ctr.Command...), ctr.Args...) {
			switch {
			case a == "--rotation-restarts" || a == "--rotation-restarts=true":
				enabled = true
			case strings.HasPrefix(a, "--restart-namespaces="):
				for _, ns := range strings.Split(strings.TrimPrefix(a, "--restart-namespaces="), ",") {
					if ns = strings.TrimSpace(ns); ns != "" {
						s.namespaces = append(s.namespaces, ns)
					}
				}
			}
		}
	}
	return s
}

// workloadAction explains what a rotation does to a workload (SPEC §7.6).
func workloadAction(w k8sWorkload, s restartSettings) (bool, string) {
	optedIn := w.obj.GetAnnotations()[restartOptInAnnotation] == "true"
	keep := "not restarted: running pods keep the old generation until they restart"
	if !optedIn {
		return false, keep + " (annotate the workload " + restartOptInAnnotation + "=true to opt in)"
	}
	if s.enabled == nil {
		return true, "rolling restart if operator.rotationRestarts.enabled (operator configuration unreadable)"
	}
	if !*s.enabled {
		return true, keep + " (operator.rotationRestarts.enabled=false)"
	}
	if len(s.namespaces) > 0 && !containsString(s.namespaces, w.obj.GetNamespace()) {
		return true, keep + " (namespace outside operator.rotationRestarts.namespaces)"
	}
	switch {
	case w.onDelete:
		return true, "RestartNotApplicable: OnDelete strategy; barrier applied, manual pod replacement required"
	case w.deferred != "":
		return true, "RestartDeferred: " + w.deferred
	}
	return true, "rolling restart: operator patches brewlet.sh/rotated-to-generation; the controller rolls per its update strategy"
}

func containsString(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// runtimeBindings lists containers the node shims report as leasing id
// (status.taskObservations, SPEC §7.6/§13).
func runtimeBindings(im *api.RuntimeImage) []k8sBindingRef {
	out := []k8sBindingRef{}
	if im == nil {
		return out
	}
	for _, o := range im.Status.TaskObservations {
		for _, b := range o.Bindings {
			out = append(out, k8sBindingRef{Node: o.NodeName, Pod: b.PodNamespace + "/" + b.PodName, Container: b.ContainerName, Generation: b.Generation, State: b.State})
		}
	}
	return out
}

func approvalSummary(im *api.RuntimeImage) string {
	if !im.DeletionTimestamp.IsZero() {
		return "Deleting"
	}
	c := meta.FindStatusCondition(im.Status.Conditions, "Approved")
	switch {
	case c == nil:
		return "Pending"
	case c.Status == "True" && c.ObservedGeneration == im.Generation:
		return "True"
	case c.Status == "True":
		return "Pending"
	default:
		return string(c.Status) + " (" + c.Reason + ")"
	}
}

func rotationSummary(im *api.RuntimeImage) string {
	c := meta.FindStatusCondition(im.Status.Conditions, "RotationComplete")
	switch {
	case c == nil:
		return "-"
	case c.Status == "True":
		return "Complete"
	case c.Reason == "RuntimeImageRevoked":
		return "Revoked"
	case c.Status == "Unknown":
		return "Unknown"
	default:
		return "Progressing"
	}
}
