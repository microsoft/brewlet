// Package v2alpha1 defines Brewlet's language-neutral cluster inventory.
package v2alpha1

import (
	"encoding/json"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var GroupVersion = schema.GroupVersion{Group: "node.brewlet.sh", Version: "v2alpha1"}
var SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &RuntimeImage{}, &RuntimeImageList{}, &NodeProfile{}, &NodeProfileList{}, &NodeRetirementEvidence{}, &NodeRetirementEvidenceList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
})
var AddToScheme = SchemeBuilder.AddToScheme

const (
	OwnerUID            = "brewlet.sh/owner-uid"
	OwnerNodeUID        = "brewlet.sh/owner-node-uid"
	OwnerName           = "brewlet.sh/owner-name"
	StartupTaint        = "startup-taint.cluster-autoscaler.kubernetes.io/brewlet"
	RuntimeLabel        = "brewlet.sh/runtime"
	InventoryAnnotation = "brewlet.sh/runtime-images"
	RuntimeAnnotation   = "brewlet.sh/runtime-image"
	BarrierAnnotation   = "brewlet.sh/rotated-to-generation"
	CleanupFinalizer    = "node.brewlet.sh/cleanup"
	EvidenceFinalizer   = "node.brewlet.sh/retain-retirement-evidence"
)

type Source struct {
	Image string `json:"image"`
}
type User struct {
	UID int64 `json:"uid"`
	GID int64 `json:"gid"`
}
type Compatibility struct {
	Ecosystem string `json:"ecosystem,omitempty"`
	ABI       string `json:"abi,omitempty"`
}
type RuntimeImageSpec struct {
	Source        Source        `json:"source"`
	Description   string        `json:"description,omitempty"`
	User          User          `json:"user"`
	Compatibility Compatibility `json:"compatibility,omitempty"`
}
type RotationNode struct {
	Name               string       `json:"name"`
	UID                types.UID    `json:"uid"`
	Reason             string       `json:"reason,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	AssignedAt         *metav1.Time `json:"assignedAt,omitempty"`
	LastProgressAt     *metav1.Time `json:"lastProgressAt,omitempty"`
}
type RotationStatus struct {
	Generation    int64          `json:"generation"`
	Digest        string         `json:"digest"`
	Participants  []RotationNode `json:"participants,omitempty"`
	ExcludedNodes []RotationNode `json:"excludedNodes,omitempty"`
}
type RuntimeImageStatus struct {
	Generation int64  `json:"generation,omitempty"`
	Digest     string `json:"digest,omitempty"`
	// SourceImage also tracks repository-only changes; a generation is never reused
	// for another source even when its content digest happens to match.
	SourceImage           string                   `json:"sourceImage,omitempty"`
	ApprovedUser          *User                    `json:"approvedUser,omitempty"`
	ApprovedCompatibility *Compatibility           `json:"approvedCompatibility,omitempty"`
	Conditions            []metav1.Condition       `json:"conditions,omitempty"`
	Rotation              RotationStatus           `json:"rotation,omitzero"`
	RotationCompletedAt   *metav1.Time             `json:"rotationCompletedAt,omitempty"`
	Restarts              RestartSummary           `json:"restarts,omitzero"`
	RevocationTargets     []InventoryTarget        `json:"revocationTargets,omitempty"`
	TaskObservations      []RuntimeTaskObservation `json:"taskObservations,omitempty"`
}
type RuntimeTaskObservation struct {
	NodeName          string               `json:"nodeName"`
	NodeUID           types.UID            `json:"nodeUID"`
	ProfileUID        types.UID            `json:"profileUID"`
	ProfileGeneration int64                `json:"profileGeneration"`
	ObservedAt        time.Time            `json:"observedAt"`
	Current           bool                 `json:"current"`
	Reason            string               `json:"reason"`
	Bindings          []RuntimeTaskBinding `json:"bindings,omitempty"`
}
type RuntimeTaskBinding struct {
	ContainerID         string    `json:"containerID"`
	ContainerdNamespace string    `json:"containerdNamespace"`
	PodUID              types.UID `json:"podUID"`
	PodName             string    `json:"podName"`
	PodNamespace        string    `json:"podNamespace"`
	ContainerName       string    `json:"containerName"`
	InventoryUID        types.UID `json:"inventoryUID"`
	Generation          int64     `json:"generation"`
	Digest              string    `json:"digest"`
	State               string    `json:"state"`
}
type InventoryTarget struct {
	Generation         int64     `json:"generation"`
	Digest             string    `json:"digest"`
	NodeName           string    `json:"nodeName"`
	NodeUID            types.UID `json:"nodeUID"`
	ProfileName        string    `json:"profileName"`
	ProfileUID         types.UID `json:"profileUID"`
	ProfileGeneration  int64     `json:"profileGeneration"`
	ObservedGeneration int64     `json:"observedGeneration,omitempty"`
	ProviderID         string    `json:"providerID,omitempty"`
	SystemUUID         string    `json:"systemUUID,omitempty"`
	Resolved           bool      `json:"resolved,omitempty"`
	Resolution         string    `json:"resolution,omitempty"`
	EvidenceName       string    `json:"evidenceName,omitempty"`
	EvidenceUID        types.UID `json:"evidenceUID,omitempty"`
}
type RestartBlockedPod struct {
	Name              string    `json:"name"`
	Namespace         string    `json:"namespace"`
	UID               types.UID `json:"uid"`
	Node              string    `json:"node,omitempty"`
	Reason            string    `json:"reason"`
	MinimumGeneration int64     `json:"minimumGeneration"`
}
type WorkloadRestart struct {
	ObservedPodUIDs    []types.UID         `json:"observedPodUIDs,omitempty"`
	Kind               string              `json:"kind"`
	Name               string              `json:"name"`
	Namespace          string              `json:"namespace"`
	UID                types.UID           `json:"uid"`
	InventoryUID       types.UID           `json:"inventoryUID"`
	Generation         int64               `json:"generation"`
	Phase              string              `json:"phase"`
	Reason             string              `json:"reason"`
	Message            string              `json:"message,omitempty"`
	StartedAt          *metav1.Time        `json:"startedAt,omitempty"`
	CompletedAt        *metav1.Time        `json:"completedAt,omitempty"`
	WorkloadGeneration int64               `json:"workloadGeneration,omitempty"`
	Patched            bool                `json:"patched,omitempty"`
	NonActionable      bool                `json:"nonActionable,omitempty"`
	BlockedPods        []RestartBlockedPod `json:"blockedPods,omitempty"`
}
type RestartSummary struct {
	Workloads     []WorkloadRestart `json:"workloads,omitempty"`
	Pending       int32             `json:"pending,omitempty"`
	Rolling       int32             `json:"rolling,omitempty"`
	Complete      int32             `json:"complete,omitempty"`
	Deferred      int32             `json:"deferred,omitempty"`
	NotApplicable int32             `json:"notApplicable,omitempty"`
	Failed        int32             `json:"failed,omitempty"`
}
type RuntimeImage struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              RuntimeImageSpec   `json:"spec"`
	Status            RuntimeImageStatus `json:"status,omitempty"`
}
type RuntimeImageList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RuntimeImage `json:"items"`
}
type NodePool struct {
	Names               []string `json:"names,omitempty"`
	Key                 string   `json:"key,omitempty"`
	IncludeControlPlane bool     `json:"includeControlPlane,omitempty"`
}
type RuntimeEntry struct {
	ID      string `json:"id"`
	Install string `json:"install,omitempty"`
	IdleTTL string `json:"idleTTL,omitempty"`
}

func (e RuntimeEntry) Mode() string {
	if e.Install == "" {
		return "Eager"
	}
	return e.Install
}

type Registry struct {
	Mirrors map[string]string `json:"mirrors,omitempty"`
}
type Rollout struct {
	MaxUnavailable    int32  `json:"maxUnavailable,omitempty"`
	ContainerdRestart string `json:"containerdRestart,omitempty"`
}

func (r Rollout) RestartPolicy() string {
	if r.ContainerdRestart == "" {
		return "validated"
	}
	return r.ContainerdRestart
}

type NodeProfileSpec struct {
	NodePool      NodePool            `json:"nodePool"`
	RuntimeImages []RuntimeEntry      `json:"runtimeImages"`
	Tolerations   []corev1.Toleration `json:"tolerations,omitempty"`
	Registry      Registry            `json:"registry,omitempty"`
	Rollout       Rollout             `json:"rollout,omitempty"`
}
type Target struct {
	Name                   string    `json:"name"`
	UID                    types.UID `json:"uid"`
	Claimed                bool      `json:"claimed"`
	ContainerdRestart      string    `json:"containerdRestart"`
	ProviderID             string    `json:"providerID,omitempty"`
	SystemUUID             string    `json:"systemUUID,omitempty"`
	RetirementEvidenceName string    `json:"retirementEvidenceName,omitempty"`
	RetirementEvidenceUID  types.UID `json:"retirementEvidenceUID,omitempty"`
}
type Retirement struct {
	Targets    []Target        `json:"targets"`
	Generation int64           `json:"generation"`
	Spec       NodeProfileSpec `json:"spec"`
	Phase      string          `json:"phase"`
}
type NodeProfileStatus struct {
	AssignedNodes              int32              `json:"assignedNodes,omitempty"`
	ReadyNodes                 int32              `json:"readyNodes,omitempty"`
	Conditions                 []metav1.Condition `json:"conditions,omitempty"`
	Targets                    []Target           `json:"targets,omitempty"`
	OwnershipInitialized       bool               `json:"ownershipInitialized,omitempty"`
	ProvisioningGeneration     int64              `json:"provisioningGeneration,omitempty"`
	ProvisioningSpec           *NodeProfileSpec   `json:"provisioningSpec,omitempty"`
	Retirement                 *Retirement        `json:"retirement,omitempty"`
	DetachedRetirements        []Retirement       `json:"detachedRetirements,omitempty"`
	ExternalRetirementResolved bool               `json:"externalRetirementResolved,omitempty"`
}
type NodeProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeProfileSpec   `json:"spec"`
	Status            NodeProfileStatus `json:"status,omitempty"`
}
type NodeProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeProfile `json:"items"`
}
type NodeRetirementEvidenceSpec struct {
	ProfileName               string    `json:"profileName"`
	ProfileUID                types.UID `json:"profileUID"`
	NodeName                  string    `json:"nodeName"`
	NodeUID                   types.UID `json:"nodeUID"`
	ProviderID                string    `json:"providerID,omitempty"`
	SystemUUID                string    `json:"systemUUID,omitempty"`
	ExternalInstanceID        string    `json:"externalInstanceID"`
	PlatformRecord            string    `json:"platformRecord"`
	PermanentlyDecommissioned bool      `json:"permanentlyDecommissioned"`
	LegacyMapping             string    `json:"legacyMapping,omitempty"`
}
type NodeRetirementEvidenceStatus struct {
	AcceptedAt        *metav1.Time       `json:"acceptedAt,omitempty"`
	ResolvedAt        *metav1.Time       `json:"resolvedAt,omitempty"`
	OriginalTarget    *Target            `json:"originalTarget,omitempty"`
	CleanupGeneration int64              `json:"cleanupGeneration,omitempty"`
	CleanupSpec       *NodeProfileSpec   `json:"cleanupSpec,omitempty"`
	Conditions        []metav1.Condition `json:"conditions,omitempty"`
}
type NodeRetirementEvidence struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              NodeRetirementEvidenceSpec   `json:"spec"`
	Status            NodeRetirementEvidenceStatus `json:"status,omitempty"`
}
type NodeRetirementEvidenceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeRetirementEvidence `json:"items"`
}

func (e *NodeRetirementEvidence) DeepCopy() *NodeRetirementEvidence  { return clone(e) }
func (e *NodeRetirementEvidence) DeepCopyObject() runtime.Object     { return e.DeepCopy() }
func (e *NodeRetirementEvidenceList) DeepCopyObject() runtime.Object { return clone(e) }

type NodeRuntime struct {
	Install      string    `json:"install"`
	State        string    `json:"state"`
	Installed    bool      `json:"installed"`
	InventoryUID types.UID `json:"inventoryUID"`
	Generation   int64     `json:"generation"`
	Digest       string    `json:"digest"`
}
type Barrier struct {
	ID           string    `json:"id"`
	InventoryUID types.UID `json:"inventoryUID"`
	Generation   int64     `json:"generation"`
}

// These API values contain maps and slices; JSON cloning avoids accidental aliasing.
func clone[T any](in *T) *T {
	if in == nil {
		return nil
	}
	b, _ := json.Marshal(in)
	out := new(T)
	_ = json.Unmarshal(b, out)
	return out
}
func (r *RuntimeImage) DeepCopy() *RuntimeImage            { return clone(r) }
func (r *RuntimeImage) DeepCopyObject() runtime.Object     { return r.DeepCopy() }
func (r *RuntimeImageList) DeepCopyObject() runtime.Object { return clone(r) }
func (r *NodeProfile) DeepCopy() *NodeProfile              { return clone(r) }
func (r *NodeProfile) DeepCopyObject() runtime.Object      { return r.DeepCopy() }
func (r *NodeProfileList) DeepCopyObject() runtime.Object  { return clone(r) }
