package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/control"
	noderuntime "github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func offers(spec api.NodeProfileSpec, id string) bool {
	for _, e := range spec.RuntimeImages {
		if e.ID == id {
			return true
		}
	}
	return false
}
func (r *RuntimeReconciler) collectRevocationTargets(ctx context.Context, im *api.RuntimeImage) (bool, error) {
	var profiles api.NodeProfileList
	if err := r.Reader.List(ctx, &profiles); err != nil {
		return false, err
	}
	changed := false
	add := func(p *api.NodeProfile, targets []api.Target, spec api.NodeProfileSpec, g int64, active bool) {
		if !offers(spec, im.Name) {
			return
		}
		for _, t := range targets {
			if !t.Claimed {
				continue
			}
			idx := slices.IndexFunc(im.Status.RevocationTargets, func(old api.InventoryTarget) bool { return old.NodeUID == t.UID && old.ProfileUID == p.UID })
			if idx < 0 {
				im.Status.RevocationTargets = append(im.Status.RevocationTargets, api.InventoryTarget{NodeName: t.Name, NodeUID: t.UID, ProfileName: p.Name, ProfileUID: p.UID, ProfileGeneration: g, ProviderID: t.ProviderID, SystemUUID: t.SystemUUID, Generation: im.Status.Generation, Digest: im.Status.Digest})
				changed = true
			} else if active {
				old := &im.Status.RevocationTargets[idx]
				retiring := p.Status.Retirement != nil && slices.ContainsFunc(p.Status.Retirement.Targets, func(x api.Target) bool { return x.UID == t.UID })
				if !retiring && (old.Resolved || old.ProfileGeneration != g || old.Generation != im.Status.Generation || old.Digest != im.Status.Digest) {
					old.Resolved = false
					old.Resolution = ""
					old.EvidenceName = ""
					old.EvidenceUID = ""
					old.ProfileGeneration = g
					old.Generation = im.Status.Generation
					old.Digest = im.Status.Digest
					changed = true
				}
			}
		}
	}
	for i := range profiles.Items {
		p := &profiles.Items[i]
		if p.Status.ProvisioningSpec != nil {
			add(p, p.Status.Targets, *p.Status.ProvisioningSpec, p.Status.ProvisioningGeneration, false)
		}
		active := policy.Approved(im) && p.DeletionTimestamp.IsZero() && p.Generation == p.Status.ProvisioningGeneration && policy.Profile(p, profiles.Items, r.AllowedMirrorHosts) == nil
		add(p, p.Status.Targets, p.Spec, p.Status.ProvisioningGeneration, active)
		if p.Status.Retirement != nil {
			add(p, p.Status.Retirement.Targets, p.Status.Retirement.Spec, p.Status.Retirement.Generation, false)
		}
		for _, d := range p.Status.DetachedRetirements {
			add(p, d.Targets, d.Spec, d.Generation, false)
		}
	}
	var evidence api.NodeRetirementEvidenceList
	if err := r.Reader.List(ctx, &evidence); err != nil {
		return changed, err
	}
	for _, e := range evidence.Items {
		if e.Status.AcceptedAt != nil && e.Status.OriginalTarget != nil && e.Status.CleanupSpec != nil {
			p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: e.Spec.ProfileName, UID: e.Spec.ProfileUID}}
			add(p, []api.Target{*e.Status.OriginalTarget}, *e.Status.CleanupSpec, e.Status.CleanupGeneration, false)
		}
	}
	var nodes corev1.NodeList
	if err := r.Reader.List(ctx, &nodes); err != nil {
		return changed, err
	}
	for _, n := range nodes.Items {
		inv, err := control.ParseInventory(&n)
		if err != nil {
			return changed, err
		}
		_, label := n.Labels["brewlet.sh/runtime."+im.Name]
		var receipts map[string]noderuntime.RevocationReceipt
		if raw := n.Annotations[noderuntime.RevocationsAnnotation]; raw != "" {
			if err := json.Unmarshal([]byte(raw), &receipts); err != nil {
				return changed, err
			}
		}
		receipt := receipts[im.Name]
		if local := inv[im.Name]; local.InventoryUID == im.UID {
			for i := range im.Status.RevocationTargets {
				t := &im.Status.RevocationTargets[i]
				if t.NodeUID == n.UID && t.ProfileUID == types.UID(n.Labels[api.OwnerUID]) && t.ObservedGeneration != local.Generation {
					t.ObservedGeneration = local.Generation
					changed = true
				}
			}
		}
		if receipt.InventoryUID == string(im.UID) && receipt.NodeUID == string(n.UID) && receipt.ProfileUID == n.Labels[api.OwnerUID] {
			// Recovery of pre-ledger in-flight revocation still requires a fresh
			// claimed target. A receipt never grants installation authority.
			for i := range profiles.Items {
				p := &profiles.Items[i]
				if string(p.UID) != receipt.ProfileUID {
					continue
				}
				for _, t := range p.Status.Targets {
					if t.Name == n.Name && t.UID == n.UID && t.Claimed {
						snapshot := api.NodeProfileSpec{RuntimeImages: []api.RuntimeEntry{{ID: im.Name}}}
						add(p, []api.Target{t}, snapshot, receipt.ProfileGeneration, false)
					}
				}
			}
		}
		if !label && inv[im.Name].InventoryUID != im.UID && receipt.InventoryUID != string(im.UID) {
			continue
		}
		if !slices.ContainsFunc(im.Status.RevocationTargets, func(t api.InventoryTarget) bool {
			return t.NodeUID == n.UID && (t.ProfileUID == types.UID(n.Labels[api.OwnerUID]) || t.Resolved && t.ProfileUID == types.UID(receipt.ProfileUID))
		}) {
			return changed, fmt.Errorf("unrecorded runtime advertisement/epoch on Node %s; no cleanup authority can be inferred", n.Name)
		}
	}
	return changed, nil
}
func (r *RuntimeReconciler) revocationCheckpoint(ctx context.Context, im *api.RuntimeImage) error {
	// Capture intent before Update: a server-pruned response may overwrite im.
	// Comparing two already-pruned response objects would conceal lost deadlines.
	expected, _ := json.Marshal(im.Status)
	uid := im.UID
	if err := r.Status().Update(ctx, im); err != nil {
		return err
	}
	fresh := new(api.RuntimeImage)
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(im), fresh); err != nil {
		return err
	}
	observed, _ := json.Marshal(fresh.Status)
	if fresh.UID != uid || !bytes.Equal(expected, observed) {
		return fmt.Errorf("runtime status checkpoint not observed (targets, observations, rotation progress/deadlines); matching CRD required")
	}
	return nil
}
func (r *RuntimeReconciler) externalResolution(ctx context.Context, t *api.InventoryTarget) error {
	var evidence api.NodeRetirementEvidenceList
	if err := r.Reader.List(ctx, &evidence); err != nil {
		return err
	}
	for _, e := range evidence.Items {
		if e.Spec.ProfileUID != t.ProfileUID || e.Spec.NodeUID != t.NodeUID || e.Status.AcceptedAt == nil || e.Status.ResolvedAt == nil || e.Status.OriginalTarget == nil || e.Status.CleanupSpec == nil || !e.Spec.PermanentlyDecommissioned {
			continue
		}
		original := e.Status.OriginalTarget
		if original.ProviderID != t.ProviderID || original.SystemUUID != t.SystemUUID {
			continue
		}
		if busy, err := control.WorkersMayAuthorize(ctx, r.Reader, t.ProfileUID, t.NodeUID); err != nil || busy {
			if err == nil {
				err = fmt.Errorf("writer can still authorize externally retired target")
			}
			return err
		}
		t.Resolved = true
		t.Resolution = "ExternalDisposition"
		t.EvidenceName = e.Name
		t.EvidenceUID = e.UID
		return nil
	}
	return fmt.Errorf("missing/replaced Node %s/%s has no retained resolved external disposition", t.NodeName, t.NodeUID)
}
func (r *RuntimeReconciler) verifyReceipt(ctx context.Context, im *api.RuntimeImage, t *api.InventoryTarget, n *corev1.Node) error {
	if _, present := n.Labels["brewlet.sh/runtime."+im.Name]; present && !control.StartupFenced(n) {
		return fmt.Errorf("runtime capability remains without confirmed startup fence on %s", n.Name)
	}
	p := new(api.NodeProfile)
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: t.ProfileName}, p); err != nil {
		return err
	}
	if p.UID != t.ProfileUID || n.Labels[api.OwnerUID] != string(p.UID) || n.Labels[api.OwnerNodeUID] != string(n.UID) || n.Annotations[api.OwnerName] != p.Name {
		return fmt.Errorf("revocation receipt Node/profile claim no longer matches original target")
	}
	var receipts map[string]noderuntime.RevocationReceipt
	if err := json.Unmarshal([]byte(n.Annotations[noderuntime.RevocationsAnnotation]), &receipts); err != nil {
		return fmt.Errorf("missing/malformed root revocation receipt: %w", err)
	}
	receipt, ok := receipts[im.Name]
	knownGeneration := receipt.Generation == im.Status.Generation && receipt.Digest == im.Status.Digest || receipt.Generation == t.Generation && receipt.Digest == t.Digest
	if !ok || receipt.InventoryUID != string(im.UID) || receipt.NodeUID != string(t.NodeUID) || receipt.ProfileUID != string(t.ProfileUID) || receipt.Generation < t.Generation || !knownGeneration || !receipt.RootsCleaned || receipt.RevokedAt.IsZero() || receipt.AuthorizationExpiresAt.IsZero() {
		return fmt.Errorf("root/auth revocation receipt does not prove complete cleanup for current epoch/target")
	}
	a := control.Authority{NodeName: n.Name, NodeUID: n.UID, ProfileName: p.Name, ProfileUID: p.UID, Generation: receipt.ProfileGeneration, TargetUIDs: []types.UID{n.UID}, Mode: "provisioning"}
	if p.Status.Retirement != nil && slices.ContainsFunc(p.Status.Retirement.Targets, func(old api.Target) bool { return old.UID == n.UID }) {
		a.Mode = "cleanup"
	}
	if _, err := control.Check(ctx, r.Reader, a); err != nil {
		d := p.Status.Retirement
		provenTeardown := d != nil && d.Phase == "Teardown" && d.Generation == receipt.ProfileGeneration && slices.ContainsFunc(d.Targets, func(old api.Target) bool { return old.Name == n.Name && old.UID == n.UID && old.Claimed }) && n.Annotations["brewlet.sh/cleanup-complete"] == fmt.Sprintf("%s/%d", p.UID, d.Generation)
		if !provenTeardown {
			return err
		}
	}

	t.Resolved = true
	t.Resolution = "FencedLocalRevocation"
	return nil
}

// deletionProgress never treats Node absence, an attempted write, or local
// authorization expiry alone as proof that runtime roots have been reclaimed.
func (r *RuntimeReconciler) deletionProgress(ctx context.Context, im *api.RuntimeImage) (bool, error) {
	changed, err := r.collectRevocationTargets(ctx, im)
	if err != nil {
		return false, err
	}
	if changed {
		runtimeCondition(im, "CleanupComplete", "RevocationPending", "UID-bound revocation targets recorded; awaiting local receipts or retained external evidence", metav1.ConditionFalse)
		return false, r.revocationCheckpoint(ctx, im)
	}
	pending := []string{}
	updated := false
	for i := range im.Status.RevocationTargets {
		t := &im.Status.RevocationTargets[i]
		n := new(corev1.Node)
		err := r.Reader.Get(ctx, client.ObjectKey{Name: t.NodeName}, n)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		if apierrors.IsNotFound(err) || n.UID != t.NodeUID {
			if !t.Resolved {
				if err := r.externalResolution(ctx, t); err != nil {
					pending = append(pending, err.Error())
					continue
				}
				updated = true
			}
			continue
		}
		if _, present := n.Labels["brewlet.sh/runtime."+im.Name]; present && !control.StartupFenced(n) {
			pending = append(pending, "Node "+n.Name+" still advertises the revoked runtime without startup fence")
			continue
		}
		if t.Resolved {
			continue
		}
		if err := r.verifyReceipt(ctx, im, t, n); err != nil {
			pending = append(pending, n.Name+": "+err.Error())
			continue
		}
		updated = true
	}
	if len(pending) > 0 {
		runtimeCondition(im, "CleanupComplete", "CleanupBlocked", strings.Join(pending, "; "), metav1.ConditionFalse)
		return false, r.revocationCheckpoint(ctx, im)
	}
	c := meta.FindStatusCondition(im.Status.Conditions, "CleanupComplete")
	if updated || c == nil || c.Status != metav1.ConditionTrue || c.ObservedGeneration != im.Generation {
		runtimeCondition(im, "CleanupComplete", "InventoryRevocationComplete", "all UID-bound epoch roots/auth are cleaned or independently resolved; scheduling withdrawal/fence confirmed", metav1.ConditionTrue)
		return false, r.revocationCheckpoint(ctx, im)
	}
	return true, nil
}
