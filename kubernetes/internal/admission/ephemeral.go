package admission

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	webhook "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func (h *PodHandler) ephemeralUpdate(ctx context.Context, req webhook.Request, p *corev1.Pod) webhook.Response {
	if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != "brewlet" {
		return webhook.Allowed("not a Brewlet pod")
	}
	old := new(corev1.Pod)
	if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
		return webhook.Errored(http.StatusBadRequest, err)
	}
	existing := map[string]bool{}
	for _, c := range old.Spec.EphemeralContainers {
		existing[c.Name] = true
	}
	contexts := map[int]*corev1.SecurityContext{}
	warnings := []string{}
	for i, c := range p.Spec.EphemeralContainers {
		if existing[c.Name] {
			continue
		}
		// Reuse executable security/inventory checks on an isolated candidate.
		// Scheduling, existing executables and creation-only pod metadata cannot
		// be rewritten through the ephemeralcontainers subresource.
		candidate := p.DeepCopy()
		candidate.Spec.Containers = nil
		candidate.Spec.InitContainers = nil
		candidate.Spec.EphemeralContainers = []corev1.EphemeralContainer{c}
		candidate.Spec.Tolerations = nil
		candidate.Spec.Affinity = nil
		candidate.Spec.NodeSelector = nil
		for _, key := range []string{"brewlet.sh/jdk", "brewlet.sh/jvm-args", "brewlet.sh/arch"} {
			delete(candidate.Annotations, key)
		}
		if candidate.Spec.SecurityContext == nil {
			candidate.Spec.SecurityContext = &corev1.PodSecurityContext{}
		}
		if c.SecurityContext != nil {
			if c.SecurityContext.RunAsUser != nil {
				candidate.Spec.SecurityContext.RunAsUser = c.SecurityContext.RunAsUser
			}
			if c.SecurityContext.RunAsGroup != nil {
				candidate.Spec.SecurityContext.RunAsGroup = c.SecurityContext.RunAsGroup
			}
			if c.SecurityContext.RunAsNonRoot != nil {
				candidate.Spec.SecurityContext.RunAsNonRoot = c.SecurityContext.RunAsNonRoot
			}
		}
		w, err := h.Mutator.Mutate(ctx, candidate)
		if err != nil {
			var outage *InventoryUnavailable
			if errors.As(err, &outage) {
				return webhook.Allowed("inventory outage; shim independently enforces new executable authorization").WithWarnings(outage.Error() + "; ephemeral defaults were not applied")
			}
			return webhook.Denied(err.Error())
		}
		warnings = append(warnings, w...)
		contexts[i] = candidate.Spec.EphemeralContainers[0].SecurityContext
	}
	if len(contexts) == 0 {
		return webhook.Allowed("no new ephemeral executable")
	}
	// Preserve the request's raw fields. Typed whole-Pod serialization can
	// normalize unrelated defaults or remove fields owned by a newer API server.
	var podRaw, specRaw map[string]json.RawMessage
	var containers []json.RawMessage
	if err := json.Unmarshal(req.Object.Raw, &podRaw); err != nil {
		return webhook.Errored(http.StatusBadRequest, err)
	}
	if err := json.Unmarshal(podRaw["spec"], &specRaw); err != nil {
		return webhook.Errored(http.StatusBadRequest, err)
	}
	if err := json.Unmarshal(specRaw["ephemeralContainers"], &containers); err != nil {
		return webhook.Errored(http.StatusBadRequest, err)
	}
	for i, security := range contexts {
		var container map[string]json.RawMessage
		if err := json.Unmarshal(containers[i], &container); err != nil {
			return webhook.Errored(http.StatusBadRequest, err)
		}
		container["securityContext"], _ = json.Marshal(security)
		containers[i], _ = json.Marshal(container)
	}
	specRaw["ephemeralContainers"], _ = json.Marshal(containers)
	podRaw["spec"], _ = json.Marshal(specRaw)
	data, _ := json.Marshal(podRaw)
	return webhook.PatchResponseFromRaw(req.Object.Raw, data).WithWarnings(warnings...)
}
