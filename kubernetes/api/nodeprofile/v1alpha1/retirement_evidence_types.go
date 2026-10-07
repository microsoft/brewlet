// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

// NodeRetirementEvidence is an administrator's immutable attestation, retained
// independently of the profile. RBAC creation authority is the trust boundary;
// Brewlet does not verify the external platform record.
type NodeRetirementEvidence struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RetirementEvidenceSpec   `json:"spec"`
	Status            RetirementEvidenceStatus `json:"status,omitempty"`
}

type RetirementEvidenceSpec struct {
	ProfileName               string      `json:"profileName"`
	ProfileUID                types.UID   `json:"profileUID"`
	NodeName                  string      `json:"nodeName"`
	NodeUID                   types.UID   `json:"nodeUID"`
	ProviderID                string      `json:"providerID"`
	InstanceID                string      `json:"instanceID"`
	SystemUUID                string      `json:"systemUUID,omitempty"`
	EvidenceRef               string      `json:"evidenceRef"`
	IdentityBinding           string      `json:"identityBinding"`
	RetiredAt                 metav1.Time `json:"retiredAt"`
	PermanentlyDecommissioned bool        `json:"permanentlyDecommissioned"`
}

type RetirementEvidenceStatus struct {
	Phase      string       `json:"phase,omitempty"`
	Message    string       `json:"message,omitempty"`
	AcceptedAt *metav1.Time `json:"acceptedAt,omitempty"`
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`
	// Obligation preserves the original target and authorized cleanup policy.
	Obligation *NodeRetirement `json:"obligation,omitempty"`
}

const (
	EvidenceBlocked  = "Blocked"
	EvidenceAccepted = "Accepted"
	EvidenceResolved = "Resolved"
)

type NodeRetirementEvidenceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeRetirementEvidence `json:"items"`
}

func (in *NodeRetirementEvidence) DeepCopy() *NodeRetirementEvidence {
	if in == nil {
		return nil
	}
	out := new(NodeRetirementEvidence)
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Status.AcceptedAt = in.Status.AcceptedAt.DeepCopy()
	out.Status.ResolvedAt = in.Status.ResolvedAt.DeepCopy()
	if in.Status.Obligation != nil {
		out.Status.Obligation = new(NodeRetirement)
		*out.Status.Obligation = *in.Status.Obligation
		out.Status.Obligation.Targets = append([]NodeTarget(nil), in.Status.Obligation.Targets...)
		in.Status.Obligation.Spec.DeepCopyInto(&out.Status.Obligation.Spec)
	}
	return out
}

func (in *NodeRetirementEvidence) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	return in.DeepCopy()
}

func (in *NodeRetirementEvidenceList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(NodeRetirementEvidenceList)
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	out.Items = make([]NodeRetirementEvidence, len(in.Items))
	for i := range in.Items {
		out.Items[i] = *in.Items[i].DeepCopy()
	}
	return out
}
