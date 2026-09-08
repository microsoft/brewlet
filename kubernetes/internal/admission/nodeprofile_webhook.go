// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"net/http"
	"reflect"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/controller"

	admissionv1 "k8s.io/api/admission/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// NodeProfileValidator is the controller-runtime admission handler that rejects
// malformed NodeProfiles on CREATE/UPDATE
// (https://github.com/microsoft/brewlet/tree/main/specs): a non-empty JDK list,
// digest-pinned explicit sources, approved mirrors, valid rollout settings, and
// no two profiles naming the same pool. Reconciliation repeats the same checks
// so bypassing this webhook cannot reach the privileged provisioner.
type NodeProfileValidator struct {
	Client  client.Reader
	Decoder admission.Decoder
	Policy  controller.NodeProfilePolicy
}

// Handle implements admission.Handler.
func (v *NodeProfileValidator) Handle(ctx context.Context, req admission.Request) admission.Response {
	log := logf.FromContext(ctx)

	profile := &nodev1alpha1.NodeProfile{}
	if err := v.Decoder.Decode(req, profile); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	if req.Operation == admissionv1.Update && !profile.DeletionTimestamp.IsZero() {
		oldProfile := &nodev1alpha1.NodeProfile{}
		if err := v.Decoder.DecodeRaw(req.OldObject, oldProfile); err != nil {
			return admission.Errored(http.StatusBadRequest, err)
		}
		if deletingFinalizersOnlyRemoved(oldProfile, profile) &&
			!controller.HasNodeProfileCleanupObligations(oldProfile) {
			return admission.Allowed("allowing finalizer removal from deleting NodeProfile")
		}
	}

	if err := v.Policy.Validate(profile); err != nil {
		log.Info("rejecting NodeProfile", "name", profile.Name, "reason", err.Error())
		return denied("InvalidNodeProfile", "NodeProfile rejected: "+err.Error())
	}

	var existing nodev1alpha1.NodeProfileList
	if err := v.Client.List(ctx, &existing); err != nil {
		log.Error(err, "listing NodeProfiles for ownership check")
		return admission.Errored(http.StatusServiceUnavailable, err)
	}
	if err := controller.ValidateNoPoolConflicts(profile, existing.Items); err != nil {
		log.Info("rejecting NodeProfile", "name", profile.Name, "reason", err.Error())
		return denied("PoolConflict", "NodeProfile rejected: "+err.Error())
	}

	return admission.Allowed("valid NodeProfile")
}

func deletingFinalizersOnlyRemoved(oldProfile, newProfile *nodev1alpha1.NodeProfile) bool {
	if oldProfile.DeletionTimestamp.IsZero() ||
		newProfile.DeletionTimestamp.IsZero() ||
		!finalizersOnlyRemoved(oldProfile.Finalizers, newProfile.Finalizers) {
		return false
	}

	oldCopy := oldProfile.DeepCopy()
	newCopy := newProfile.DeepCopy()
	oldCopy.Finalizers = nil
	newCopy.Finalizers = nil
	oldCopy.ManagedFields = nil
	newCopy.ManagedFields = nil
	return reflect.DeepEqual(oldCopy, newCopy)
}

func finalizersOnlyRemoved(oldFinalizers, newFinalizers []string) bool {
	if len(newFinalizers) >= len(oldFinalizers) {
		return false
	}
	remaining := make(map[string]int, len(oldFinalizers))
	for _, finalizer := range oldFinalizers {
		remaining[finalizer]++
	}
	for _, finalizer := range newFinalizers {
		if remaining[finalizer] == 0 {
			return false
		}
		remaining[finalizer]--
	}
	return true
}
