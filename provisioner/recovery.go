package provisioner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type recoveryPlan struct {
	wide      bool
	ids       map[string]bool
	installed map[string]bool
}

func ineligibleRuntime(err error) bool {
	if apierrors.IsNotFound(err) {
		return true
	}
	e, ok := err.(*node.Error)
	return ok && e.Reason == "RuntimeImageRevoked"
}

// Retained advertisements are evidence during recovery, not inputs to repair
// by deleting/normalizing them before deciding whether the fence can be lifted.
func (w *Worker) recoveryPlan(ctx context.Context, s *control.Snapshot) (recoveryPlan, error) {
	p := recoveryPlan{wide: control.HasStartupTaint(s.Node), ids: map[string]bool{}, installed: map[string]bool{}}
	nodeReady := false
	for _, condition := range s.Node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			nodeReady = condition.Status == corev1.ConditionTrue
		}
	}
	unavailable := !nodeReady || s.Node.Spec.Unschedulable || !s.Node.DeletionTimestamp.IsZero()
	entries := map[string]api.RuntimeEntry{}
	for _, entry := range s.Spec.RuntimeImages {
		entries[entry.ID] = entry
	}
	if p.wide {
		for _, taint := range s.Node.Spec.Taints {
			if taint.Key == api.StartupTaint && taint.Effect == corev1.TaintEffectNoSchedule && taint.Value != "provisioning" {
				return p, fmt.Errorf("node-advertisement-failed: nonstandard startup fence requires pool correction")
			}
		}
		if value, exists := s.Node.Labels[api.RuntimeLabel]; exists && value != "ready" {
			return p, fmt.Errorf("node-advertisement-failed: invalid retained runtime baseline label")
		}
		for key, value := range s.Node.Labels {
			if strings.HasPrefix(key, "brewlet.sh/jdk.") || strings.HasPrefix(key, "brewlet.sh/jdk-feature.") {
				return p, fmt.Errorf("node-advertisement-failed: obsolete retained capability %s requires pool correction", key)
			}
			if !strings.HasPrefix(key, "brewlet.sh/runtime.") {
				continue
			}
			id := strings.TrimPrefix(key, "brewlet.sh/runtime.")
			entry, offered := entries[id]
			if node.ValidateID(id) != nil || !offered || (value != "ready" && value != "available") || (entry.Mode() == "Eager" && value != "ready") {
				return p, fmt.Errorf("node-advertisement-failed: invalid/nonoffered retained capability %s=%s; correct pool labels", key, value)
			}
			if _, err := w.runtime(ctx, id); err != nil {
				return p, fmt.Errorf("node-advertisement-failed: retained capability %s is not currently authorized: %w", key, err)
			}
		}
	}
	previous, err := control.ParseInventory(s.Node)
	if err != nil {
		return p, fmt.Errorf("node-advertisement-failed: invalid retained inventory: %w", err)
	}
	previouslyInstalled := map[string]bool{}
	if _, err := os.Lstat(w.Store.Root); err == nil {
		authorizations, err := w.Store.Authorizations()
		if err != nil {
			return p, err
		}
		for _, a := range authorizations {
			if a.ProfileUID == string(w.Authority.ProfileUID) && a.State == "ready" {
				previouslyInstalled[a.ID] = true
			}
		}
	} else if !os.IsNotExist(err) {
		return p, err
	}
	for _, entry := range s.Spec.RuntimeImages {
		id := entry.ID
		r, err := w.runtime(ctx, id)
		if err != nil && !ineligibleRuntime(err) {
			return p, err
		}
		excluded := false
		if r != nil {
			for _, excludedNode := range r.Status.Rotation.ExcludedNodes {
				excluded = excluded || excludedNode.UID == s.Node.UID
			}
		}
		old := previous[id]
		withdrawn := (old.Installed || old.State == "ready") && s.Node.Labels["brewlet.sh/runtime."+id] != "ready"
		p.ids[id] = p.wide || excluded || withdrawn || unavailable
		if !p.ids[id] {
			continue
		}
		p.installed[id] = entry.Mode() == "Eager" || old.Installed || old.State == "ready" || s.Node.Labels["brewlet.sh/runtime."+id] == "ready" || previouslyInstalled[id]
		if _, err := os.Lstat(filepath.Join(w.Store.Root, id, "current")); err == nil {
			p.installed[id] = true
		} else if !os.IsNotExist(err) {
			return p, err
		}
	}
	return p, nil
}

func (w *Worker) validatePublication(ctx context.Context, s *control.Snapshot, inventory map[string]api.NodeRuntime) (recoveryPlan, error) {
	p, err := w.recoveryPlan(ctx, s)
	if err != nil {
		return p, err
	}
	recovering := p.wide
	for _, value := range p.ids {
		recovering = recovering || value
	}
	if recovering {
		ready := false
		for _, condition := range s.Node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				ready = condition.Status == corev1.ConditionTrue
			}
		}
		if !ready || s.Node.Spec.Unschedulable || !s.Node.DeletionTimestamp.IsZero() {
			return p, fmt.Errorf("node-advertisement-failed: excluded node is not ready, schedulable and live")
		}
	}
	auths, err := w.Store.Authorizations()
	if err != nil {
		return p, err
	}
	byID := map[string]node.Authorization{}
	for _, a := range auths {
		byID[a.ID] = a
	}
	offered := map[string]bool{}
	for _, entry := range s.Spec.RuntimeImages {
		id := entry.ID
		offered[id] = true
		r, err := w.runtime(ctx, id)
		if err != nil {
			if ineligibleRuntime(err) {
				if _, advertised := inventory[id]; !advertised {
					continue
				}
			}
			return p, err
		}
		state, exists := inventory[id]
		if !exists || state.Install != entry.Mode() || (state.State != "ready" && state.State != "available") {
			return p, fmt.Errorf("node-advertisement-failed: approved runtime %s lacks valid proposed state", id)
		}
		if state.Installed != (state.State == "ready") {
			return p, fmt.Errorf("node-advertisement-failed: installation evidence contradicts capability for %s", id)
		}
		if err := w.validateRuntimePlatforms(ctx, r); err != nil {
			return p, err
		}
		a, exists := byID[id]
		if !exists || a.ProfileUID != string(w.Authority.ProfileUID) || a.ProfileGeneration != w.Authority.Generation || a.InventoryUID != string(r.UID) || a.Revoked || !time.Now().Before(a.ExpiresAt) || a.Install != entry.Mode() || a.State != state.State {
			return p, fmt.Errorf("node-advertisement-failed: live local authorization missing for %s", id)
		}
		if a.Generation > r.Status.Generation || (a.Generation == r.Status.Generation && a.Digest != r.Status.Digest) {
			return p, fmt.Errorf("node-advertisement-failed: authorization contradicts approved target for %s", id)
		}
		if p.ids[id] && (a.Generation != r.Status.Generation || a.Digest != r.Status.Digest) {
			return p, &node.Error{Reason: "RuntimeImageUpdating", Detail: "recovery authorization is not latest for " + id}
		}
		current, _, currentErr := w.Store.Current(ctx, id)
		if state.State == "available" {
			if entry.Mode() != "OnDemand" || currentErr == nil || !os.IsNotExist(currentErr) || p.installed[id] {
				return p, fmt.Errorf("node-advertisement-failed: previously installed/invalid runtime %s cannot recover as cold", id)
			}
			if state.InventoryUID != r.UID || state.Generation != a.Generation || state.Digest != a.Digest {
				return p, fmt.Errorf("node-advertisement-failed: available runtime evidence mismatch for %s", id)
			}
			continue
		}
		if currentErr != nil {
			return p, currentErr
		}
		if state.InventoryUID != r.UID || string(state.InventoryUID) != current.InventoryUID || state.Generation != current.Generation || state.Digest != current.SourceDigest {
			return p, fmt.Errorf("node-advertisement-failed: advertised state is not validated current for %s", id)
		}
		if p.ids[id] && (current.Generation != r.Status.Generation || current.SourceDigest != r.Status.Digest) {
			return p, &node.Error{Reason: "RuntimeImageUpdating", Detail: "excluded/fenced runtime current is not latest for " + id}
		}
	}
	for id := range inventory {
		if !offered[id] {
			return p, fmt.Errorf("node-advertisement-failed: nonoffered proposed inventory %s", id)
		}
	}
	return p, nil
}

func (w *Worker) probePublicationMutability(ctx context.Context, inventory map[string]api.NodeRuntime, recovering bool) error {
	if w.checkedMutable == nil {
		w.checkedMutable = map[string]bool{}
	}
	for id, state := range inventory {
		if state.Install != "OnDemand" || (!recovering && w.checkedMutable[id]) {
			continue
		}
		s, err := w.check(ctx)
		if err != nil {
			return err
		}
		if _, exists := s.Node.Labels["brewlet.sh/runtime."+id]; !exists {
			n := s.Node.DeepCopy()
			if n.Labels == nil {
				n.Labels = map[string]string{}
			}
			n.Labels["brewlet.sh/runtime."+id] = state.State
			if err := w.Client.Update(ctx, n); err != nil {
				return fmt.Errorf("node-advertisement-failed: capability mutability probe: %w", err)
			}
		}
		broader, err := control.Withdraw(ctx, w.Client, w.Reader, w.Authority, []string{"brewlet.sh/runtime." + id})
		if err != nil {
			return err
		}
		if broader {
			return fmt.Errorf("node-advertisement-failed: provider-pinned OnDemand capability; retain startup fence and pool-label evidence")
		}
		s, err = w.check(ctx)
		if err != nil {
			return err
		}
		n := s.Node.DeepCopy()
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		n.Labels["brewlet.sh/runtime."+id] = state.State
		if err := w.Client.Update(ctx, n); err != nil {
			return fmt.Errorf("node-advertisement-failed: restore mutable capability after probe: %w", err)
		}
		w.checkedMutable[id] = true
	}
	return nil
}
