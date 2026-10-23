package control

import (
	"context"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestClaimReleaseResourceVersionRaceCannotReleaseForeignClaim(t *testing.T) {
	raced := false
	c, a := fixture(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if n, ok := obj.(*corev1.Node); ok && n.Labels[api.OwnerUID] == "" && !raced {
			raced = true
			live := new(corev1.Node)
			if err := c.Get(ctx, client.ObjectKey{Name: n.Name}, live); err != nil {
				return err
			}
			live.Labels[api.OwnerUID] = "other-profile"
			if err := c.Update(ctx, live); err != nil {
				return err
			}
		}
		return c.Update(ctx, obj, opts...)
	}})
	ctx := context.Background()
	p := new(api.NodeProfile)
	_ = c.Get(ctx, client.ObjectKey{Name: a.ProfileName}, p)
	p.Status.Retirement = &api.Retirement{Targets: p.Status.Targets, Generation: 1, Spec: p.Spec, Phase: "Teardown"}
	if err := c.Status().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	n := new(corev1.Node)
	_ = c.Get(ctx, client.ObjectKey{Name: a.NodeName}, n)
	delete(n.Labels, api.RuntimeLabel)
	delete(n.Labels, "brewlet.sh/runtime.python-3.12")
	if err := c.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := ReleaseClaim(ctx, c, c, a); !apierrors.IsConflict(err) {
		t.Fatalf("release did not use resourceVersion CAS: %v", err)
	}
	if err := ReleaseClaim(ctx, c, c, a); err == nil {
		t.Fatal("retry released another profile's claim")
	}
	_ = c.Get(ctx, client.ObjectKey{Name: a.NodeName}, n)
	if n.Labels[api.OwnerUID] != "other-profile" {
		t.Fatal("foreign owner overwritten")
	}
}
