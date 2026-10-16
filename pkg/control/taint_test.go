package control

import (
	"context"
	"fmt"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestFallbackNormalizesNonstandardStartupValueBeforeConfirmation(t *testing.T) {
	c, a := fixture(t, interceptor.Funcs{Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
		if n, ok := obj.(*corev1.Node); ok {
			if _, exists := n.Labels["brewlet.sh/runtime.python-3.12"]; !exists {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, n.Name, fmt.Errorf("pinned label"))
			}
		}
		return c.Update(ctx, obj, opts...)
	}})
	ctx := context.Background()
	n := new(corev1.Node)
	_ = c.Get(ctx, client.ObjectKey{Name: a.NodeName}, n)
	n.Spec.Taints = []corev1.Taint{{Key: api.StartupTaint, Value: "not-the-contract-value", Effect: corev1.TaintEffectNoSchedule}}
	_ = c.Update(ctx, n)
	if !HasStartupTaint(n) || StartupFenced(n) {
		t.Fatal("nonstandard taint was treated as the confirmed application scheduling fence")
	}
	fenced, err := Withdraw(ctx, c, c, a, []string{"brewlet.sh/runtime.python-3.12"})
	if err != nil || !fenced {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKey{Name: a.NodeName}, n)
	if len(n.Spec.Taints) != 1 || !StartupFenced(n) {
		t.Fatal("fallback did not read back the exact key/value/effect fence")
	}
}
