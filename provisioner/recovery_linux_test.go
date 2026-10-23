//go:build linux

package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func recoveryFixture(t *testing.T, modes map[string]string, fenced bool) (*Worker, map[string]api.NodeRuntime) {
	t.Helper()
	w, _ := authorityFixture(t)
	requireRootWorkdir(t, w)
	w.Baseline = baselineSuccess{}
	ctx := context.Background()
	p := new(api.NodeProfile)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "profile"}, p); err != nil {
		t.Fatal(err)
	}
	p.Spec.RuntimeImages = nil
	inventory := map[string]api.NodeRuntime{}
	for id, mode := range modes {
		p.Spec.RuntimeImages = append(p.Spec.RuntimeImages, api.RuntimeEntry{ID: id, Install: mode})
		digest := "sha256:" + strings.Repeat("c", 64)
		r := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: id, UID: types.UID("epoch-" + id), Generation: 1}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: "registry.example.com/" + id + "@" + digest}, User: api.User{UID: 1000, GID: 1000}}, Status: api.RuntimeImageStatus{Generation: 2, Digest: digest, SourceImage: "registry.example.com/" + id + "@" + digest, Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}, RevocationTargets: []api.InventoryTarget{{NodeName: w.Authority.NodeName, NodeUID: w.Authority.NodeUID, ProfileName: w.Authority.ProfileName, ProfileUID: w.Authority.ProfileUID, ProfileGeneration: 1, Generation: 2, Digest: digest}}}}
		if err := w.Client.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := w.Store.WriteAuthorization(ctx, node.Authorization{ID: id, InventoryUID: string(r.UID), Generation: 2, Digest: digest, ProfileUID: string(w.Authority.ProfileUID), ProfileGeneration: 1, ExpiresAt: time.Now().Add(5 * time.Minute), Install: mode, State: "ready"}); err != nil {
			t.Fatal(err)
		}
		inventory[id] = setRecoveryCurrent(t, w, id, mode, 1, "sha256:"+strings.Repeat("a", 64))
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
	n.Labels[api.RuntimeLabel] = "ready"
	for id := range modes {
		n.Labels["brewlet.sh/runtime."+id] = "ready"
	}
	data, _ := json.Marshal(inventory)
	n.Annotations[api.InventoryAnnotation] = string(data)
	if fenced {
		n.Spec.Taints = []corev1.Taint{{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}}
	}
	if err := w.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	return w, inventory
}

func setRecoveryCurrent(t *testing.T, w *Worker, id, mode string, generation int64, digest string) api.NodeRuntime {
	t.Helper()
	uid := "epoch-" + id
	base := filepath.Join(w.Store.Root, id, uid, strconv.FormatInt(generation, 10))
	if err := os.MkdirAll(filepath.Join(base, "rootfs"), 0700); err != nil {
		t.Fatal(err)
	}
	m := node.Metadata{ID: id, InventoryUID: uid, Generation: generation, SourceDigest: digest, ManifestDigest: "sha256:" + strings.Repeat("b", 64), Platform: ocispec.Platform{OS: "linux", Architecture: runtime.GOARCH}, Env: []string{"PATH=/bin"}, UID: 1000, GID: 1000, Home: "/home/app", Homes: map[uint32]string{1000: "/home/app"}, ValidatedAt: time.Now()}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "metadata.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "rootfs", ".brewlet-source"), []byte(digest+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(w.Store.Root, id, "current")
	if err := os.Remove(pointer); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(uid, strconv.FormatInt(generation, 10)), pointer); err != nil {
		t.Fatal(err)
	}
	return api.NodeRuntime{Install: mode, State: "ready", Installed: true, InventoryUID: types.UID(uid), Generation: generation, Digest: digest}
}

func TestRecoveryQuarantineRejectsOldCurrentButHealthyRotationAllowsIt(t *testing.T) {
	w, inventory := recoveryFixture(t, map[string]string{"static": "Eager"}, false)
	ctx := context.Background()
	if err := w.publish(ctx, inventory); err != nil {
		t.Fatal("healthy compatible old current rejected", err)
	}

	r := new(api.RuntimeImage)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "static"}, r); err != nil {
		t.Fatal(err)
	}
	r.Status.Rotation.ExcludedNodes = []api.RotationNode{{Name: "node", UID: w.Authority.NodeUID, Reason: "RotationStalled"}}
	if err := w.Client.Update(ctx, r); err != nil {
		t.Fatal(err)
	}
	n := new(corev1.Node)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	delete(n.Labels, "brewlet.sh/runtime.static")
	if err := w.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := w.publish(ctx, inventory); err == nil {
		t.Fatal("quarantined old current republished")
	}
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	if _, exists := n.Labels["brewlet.sh/runtime.static"]; exists {
		t.Fatal("old generation advertised ready")
	}
	inventory["static"] = setRecoveryCurrent(t, w, "static", "Eager", 2, r.Status.Digest)
	if err := w.publish(ctx, inventory); err != nil {
		t.Fatal("latest recovered generation could not rejoin", err)
	}
}

func TestPublicationRejectsContradictoryInstallationEvidence(t *testing.T) {
	for _, state := range []string{"ready", "available"} {
		t.Run(state, func(t *testing.T) {
			w, inventory := recoveryFixture(t, map[string]string{"static": "OnDemand"}, false)
			value := inventory["static"]
			value.State = state
			value.Installed = state != "ready"
			inventory["static"] = value
			err := w.publish(context.Background(), inventory)
			if err == nil || !strings.Contains(err.Error(), "installation evidence contradicts capability") {
				t.Fatalf("contradictory installation evidence accepted: %v", err)
			}
		})
	}
}

func TestRecoveryInstalledFailedEvidenceCannotBecomeColdAfterPointerLoss(t *testing.T) {
	w, inventory := recoveryFixture(t, map[string]string{"static": "OnDemand"}, false)
	ctx := context.Background()
	n := new(corev1.Node)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	value := inventory["static"]
	value.State = "failed"
	inventory["static"] = value
	raw, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	n.Annotations[api.InventoryAnnotation] = string(raw)
	delete(n.Labels, "brewlet.sh/runtime.static")
	if err := w.Client.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	authorizations, err := w.Store.Authorizations()
	if err != nil {
		t.Fatal(err)
	}
	for _, authorization := range authorizations {
		authorization.State = "failed"
		if err := w.Store.WriteAuthorization(ctx, authorization); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(w.Store.Root, "static", "current")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := w.check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := w.recoveryPlan(ctx, snapshot)
	if err != nil || !recovery.ids["static"] || !recovery.installed["static"] {
		t.Fatalf("explicit installed history was reclassified as cold: %+v, %v", recovery, err)
	}
}

func TestRecoveryAllIDsMustBeLatestBeforeNodeWideRelease(t *testing.T) {
	w, inventory := recoveryFixture(t, map[string]string{"static": "Eager", "unaffected": "Eager"}, true)
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("c", 64)
	inventory["static"] = setRecoveryCurrent(t, w, "static", "Eager", 2, digest)
	if err := w.publish(ctx, inventory); err == nil {
		t.Fatal("unaffected stale ID did not block all-ID recovery")
	}
	n := new(corev1.Node)
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	if !control.StartupFenced(n) {
		t.Fatal("node-wide fence released for only one updated ID")
	}
	inventory["unaffected"] = setRecoveryCurrent(t, w, "unaffected", "Eager", 2, digest)
	if err := w.publish(ctx, inventory); err != nil {
		t.Fatal(err)
	}
	if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
		t.Fatal(err)
	}
	if control.HasStartupTaint(n) {
		t.Fatal("all-latest recovery did not release fence")
	}
}

type recoveryPinClient struct {
	client.Client
	key, value string
}

func (c recoveryPinClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	if n, ok := object.(*corev1.Node); ok && n.Labels[c.key] != c.value {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, n.Name, fmt.Errorf("provider pins %s", c.key))
	}
	return c.Client.Update(ctx, object, opts...)
}
func TestRecoveryPreservesStaleKeysAndProviderPinnedOnDemandEvidence(t *testing.T) {
	for _, key := range []string{"brewlet.sh/runtime.retired", "brewlet.sh/runtime.Bad", "brewlet.sh/jdk.stale", "brewlet.sh/runtime.static"} {
		t.Run(key, func(t *testing.T) {
			mode := "Eager"
			value := "ready"
			if key == "brewlet.sh/runtime.static" {
				mode = "OnDemand"
			}
			w, inventory := recoveryFixture(t, map[string]string{"static": mode}, true)
			inventory["static"] = setRecoveryCurrent(t, w, "static", mode, 2, "sha256:"+strings.Repeat("c", 64))
			ctx := context.Background()
			n := new(corev1.Node)
			if err := w.Client.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			n.Labels[key] = value
			if err := w.Client.Update(ctx, n); err != nil {
				t.Fatal(err)
			}
			base := w.Client
			w.Client = recoveryPinClient{base, key, value}
			w.checkedMutable = map[string]bool{"static": true}
			if err := w.Reconcile(ctx); err == nil {
				t.Fatal("stale/pinned capability allowed recovery")
			}
			if err := base.Get(ctx, client.ObjectKey{Name: "node"}, n); err != nil {
				t.Fatal(err)
			}
			if !control.StartupFenced(n) || n.Labels[key] != value {
				t.Fatal("fence or pool-label evidence was normalized away")
			}
			// Actual provider correction, not an attempted worker deletion.
			w.Client = base
			if key != "brewlet.sh/runtime.static" {
				delete(n.Labels, key)
				if err := base.Update(ctx, n); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.publish(ctx, inventory); err != nil {
				t.Fatal("corrected pool could not recover", err)
			}
		})
	}
}
