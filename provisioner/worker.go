// Package provisioner implements the exclusive, freshly fenced runtime writer.
package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/control"
	"github.com/microsoft/brewlet/pkg/node"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Installer only acquires approved runtime bytes; it cannot publish current.
type Installer interface {
	Stage(context.Context, *api.RuntimeImage, string) (string, node.Metadata, error)
}
type Baseline interface{ Validate(context.Context) error }
type Activation interface {
	Ensure(context.Context, string, func(context.Context) error) error
	Restore(context.Context, func(context.Context) error) error
}
type Worker struct {
	Client                 client.Client
	Reader                 client.Reader
	Authority              control.Authority
	Store                  *node.Store
	Installer              Installer
	Baseline               Baseline
	AllowedMirrorHosts     []string
	checkedMutable         map[string]bool
	Activation             Activation
	Evidence               node.Evidence
	lastSweep              time.Time
	Metrics                *Metrics
	CacheDir               string
	CacheHostPath          string
	Tasks                  *containerd.Client
	lastBindingPublication time.Time
	lastBindingInspection  time.Time
	lastBindings           []node.RuntimeBinding
}

func (w *Worker) check(ctx context.Context) (*control.Snapshot, error) {
	if w.Reader == nil || w.Client == nil || w.Store == nil {
		return nil, fmt.Errorf("worker dependencies missing")
	}
	return control.Check(ctx, w.Reader, w.Authority)
}
func (w *Worker) runtime(ctx context.Context, id string) (*api.RuntimeImage, error) {
	r := new(api.RuntimeImage)
	if err := w.Reader.Get(ctx, client.ObjectKey{Name: id}, r); err != nil {
		return nil, err
	}
	if !policy.Approved(r) {
		if pendingApproval(r) {
			return r, &node.Error{Reason: "RuntimeImageUpdating", Detail: "awaiting allocated approval for " + id}
		}
		return nil, &node.Error{Reason: "RuntimeImageRevoked", Detail: id}
	}
	recorded := false
	for _, target := range r.Status.RevocationTargets {
		if target.NodeUID == w.Authority.NodeUID && target.ProfileUID == w.Authority.ProfileUID && target.NodeName == w.Authority.NodeName && target.ProfileName == w.Authority.ProfileName && !target.Resolved && target.Generation == r.Status.Generation && target.Digest == r.Status.Digest {
			recorded = true
			break
		}
	}
	if !recorded {
		return nil, fmt.Errorf("ownership-fence-failed: runtime target obligation not durably recorded for %s", id)
	}
	return r, nil
}
func authorization(r *api.RuntimeImage, a control.Authority, entry api.RuntimeEntry, state string) node.Authorization {
	return node.Authorization{ID: r.Name, InventoryUID: string(r.UID), Generation: r.Status.Generation, Digest: r.Status.Digest, ProfileUID: string(a.ProfileUID), ProfileGeneration: a.Generation, ExpiresAt: time.Now().Add(5 * time.Minute).UTC(), Install: entry.Install, State: state}
}

// Reconcile runs synchronously; retargeting/deletion cancellation joins this
// loop before the exclusive writer lock can pass to any successor.
func (w *Worker) Reconcile(ctx context.Context) error {
	s, err := w.check(ctx)
	if err != nil {
		return err
	}
	if w.Authority.Mode != "provisioning" {
		return fmt.Errorf("cleanup requires explicit Cleanup call")
	}
	defer func() {
		if err := w.publishBindings(ctx); err != nil {
			log.Printf("runtime binding observation unavailable: %v", err)
		}
	}()
	var profiles api.NodeProfileList
	if err := w.Reader.List(ctx, &profiles); err != nil {
		return err
	}
	if err := policy.Profile(s.Profile, profiles.Items, w.AllowedMirrorHosts); err != nil {
		return err
	}
	if w.Baseline == nil {
		return fmt.Errorf("baseline validation unavailable; readiness remains fenced")
	}
	if w.Activation != nil {
		if _, err := w.check(ctx); err != nil {
			return err
		}
		if err := w.Store.Initialize(); err != nil {
			return err
		}
		if recovery, ok := w.Installer.(interface {
			RecoverStaging(context.Context, string) error
		}); ok {
			if err := recovery.RecoverStaging(ctx, w.Store.Root); err != nil {
				log.Printf("staging reconciliation blocked: %v", err)
			}
		}
		fence := func(ctx context.Context) error { _, err := w.check(ctx); return err }
		if err := w.Activation.Ensure(ctx, s.Spec.Rollout.RestartPolicy(), fence); err != nil {
			return err
		}
	}
	if err := w.Baseline.Validate(ctx); err != nil {
		return err
	}
	if _, err = w.check(ctx); err != nil {
		return err
	}
	if err := w.Store.Initialize(); err != nil {
		return err
	}
	requests, err := w.Store.Requests()
	if err != nil {
		return err
	}
	inventory := map[string]api.NodeRuntime{}
	offered := map[string]bool{}
	for _, entry := range s.Spec.RuntimeImages {
		offered[entry.ID] = true
	}
	var failures []error
	for _, entry := range s.Spec.RuntimeImages {
		if entry.Install == "" {
			entry.Install = "Eager"
		}
		if err := node.ValidateID(entry.ID); err != nil {
			return err
		}
		r, err := w.runtime(ctx, entry.ID)
		if err != nil {
			if isUpdating(err) {
				auths, readErr := w.Store.Authorizations()
				if readErr != nil {
					return readErr
				}
				live := false
				for _, a := range auths {
					if a.ID == entry.ID && a.InventoryUID == string(r.UID) && a.ProfileUID == string(w.Authority.ProfileUID) && !a.Revoked && time.Now().Before(a.ExpiresAt) {
						live = true
						break
					}
				}
				if !live {
					if _, withdrawErr := control.Withdraw(ctx, w.Client, w.Reader, w.Authority, []string{"brewlet.sh/runtime." + entry.ID}); withdrawErr != nil {
						return withdrawErr
					}
				}
				failures = append(failures, err)
				continue
			}
			// Never renew an authorization when a fresh inventory read fails.
			if revokeErr := w.revokeRuntime(ctx, entry.ID); revokeErr != nil {
				return revokeErr
			}
			var reason *node.Error
			if apierrors.IsNotFound(err) || (errors.As(err, &reason) && reason.Reason == "RuntimeImageRevoked") {
				log.Printf("UnknownRuntimeImage: %s is not approved/offered; known approved entries continue: %v", entry.ID, err)
				continue
			}
			failures = append(failures, err)
			continue
		}
		if err := w.clearRevocation(ctx, entry.ID); err != nil {
			return err
		}
		authorizations, err := w.Store.Authorizations()
		if err != nil {
			return err
		}
		for _, a := range authorizations {
			if a.ProfileUID != string(w.Authority.ProfileUID) {
				return fmt.Errorf("ownership-fence-failed: foreign local inventory")
			}
			if !offered[a.ID] {
				if err := w.revokeRuntime(ctx, a.ID); err != nil {
					return err
				}
			}
		}
		state := "available"
		installed := false
		current := filepath.Join(w.Store.Root, entry.ID, "current")
		if _, err := os.Readlink(current); err == nil {
			installed = true
		} else if !os.IsNotExist(err) {
			return err
		}
		if installed && entry.Install == "OnDemand" && entry.IdleTTL != "" && !slices.Contains(requests, entry.ID) && w.Evidence != nil {
			if _, err := w.check(ctx); err != nil {
				return err
			}
			if err := w.Store.WriteAuthorization(ctx, authorization(r, w.Authority, entry, "ready")); err != nil {
				return err
			}
			ttl, err := time.ParseDuration(entry.IdleTTL)
			if err != nil {
				return err
			}
			fence := func(ctx context.Context) error {
				if _, err := w.check(ctx); err != nil {
					return err
				}
				fresh, err := w.runtime(ctx, entry.ID)
				if err != nil {
					return err
				}
				if fresh.UID != r.UID || fresh.Status.Generation != r.Status.Generation || fresh.Status.Digest != r.Status.Digest {
					return fmt.Errorf("idle reclamation target changed")
				}
				return nil
			}
			reclaimed, err := w.Store.IdleUninstall(ctx, entry.ID, ttl, w.Evidence, fence)
			if err != nil {
				return err
			}
			if reclaimed {
				installed = false
			}
		}
		needed := entry.Install == "Eager" || installed || slices.Contains(requests, entry.ID)
		if needed {
			state = "installing"
		}
		if _, err := w.check(ctx); err != nil {
			return err
		}
		auth := authorization(r, w.Authority, entry, state)
		if err := w.Store.WriteAuthorization(ctx, auth); err != nil {
			return err
		}
		if needed {
			m, _, currentErr := w.Store.Current(ctx, entry.ID)
			upToDate := currentErr == nil && m.InventoryUID == string(r.UID) && m.Generation == r.Status.Generation && m.SourceDigest == r.Status.Digest
			if !upToDate {
				if entry.Install == "OnDemand" && !installed {
					if err := w.advertiseAvailable(ctx, entry.ID); err != nil {
						return err
					}
				}
				if w.Installer == nil {
					return fmt.Errorf("runtime installer unavailable")
				}
				installStart := time.Now()
				stage, m, err := w.stage(ctx, r)
				if err != nil {
					if entry.Install == "OnDemand" {
						w.Metrics.install(entry.ID, "failed", installStart)
					}
					failures = append(failures, w.installFailed(ctx, auth, err))
					continue
				}
				// No deletion of staged data after losing authority.
				if _, err := w.check(ctx); err != nil {
					return err
				}
				fresh, err := w.runtime(ctx, entry.ID)
				if err != nil {
					return err
				}
				if fresh.UID != r.UID || fresh.Status.Generation != r.Status.Generation || fresh.Status.Digest != r.Status.Digest {
					return fmt.Errorf("approved runtime changed while installing")
				}
				if err := w.Store.WriteAuthorization(ctx, authorization(fresh, w.Authority, entry, "installing")); err != nil {
					return err
				}
				m.ID = r.Name
				m.InventoryUID = string(r.UID)
				m.Generation = r.Status.Generation
				m.SourceDigest = r.Status.Digest
				fence := func(ctx context.Context) error {
					if _, err := w.check(ctx); err != nil {
						return err
					}
					target, err := w.runtime(ctx, entry.ID)
					if err != nil {
						return err
					}
					if target.UID != r.UID || target.Status.Generation != r.Status.Generation || target.Status.Digest != r.Status.Digest {
						return fmt.Errorf("runtime target changed at publication fence")
					}
					return nil
				}
				if err := w.Store.Publish(ctx, stage, m, fence); err != nil {
					if entry.Install == "OnDemand" {
						w.Metrics.install(entry.ID, "failed", installStart)
					}
					return err
				}
				if entry.Install == "OnDemand" {
					w.Metrics.install(entry.ID, "ready", installStart)
				}
			}
			if _, err := w.check(ctx); err != nil {
				return err
			}
			state = "ready"
			auth = authorization(r, w.Authority, entry, state)
			if err := w.Store.WriteAuthorization(ctx, auth); err != nil {
				return err
			}
			if err := w.Store.RemoveRequest(entry.ID); err != nil {
				return err
			}
		}
		inventory[entry.ID] = api.NodeRuntime{Install: entry.Install, State: state, InventoryUID: r.UID, Generation: r.Status.Generation, Digest: r.Status.Digest}
	}
	for _, id := range requests {
		if !offered[id] {
			if _, err := w.check(ctx); err != nil {
				return err
			}
			if err := w.Store.RemoveRequest(id); err != nil {
				return err
			}
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	if w.checkedMutable == nil {
		w.checkedMutable = map[string]bool{}
	}
	for id, state := range inventory {
		if state.Install != "OnDemand" || w.checkedMutable[id] {
			continue
		}
		if err := w.advertiseAvailable(ctx, id); err != nil {
			return err
		}
		broader, err := control.Withdraw(ctx, w.Client, w.Reader, w.Authority, []string{"brewlet.sh/runtime." + id})
		if err != nil {
			return err
		}
		if broader {
			return fmt.Errorf("node-advertisement-failed: provider-pinned OnDemand capability; retain startup fence")
		}
		w.checkedMutable[id] = true
	}
	if err := w.publish(ctx, inventory); err != nil {
		return err
	}
	if w.Evidence != nil && time.Since(w.lastSweep) >= time.Minute {
		if _, err := w.check(ctx); err != nil {
			return err
		}
		sweepCtx, cancelSweep := context.WithTimeout(ctx, 10*time.Second)
		recovered := false
		if err := w.Store.Recover(sweepCtx, w.Evidence); err != nil {
			log.Printf("runtime collection blocked: %v", err)
		} else {
			recovered = true
			for id, state := range inventory {
				if state.State != "ready" {
					continue
				}
				fence := func(ctx context.Context) error { _, err := w.check(ctx); return err }
				if err := w.Store.Collect(sweepCtx, id, time.Hour, w.Evidence, fence); err != nil {
					log.Printf("runtime collection blocked for %s: %v", id, err)
				}
			}
		}
		cancelSweep()
		if mounts, ok := w.Evidence.(node.MountEvidence); recovered && ok && w.CacheDir != "" {
			cacheCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			fence := func(ctx context.Context) error { _, err := w.check(ctx); return err }
			if err := CollectLayerCache(cacheCtx, w.Store, w.CacheDir, w.CacheHostPath, time.Hour, mounts, fence); err != nil {
				log.Printf("layer cache collection blocked: %v", err)
			}
			cancel()
		}
		w.lastSweep = time.Now()
	}
	return nil
}

func (w *Worker) advertiseAvailable(ctx context.Context, id string) error {
	s, err := w.check(ctx)
	if err != nil {
		return err
	}
	n := s.Node.DeepCopy()
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels["brewlet.sh/runtime."+id] = "available"
	if err := w.Client.Update(ctx, n); err != nil {
		if _, withdrawErr := control.Withdraw(ctx, w.Client, w.Reader, w.Authority, []string{"brewlet.sh/runtime." + id}); withdrawErr != nil {
			return withdrawErr
		}
		return fmt.Errorf("node-advertisement-failed: mutable OnDemand capability required: %w", err)
	}
	s, err = w.check(ctx)
	if err != nil {
		return err
	}
	if s.Node.Labels["brewlet.sh/runtime."+id] != "available" {
		return fmt.Errorf("node-advertisement-failed: available capability readback")
	}
	return nil
}
func (w *Worker) installFailed(ctx context.Context, a node.Authorization, cause error) error {
	if _, err := w.check(ctx); err != nil {
		return err
	}
	current, approvalErr := w.runtime(ctx, a.ID)
	if isUpdating(approvalErr) {
		return approvalErr
	}
	if approvalErr != nil || string(current.UID) != a.InventoryUID {
		if err := w.revokeRuntime(ctx, a.ID); err != nil {
			return err
		}
		return fmt.Errorf("runtime approval changed during installation: %w", cause)
	}
	if current.Status.Generation != a.Generation || current.Status.Digest != a.Digest {
		// A compatible newer target is not an install failure for the old
		// validated current. Normal reconciliation will renew the new target.
		return fmt.Errorf("runtime target changed during installation: %w", cause)
	}
	a.State = "failed"
	stateErr := w.Store.WriteAuthorization(ctx, a)
	_, err := control.Withdraw(ctx, w.Client, w.Reader, w.Authority, []string{"brewlet.sh/runtime." + a.ID})
	if err != nil {
		return errors.Join(stateErr, err)
	}
	if stateErr != nil {
		return stateErr
	}
	return fmt.Errorf("runtime install failed: %w", cause)
}
func (w *Worker) publish(ctx context.Context, inventory map[string]api.NodeRuntime) error {
	s, err := w.check(ctx)
	if err != nil {
		return err
	}
	n := s.Node.DeepCopy()
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	if n.Annotations == nil {
		n.Annotations = map[string]string{}
	}
	for key := range n.Labels {
		if len(key) > len("brewlet.sh/runtime.") && key[:len("brewlet.sh/runtime.")] == "brewlet.sh/runtime." {
			if _, ok := inventory[key[len("brewlet.sh/runtime."):]]; !ok {
				return fmt.Errorf("node-advertisement-failed: stale runtime capability requires withdrawal")
			}
		}
	}
	for id, state := range inventory {
		n.Labels["brewlet.sh/runtime."+id] = state.State
	}
	n.Labels[api.RuntimeLabel] = "ready"
	b, err := json.Marshal(inventory)
	if err != nil {
		return err
	}
	n.Annotations[api.InventoryAnnotation] = string(b)
	if err := w.Client.Update(ctx, n); err != nil {
		return fmt.Errorf("node-advertisement-failed: %w", err)
	}
	// Readback precedes startup-fence release.
	s, err = w.check(ctx)
	if err != nil {
		return err
	}
	for id, state := range inventory {
		if s.Node.Labels["brewlet.sh/runtime."+id] != state.State {
			return fmt.Errorf("node-advertisement-failed: capability readback")
		}
	}
	n = s.Node.DeepCopy()
	n.Spec.Taints = slices.DeleteFunc(n.Spec.Taints, func(t corev1.Taint) bool {
		return t.Key == api.StartupTaint && t.Effect == corev1.TaintEffectNoSchedule
	})
	if err := w.Client.Update(ctx, n); err != nil {
		return fmt.Errorf("node-advertisement-failed: startup fence release: %w", err)
	}
	s, err = w.check(ctx)
	if err != nil {
		return err
	}
	if control.StartupFenced(s.Node) {
		return fmt.Errorf("node-advertisement-failed: startup fence remains")
	}
	return nil
}
