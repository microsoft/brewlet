// Package policy contains checks shared by admission, node workers and reconciliation.
package policy

import (
	_ "crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"
	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/pkg/node"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

var idPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func ValidID(s string) bool { return len(s) > 0 && len(s) <= 55 && idPattern.MatchString(s) }
func Source(s string) error {
	n, err := reference.ParseNormalizedNamed(s)
	if err != nil {
		return fmt.Errorf("invalid canonical image source: %w", err)
	}
	c, ok := n.(reference.Canonical)
	if !ok || !digestPattern.MatchString(c.Digest().String()) || n.String() != s || reference.IsNameOnly(n) || reference.Domain(n) != strings.ToLower(reference.Domain(n)) {
		return fmt.Errorf("source must be canonical repo@sha256:<64 lowercase hex>")
	}
	if _, tagged := n.(reference.Tagged); tagged {
		return fmt.Errorf("source tags are forbidden")
	}
	return nil
}
func Runtime(r *api.RuntimeImage, old *api.RuntimeImage) error {
	if !ValidID(r.Name) {
		return fmt.Errorf("InvalidRuntimeImageID")
	}
	if err := Source(r.Spec.Source.Image); err != nil {
		return err
	}
	if r.Spec.User.UID <= 0 || r.Spec.User.GID <= 0 || r.Spec.User.UID > 4294967294 || r.Spec.User.GID > 4294967294 {
		return fmt.Errorf("RootNotSupported: user must contain usable non-root Linux uid and gid")
	}
	if err := node.ValidateVolumeMountPaths(r.Spec.VolumeMountPaths); err != nil {
		return err
	}
	if old != nil && (r.Spec.User != old.Spec.User || r.Spec.Compatibility != old.Spec.Compatibility ||
		!slices.Equal(r.Spec.VolumeMountPaths, old.Spec.VolumeMountPaths)) {
		return fmt.Errorf("runtime identity is immutable; create a new ID")
	}
	return nil
}
func Approved(r *api.RuntimeImage) bool {
	c := meta.FindStatusCondition(r.Status.Conditions, "Approved")
	return r.DeletionTimestamp.IsZero() && Runtime(r, nil) == nil && c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == r.Generation && r.Status.Generation > 0 && r.Status.SourceImage == r.Spec.Source.Image && strings.HasSuffix(r.Spec.Source.Image, "@"+r.Status.Digest)
}
func MirrorPolicy(m map[string]string, allowed []string) error {
	approved := make(map[string]bool, len(allowed))
	for _, host := range allowed {
		n, err := reference.ParseNormalizedNamed(host + "/brewlet/policy")
		if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/@") || err != nil || reference.Domain(n) != host || n.String() != host+"/brewlet/policy" || approved[host] {
			return fmt.Errorf("invalid or duplicate mirror allowlist host %q", host)
		}
		approved[host] = true
	}
	for from, to := range m {
		// Source keys are exact registry authorities, never repository prefixes.
		n, err := reference.ParseNormalizedNamed(from + "/brewlet")
		if err != nil || reference.Domain(n) != from || strings.ContainsAny(from, "/@") {
			return fmt.Errorf("invalid mirror source host %q", from)
		}
		n, err = reference.ParseNormalizedNamed(to + "/brewlet")
		if err != nil || strings.ContainsAny(to, "@") || strings.HasSuffix(to, "/") || n.String() != to+"/brewlet" {
			return fmt.Errorf("invalid mirror destination %q", to)
		}
		if reference.Domain(n) == from {
			return fmt.Errorf("self-referential mirror %q", from)
		}
		if !approved[reference.Domain(n)] {
			return fmt.Errorf("mirror host %q not allowed", reference.Domain(n))
		}
	}
	return nil
}
func Profile(p *api.NodeProfile, all []api.NodeProfile, allowed []string) error {
	if len(p.Spec.RuntimeImages) == 0 {
		return fmt.Errorf("runtimeImages must not be empty")
	}

	ids := map[string]bool{}
	for _, e := range p.Spec.RuntimeImages {
		if !ValidID(e.ID) || ids[e.ID] {
			return fmt.Errorf("invalid or duplicate runtime ID %q", e.ID)
		}
		ids[e.ID] = true
		if e.Mode() != "Eager" && e.Mode() != "OnDemand" {
			return fmt.Errorf("invalid install mode")
		}
		if e.IdleTTL != "" {
			d, err := time.ParseDuration(e.IdleTTL)
			if err != nil || d <= 0 || e.Mode() != "OnDemand" {
				return fmt.Errorf("idleTTL requires OnDemand and a positive duration")
			}
		}
	}
	if err := MirrorPolicy(p.Spec.Registry.Mirrors, allowed); err != nil {
		return err
	}
	if p.Spec.Rollout.RestartPolicy() != "validated" && p.Spec.Rollout.RestartPolicy() != "none" {
		return fmt.Errorf("unsupported containerdRestart")
	}
	if p.Spec.Rollout.MaxUnavailable < 0 {
		return fmt.Errorf("maxUnavailable must be positive")
	}
	if p.Spec.NodePool.Key != "" && len(validation.IsQualifiedName(p.Spec.NodePool.Key)) != 0 {
		return fmt.Errorf("invalid pool key")
	}
	names := map[string]bool{}
	for _, s := range p.Spec.NodePool.Names {
		if len(validation.IsValidLabelValue(s)) != 0 || s == "" || names[s] {
			return fmt.Errorf("invalid or duplicate pool name")
		}
		names[s] = true
	}
	for _, t := range p.Spec.Tolerations {
		if t.Key == "" || len(validation.IsQualifiedName(t.Key)) != 0 || (t.Operator != "" && t.Operator != "Equal" && t.Operator != "Exists") || (t.Operator == "Exists" && t.Value != "") || (t.Effect != "" && t.Effect != "NoSchedule" && t.Effect != "NoExecute" && t.Effect != "PreferNoSchedule") {
			return fmt.Errorf("invalid explicit toleration")
		}
	}
	for _, q := range all {
		if q.Name == p.Name {
			continue
		}
		if len(names) == 0 && len(q.Spec.NodePool.Names) == 0 {
			return fmt.Errorf("PoolConflict: multiple catch-all profiles")
		}
		if len(names) > 0 && len(q.Spec.NodePool.Names) > 0 {
			if p.Spec.NodePool.Key != q.Spec.NodePool.Key {
				return fmt.Errorf("PoolConflict: mixed pool keys")
			}
			for _, n := range q.Spec.NodePool.Names {
				if names[n] {
					return fmt.Errorf("PoolConflict: pool %s", n)
				}
			}
		}
	}
	return nil
}
