package admission

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	webhook "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type PodHandler struct{ Mutator *PodMutator }

func (h *PodHandler) Handle(ctx context.Context, req webhook.Request) webhook.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return webhook.Allowed("not an executable pod mutation")
	}
	if req.Operation == admissionv1.Update && req.SubResource != "ephemeralcontainers" {
		return webhook.Allowed("ordinary pod updates do not create executable containers")
	}
	p := new(corev1.Pod)
	if err := json.Unmarshal(req.Object.Raw, p); err != nil {
		return webhook.Errored(http.StatusBadRequest, err)
	}
	if req.Operation == admissionv1.Update {
		return h.ephemeralUpdate(ctx, req, p)
	}
	warnings, err := h.Mutator.Mutate(ctx, p)
	if err != nil {
		var outage *InventoryUnavailable
		if errors.As(err, &outage) {
			return webhook.Allowed("inventory outage; node-local execution authorization remains enforced").WithWarnings(outage.Error() + "; Brewlet defaults and scheduling hints were not applied")
		}
		return webhook.Denied(err.Error())
	}
	data, err := json.Marshal(p)
	if err != nil {
		return webhook.Errored(http.StatusInternalServerError, err)
	}
	resp := webhook.PatchResponseFromRaw(req.Object.Raw, data)
	return resp.WithWarnings(warnings...)
}

type InventoryHandler struct {
	Reader             client.Reader
	AllowedMirrorHosts []string
	Runtime            bool
	Evidence           bool
}

func (h *InventoryHandler) Handle(ctx context.Context, req webhook.Request) webhook.Response {
	if req.Operation == admissionv1.Delete {
		if h.Evidence {
			e := new(api.NodeRetirementEvidence)
			if err := json.Unmarshal(req.OldObject.Raw, e); err != nil {
				return webhook.Errored(http.StatusBadRequest, err)
			}
			if e.Status.AcceptedAt != nil {
				return webhook.Denied("accepted/resolved external retirement evidence must be retained")
			}
		}
		return webhook.Allowed("finalizers coordinate revocation and cleanup")
	}
	if h.Evidence {
		e := new(api.NodeRetirementEvidence)
		if err := json.Unmarshal(req.Object.Raw, e); err != nil {
			return webhook.Errored(http.StatusBadRequest, err)
		}
		var old *api.NodeRetirementEvidence
		if req.Operation == admissionv1.Update {
			old = new(api.NodeRetirementEvidence)
			if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
				return webhook.Errored(http.StatusBadRequest, err)
			}
		}
		if err := policy.Evidence(e, old); err != nil {
			return webhook.Denied(err.Error())
		}
	} else if h.Runtime {
		r := new(api.RuntimeImage)
		if err := json.Unmarshal(req.Object.Raw, r); err != nil {
			return webhook.Errored(http.StatusBadRequest, err)
		}
		var old *api.RuntimeImage
		if req.Operation == admissionv1.Update {
			old = new(api.RuntimeImage)
			if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
				return webhook.Errored(http.StatusBadRequest, err)
			}
		}
		if err := policy.Runtime(r, old); err != nil {
			return webhook.Denied(err.Error())
		}
	} else {
		p := new(api.NodeProfile)
		if err := json.Unmarshal(req.Object.Raw, p); err != nil {
			return webhook.Errored(http.StatusBadRequest, err)
		}
		var all api.NodeProfileList
		if err := h.Reader.List(ctx, &all); err != nil {
			return webhook.Errored(http.StatusServiceUnavailable, err)
		}
		if err := policy.Profile(p, all.Items, h.AllowedMirrorHosts); err != nil {
			return webhook.Denied(err.Error())
		}
	}
	return webhook.Allowed("valid inventory")
}
