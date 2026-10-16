package controller

import (
	"context"
	"slices"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *RuntimeReconciler) resolveHistoricalTargets(ctx context.Context, im *api.RuntimeImage) (bool, error) {
	changed := false
	pending := []string{}
	for i := range im.Status.RevocationTargets {
		t := &im.Status.RevocationTargets[i]
		if t.Resolved {
			continue
		}
		p := new(api.NodeProfile)
		err := r.Reader.Get(ctx, client.ObjectKey{Name: t.ProfileName}, p)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		active := err == nil && p.UID == t.ProfileUID && p.DeletionTimestamp.IsZero() && offers(p.Spec, im.Name) && policy.Approved(im) && slices.ContainsFunc(p.Status.Targets, func(old api.Target) bool { return old.Name == t.NodeName && old.UID == t.NodeUID && old.Claimed })
		if active && p.Status.Retirement != nil && slices.ContainsFunc(p.Status.Retirement.Targets, func(old api.Target) bool { return old.UID == t.NodeUID }) {
			active = false
		}
		if active {
			continue
		}
		n := new(corev1.Node)
		err = r.Reader.Get(ctx, client.ObjectKey{Name: t.NodeName}, n)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		if apierrors.IsNotFound(err) || n.UID != t.NodeUID {
			if err := r.externalResolution(ctx, t); err != nil {
				pending = append(pending, err.Error())
				continue
			}
		} else {
			if err := r.verifyReceipt(ctx, im, t, n); err != nil {
				pending = append(pending, n.Name+": "+err.Error())
				continue
			}
		}
		changed = true
	}
	if len(pending) > 0 {
		runtimeCondition(im, "RetirementPending", "HistoricalRootsPending", strings.Join(pending, "; "), metav1.ConditionTrue)
	} else {
		runtimeCondition(im, "RetirementPending", "NoHistoricalRootObligations", "all retired target roots are proven drained/cleaned or externally resolved", metav1.ConditionFalse)
	}
	return changed, nil
}
