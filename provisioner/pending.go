package provisioner

import (
	"errors"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/node"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pendingApproval(r *api.RuntimeImage) bool {
	if !r.DeletionTimestamp.IsZero() || policy.Runtime(r, nil) != nil {
		return false
	}
	approved := meta.FindStatusCondition(r.Status.Conditions, "Approved")
	if approved == nil || approved.Status != metav1.ConditionTrue || r.Status.Generation < 1 || r.Status.Digest == "" {
		return false
	}
	if r.Status.ApprovedUser == nil || *r.Status.ApprovedUser != r.Spec.User || r.Status.ApprovedCompatibility == nil || *r.Status.ApprovedCompatibility != r.Spec.Compatibility {
		return false
	}
	return approved.ObservedGeneration != r.Generation || r.Status.SourceImage != r.Spec.Source.Image || !strings.HasSuffix(r.Spec.Source.Image, "@"+r.Status.Digest)
}
func isUpdating(err error) bool {
	var reason *node.Error
	return errors.As(err, &reason) && reason.Reason == "RuntimeImageUpdating"
}
