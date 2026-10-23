package policy

import (
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestExternalRetirementEvidenceImmutableAndIndependent(t *testing.T) {
	e := &api.NodeRetirementEvidence{Spec: api.NodeRetirementEvidenceSpec{ProfileName: "web", ProfileUID: "profile", NodeName: "old", NodeUID: "node", ExternalInstanceID: "permanently-removed", PlatformRecord: "audit-record", PermanentlyDecommissioned: true}}
	if err := Evidence(e, nil); err != nil {
		t.Fatal(err)
	}
	old := e.DeepCopy()
	e.Spec.PlatformRecord = "another-record"
	if Evidence(e, old) == nil {
		t.Fatal("immutable evidence mutation accepted")
	}
	e = old.DeepCopy()
	e.OwnerReferences = []metav1.OwnerReference{{UID: "profile", Name: "web", Kind: "NodeProfile"}}
	if Evidence(e, nil) == nil {
		t.Fatal("profile garbage collection could erase independent evidence")
	}
	e = old.DeepCopy()
	e.Spec.PermanentlyDecommissioned = false
	if Evidence(e, nil) == nil {
		t.Fatal("temporary/unverified disposition accepted")
	}
}
