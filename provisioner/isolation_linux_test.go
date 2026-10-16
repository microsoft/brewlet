//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type fixtureInstaller struct{ calls int }

func (i *fixtureInstaller) Stage(_ context.Context, r *api.RuntimeImage, base string) (string, node.Metadata, error) {
	i.calls++
	if err := os.MkdirAll(filepath.Join(base, ".staging"), 0700); err != nil {
		return "", node.Metadata{}, err
	}
	stage, err := os.MkdirTemp(filepath.Join(base, ".staging"), "fixture-")
	if err != nil {
		return "", node.Metadata{}, err
	}
	root := filepath.Join(stage, "rootfs")
	for _, name := range []string{"bin", "etc", "home/app", "app"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0755); err != nil {
			return "", node.Metadata{}, err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("app:x:1000:1000:app:/home/app:/bin/tool\n"), 0644); err != nil {
		return "", node.Metadata{}, err
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte("app:x:1000:\n"), 0644); err != nil {
		return "", node.Metadata{}, err
	}
	m := node.Metadata{ID: r.Name, InventoryUID: string(r.UID), Generation: r.Status.Generation, SourceDigest: r.Status.Digest, ManifestDigest: "sha256:" + strings.Repeat("b", 64), Platform: ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}, Env: []string{"PATH=/bin"}, UID: 1000, GID: 1000}
	return stage, m, nil
}
func TestUnknownEagerDoesNotBlockApprovedEntriesFakeAPI(t *testing.T) {
	for _, install := range []string{"Eager", "OnDemand"} {
		t.Run(install, func(t *testing.T) {
			w, _ := authorityFixture(t)
			requireRootWorkdir(t, w)
			approveStatic(t, w)
			w.Baseline = baselineSuccess{}
			ctx := context.Background()
			profile := new(api.NodeProfile)
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "profile"}, profile); err != nil {
				t.Fatal(err)
			}
			profile.Spec.RuntimeImages = []api.RuntimeEntry{{ID: "missing", Install: "Eager"}, {ID: "static", Install: install}}
			copy := profile.Spec
			profile.Status.ProvisioningSpec = &copy
			if err := w.Client.Update(ctx, profile); err != nil {
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
			installer := new(fixtureInstaller)
			w.Installer = installer
			if err := w.Reconcile(ctx); err != nil {
				t.Fatal("unknown ID blocked valid inventory", err)
			}
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			expected := "available"
			calls := 0
			if install == "Eager" {
				expected = "ready"
				calls = 1
			}
			if n.Labels[api.RuntimeLabel] != "ready" || n.Labels["brewlet.sh/runtime.static"] != expected || control.HasStartupTaint(n) || installer.calls != calls {
				t.Fatal(n.Labels, n.Spec.Taints, installer.calls)
			}
			if _, exists := n.Labels["brewlet.sh/runtime.missing"]; exists {
				t.Fatal("unknown capability published")
			}
			if _, _, err := w.Store.Resolve(ctx, "missing", "k8s.io", "unknown-start", nil, time.Millisecond); err == nil {
				t.Fatal("unknown ID authorized a start")
			}
			auths, err := w.Store.Authorizations()
			if err != nil || len(auths) != 1 || auths[0].ID != "static" {
				t.Fatal(auths, err)
			}
			var inventory map[string]api.NodeRuntime
			if err := json.Unmarshal([]byte(n.Annotations[api.InventoryAnnotation]), &inventory); err != nil {
				t.Fatal(err)
			}
			if len(inventory) != 1 || inventory["static"].State != expected {
				t.Fatal(fmt.Sprintf("%+v", inventory))
			}
		})
	}
}
