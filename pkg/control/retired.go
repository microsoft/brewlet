package control

import (
	"context"
	"encoding/json"
	"fmt"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type RetiredIdentity struct {
	ProfileName  string
	ProfileUID   types.UID
	Target       api.Target
	EvidenceName string
	EvidenceUID  types.UID
	ActiveLedger bool
}

func IdentityMatches(t api.Target, n *corev1.Node) bool {
	return t.UID == n.UID || t.ProviderID != "" && t.ProviderID == n.Spec.ProviderID || t.SystemUUID != "" && t.SystemUUID == n.Status.NodeInfo.SystemUUID
}

// WithdrawRetiredIdentity removes scheduling eligibility only, including from
// unowned or foreign-claimed registrations of a retired physical host. It never
// modifies ownership or confers host cleanup authority.
func WithdrawRetiredIdentity(ctx context.Context, c client.Client, r client.Reader, name string, uid types.UID, identity RetiredIdentity, keys []string) (bool, error) {
	check := func(ctx context.Context, r client.Reader, a Authority) (*Snapshot, error) {
		proven := false
		if identity.EvidenceUID != "" {
			e := new(api.NodeRetirementEvidence)
			if err := r.Get(ctx, client.ObjectKey{Name: identity.EvidenceName}, e); err != nil {
				return nil, err
			}
			proven = e.UID == identity.EvidenceUID && e.Spec.ProfileUID == identity.ProfileUID && e.Spec.NodeUID == identity.Target.UID && e.Status.AcceptedAt != nil && e.Status.OriginalTarget != nil && e.Status.OriginalTarget.UID == identity.Target.UID && e.Status.OriginalTarget.ProviderID == identity.Target.ProviderID && e.Status.OriginalTarget.SystemUUID == identity.Target.SystemUUID && len(e.OwnerReferences) == 0
		} else {
			p := new(api.NodeProfile)
			if err := r.Get(ctx, client.ObjectKey{Name: identity.ProfileName}, p); err != nil {
				return nil, err
			}
			if p.UID == identity.ProfileUID && !identity.ActiveLedger {
				for _, d := range p.Status.DetachedRetirements {
					for _, t := range d.Targets {
						if t.UID == identity.Target.UID && t.ProviderID == identity.Target.ProviderID && t.SystemUUID == identity.Target.SystemUUID {
							proven = true
						}
					}
				}
			}
			if p.UID == identity.ProfileUID && identity.ActiveLedger && p.Status.OwnershipInitialized {
				targets := append([]api.Target{}, p.Status.Targets...)
				if p.Status.Retirement != nil {
					targets = append(targets, p.Status.Retirement.Targets...)
				}
				recorded := false
				for _, t := range targets {
					if t.Claimed && t.UID == identity.Target.UID && t.ProviderID == identity.Target.ProviderID && t.SystemUUID == identity.Target.SystemUUID {
						recorded = true
					}
				}
				if recorded {
					var nodes corev1.NodeList
					if err := r.List(ctx, &nodes); err != nil {
						return nil, err
					}
					originalPresent, alias := false, false
					for _, n := range nodes.Items {
						if n.UID == identity.Target.UID {
							originalPresent = true
						} else if IdentityMatches(identity.Target, &n) {
							alias = true
						}
					}
					proven = !originalPresent || alias
				}
			}
		}
		if !proven {
			return nil, fmt.Errorf("ownership-fence-failed: no durable retired identity proof")
		}
		n := new(corev1.Node)
		if err := r.Get(ctx, client.ObjectKey{Name: a.NodeName}, n); err != nil {
			return nil, err
		}
		if n.UID != a.NodeUID || !IdentityMatches(identity.Target, n) {
			return nil, fmt.Errorf("ownership-fence-failed: returning Node identity changed")
		}
		return &Snapshot{Node: n}, nil
	}
	return withdraw(ctx, c, r, Authority{NodeName: name, NodeUID: uid}, keys, check)
}

// WorkersMayAuthorize is deliberately global and read-only. A legacy/malformed
// target set is unbounded authority, not evidence that the old host is excluded.
func WorkersMayAuthorize(ctx context.Context, r client.Reader, profileUID, nodeUID types.UID) (bool, error) {
	var daemonsets appsv1.DaemonSetList
	if err := r.List(ctx, &daemonsets); err != nil {
		return false, err
	}
	pods := []corev1.Pod{}
	for _, d := range daemonsets.Items {
		pods = append(pods, corev1.Pod{ObjectMeta: d.Spec.Template.ObjectMeta, Spec: d.Spec.Template.Spec})
	}
	var live corev1.PodList
	if err := r.List(ctx, &live); err != nil {
		return false, err
	}
	pods = append(pods, live.Items...)
	for _, p := range pods {
		for _, c := range append(append([]corev1.Container{}, p.Spec.Containers...), p.Spec.InitContainers...) {
			profile, targets := "", ""
			known := p.Labels[api.OwnerUID] == string(profileUID)
			for _, arg := range c.Command {
				if arg == "/brewlet-node" {
					known = true
				}
			}
			for _, e := range c.Env {
				if e.Name == "BREWLET_PROFILE_UID" {
					profile = e.Value
					known = true
				}
				if e.Name == "BREWLET_TARGET_UIDS" {
					targets = e.Value
				}
			}
			if !known {
				continue
			}
			if profile == "" {
				return true, nil
			}
			if profile != string(profileUID) {
				continue
			}
			var ids []types.UID
			if targets == "" || json.Unmarshal([]byte(targets), &ids) != nil || len(ids) == 0 {
				return true, nil
			}
			for _, id := range ids {
				if id == nodeUID {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
