package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestProfileWorkerPreservesExplicitPlacementAndRolloutPolicy(t *testing.T) {
	r, p, _ := profileFixture(t)
	ctx := context.Background()
	p.Spec.Tolerations = []corev1.Toleration{
		{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	p.Spec.Registry.Mirrors = map[string]string{"registry.example.com": "mirror.internal/base"}
	p.Spec.Rollout.MaxUnavailable = 2
	r.AllowedMirrorHosts = []string{"mirror.internal"}
	if err := r.Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	advance(t, r, p, 6)
	ds := new(appsv1.DaemonSet)
	if err := r.Get(ctx, client.ObjectKey{Name: "brewlet-node-provisioner-web", Namespace: r.Namespace}, ds); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ds.Spec.Template.Spec.Tolerations, p.Spec.Tolerations) {
		t.Fatalf("worker changed explicit tolerations: %+v", ds.Spec.Template.Spec.Tolerations)
	}
	var spec api.NodeProfileSpec
	if err := json.Unmarshal([]byte(workerEnv(ds, "BREWLET_PROFILE_SPEC")), &spec); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(spec, p.Spec) || workerEnv(ds, "SOURCE_ALLOWED_MIRROR_HOSTS") != `["mirror.internal"]` {
		t.Fatal("worker lost validated mirror, placement or rollout policy")
	}
	if ds.Spec.UpdateStrategy.RollingUpdate == nil || ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable == nil || ds.Spec.UpdateStrategy.RollingUpdate.MaxUnavailable.IntValue() != 2 {
		t.Fatal("worker lost explicit rollout bound")
	}
}

func TestInvalidMirrorPolicyCannotGrantClaimsOrCreateWorkers(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			r, p, n := profileFixture(t)
			ctx := context.Background()
			r.AllowedMirrorHosts = []string{"registry.example.com"}
			p.Spec.Registry.Mirrors = map[string]string{"registry.example.com": "registry.example.com/base"}
			if duplicate {
				p.Spec.Registry.Mirrors = nil
				r.AllowedMirrorHosts = append(r.AllowedMirrorHosts, r.AllowedMirrorHosts[0])
			}
			if err := r.Update(ctx, p); err != nil {
				t.Fatal(err)
			}
			advance(t, r, p, 3)
			if err := r.Get(ctx, client.ObjectKeyFromObject(p), p); err != nil {
				t.Fatal(err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
				t.Fatal(err)
			}
			condition := meta.FindStatusCondition(p.Status.Conditions, "Ready")
			if condition == nil || condition.Status != metav1.ConditionFalse || len(p.Status.Targets) != 0 || n.Labels[api.OwnerUID] != "" {
				t.Fatal("invalid mirror policy gained provisioning authority")
			}
			err := r.Get(ctx, client.ObjectKey{Name: "brewlet-node-provisioner-web", Namespace: r.Namespace}, new(appsv1.DaemonSet))
			if !apierrors.IsNotFound(err) {
				t.Fatalf("invalid mirror policy created a worker: %v", err)
			}
		})
	}
}

func TestEmptyPoolMetadataFinalizerPatchDoesNotRewriteSpec(t *testing.T) {
	r, p, n := profileFixture(t)
	ctx := context.Background()
	_ = r.Delete(ctx, n)
	source := "registry.example.com/python@sha256:" + strings.Repeat("a", 64)
	im := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12", UID: "image", Generation: 1}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: source}, User: api.User{UID: 1000, GID: 1000}}, Status: api.RuntimeImageStatus{Generation: 1, Digest: "sha256:" + strings.Repeat("a", 64), SourceImage: source, Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	_ = r.Create(ctx, im)
	metadataPatches := 0
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, p client.Patch, opts ...client.PatchOption) error {
		if _, ok := obj.(*api.NodeProfile); ok {
			data, err := p.Data(obj)
			if err != nil {
				return err
			}
			var fields map[string]any
			_ = json.Unmarshal(data, &fields)
			if _, has := fields["spec"]; has {
				t.Fatal("finalizer metadata patch serialized/defaulted spec")
			}
			metadataPatches++
		}
		return c.Patch(ctx, obj, p, opts...)
	}})
	advance(t, r, p, 3)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	c := meta.FindStatusCondition(p.Status.Conditions, "Ready")
	if metadataPatches != 1 || c == nil || c.Reason != "EmptyPool" || p.Generation != 1 {
		t.Fatalf("empty-pool/finalizer behavior wrong: patches=%d generation=%d condition=%+v", metadataPatches, p.Generation, c)
	}
}
func TestReturningPhysicalHostIsQuarantinedBeforeMissingLedgerDetaches(t *testing.T) {
	r, p, n := profileFixture(t)
	ctx := context.Background()
	if err := r.Get(ctx, client.ObjectKeyFromObject(n), n); err != nil {
		t.Fatal(err)
	}
	n.Spec.ProviderID = "cloud://same-physical-host"
	n.Status.NodeInfo.SystemUUID = "physical-system"
	if err := r.Update(ctx, n); err != nil {
		t.Fatal(err)
	}
	advance(t, r, p, 5)
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if !p.Status.Targets[0].Claimed {
		t.Fatal("fixture not durably claimed")
	}
	_ = r.Delete(ctx, n)
	returning := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "new-registration", UID: "replacement", Labels: map[string]string{"agentpool": "batch", api.RuntimeLabel: "ready", "brewlet.sh/runtime.python-3.12": "ready"}}, Spec: corev1.NodeSpec{ProviderID: "cloud://same-physical-host"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{SystemUUID: "physical-system"}}}
	_ = r.Create(ctx, returning)
	other := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "batch", UID: "other-profile", Generation: 1}, Spec: p.Spec}
	other.Spec.NodePool.Names = []string{"batch"}
	_ = r.Create(ctx, other)
	advance(t, r, other, 3)
	_ = r.Get(ctx, client.ObjectKeyFromObject(other), other)
	_ = r.Get(ctx, client.ObjectKeyFromObject(returning), returning)
	if len(other.Status.Targets) != 0 || returning.Labels[api.OwnerUID] != "" || returning.Labels[api.RuntimeLabel] != "" {
		t.Fatalf("returning physical host was adopted in copy-before-removal window: target=%+v labels=%v original=%+v", other.Status.Targets, returning.Labels, p.Status.Targets)
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(p), p)
	if len(p.Status.Targets) != 1 || len(p.Status.DetachedRetirements) != 0 {
		t.Fatal("test accidentally relied on already-detached history")
	}
}
