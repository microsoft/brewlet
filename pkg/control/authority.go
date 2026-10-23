// Package control implements UID-bound control-plane fences for node workers.
// All reads used for authority must use an uncached APIReader.
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Authority struct {
	NodeName    string
	NodeUID     types.UID
	ProfileName string
	ProfileUID  types.UID
	Generation  int64
	TargetUIDs  []types.UID
	Mode        string
}
type Snapshot struct {
	Node    *corev1.Node
	Profile *api.NodeProfile
	Spec    api.NodeProfileSpec
	Target  api.Target
}

func ownership(n *corev1.Node, a Authority) error {
	if a.NodeUID == "" || a.ProfileUID == "" || n.UID != a.NodeUID || n.Labels[api.OwnerUID] != string(a.ProfileUID) || n.Labels[api.OwnerNodeUID] != string(a.NodeUID) || n.Annotations[api.OwnerName] != a.ProfileName {
		return fmt.Errorf("ownership-fence-failed: Node UID or claim mismatch")
	}
	return nil
}
func Check(ctx context.Context, r client.Reader, a Authority) (*Snapshot, error) {
	fail := func() (*Snapshot, error) {
		return nil, fmt.Errorf("ownership-fence-failed: missing durable worker authority")
	}
	if a.ProfileUID == "" || a.NodeUID == "" || a.Generation <= 0 || !slices.Contains(a.TargetUIDs, a.NodeUID) {
		return fail()
	}
	p := new(api.NodeProfile)
	if err := r.Get(ctx, client.ObjectKey{Name: a.ProfileName}, p); err != nil {
		return nil, err
	}
	n := new(corev1.Node)
	if err := r.Get(ctx, client.ObjectKey{Name: a.NodeName}, n); err != nil {
		return nil, err
	}
	if p.UID != a.ProfileUID || !p.Status.OwnershipInitialized {
		return fail()
	}
	if err := ownership(n, a); err != nil {
		return nil, err
	}
	targets := p.Status.Targets
	var spec api.NodeProfileSpec
	switch a.Mode {
	case "provisioning":
		if !p.DeletionTimestamp.IsZero() || p.Status.ProvisioningGeneration != a.Generation || p.Generation != a.Generation || p.Status.ProvisioningSpec == nil {
			return fail()
		}
		for _, d := range p.Status.DetachedRetirements {
			for _, t := range d.Targets {
				if t.UID == a.NodeUID {
					return fail()
				}
			}
		}
		if p.Status.Retirement != nil {
			for _, t := range p.Status.Retirement.Targets {
				if t.UID == a.NodeUID {
					return fail()
				}
			}
		}
		spec = *p.Status.ProvisioningSpec
		if !reflect.DeepEqual(spec, p.Spec) {
			return fail()
		}
	case "cleanup":
		d := p.Status.Retirement
		if d == nil || d.Phase != "Cleaning" || d.Generation != a.Generation {
			return fail()
		}
		targets = d.Targets
		spec = d.Spec
	default:
		return fail()
	}
	if spec.Rollout.RestartPolicy() != "validated" && spec.Rollout.RestartPolicy() != "none" {
		return fail()
	}
	for _, t := range targets {
		if t.Name == n.Name && t.UID == n.UID && t.Claimed && t.RetirementEvidenceName == "" && t.RetirementEvidenceUID == "" {
			if t.ProviderID != "" && t.ProviderID != n.Spec.ProviderID || t.SystemUUID != "" && t.SystemUUID != n.Status.NodeInfo.SystemUUID {
				return nil, fmt.Errorf("ownership-fence-failed: physical host identity differs from frozen target")
			}
			if t.ContainerdRestart != "validated" && t.ContainerdRestart != "none" {
				return fail()
			}
			return &Snapshot{Node: n, Profile: p, Spec: spec, Target: t}, nil
		}
	}
	return fail()
}

func StartupFenced(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == api.StartupTaint && t.Value == "provisioning" && t.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func HasStartupTaint(n *corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Key == api.StartupTaint && t.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

// Withdraw removes scheduling eligibility. If admission pins labels, only a
// successful write followed by an independent readback proves the fallback.
// A conflict/replacement never enters the fallback path with stale authority.
func Withdraw(ctx context.Context, c client.Client, r client.Reader, a Authority, keys []string) (bool, error) {
	return withdraw(ctx, c, r, a, keys, Check)
}

// WithdrawAdvertisement is the controller's scheduling-only revocation path.
// Unlike host mutation authority, revocation must remain possible after a spec
// edit or deletion invalidates provisioning. It still requires a durable,
// claimed UID-bound target and never grants a worker host-write permission.
func WithdrawAdvertisement(ctx context.Context, c client.Client, r client.Reader, a Authority, keys []string) (bool, error) {
	return withdraw(ctx, c, r, a, keys, claimSnapshot)
}
func claimSnapshot(ctx context.Context, r client.Reader, a Authority) (*Snapshot, error) {
	p := new(api.NodeProfile)
	if err := r.Get(ctx, client.ObjectKey{Name: a.ProfileName}, p); err != nil {
		return nil, err
	}
	if p.UID != a.ProfileUID || !p.Status.OwnershipInitialized {
		return nil, fmt.Errorf("ownership-fence-failed: missing durable ownership")
	}
	n := new(corev1.Node)
	if err := r.Get(ctx, client.ObjectKey{Name: a.NodeName}, n); err != nil {
		return nil, err
	}
	if err := ownership(n, a); err != nil {
		return nil, err
	}
	targets := append([]api.Target{}, p.Status.Targets...)
	if p.Status.Retirement != nil {
		targets = append(targets, p.Status.Retirement.Targets...)
	}
	for _, t := range targets {
		if t.Name == a.NodeName && t.UID == a.NodeUID && t.Claimed && t.RetirementEvidenceName == "" && t.RetirementEvidenceUID == "" {
			return &Snapshot{Node: n, Profile: p, Target: t}, nil
		}
	}
	return nil, fmt.Errorf("ownership-fence-failed: target not claimed in durable ledger")
}

type checkFunc func(context.Context, client.Reader, Authority) (*Snapshot, error)

func withdraw(ctx context.Context, c client.Client, r client.Reader, a Authority, keys []string, check checkFunc) (bool, error) {
	for _, k := range keys {
		if k != api.RuntimeLabel && !strings.HasPrefix(k, "brewlet.sh/runtime.") {
			return false, fmt.Errorf("refusing withdrawal of non-capability key %s", k)
		}
	}
	s, err := check(ctx, r, a)
	if err != nil {
		return false, err
	}
	n := s.Node.DeepCopy()
	for _, k := range keys {
		delete(n.Labels, k)
	}
	err = c.Update(ctx, n)
	if err == nil {
		fresh, err := check(ctx, r, a)
		if err != nil {
			return false, err
		}
		for _, k := range keys {
			if _, ok := fresh.Node.Labels[k]; ok {
				return false, fmt.Errorf("node-advertisement-failed: withdrawal not observed")
			}
		}
		return false, nil
	}
	first := err
	// Re-read even for provider refusals: concurrent profile/node replacement
	// must invalidate the old writer before a taint is attempted.
	s, err = check(ctx, r, a)
	if err != nil {
		return false, err
	}
	if s.Node.ResourceVersion != n.ResourceVersion {
		return false, fmt.Errorf("node-advertisement-failed: concurrent Node mutation after withdrawal: %w", first)
	}
	if !apierrors.IsForbidden(first) && !apierrors.IsInvalid(first) {
		return false, fmt.Errorf("node-advertisement-failed: withdrawal not confirmed; refusing fallback for non-provider error: %w", first)
	}
	n = s.Node.DeepCopy()
	if !StartupFenced(n) {
		replaced := false
		for i := range n.Spec.Taints {
			if n.Spec.Taints[i].Key == api.StartupTaint && n.Spec.Taints[i].Effect == corev1.TaintEffectNoSchedule {
				n.Spec.Taints[i].Value = "provisioning"
				replaced = true
			}
		}
		if !replaced {
			n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule})
		}
	}
	if err = c.Update(ctx, n); err != nil {
		return false, fmt.Errorf("node-advertisement-failed: withdrawal: %v; fallback: %w", first, err)
	}
	s, err = check(ctx, r, a)
	if err != nil {
		return false, err
	}
	if !StartupFenced(s.Node) {
		return false, fmt.Errorf("node-advertisement-failed: startup fence readback failed")
	}
	return true, nil
}

// ReleaseClaim uses a resourceVersion CAS and requires cleanup's durable
// Teardown checkpoint and complete worker teardown. A failed CAS must be
// retried by calling this function again, not by reusing the old Node.
func ReleaseClaim(ctx context.Context, c client.Client, r client.Reader, a Authority) error {
	p := new(api.NodeProfile)
	if err := r.Get(ctx, client.ObjectKey{Name: a.ProfileName}, p); err != nil {
		return err
	}
	if p.UID != a.ProfileUID || p.Status.Retirement == nil || p.Status.Retirement.Phase != "Teardown" || p.Status.Retirement.Generation != a.Generation {
		return fmt.Errorf("ownership-fence-failed: no durable teardown")
	}
	recorded := false
	for _, t := range p.Status.Retirement.Targets {
		if t.UID == a.NodeUID && t.Name == a.NodeName {
			recorded = true
		}
	}
	if !recorded {
		return fmt.Errorf("ownership-fence-failed: unrecorded target")
	}
	var pods corev1.PodList
	var daemonsets appsv1.DaemonSetList
	if err := r.List(ctx, &daemonsets); err != nil {
		return err
	}
	for _, d := range daemonsets.Items {
		for _, c := range append(append([]corev1.Container{}, d.Spec.Template.Spec.Containers...), d.Spec.Template.Spec.InitContainers...) {
			for _, e := range c.Env {
				if e.Name == "BREWLET_PROFILE_UID" && e.Value == string(a.ProfileUID) {
					return fmt.Errorf("cleanup DaemonSet remains")
				}
			}
		}
	}
	if err := r.List(ctx, &pods); err != nil {
		return err
	}
	for _, p := range pods.Items {
		for _, c := range append(append([]corev1.Container{}, p.Spec.Containers...), p.Spec.InitContainers...) {
			for _, e := range c.Env {
				if e.Name == "BREWLET_PROFILE_UID" && e.Value == string(a.ProfileUID) {
					return fmt.Errorf("cleanup workers remain")
				}
			}
		}
	}
	n := new(corev1.Node)
	if err := r.Get(ctx, client.ObjectKey{Name: a.NodeName}, n); err != nil {
		return err
	}
	if err := ownership(n, a); err != nil {
		return err
	}
	for k := range n.Labels {
		if k == api.RuntimeLabel || len(k) > len("brewlet.sh/runtime.") && k[:len("brewlet.sh/runtime.")] == "brewlet.sh/runtime." {
			return fmt.Errorf("node-advertisement-failed: runtime labels remain; preserve startup fence and claim")
		}
	}
	if raw := n.Annotations[api.InventoryAnnotation]; raw != "" && raw != "{}" {
		return fmt.Errorf("runtime inventory remains")
	}
	delete(n.Labels, api.OwnerUID)
	delete(n.Labels, api.OwnerNodeUID)
	delete(n.Annotations, api.OwnerName)
	delete(n.Annotations, "brewlet.sh/provision-state")
	return c.Update(ctx, n)
}

func ParseInventory(n *corev1.Node) (map[string]api.NodeRuntime, error) {
	v := map[string]api.NodeRuntime{}
	if n.Annotations[api.InventoryAnnotation] == "" {
		return v, nil
	}
	if err := json.Unmarshal([]byte(n.Annotations[api.InventoryAnnotation]), &v); err != nil {
		return nil, err
	}
	return v, nil
}
