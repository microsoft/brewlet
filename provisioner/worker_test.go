package provisioner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type baselineFailure struct{}
type fixturePlatforms struct{}

type countedPlatforms struct {
	calls int
	err   error
	after func()
}

func (v *countedPlatforms) ValidatePlatforms(context.Context, *api.RuntimeImage, []ocispec.Platform) error {
	v.calls++
	if v.after != nil {
		v.after()
	}
	return v.err
}

func (fixturePlatforms) ValidatePlatforms(context.Context, *api.RuntimeImage, []ocispec.Platform) error {
	return nil
}

func (baselineFailure) Validate(context.Context) error { return errors.New("handler not configured") }
func authorityFixture(t *testing.T) (*Worker, string) {
	t.Helper()
	root, err := os.MkdirTemp(".", ".worker-test-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	spec := api.NodeProfileSpec{RuntimeImages: []api.RuntimeEntry{{ID: "static", Install: "OnDemand"}}}
	target := api.Target{Name: "node", UID: "node-one", Claimed: true, ContainerdRestart: "none"}
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "profile", UID: "profile-one", Generation: 1}, Spec: spec, Status: api.NodeProfileStatus{OwnershipInitialized: true, ProvisioningGeneration: 1, ProvisioningSpec: &spec, Targets: []api.Target{target}}}
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-one", Labels: map[string]string{api.OwnerUID: "profile-one", api.OwnerNodeUID: "node-one"}, Annotations: map[string]string{api.OwnerName: "profile"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: goruntime.GOARCH}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p, n).Build()
	a := control.Authority{NodeName: "node", NodeUID: "node-one", ProfileName: "profile", ProfileUID: "profile-one", Generation: 1, TargetUIDs: []types.UID{"node-one"}, Mode: "provisioning"}
	return &Worker{Client: c, Reader: c, Authority: a, Store: node.NewStore(filepath.Join(root, "runtimes")), Baseline: baselineFailure{}, Platforms: fixturePlatforms{}}, root
}
func TestInitialFenceDoesNotMutateHost(t *testing.T) {
	w, root := authorityFixture(t)
	w.Authority.NodeUID = "replacement"
	if err := w.Reconcile(context.Background()); err == nil {
		t.Fatal("accepted replaced node")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("failed initial fence mutated host")
	}
}
func TestBaselineFailureKeepsHostUntouched(t *testing.T) {
	for _, policy := range []string{"validated", "none"} {
		t.Run(policy, func(t *testing.T) {
			w, root := authorityFixture(t)
			ctx := context.Background()
			p := new(api.NodeProfile)
			if err := w.Client.Get(ctx, types.NamespacedName{Name: w.Authority.ProfileName}, p); err != nil {
				t.Fatal(err)
			}
			p.Spec.Rollout.ContainerdRestart = policy
			snapshot := p.Spec
			p.Status.ProvisioningSpec = &snapshot
			p.Status.Targets[0].ContainerdRestart = policy
			if err := w.Client.Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err := w.Reconcile(ctx); err == nil || !strings.Contains(err.Error(), "handler not configured") {
				t.Fatalf("required baseline validation was bypassed: %v", err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatal("baseline failure published inventory")
			}
		})
	}
}
func TestExpiredInventoryApprovalIsNotAuthority(t *testing.T) {
	w, _ := authorityFixture(t)
	r := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "static", UID: "epoch-one", Generation: 2}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: "registry.example.com/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, User: api.User{UID: 1000, GID: 1000}}, Status: api.RuntimeImageStatus{Generation: 1, Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	if err := w.Client.Create(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if _, err := w.runtime(context.Background(), "static"); err == nil {
		t.Fatal("stale approval accepted")
	}
}

func TestRuntimePlatformProofCacheInvalidatesChangedTargetsAndPools(t *testing.T) {
	for _, change := range []string{"digest", "inventory-uid", "offered-platforms"} {
		t.Run(change, func(t *testing.T) {
			w, _ := authorityFixture(t)
			validator := new(countedPlatforms)
			w.Platforms = validator
			r := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "static", UID: "epoch-one"}, Status: api.RuntimeImageStatus{Digest: "sha256:" + strings.Repeat("a", 64)}}
			ctx := context.Background()
			for range 2 {
				if err := w.validateRuntimePlatforms(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			if validator.calls != 1 {
				t.Fatal("unchanged target revalidated", validator.calls)
			}
			switch change {
			case "digest":
				r.Status.Digest = "sha256:" + strings.Repeat("b", 64)
			case "inventory-uid":
				r.UID = "epoch-two"
			case "offered-platforms":
				arch := "amd64"
				if goruntime.GOARCH == arch {
					arch = "arm64"
				}
				n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "other-node"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: arch}}}
				if err := w.Client.Create(ctx, n); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := w.validateRuntimePlatforms(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			if validator.calls != 2 || len(w.platformProofs) != 1 {
				t.Fatal("changed target did not replace its cached proof", validator.calls, w.platformProofs)
			}
		})
	}
}

func TestRuntimePlatformProofCacheDoesNotCacheFailuresOrBypassFencing(t *testing.T) {
	w, _ := authorityFixture(t)
	ctx := context.Background()
	r := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "static", UID: "epoch-one"}, Status: api.RuntimeImageStatus{Digest: "sha256:" + strings.Repeat("a", 64)}}
	validator := &countedPlatforms{err: errors.New("registry unavailable")}
	w.Platforms = validator
	for range 2 {
		if err := w.validateRuntimePlatforms(ctx, r); err == nil {
			t.Fatal("failed metadata validation accepted")
		}
	}
	validator.err = nil
	validator.after = func() { w.Authority.NodeUID = "replacement" }
	if err := w.validateRuntimePlatforms(ctx, r); err == nil || len(w.platformProofs) != 0 {
		t.Fatal("unfenced validation cached", err, w.platformProofs)
	}
	w.Authority.NodeUID = "node-one"
	validator.after = nil
	if err := w.validateRuntimePlatforms(ctx, r); err != nil {
		t.Fatal(err)
	}
	if validator.calls != 4 {
		t.Fatal("failed validation was not retried", validator.calls)
	}
	w.Authority.NodeUID = "replacement"
	if err := w.validateRuntimePlatforms(ctx, r); err == nil {
		t.Fatal("cached proof bypassed fresh ownership fence")
	}
}
