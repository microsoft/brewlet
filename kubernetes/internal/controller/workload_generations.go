package controller

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type workloadRef struct {
	APIVersion, Kind, Name, Namespace string
	UID                               types.UID
}

// workloadOwners caches pod UID to owning workload. Controller ownership of a
// pod UID does not change, so only newly bound pods cost API reads.
type workloadOwners struct {
	mu   sync.Mutex
	pods map[types.UID]workloadRef
}

type workloadGenerationChange struct {
	ref                workloadRef
	runtime            string
	previous, observed []api.WorkloadGenerationCount
}

// observeWorkloadGenerations records the runtime generations each owning
// workload's running containers are bound to. Records change only when every
// node currently claimed for this runtime reports a fresh, complete and valid
// observation, so missing evidence never appears as a generation change.
func (r *RuntimeReconciler) observeWorkloadGenerations(ctx context.Context, im *api.RuntimeImage) ([]workloadGenerationChange, error) {
	if !policy.Approved(im) {
		return nil, nil
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	var nodes corev1.NodeList
	if err := r.Reader.List(ctx, &nodes); err != nil {
		return nil, err
	}
	type podBinding struct {
		namespace, name string
		generations     map[int64]string
	}
	pods := map[types.UID]*podBinding{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		claimed := slices.ContainsFunc(im.Status.RevocationTargets, func(t api.InventoryTarget) bool {
			return !t.Resolved && t.NodeUID == n.UID && string(t.ProfileUID) == n.Labels[api.OwnerUID]
		})
		if !claimed {
			continue
		}
		snapshot, ok := freshBindingSnapshot(n, now)
		if !ok {
			return nil, nil
		}
		for _, b := range snapshot.Bindings {
			if b.ID != im.Name || b.InventoryUID != string(im.UID) {
				continue
			}
			if b.PodUID == "" || b.PodName == "" || b.PodNamespace == "" || b.Generation <= 0 || b.Digest == "" {
				return nil, nil
			}
			p := pods[types.UID(b.PodUID)]
			if p == nil {
				p = &podBinding{namespace: b.PodNamespace, name: b.PodName, generations: map[int64]string{}}
				pods[types.UID(b.PodUID)] = p
			}
			p.generations[b.Generation] = b.Digest
		}
	}
	type observedWorkload struct {
		ref    workloadRef
		counts map[int64]*api.WorkloadGenerationCount
	}
	observed := map[types.UID]*observedWorkload{}
	seen := map[types.UID]workloadRef{}
	for uid, p := range pods {
		ref, found, err := r.workloadOwner(ctx, uid, p.namespace, p.name)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		seen[uid] = ref
		w := observed[ref.UID]
		if w == nil {
			w = &observedWorkload{ref: ref, counts: map[int64]*api.WorkloadGenerationCount{}}
			observed[ref.UID] = w
		}
		for g, digest := range p.generations {
			c := w.counts[g]
			if c == nil {
				c = &api.WorkloadGenerationCount{Generation: g, Digest: digest}
				w.counts[g] = c
			}
			c.Pods++
		}
	}
	r.owners.mu.Lock()
	r.owners.pods = seen
	r.owners.mu.Unlock()

	at := metav1.NewTime(now.UTC().Truncate(time.Second))
	var records []api.WorkloadGeneration
	var changes []workloadGenerationChange
	for _, old := range im.Status.WorkloadGenerations {
		w, ok := observed[old.UID]
		if !ok {
			exists, err := r.workloadExists(ctx, old)
			if err != nil {
				return nil, err
			}
			if !exists {
				continue
			}
			for i := range old.Generations {
				old.Generations[i].Pods = 0
			}
			records = append(records, old)
			continue
		}
		delete(observed, old.UID)
		counts := sortedCounts(w.counts)
		if !sameGenerations(old.Generations, counts) {
			changes = append(changes, workloadGenerationChange{ref: w.ref, runtime: im.Name, previous: old.Generations, observed: counts})
			old.PreviousGenerations = old.Generations
			old.ChangedAt = &at
		}
		old.Generations = counts
		records = append(records, old)
	}
	for _, w := range observed {
		records = append(records, api.WorkloadGeneration{APIVersion: w.ref.APIVersion, Kind: w.ref.Kind, Name: w.ref.Name, Namespace: w.ref.Namespace, UID: w.ref.UID, Generations: sortedCounts(w.counts), FirstObservedAt: at})
	}
	slices.SortFunc(records, func(a, b api.WorkloadGeneration) int {
		return strings.Compare(a.Namespace+"/"+a.Kind+"/"+a.Name+"/"+string(a.UID), b.Namespace+"/"+b.Kind+"/"+b.Name+"/"+string(b.UID))
	})
	im.Status.WorkloadGenerations = records
	return changes, nil
}

func sortedCounts(m map[int64]*api.WorkloadGenerationCount) []api.WorkloadGenerationCount {
	out := make([]api.WorkloadGenerationCount, 0, len(m))
	for _, c := range m {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b api.WorkloadGenerationCount) int { return int(a.Generation - b.Generation) })
	return out
}

// sameGenerations ignores pod counts: scaling is not a generation change.
func sameGenerations(a, b []api.WorkloadGenerationCount) bool {
	return slices.EqualFunc(a, b, func(x, y api.WorkloadGenerationCount) bool {
		return x.Generation == y.Generation && x.Digest == y.Digest
	})
}

func metadataObject(apiVersion, kind, namespace, name string) *metav1.PartialObjectMetadata {
	o := &metav1.PartialObjectMetadata{}
	o.SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, kind))
	o.Namespace, o.Name = namespace, name
	return o
}

// workloadOwner follows controller ownerReferences from a pod to its workload,
// collapsing a Deployment-owned ReplicaSet into the Deployment. A pod with no
// controller is its own workload. found is false when the pod is gone.
func (r *RuntimeReconciler) workloadOwner(ctx context.Context, podUID types.UID, namespace, name string) (workloadRef, bool, error) {
	r.owners.mu.Lock()
	ref, ok := r.owners.pods[podUID]
	r.owners.mu.Unlock()
	if ok {
		return ref, true, nil
	}
	pod := metadataObject("v1", "Pod", namespace, name)
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		if apierrors.IsNotFound(err) {
			return workloadRef{}, false, nil
		}
		return workloadRef{}, false, err
	}
	if pod.UID != podUID {
		return workloadRef{}, false, nil
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return workloadRef{"v1", "Pod", name, namespace, podUID}, true, nil
	}
	ref = workloadRef{owner.APIVersion, owner.Kind, owner.Name, namespace, owner.UID}
	if owner.Kind == "ReplicaSet" && owner.APIVersion == "apps/v1" {
		rs := metadataObject("apps/v1", "ReplicaSet", namespace, owner.Name)
		err := r.Reader.Get(ctx, client.ObjectKeyFromObject(rs), rs)
		if err != nil && !apierrors.IsNotFound(err) {
			return workloadRef{}, false, err
		}
		if err == nil && rs.UID == owner.UID {
			if d := metav1.GetControllerOf(rs); d != nil && d.Kind == "Deployment" && d.APIVersion == "apps/v1" {
				ref = workloadRef{d.APIVersion, d.Kind, d.Name, namespace, d.UID}
			}
		}
	}
	return ref, true, nil
}

// workloadExists retains a record unless its exact workload UID is gone.
// Unreadable workloads are retained rather than presumed deleted.
func (r *RuntimeReconciler) workloadExists(ctx context.Context, w api.WorkloadGeneration) (bool, error) {
	o := metadataObject(w.APIVersion, w.Kind, w.Namespace, w.Name)
	err := r.Reader.Get(ctx, client.ObjectKeyFromObject(o), o)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return true, nil
	}
	return o.UID == w.UID, nil
}

func describeGenerations(counts []api.WorkloadGenerationCount) string {
	if len(counts) == 0 {
		return "no generation"
	}
	parts := make([]string, 0, len(counts))
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("generation %s (%s)", strconv.FormatInt(c.Generation, 10), c.Digest))
	}
	return strings.Join(parts, ", ")
}

func (r *RuntimeReconciler) recordWorkloadGenerationChanges(changes []workloadGenerationChange) {
	if r.Recorder == nil {
		return
	}
	for _, c := range changes {
		obj := metadataObject(c.ref.APIVersion, c.ref.Kind, c.ref.Namespace, c.ref.Name)
		obj.UID = c.ref.UID
		pods := int32(0)
		for _, g := range c.observed {
			pods += g.Pods
		}
		r.Recorder.Eventf(obj, corev1.EventTypeNormal, "RuntimeGenerationChanged", "Runtime %s: running containers moved from %s to %s across %d pod(s)", c.runtime, describeGenerations(c.previous), describeGenerations(c.observed), pods)
	}
}
