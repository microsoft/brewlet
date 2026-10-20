// Package inventory exports read-only observed dependency facts for external
// scanners. It never resolves packages or changes Kubernetes workloads.
package inventory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/artifact"
	"github.com/microsoft/brewlet/pkg/node"
	brewletplatform "github.com/microsoft/brewlet/pkg/platform"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ObjectIdentity struct {
	Kind      string    `json:"kind"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	UID       types.UID `json:"uid"`
}

type Container struct {
	Pod             ObjectIdentity       `json:"pod"`
	Workload        ObjectIdentity       `json:"workload"`
	NodeName        string               `json:"nodeName"`
	Name            string               `json:"name"`
	ContainerID     string               `json:"containerID"`
	ObservedImageID string               `json:"observedImageID"`
	Status          string               `json:"status"`
	Reason          string               `json:"reason,omitempty"`
	Binding         *node.RuntimeBinding `json:"binding,omitempty"`
	EvidenceKey     string               `json:"evidenceKey,omitempty"`
}

type Report struct {
	SchemaVersion int                                  `json:"schemaVersion"`
	Cluster       string                               `json:"cluster"`
	StartedAt     time.Time                            `json:"startedAt"`
	FinishedAt    time.Time                            `json:"finishedAt"`
	Namespaces    []string                             `json:"namespaces"`
	Workloads     []string                             `json:"workloads,omitempty"`
	Complete      bool                                 `json:"complete"`
	Containers    []Container                          `json:"containers"`
	Artifacts     map[string]artifact.DependencyReport `json:"artifacts"`
	Warnings      []string                             `json:"warnings"`
}

type Options struct {
	Cluster    string
	Namespaces []string
	Workloads  []string
	MaxAge     time.Duration
	ReadImage  func(context.Context, node.ApplicationIdentity) (artifact.DependencyReport, error)
}

func controllerOwner(object client.Object) (*metav1.OwnerReference, error) {
	var owner *metav1.OwnerReference
	for _, ref := range object.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			if owner != nil {
				return nil, fmt.Errorf("ambiguous controller owner references")
			}
			copy := ref
			owner = &copy
		}
	}
	return owner, nil
}

func workload(ctx context.Context, reader client.Reader, pod *corev1.Pod) (ObjectIdentity, error) {
	var object client.Object = pod
	kind := "Pod"
	seen := map[types.UID]bool{}
	for depth := 0; depth < 16; depth++ {
		if object.GetUID() == "" || seen[object.GetUID()] {
			return ObjectIdentity{}, fmt.Errorf("missing/cyclic owner UID")
		}
		seen[object.GetUID()] = true
		owner, err := controllerOwner(object)
		if err != nil {
			return ObjectIdentity{}, err
		}
		if owner == nil {
			return ObjectIdentity{kind, object.GetNamespace(), object.GetName(), object.GetUID()}, nil
		}
		var parent client.Object
		switch owner.APIVersion + "/" + owner.Kind {
		case "apps/v1/ReplicaSet":
			parent = &appsv1.ReplicaSet{}
		case "apps/v1/Deployment":
			parent = &appsv1.Deployment{}
		case "apps/v1/StatefulSet":
			parent = &appsv1.StatefulSet{}
		case "apps/v1/DaemonSet":
			parent = &appsv1.DaemonSet{}
		case "batch/v1/Job":
			parent = &batchv1.Job{}
		case "batch/v1/CronJob":
			parent = &batchv1.CronJob{}
		case "v1/ReplicationController":
			parent = &corev1.ReplicationController{}
		default:
			return ObjectIdentity{}, fmt.Errorf("unsupported controller owner %s/%s", owner.APIVersion, owner.Kind)
		}
		if err = reader.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}, parent); err != nil {
			return ObjectIdentity{}, err
		}
		if parent.GetUID() != owner.UID {
			return ObjectIdentity{}, fmt.Errorf("controller owner UID changed")
		}
		object, kind = parent, owner.Kind
	}
	return ObjectIdentity{}, fmt.Errorf("controller owner depth limit")
}

func observedBinding(ctx context.Context, reader client.Reader, pod *corev1.Pod, status corev1.ContainerStatus, options Options) (*node.RuntimeBinding, error) {
	var host corev1.Node
	if err := reader.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &host); err != nil {
		return nil, err
	}
	var observation node.BindingObservation
	if err := artifact.StrictJSON([]byte(host.Annotations[node.BindingsAnnotation]), &observation); err != nil {
		return nil, fmt.Errorf("node binding observation: %w", err)
	}
	now := time.Now()
	if !observation.Complete || observation.NodeUID != string(host.UID) ||
		observation.ProfileUID != host.Labels[api.OwnerUID] || host.Labels[api.OwnerNodeUID] != string(host.UID) ||
		observation.ObservedAt.IsZero() || observation.ObservedAt.After(now.Add(30*time.Second)) ||
		now.Sub(observation.ObservedAt) > options.MaxAge {
		return nil, fmt.Errorf("stale/incomplete/foreign node binding observation")
	}
	var profile api.NodeProfile
	if host.Annotations[api.OwnerName] == "" {
		return nil, fmt.Errorf("node profile owner annotation unavailable")
	}
	if err := reader.Get(ctx, types.NamespacedName{Name: host.Annotations[api.OwnerName]}, &profile); err != nil {
		return nil, err
	}
	if string(profile.UID) != observation.ProfileUID || profile.Generation != observation.ProfileGeneration {
		return nil, fmt.Errorf("node binding profile identity changed")
	}
	id, ok := strings.CutPrefix(status.ContainerID, "containerd://")
	if !ok || id == "" {
		return nil, fmt.Errorf("protected containerd container identity unavailable")
	}
	var found *node.RuntimeBinding
	for _, binding := range observation.Bindings {
		if binding.ContainerID != id {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("ambiguous container binding")
		}
		copy := binding
		found = &copy
	}
	if found == nil || found.ContainerdNamespace != "k8s.io" || found.PodUID != string(pod.UID) ||
		found.PodName != pod.Name || found.PodNamespace != pod.Namespace || found.ContainerName != status.Name ||
		found.State != "running" || found.Application == nil {
		return nil, fmt.Errorf("actual running application binding unavailable or inconsistent")
	}
	if err := found.Application.Validate(); err != nil {
		return nil, err
	}
	imageID := strings.TrimPrefix(strings.TrimPrefix(status.ImageID, "containerd://"), "docker-pullable://")
	if _, suffix, ok := strings.Cut(imageID, "@"); ok {
		imageID = suffix
	}
	if !slices.Contains([]string{found.Application.RootDigest, found.Application.ManifestDigest, found.Application.ConfigDigest}, imageID) {
		return nil, fmt.Errorf("observed CRI image identity differs from protected application graph")
	}
	return found, nil
}

func Capture(ctx context.Context, reader client.Reader, options Options) (Report, error) {
	report := Report{SchemaVersion: 1, Cluster: options.Cluster, StartedAt: time.Now().UTC(), Namespaces: slices.Clone(options.Namespaces),
		Workloads: slices.Clone(options.Workloads), Complete: true, Containers: []Container{}, Artifacts: map[string]artifact.DependencyReport{},
		Warnings: []string{"Point-in-time API reads are not an atomic cluster snapshot; absence outside the explicit scope is not evidence."}}
	if options.Cluster == "" || len(options.Namespaces) == 0 || options.MaxAge <= 0 || options.MaxAge > 2*time.Minute || options.ReadImage == nil {
		return report, fmt.Errorf("cluster, explicit namespaces, positive observation age and image evidence reader required")
	}
	for _, selector := range options.Workloads {
		parts := strings.Split(selector, "/")
		if len(parts) != 3 || parts[2] == "" || !slices.Contains(options.Namespaces, parts[1]) ||
			!slices.Contains([]string{"Pod", "ReplicaSet", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "ReplicationController"}, parts[0]) {
			return report, fmt.Errorf("workload selector must be a native Kind/selected-namespace/name")
		}
	}
	seen := map[string]bool{}
	for _, namespace := range options.Namespaces {
		if namespace == "" || seen[namespace] {
			return report, fmt.Errorf("explicit unique nonempty namespaces required")
		}
		seen[namespace] = true
		var pods corev1.PodList
		if err := reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
			report.Complete = false
			report.Warnings = append(report.Warnings, fmt.Sprintf("namespace %s unavailable: %v", namespace, err))
			continue
		}
		slices.SortFunc(pods.Items, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
		for _, pod := range pods.Items {
			if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "brewlet" {
				continue
			}
			owner, ownerErr := workload(ctx, reader, &pod)
			if ownerErr == nil && len(options.Workloads) > 0 && !slices.Contains(options.Workloads, owner.Kind+"/"+owner.Namespace+"/"+owner.Name) {
				continue
			}
			statuses := append(append(slices.Clone(pod.Status.ContainerStatuses), pod.Status.InitContainerStatuses...), pod.Status.EphemeralContainerStatuses...)
			running := 0
			for _, status := range statuses {
				if status.State.Running == nil {
					continue
				}
				running++
				row := Container{Pod: ObjectIdentity{"Pod", pod.Namespace, pod.Name, pod.UID}, Workload: owner, NodeName: pod.Spec.NodeName,
					Name: status.Name, ContainerID: status.ContainerID, ObservedImageID: status.ImageID, Status: "unverified"}
				err := ownerErr
				if err == nil {
					row.Binding, err = observedBinding(ctx, reader, &pod, status, options)
				}
				if err == nil {
					identity := *row.Binding.Application
					key := identity.Reference + "#" + identity.ManifestDigest
					facts, exists := report.Artifacts[key]
					if !exists {
						facts, err = options.ReadImage(ctx, identity)
					}
					if err == nil {
						p := facts.Verification.Predicate
						if p.Output.String() != identity.ManifestDigest || p.OutputConfig.Digest.String() != identity.ConfigDigest ||
							!brewletplatform.Exact(p.Platform, identity.Platform) ||
							!facts.Verification.Structural || !facts.Verification.CompositionSignature || !facts.Verification.PublisherProvenance ||
							facts.Verification.PolicyMode != "production" {
							err = fmt.Errorf("observed graph lacks production-verified composition/publisher evidence")
						} else {
							report.Artifacts[key] = facts
							row.Status, row.EvidenceKey = "verified", key
						}
					}
				}
				if err != nil {
					row.Reason = err.Error()
					report.Complete = false
				}
				report.Containers = append(report.Containers, row)
			}
			if running == 0 && pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed {
				report.Complete = false
				report.Warnings = append(report.Warnings, fmt.Sprintf("Pod %s/%s UID %s has no observed running containers", pod.Namespace, pod.Name, pod.UID))
			}
		}
	}
	report.FinishedAt = time.Now().UTC()
	return report, nil
}
