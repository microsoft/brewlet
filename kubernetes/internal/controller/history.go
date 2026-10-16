package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/pkg/control"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *ProfileReconciler) quarantineHistory(ctx context.Context, profiles []api.NodeProfile, nodes []corev1.Node) (map[types.UID]bool, error) {
	identities := []control.RetiredIdentity{}
	for _, p := range profiles {
		active := append([]api.Target{}, p.Status.Targets...)
		if p.Status.Retirement != nil {
			active = append(active, p.Status.Retirement.Targets...)
		}
		for _, t := range active {
			if t.UID == "" || p.UID == "" {
				return nil, fmt.Errorf("unbounded missing ownership identity; preserve all new ownership")
			}
			present, alias := false, false
			for _, n := range nodes {
				if n.UID == t.UID {
					present = true
				} else if control.IdentityMatches(t, &n) {
					alias = true
				}
			}
			if !present || alias {
				identities = append(identities, control.RetiredIdentity{ProfileName: p.Name, ProfileUID: p.UID, Target: t, ActiveLedger: true})
			}
		}
		for _, d := range p.Status.DetachedRetirements {
			for _, t := range d.Targets {
				if t.UID == "" || p.UID == "" {
					return nil, fmt.Errorf("unbounded detached host identity; new ownership remains blocked")
				}
				identities = append(identities, control.RetiredIdentity{ProfileName: p.Name, ProfileUID: p.UID, Target: t})
			}
		}
	}
	var evidence api.NodeRetirementEvidenceList
	if err := r.Reader.List(ctx, &evidence); err != nil {
		return nil, err
	}
	for _, e := range evidence.Items {
		if e.Status.AcceptedAt != nil {
			t := api.Target{Name: e.Spec.NodeName, UID: e.Spec.NodeUID, ProviderID: e.Spec.ProviderID, SystemUUID: e.Spec.SystemUUID}
			if e.Status.OriginalTarget != nil {
				t = *e.Status.OriginalTarget
			}
			identities = append(identities, control.RetiredIdentity{ProfileName: e.Spec.ProfileName, ProfileUID: e.Spec.ProfileUID, Target: t, EvidenceName: e.Name, EvidenceUID: e.UID})
		}
	}
	blocked := map[types.UID]bool{}
	failures := []string{}
	for _, n := range nodes {
		for _, id := range identities {
			if control.IdentityMatches(id.Target, &n) {
				blocked[n.UID] = true
				if _, err := control.WithdrawRetiredIdentity(ctx, r.Client, r.Reader, n.Name, n.UID, id, capabilityKeys([]corev1.Node{n}, api.Target{Name: n.Name, UID: n.UID})); err != nil {
					failures = append(failures, n.Name+": "+err.Error())
				}
				break
			}
		}
	}
	if len(failures) > 0 {
		return blocked, fmt.Errorf("retired identity quarantine not confirmed: %s", strings.Join(failures, "; "))
	}
	return blocked, nil
}

func (r *ProfileReconciler) resolveDetached(ctx context.Context, p *api.NodeProfile) (bool, error) {
	for i, d := range p.Status.DetachedRetirements {
		if len(d.Targets) != 1 || d.Phase != "Missing" {
			return false, fmt.Errorf("malformed detached obligation; preserving history")
		}
		t := d.Targets[0]
		if t.RetirementEvidenceUID == "" && t.RetirementEvidenceName == "" {
			continue
		}
		e := new(api.NodeRetirementEvidence)
		if err := r.Reader.Get(ctx, client.ObjectKey{Name: t.RetirementEvidenceName}, e); err != nil {
			return false, err
		}
		if e.UID != t.RetirementEvidenceUID || e.Status.AcceptedAt == nil || e.Status.OriginalTarget == nil || e.Status.CleanupSpec == nil || e.Status.CleanupGeneration != d.Generation || !reflect.DeepEqual(*e.Status.CleanupSpec, d.Spec) {
			return false, fmt.Errorf("foreign/recreated or incomplete immutable evidence receipt")
		}
		if err := evidenceMatches(e, p, &d, t); err != nil {
			return false, err
		}
		if e.Status.ResolvedAt == nil {
			continue
		}
		if err := inventoryTargetsResolved(ctx, r.Reader, p, d.Targets); err != nil {
			return false, err
		}
		if busy, err := control.WorkersMayAuthorize(ctx, r.Reader, p.UID, t.UID); err != nil || busy {
			if err == nil {
				err = fmt.Errorf("worker still authorizes detached Node UID")
			}
			return false, err
		}
		p.Status.DetachedRetirements = append(p.Status.DetachedRetirements[:i], p.Status.DetachedRetirements[i+1:]...)
		p.Status.ExternalRetirementResolved = true
		return true, nil
	}
	return false, nil
}
