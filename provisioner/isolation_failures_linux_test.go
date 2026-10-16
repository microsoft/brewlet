//go:build linux

package provisioner

import (
	"context"
	"errors"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type unavailableImageReader struct{ client.Reader }

func (r unavailableImageReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if _, ok := object.(*api.RuntimeImage); ok && key.Name == "missing" {
		return apierrors.NewServiceUnavailable("lookup not verified")
	}
	return r.Reader.Get(ctx, key, object, opts...)
}

type failedInstaller struct{}

func (failedInstaller) Stage(context.Context, *api.RuntimeImage, string) (string, node.Metadata, error) {
	return "", node.Metadata{}, errors.New("approved eager install failed")
}
func TestUnverifiedLookupAndApprovedEagerFailureStillGateBaselineFakeAPI(t *testing.T) {
	for _, failure := range []string{"unverified lookup", "approved eager failure"} {
		t.Run(failure, func(t *testing.T) {
			w, _ := authorityFixture(t)
			requireRootWorkdir(t, w)
			approveStatic(t, w)
			w.Baseline = baselineSuccess{}
			ctx := context.Background()
			p := new(api.NodeProfile)
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "profile"}, p); err != nil {
				t.Fatal(err)
			}
			p.Spec.RuntimeImages = []api.RuntimeEntry{{ID: "static", Install: "Eager"}}
			if failure == "unverified lookup" {
				p.Spec.RuntimeImages = append([]api.RuntimeEntry{{ID: "missing", Install: "Eager"}}, p.Spec.RuntimeImages...)
				w.Reader = unavailableImageReader{w.Reader}
				w.Installer = new(fixtureInstaller)
			} else {
				w.Installer = failedInstaller{}
			}
			copy := p.Spec
			p.Status.ProvisioningSpec = &copy
			if err := w.Client.Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			n := new(corev1.Node)
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			n.Spec.Taints = []corev1.Taint{{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}}
			if err := w.Client.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			if err := w.Reconcile(ctx); err == nil {
				t.Fatal("unverified required work released baseline")
			}
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			if n.Labels[api.RuntimeLabel] == "ready" || len(n.Spec.Taints) == 0 {
				t.Fatal("failed required check released baseline", n.Labels, n.Spec.Taints)
			}
		})
	}
}
