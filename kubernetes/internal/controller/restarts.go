package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RestartEngine is invoked only by the single-worker RuntimeImage controller.
// Leader election and that serialized queue make its persisted global slot
// accounting exclusive across all RuntimeImage resources.
type RestartEngine struct {
	Client                   client.Client
	Reader                   client.Reader
	MaxConcurrent            int
	WorkloadProgressDeadline time.Duration
	Now                      func() time.Time
	Recorder                 record.EventRecorder
	Leadership               *RestartLeadership
	// Namespaces limits restart candidates; empty means cluster-wide.
	Namespaces []string
}

func (e *RestartEngine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
func (e *RestartEngine) deadline() time.Duration {
	if e.WorkloadProgressDeadline <= 0 {
		return 10 * time.Minute
	}
	return e.WorkloadProgressDeadline
}
func (e *RestartEngine) persist(ctx context.Context, im *api.RuntimeImage) error {
	countRestarts(im)
	if err := e.Client.Status().Update(ctx, im); err != nil {
		return err
	}
	fresh := new(api.RuntimeImage)
	if err := e.Reader.Get(ctx, client.ObjectKeyFromObject(im), fresh); err != nil {
		return err
	}
	want, _ := json.Marshal(im.Status.Restarts)
	got, _ := json.Marshal(fresh.Status.Restarts)
	if fresh.UID != im.UID || fresh.Status.Generation != im.Status.Generation || !policy.Approved(fresh) || !bytes.Equal(want, got) {
		return fmt.Errorf("restart intent/status checkpoint not observed or target changed; matching CRD required")
	}
	return nil
}
func activeSlot(s api.WorkloadRestart) bool {
	return !s.NonActionable && (s.Phase == "Patching" || s.Phase == "Rolling")
}
func (e *RestartEngine) expired(s api.WorkloadRestart) bool {
	return s.StartedAt != nil && e.now().Sub(s.StartedAt.Time) >= e.deadline()
}
func (e *RestartEngine) slots(ctx context.Context, im *api.RuntimeImage) (int, error) {
	var images api.RuntimeImageList
	if err := e.Reader.List(ctx, &images); err != nil {
		return 0, err
	}
	n := 0
	for _, r := range images.Items {
		if !policy.Approved(&r) {
			continue
		}
		records := r.Status.Restarts.Workloads
		if r.UID == im.UID {
			records = im.Status.Restarts.Workloads
		}
		for _, s := range records {
			if s.InventoryUID == r.UID && s.Generation == r.Status.Generation && activeSlot(s) && !e.expired(s) {
				n++
			}
		}
	}
	return n, nil
}
func countRestarts(im *api.RuntimeImage) {
	s := &im.Status.Restarts
	s.Pending = 0
	s.Rolling = 0
	s.Complete = 0
	s.Deferred = 0
	s.NotApplicable = 0
	s.Failed = 0
	for _, w := range s.Workloads {
		if w.InventoryUID != im.UID || w.Generation != im.Status.Generation {
			continue
		}
		switch w.Phase {
		case "Pending":
			s.Pending++
		case "Patching", "Rolling":
			s.Rolling++
		case "Complete":
			s.Complete++
		case "Deferred":
			s.Deferred++
		case "NotApplicable":
			s.NotApplicable++
		case "Failed", "EpochMismatch":
			s.Failed++
		}
	}
}
func (e *RestartEngine) outcome(w restartWorkload, s *api.WorkloadRestart, phase, reason, message string) {
	changed := s.Phase != phase || s.Reason != reason || s.Message != message
	s.Phase = phase
	s.Reason = reason
	s.Message = message
	if phase == "Complete" && s.CompletedAt == nil {
		v := metav1.NewTime(e.now())
		s.CompletedAt = &v
	}
	if changed && e.Recorder != nil {
		kind := corev1.EventTypeNormal
		if phase == "Failed" || phase == "Deferred" || phase == "EpochMismatch" {
			kind = corev1.EventTypeWarning
		}
		e.Recorder.Event(w.object, kind, reason, message)
	}
}
func barrierValue(raw, id string, uid types.UID) (api.Barrier, error) {
	parsed, err := node.ParseBarrier(raw, id)
	if err != nil || parsed == nil {
		return api.Barrier{}, fmt.Errorf("InvalidRuntimeGeneration")
	}
	b := api.Barrier{ID: parsed.ID, InventoryUID: types.UID(parsed.InventoryUID), Generation: parsed.Generation}
	if b.InventoryUID != uid {
		return b, fmt.Errorf("RuntimeImageEpochMismatch")
	}
	return b, nil
}
func (e *RestartEngine) Reconcile(ctx context.Context, im *api.RuntimeImage) error {
	if e.Leadership != nil {
		lead := e.Leadership.current()
		if lead == nil || lead.Err() != nil {
			return nil
		}
		operation, cancel := context.WithCancel(lead)
		stop := context.AfterFunc(ctx, cancel)
		defer stop()
		defer cancel()
		ctx = operation
	}
	if !policy.Approved(im) || im.Status.RotationCompletedAt == nil {
		return nil
	}
	allowPatches := meta.IsStatusConditionTrue(im.Status.Conditions, "RotationComplete")
	workloads, err := listRestartWorkloads(ctx, e.Reader, e.Namespaces)
	if err != nil {
		return err
	}
	slices.SortFunc(workloads, func(a, b restartWorkload) int {
		ka := a.kind + "/" + a.object.GetNamespace() + "/" + a.object.GetName()
		kb := b.kind + "/" + b.object.GetNamespace() + "/" + b.object.GetName()
		if ka < kb {
			return -1
		}
		if ka > kb {
			return 1
		}
		return 0
	})
	currentUIDs := map[types.UID]bool{}
	for _, w := range workloads {
		currentUIDs[w.object.GetUID()] = true
	}
	for i := range im.Status.Restarts.Workloads {
		s := &im.Status.Restarts.Workloads[i]
		if s.InventoryUID != im.UID || s.Generation != im.Status.Generation {
			if s.Phase != "Complete" {
				s.Phase = "Superseded"
				s.Reason = "NewerRotation"
				s.Message = "newer epoch/generation superseded this restart"
			}
			continue
		}
		if !currentUIDs[s.UID] && s.Phase != "Complete" {
			s.Phase = "Superseded"
			s.Reason = "WorkloadDeleted"
			s.Message = "original workload UID no longer exists"
		}
	}
	for _, w := range workloads {
		idx := slices.IndexFunc(im.Status.Restarts.Workloads, func(s api.WorkloadRestart) bool {
			return s.UID == w.object.GetUID() && s.InventoryUID == im.UID && s.Generation == im.Status.Generation
		})
		opted := w.object.GetAnnotations()["brewlet.sh/restart-on-rotation"] == "true" && w.template.Annotations[api.RuntimeAnnotation] == im.Name && w.template.Spec.RuntimeClassName != nil && *w.template.Spec.RuntimeClassName == "brewlet"
		if !opted {
			if idx >= 0 && im.Status.Restarts.Workloads[idx].Phase != "Complete" {
				e.outcome(w, &im.Status.Restarts.Workloads[idx], "Deferred", "RestartDeferred", "workload opted out or changed runtime; existing barrier is not removed")
			}
			continue
		}
		pods, err := workloadPods(ctx, e.Reader, w)
		if err != nil {
			return err
		}
		oldPods := stalePods(pods, im, e.now())
		if idx < 0 && !oldPods {
			continue
		}
		if idx < 0 && !allowPatches {
			continue
		}
		if idx < 0 {
			im.Status.Restarts.Workloads = append(im.Status.Restarts.Workloads, api.WorkloadRestart{Kind: w.kind, Namespace: w.object.GetNamespace(), Name: w.object.GetName(), UID: w.object.GetUID(), InventoryUID: im.UID, Generation: im.Status.Generation, Phase: "Pending", Reason: "RestartPending"})
			idx = len(im.Status.Restarts.Workloads) - 1
		}
		s := &im.Status.Restarts.Workloads[idx]
		for _, p := range pods {
			if !slices.Contains(s.ObservedPodUIDs, p.UID) {
				s.ObservedPodUIDs = append(s.ObservedPodUIDs, p.UID)
			}
		}
		if s.Phase == "Complete" {
			if len(olderTaskBlockers(im, s)) == 0 {
				continue
			}
			s.CompletedAt = nil
			e.outcome(w, s, "Deferred", "RestartDeferred", "a later genuine observation reports outstanding older tasks; native rollout availability is not full runtime replacement proof")
		}
		runtimeBlockers := restartBlockers(pods, im)
		s.BlockedPods = runtimeBlockers
		taskBlockers := olderTaskBlockers(im, s)
		s.BlockedPods = append(s.BlockedPods, taskBlockers...)
		raw := w.template.Annotations[api.BarrierAnnotation]
		hasBarrier := false
		if raw != "" {
			b, err := barrierValue(raw, im.Name, im.UID)
			if err != nil {
				phase := "Failed"
				if err.Error() == "RuntimeImageEpochMismatch" {
					phase = "EpochMismatch"
				}
				e.outcome(w, s, phase, err.Error(), "workload owner must explicitly remove or update the incompatible barrier")
				continue
			}
			if b.Generation > im.Status.Generation {
				e.outcome(w, s, "Deferred", "RestartDeferred", "workload has a future minimum-generation barrier; it is not lowered")
				continue
			}
			hasBarrier = b.Generation == im.Status.Generation
		}
		if hasBarrier {
			s.Patched = true
			if s.StartedAt == nil {
				v := metav1.NewTime(e.now())
				s.StartedAt = &v
			}
		}
		// Timed-out/deferred work continues observing eventual recovery, but is
		// never repeatedly patched or counted as a successful restart.
		if s.Patched && hasBarrier {
			if w.onDelete {
				e.outcome(w, s, "NotApplicable", "RestartNotApplicable", "OnDelete requires manual pod replacement; the template barrier is installed")
				continue
			}
			if w.deferred != "" {
				e.outcome(w, s, "Deferred", "RestartDeferred", w.deferred)
				continue
			}
			if w.complete && len(taskBlockers) > 0 {
				e.outcome(w, s, "Deferred", "RestartDeferred", "workload controller is available, but genuine older-task observations remain outstanding or stale")
				continue
			}
			if w.complete && !oldPods {
				e.outcome(w, s, "Complete", "RestartComplete", "workload controller reports availability and no running pod is observed or presumed on an older runtime generation")
				continue
			}
			if w.nativeFailure != "" {
				e.outcome(w, s, "Failed", "RestartFailed", "Deployment progress deadline exceeded: "+w.nativeFailure)
				continue
			}
			if e.expired(*s) {
				if len(runtimeBlockers) > 0 {
					e.outcome(w, s, "Deferred", "RestartDeferred", "runtime installation/authorization or excluded-node generation barrier exceeded the workload deadline")
				} else {
					e.outcome(w, s, "Failed", "RestartFailed", "workload rollout exceeded progress deadline")
				}
				continue
			}
			if s.Phase == "Failed" || s.Phase == "Deferred" || s.Phase == "NotApplicable" {
				if s.Phase == "Deferred" {
					e.outcome(w, s, "Deferred", "RestartDeferred", "controller is able to progress again; observing recovery without repatching or reserving a slot")
				}
				continue
			}
			e.outcome(w, s, "Rolling", "RestartProgressing", "template barrier installed; observing workload controller")
			continue
		}
		if s.Patched && !hasBarrier {
			e.outcome(w, s, "Deferred", "RestartDeferred", "workload owner changed or removed the barrier after patching; it is not repeatedly restored")
			continue
		}
		if !oldPods {
			e.outcome(w, s, "Complete", "RestartComplete", "no running pod is observed or presumed on an older runtime generation; no restart patch needed")
			continue
		}
		if !allowPatches {
			e.outcome(w, s, "Pending", "RestartPending", "rotation participant set is converging; new restart patches are withheld")
			continue
		}
		nonActionable := w.onDelete || w.deferred != ""
		if !nonActionable && !activeSlot(*s) {
			slots, err := e.slots(ctx, im)
			if err != nil {
				return err
			}
			limit := e.MaxConcurrent
			if limit <= 0 {
				limit = 5
			}
			if slots >= limit {
				e.outcome(w, s, "Pending", "RestartPending", "waiting for a cluster-wide rollout slot")
				continue
			}
		}
		s.NonActionable = nonActionable
		if s.StartedAt == nil {
			v := metav1.NewTime(e.now())
			s.StartedAt = &v
		}
		if e.expired(*s) {
			e.outcome(w, s, "Failed", "RestartFailed", "restart intent could not be applied before its deadline")
			continue
		}
		e.outcome(w, s, "Patching", "RestartProgressing", "durable UID-bound restart intent reserved")
		if err := e.persist(ctx, im); err != nil {
			return err
		}
		s = &im.Status.Restarts.Workloads[idx]
		fresh, err := refreshWorkload(ctx, e.Reader, w)
		if err != nil {
			return err
		}
		if fresh.object.GetAnnotations()["brewlet.sh/restart-on-rotation"] != "true" || fresh.template.Annotations[api.RuntimeAnnotation] != im.Name || fresh.template.Spec.RuntimeClassName == nil || *fresh.template.Spec.RuntimeClassName != "brewlet" {
			e.outcome(w, s, "Deferred", "RestartDeferred", "workload changed while reserving restart intent")
			continue
		}
		s.NonActionable = fresh.onDelete || fresh.deferred != ""
		if !s.NonActionable {
			used, err := e.slots(ctx, im)
			if err != nil {
				return err
			}
			limit := e.MaxConcurrent
			if limit <= 0 {
				limit = 5
			}
			if used > limit {
				e.outcome(fresh, s, "Pending", "RestartPending", "controller settings changed; waiting for a global rollout slot")
				continue
			}
		}
		if value := fresh.template.Annotations[api.BarrierAnnotation]; value != "" {
			b, err := barrierValue(value, im.Name, im.UID)
			if err != nil {
				phase := "Failed"
				if err.Error() == "RuntimeImageEpochMismatch" {
					phase = "EpochMismatch"
				}
				e.outcome(w, s, phase, err.Error(), "barrier changed while reserving intent; owner acknowledgement required")
				continue
			}
			if b.Generation > im.Status.Generation {
				e.outcome(w, s, "Deferred", "RestartDeferred", "barrier advanced beyond this target; it is not lowered")
				continue
			}
			if b.Generation == im.Status.Generation {
				s.Patched = true
				s.WorkloadGeneration = fresh.object.GetGeneration()
				if fresh.onDelete {
					e.outcome(fresh, s, "NotApplicable", "RestartNotApplicable", "OnDelete requires manual replacement; concurrent barrier observed")
				} else if fresh.deferred != "" {
					e.outcome(fresh, s, "Deferred", "RestartDeferred", fresh.deferred)
				} else {
					e.outcome(fresh, s, "Rolling", "RestartProgressing", "target barrier advanced concurrently; observing without another patch")
				}
				continue
			}
		}
		current := new(api.RuntimeImage)
		if err := e.Reader.Get(ctx, client.ObjectKeyFromObject(im), current); err != nil {
			return err
		}
		if current.UID != im.UID || current.Status.Generation != im.Status.Generation || !policy.Approved(current) || !meta.IsStatusConditionTrue(current.Status.Conditions, "RotationComplete") {
			return fmt.Errorf("rotation target changed before workload patch")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		target, _ := json.Marshal(api.Barrier{ID: im.Name, InventoryUID: im.UID, Generation: im.Status.Generation})
		patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": s.UID, "resourceVersion": fresh.object.GetResourceVersion()}, "spec": map[string]any{"template": map[string]any{"metadata": map[string]any{"annotations": map[string]string{api.BarrierAnnotation: string(target)}}}}})
		if err := e.Client.Patch(ctx, fresh.object, client.RawPatch(types.MergePatchType, patch)); err != nil {
			e.outcome(w, s, "Pending", "RestartPatchFailed", err.Error())
			continue
		}
		observed, err := refreshWorkload(ctx, e.Reader, fresh)
		if err != nil {
			return err
		}
		if observed.template.Annotations[api.BarrierAnnotation] != string(target) {
			return fmt.Errorf("restart barrier patch was not observed for %s/%s", s.Namespace, s.Name)
		}
		s.Patched = true
		s.WorkloadGeneration = observed.object.GetGeneration()
		if observed.onDelete {
			e.outcome(observed, s, "NotApplicable", "RestartNotApplicable", "OnDelete requires manual pod replacement; barrier installed")
		} else if observed.deferred != "" {
			e.outcome(observed, s, "Deferred", "RestartDeferred", observed.deferred)
		} else {
			e.outcome(observed, s, "Rolling", "RestartProgressing", "template barrier installed; workload controller owns replacement")
		}
		if err := e.persist(ctx, im); err != nil {
			return err
		}
	}
	return e.persist(ctx, im)
}
