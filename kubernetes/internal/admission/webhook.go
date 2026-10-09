// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"net/http"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/controller"
	"brewlet-operator/internal/observability"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// PodMutator is the controller-runtime admission handler that applies the
// Brewlet admission/scheduling seam (§8/§14) to pods on CREATE. It reads the
// node fleet and the NodeProfile inventory through the manager's cached client,
// then delegates to the pure MutatePod logic.
type PodMutator struct {
	Client  client.Reader
	Decoder admission.Decoder
	// Policy is the NodeProfile source policy; profiles it rejects are not
	// counted as provisionable inventory.
	Policy controller.NodeProfilePolicy
}

// Handle implements admission.Handler.
func (m *PodMutator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	pod := &corev1.Pod{}
	if err := m.Decoder.Decode(req, pod); err != nil {
		observability.ObserveAdmission("error", "decode")
		return admission.Errored(http.StatusBadRequest, err)
	}
	if !IsBrewletPod(pod) {
		return admission.Allowed("not a brewlet pod")
	}

	fleet, err := m.fleet(ctx)
	if err != nil {
		// Fail open: never block scheduling because the webhook couldn't read
		// nodes. The shim still enforces image identity, JDK compatibility, and
		// AppCDS policy at runtime.
		log.Error(err, "listing nodes for fleet check; allowing pod without steering")
		observability.ObserveAdmission("fail_open", "fleet_unavailable")
		return admission.Allowed("fleet unavailable")
	}
	profiles, err := m.profiles(ctx)
	if err != nil {
		log.Error(err, "listing NodeProfiles for inventory check; allowing pod without steering")
		observability.ObserveAdmission("fail_open", "profiles_unavailable")
		return admission.Allowed("node profiles unavailable")
	}

	res := MutatePod(pod, fleet, profiles)
	if res.DenyReason != "" {
		log.Info("denying brewlet pod", "reason", res.DenyReason, "message", res.DenyMessage)
		observability.ObserveAdmission("denied", res.DenyReason)
		return denied(res.DenyReason, res.DenyMessage)
	}

	marshaled, err := json.Marshal(pod)
	if err != nil {
		observability.ObserveAdmission("error", "encode")
		return admission.Errored(http.StatusInternalServerError, err)
	}
	resp := admission.PatchResponseFromRaw(req.Object.Raw, marshaled)
	if res.Warning != "" {
		log.Info("admitted brewlet pod pending capacity",
			"artifactRef", res.ArtifactRef, "artifactDigest", res.ArtifactDigest, "warning", res.Warning)
		observability.ObserveAdmission("admitted", "pending_capacity")
		resp.Warnings = append(resp.Warnings, res.Warning)
		return resp
	}
	log.Info("admitted brewlet pod",
		"artifactRef", res.ArtifactRef, "artifactDigest", res.ArtifactDigest)
	observability.ObserveAdmission("admitted", "none")
	return resp
}

// fleet reads the current node inventory and projects it to capabilities.
func (m *PodMutator) fleet(ctx context.Context) ([]NodeCapability, error) {
	var nodes corev1.NodeList
	if err := m.Client.List(ctx, &nodes); err != nil {
		return nil, err
	}
	fleet := make([]NodeCapability, 0, len(nodes.Items))
	for i := range nodes.Items {
		fleet = append(fleet, NodeCapabilityFrom(&nodes.Items[i]))
	}
	return fleet, nil
}

// profiles reads the NodeProfiles whose pools can supply new capacity. Deleting
// profiles and profiles the reconciler would refuse to provision (source policy
// or pool-ownership violations) are excluded.
func (m *PodMutator) profiles(ctx context.Context) ([]ProfileCapability, error) {
	var list nodev1alpha1.NodeProfileList
	if err := m.Client.List(ctx, &list); err != nil {
		// Without the NodeProfile CRD there is no declared inventory: only
		// ready nodes count, which is the pre-profile behavior.
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, err
	}
	return EligibleProfiles(list.Items, m.Policy), nil
}

// EligibleProfiles projects the provisionable NodeProfiles to capabilities.
func EligibleProfiles(items []nodev1alpha1.NodeProfile, policy controller.NodeProfilePolicy) []ProfileCapability {
	out := make([]ProfileCapability, 0, len(items))
	for i := range items {
		p := &items[i]
		if !p.DeletionTimestamp.IsZero() {
			continue
		}
		if policy.Validate(p) != nil || controller.ValidateNoPoolConflicts(p, items) != nil {
			continue
		}
		out = append(out, ProfileCapabilityFrom(p))
	}
	return out
}

// denied builds a Forbidden admission response carrying the fleet-policy reason
// so the pod-creating controller surfaces a clear cause.
func denied(reason, message string) admission.Response {
	return admission.Response{
		AdmissionResponse: admissionv1.AdmissionResponse{
			Allowed: false,
			Result: &metav1.Status{
				Status:  metav1.StatusFailure,
				Code:    http.StatusForbidden,
				Reason:  metav1.StatusReason(reason),
				Message: message,
			},
		},
	}
}
