package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func coordinatorFixture(t *testing.T, objects ...client.Object) *UninstallCoordinator {
	t.Helper()
	s := runtime.NewScheme()
	_ = api.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
		if _, ok := list.(*unstructured.UnstructuredList); ok {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: api.GroupVersion.Group, Kind: "NodeRetirementEvidenceList"}}
		}
		return c.List(ctx, list, opts...)
	}}).Build()
	return &UninstallCoordinator{Client: c, ReleaseName: "test", ReleaseNamespace: "brewlet-system", PollInterval: time.Millisecond}
}
func TestUninstallRefusesForeignProfilesWithoutMutation(t *testing.T) {
	for _, annotations := range []map[string]string{nil, {"meta.helm.sh/release-name": "test"}, {"meta.helm.sh/release-name": "test", "meta.helm.sh/release-namespace": "foreign"}} {
		p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: annotations}}
		u := coordinatorFixture(t, p)
		err := u.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "foreign") {
			t.Fatalf("foreign profile accepted: %v", err)
		}
		still := new(api.NodeProfile)
		if err := u.Client.Get(context.Background(), client.ObjectKey{Name: p.Name}, still); err != nil || still.DeletionTimestamp != nil {
			t.Fatal("foreign profile mutated")
		}
	}
}
func TestUninstallOwnedDeletionWaitsWithoutFinalizerRemoval(t *testing.T) {
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile", Finalizers: []string{api.CleanupFinalizer}, Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "test", "meta.helm.sh/release-namespace": "brewlet-system"}}}
	u := coordinatorFixture(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := u.Run(ctx); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("unfinished cleanup reported success: %v", err)
	}
	still := new(api.NodeProfile)
	_ = u.Client.Get(context.Background(), client.ObjectKey{Name: p.Name}, still)
	if still.DeletionTimestamp == nil || !contains(still.Finalizers, api.CleanupFinalizer) {
		t.Fatal("coordinator bypassed controller cleanup")
	}
}
func TestUninstallGlobalWorkersAndNoMutation(t *testing.T) {
	d := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "brewlet-node-provisioner-x", Namespace: "foreign"}}
	u := coordinatorFixture(t, d)
	if err := u.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("foreign worker hidden by namespace: %v", err)
	}
	if err := u.Client.Get(context.Background(), client.ObjectKeyFromObject(d), new(appsv1.DaemonSet)); err != nil {
		t.Fatal("coordinator deleted worker")
	}
	if err := coordinatorFixture(t).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}
