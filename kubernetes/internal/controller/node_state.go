package controller

import (
	"context"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func effectiveNodeReady(n *corev1.Node, p *api.NodeProfile, approved ...map[string]bool) bool {
	if n.DeletionTimestamp != nil || n.Labels[api.RuntimeLabel] != "ready" || control.HasStartupTaint(n) || n.Annotations["brewlet.sh/provision-error"] != "" {
		return false
	}
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return false
	}
	for _, e := range p.Spec.RuntimeImages {
		// Unknown inventory is a profile error, not an installable eager root.
		// Only approved eager entries gate the node-wide validated baseline.
		if len(approved) > 0 && !approved[0][e.ID] {
			continue
		}
		if e.Mode() == "Eager" && n.Labels["brewlet.sh/runtime."+e.ID] != "ready" {
			return false
		}
	}
	return true
}
func (r *ProfileReconciler) mirrorNodes(ctx context.Context, p *api.NodeProfile, approved ...map[string]bool) error {
	for _, t := range p.Status.Targets {
		if !t.Claimed {
			continue
		}
		s, err := control.Check(ctx, r.Reader, authority(p, t, "provisioning", p.Status.ProvisioningGeneration))
		if err != nil {
			return err
		}
		n := s.Node
		state, reason, message := "Provisioning", "Provisioning", "waiting for validated shim/handler baseline and required eager roots"
		if n.Annotations["brewlet.sh/provision-error"] != "" {
			state = "Failed"
			reason = "ProvisionFailed"
			message = n.Annotations["brewlet.sh/provision-error"]
		} else if effectiveNodeReady(n, p, approved...) {
			state = "Ready"
			reason = "NodeReady"
			message = "validated runtime baseline and eager inventory without startup fence"
		}
		if n.Annotations["brewlet.sh/provision-state"] == state {
			continue
		}
		base := n.DeepCopy()
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		n.Annotations["brewlet.sh/provision-state"] = state
		if err := r.Patch(ctx, n, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		if r.Recorder != nil {
			eventType := corev1.EventTypeNormal
			if state == "Failed" {
				eventType = corev1.EventTypeWarning
			}
			r.Recorder.Event(n, eventType, reason, message)
		}
	}
	return nil
}
