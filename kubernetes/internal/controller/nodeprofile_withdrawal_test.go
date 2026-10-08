package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"brewlet-operator/internal/brewlet"
)

// pinnedLabelsInterceptor mimics a managed node pool admission webhook that
// refuses any patch removing one of the pinned labels.
func pinnedLabelsInterceptor(pinned ...string) interceptor.Funcs {
	return interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			node, ok := obj.(*corev1.Node)
			if !ok {
				return c.Patch(ctx, obj, patch, opts...)
			}
			for _, key := range pinned {
				if _, kept := node.Labels[key]; !kept {
					return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, node.Name,
						errPinnedLabel(key))
				}
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}
}

type errPinnedLabel string

func (e errPinnedLabel) Error() string {
	return "Label delete request \"" + string(e) + "\" refused. User is attempting to delete a label configured on aks node pool"
}

func withdrawalTestNode() *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pinned",
			Labels: map[string]string{
				brewlet.LabelRuntimeReady:             brewlet.ValueReady,
				brewlet.LabelJDKPrefix + "temurin-21": "true",
				brewlet.LabelLauncherPrefix + "java":  "true",
				"kubernetes.azure.com/agentpool":      "bwcaz",
				brewlet.LabelJDKFeaturePrefix + "21":  "true",
				"brewlet.sh/appcds-regeneration":      "true",
			},
			Annotations: map[string]string{
				brewlet.AnnotationProfile: "bwcaz",
				brewlet.AnnotationJDKs:    "temurin-21",
			},
		},
	}
}

func newWithdrawalReconciler(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) *NodeProfileReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
	return &NodeProfileReconciler{Client: c}
}

func TestPatchNodeWithdrawalFencesPinnedLabelsWithStartupTaint(t *testing.T) {
	ctx := context.Background()
	r := newWithdrawalReconciler(t, pinnedLabelsInterceptor(brewlet.LabelRuntimeReady, brewlet.LabelJDKPrefix+"temurin-21"), withdrawalTestNode())

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: "pinned"}, &node); err != nil {
		t.Fatal(err)
	}
	base := node.DeepCopy()
	removeNodeAdvertisements(&node)
	if err := r.patchNodeWithdrawal(ctx, base, &node); err != nil {
		t.Fatalf("patchNodeWithdrawal() error = %v", err)
	}

	var got corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: "pinned"}, &got); err != nil {
		t.Fatal(err)
	}
	if !brewlet.HasStartupTaint(&got) {
		t.Fatalf("taints = %v, want the startup taint", got.Spec.Taints)
	}
	if brewlet.RuntimeReady(&got) {
		t.Fatal("node is still runtime-ready after a fenced withdrawal")
	}
	if got.Labels[brewlet.LabelRuntimeReady] != brewlet.ValueReady {
		t.Fatalf("pinned readiness label was removed: %v", got.Labels)
	}
	if _, ok := got.Annotations[brewlet.AnnotationProfile]; ok {
		t.Fatalf("profile annotation survived the withdrawal: %v", got.Annotations)
	}
	if got.Labels["kubernetes.azure.com/agentpool"] != "bwcaz" {
		t.Fatalf("unrelated labels changed: %v", got.Labels)
	}
}

func TestPatchNodeWithdrawalRemovesLabelsWhenAllowed(t *testing.T) {
	ctx := context.Background()
	r := newWithdrawalReconciler(t, interceptor.Funcs{}, withdrawalTestNode())

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: "pinned"}, &node); err != nil {
		t.Fatal(err)
	}
	base := node.DeepCopy()
	removeNodeAdvertisements(&node)
	if err := r.patchNodeWithdrawal(ctx, base, &node); err != nil {
		t.Fatalf("patchNodeWithdrawal() error = %v", err)
	}

	var got corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: "pinned"}, &got); err != nil {
		t.Fatal(err)
	}
	if brewlet.HasStartupTaint(&got) {
		t.Fatal("an unrefused withdrawal must not taint the node")
	}
	if _, ok := got.Labels[brewlet.LabelRuntimeReady]; ok {
		t.Fatalf("readiness label survived: %v", got.Labels)
	}
}

func TestPatchNodeWithdrawalReturnsConflictWithoutFencing(t *testing.T) {
	ctx := context.Background()
	calls := 0
	funcs := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			calls++
			return apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, obj.GetName(), errPinnedLabel("x"))
		},
	}
	r := newWithdrawalReconciler(t, funcs, withdrawalTestNode())

	var node corev1.Node
	if err := r.Get(ctx, types.NamespacedName{Name: "pinned"}, &node); err != nil {
		t.Fatal(err)
	}
	base := node.DeepCopy()
	removeNodeAdvertisements(&node)
	if err := r.patchNodeWithdrawal(ctx, base, &node); !apierrors.IsConflict(err) {
		t.Fatalf("patchNodeWithdrawal() error = %v, want conflict", err)
	}
	if calls != 1 {
		t.Fatalf("patch calls = %d, want 1", calls)
	}
}
