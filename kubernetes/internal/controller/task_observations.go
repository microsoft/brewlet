package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	noderuntime "github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Node binding snapshots are accepted only within these bounds.
const (
	observationFutureSkew = 30 * time.Second
	observationMaxAge     = 2 * time.Minute
)

// Task observations are diagnostics from genuine leases/tasks, never writer
// authority or cleanup receipts. Missing/failed observations retain old tuples.
func (r *RuntimeReconciler) observeTasks(ctx context.Context, im *api.RuntimeImage) (bool, error) {
	var nodes corev1.NodeList
	if err := r.Reader.List(ctx, &nodes); err != nil {
		return false, err
	}
	old := im.DeepCopy().Status.TaskObservations
	for i := range im.Status.TaskObservations {
		im.Status.TaskObservations[i].Current = false
		im.Status.TaskObservations[i].Reason = "ObservationUnavailable"
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	for _, n := range nodes.Items {
		raw := n.Annotations[noderuntime.BindingsAnnotation]
		if raw == "" {
			continue
		}
		var snapshot noderuntime.BindingObservation
		if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
			continue
		}
		if !snapshot.Complete || snapshot.NodeUID != string(n.UID) || snapshot.ProfileUID != n.Labels[api.OwnerUID] || n.Labels[api.OwnerNodeUID] != string(n.UID) || snapshot.ProfileGeneration <= 0 || snapshot.ObservedAt.IsZero() || snapshot.ObservedAt.After(now.Add(observationFutureSkew)) || now.Sub(snapshot.ObservedAt) > observationMaxAge {
			continue
		}
		if !slices.ContainsFunc(im.Status.RevocationTargets, func(t api.InventoryTarget) bool {
			return t.NodeUID == n.UID && string(t.ProfileUID) == snapshot.ProfileUID
		}) {
			continue
		}
		var bindings []api.RuntimeTaskBinding
		valid := true
		for _, b := range snapshot.Bindings {
			if b.ID != im.Name {
				continue
			}
			if b.ContainerID == "" || b.ContainerdNamespace == "" || b.PodUID == "" || b.InventoryUID == "" || b.Generation <= 0 || b.Digest == "" {
				valid = false
				break
			}
			if policy.Approved(im) && b.InventoryUID == string(im.UID) && b.Generation >= im.Status.Generation {
				continue
			}
			bindings = append(bindings, api.RuntimeTaskBinding{ContainerID: b.ContainerID, ContainerdNamespace: b.ContainerdNamespace, PodUID: types.UID(b.PodUID), PodName: b.PodName, PodNamespace: b.PodNamespace, ContainerName: b.ContainerName, InventoryUID: types.UID(b.InventoryUID), Generation: b.Generation, Digest: b.Digest, State: b.State})
		}
		if !valid {
			continue
		}
		slices.SortFunc(bindings, func(a, b api.RuntimeTaskBinding) int {
			return strings.Compare(a.ContainerdNamespace+"/"+a.ContainerID, b.ContainerdNamespace+"/"+b.ContainerID)
		})
		observation := api.RuntimeTaskObservation{NodeName: n.Name, NodeUID: n.UID, ProfileUID: types.UID(snapshot.ProfileUID), ProfileGeneration: snapshot.ProfileGeneration, ObservedAt: snapshot.ObservedAt, Current: true, Reason: "CompleteTaskAndLeaseObservation", Bindings: bindings}
		idx := slices.IndexFunc(im.Status.TaskObservations, func(o api.RuntimeTaskObservation) bool {
			return o.NodeUID == n.UID && o.ProfileUID == observation.ProfileUID
		})
		if idx < 0 {
			im.Status.TaskObservations = append(im.Status.TaskObservations, observation)
		} else if !im.Status.TaskObservations[idx].ObservedAt.After(observation.ObservedAt) {
			im.Status.TaskObservations[idx] = observation
		}
	}
	// Independent genuine root-drain or external disposition evidence can retire
	// a stale current-epoch observation; absence of a Node or Pod cannot.
	for i := range im.Status.TaskObservations {
		o := &im.Status.TaskObservations[i]
		if slices.ContainsFunc(im.Status.RevocationTargets, func(t api.InventoryTarget) bool {
			return t.NodeUID == o.NodeUID && t.ProfileUID == o.ProfileUID && t.Resolved
		}) {
			o.Bindings = slices.DeleteFunc(o.Bindings, func(b api.RuntimeTaskBinding) bool { return b.InventoryUID == im.UID })
			o.Reason = "IndependentRetirementResolution"
		}
	}
	changed := !reflect.DeepEqual(old, im.Status.TaskObservations)
	if changed {
		total := 0
		for _, o := range im.Status.TaskObservations {
			total += len(o.Bindings)
		}
		if total > 0 {
			runtimeCondition(im, "OldGenerationTasks", "ObservedOlderOrForeignEpochTasks", fmt.Sprintf("%d actual task/lease bindings remain; stale observations are retained and are not cleanup proof", total), metav1.ConditionTrue)
		} else {
			runtimeCondition(im, "OldGenerationTasks", "NoObservedOlderTasks", "no older-task tuples in retained complete observations; this is not cleanup authority", metav1.ConditionFalse)
		}
	}
	return changed, nil
}
