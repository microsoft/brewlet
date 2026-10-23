package policy

import (
	"fmt"
	"reflect"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
)

func Evidence(e, old *api.NodeRetirementEvidence) error {
	s := e.Spec
	if len(e.OwnerReferences) != 0 {
		return fmt.Errorf("retirement evidence must not have owner references")
	}
	if s.ProfileName == "" || s.ProfileUID == "" || s.NodeName == "" || s.NodeUID == "" || strings.TrimSpace(s.ExternalInstanceID) == "" || strings.TrimSpace(s.PlatformRecord) == "" || !s.PermanentlyDecommissioned {
		return fmt.Errorf("retirement evidence requires original profile/Node UIDs and an explicit permanent external disposition with retained platform record")
	}
	if old != nil && !reflect.DeepEqual(e.Spec, old.Spec) {
		return fmt.Errorf("retirement evidence is immutable")
	}
	return nil
}
