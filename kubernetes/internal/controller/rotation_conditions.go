package controller

import (
	"fmt"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func rotationTargetMessage(im *api.RuntimeImage, msg string) string {
	return fmt.Sprintf("inventoryUID=%s generation=%d digest=%s: %s", im.UID, im.Status.Generation, im.Status.Digest, msg)
}
func rotationConditions(im *api.RuntimeImage, reason, msg string, complete metav1.ConditionStatus) {
	target := rotationTargetMessage(im, msg)
	meta.SetStatusCondition(&im.Status.Conditions, metav1.Condition{Type: "RotationComplete", Status: complete, Reason: reason, Message: target, ObservedGeneration: im.Generation})
	progress, progressReason := metav1.ConditionTrue, "TargetConverging"
	if complete == metav1.ConditionTrue {
		progress = metav1.ConditionFalse
		progressReason = "TargetConverged"
	} else if complete == metav1.ConditionUnknown {
		progress = metav1.ConditionUnknown
		progressReason = "TargetObservationUnknown"
	}
	meta.SetStatusCondition(&im.Status.Conditions, metav1.Condition{Type: "RotationProgressing", Status: progress, Reason: progressReason, Message: target, ObservedGeneration: im.Generation})
}
func rotationRevoked(im *api.RuntimeImage, msg string) {
	for _, kind := range []string{"RotationComplete", "RotationProgressing"} {
		meta.SetStatusCondition(&im.Status.Conditions, metav1.Condition{Type: kind, Status: metav1.ConditionFalse, Reason: "RuntimeImageRevoked", Message: rotationTargetMessage(im, msg), ObservedGeneration: im.Generation})
	}
}
