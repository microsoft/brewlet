package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type PodMutator struct {
	Reader             client.Reader
	AllowedMirrorHosts []string
}

func ptr[T any](v T) *T { return &v }

func (m *PodMutator) Mutate(ctx context.Context, p *corev1.Pod) ([]string, error) {
	if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != "brewlet" {
		return nil, nil
	}
	id := p.Annotations[api.RuntimeAnnotation]
	if id == "" {
		return nil, fmt.Errorf("RuntimeImageRequired")
	}
	if !policy.ValidID(id) {
		return nil, fmt.Errorf("InvalidRuntimeImageID")
	}
	for _, k := range []string{"brewlet.sh/jdk", "brewlet.sh/jvm-args", "brewlet.sh/arch"} {
		if _, ok := p.Annotations[k]; ok {
			return nil, fmt.Errorf("UnsupportedAnnotation: %s", k)
		}
	}
	r := new(api.RuntimeImage)
	if err := m.Reader.Get(ctx, client.ObjectKey{Name: id}, r); err != nil {
		if !apierrors.IsNotFound(err) {
			return nil, &InventoryUnavailable{Cause: err}
		}
		return nil, fmt.Errorf("UnknownRuntimeImage: %w", err)
	}
	if !policy.Approved(r) {
		return nil, fmt.Errorf("UnknownRuntimeImage: inventory is not approved for its current source")
	}
	if raw, ok := p.Annotations[api.BarrierAnnotation]; ok {
		b := api.Barrier{}
		d := json.NewDecoder(bytes.NewBufferString(raw))
		d.DisallowUnknownFields()
		if err := d.Decode(&b); err != nil {
			return nil, fmt.Errorf("InvalidRuntimeGeneration")
		}
		if err := d.Decode(new(any)); err != io.EOF || b.ID != id || b.InventoryUID == "" || b.Generation <= 0 {
			return nil, fmt.Errorf("InvalidRuntimeGeneration")
		}
		if b.InventoryUID != r.UID {
			return nil, fmt.Errorf("RuntimeImageEpochMismatch")
		}
	}
	for _, t := range p.Spec.Tolerations {
		if (t.Effect == "" || t.Effect == corev1.TaintEffectNoSchedule) && ((t.Operator == corev1.TolerationOpExists && (t.Key == "" || t.Key == api.StartupTaint)) || ((t.Operator == "" || t.Operator == corev1.TolerationOpEqual) && t.Key == api.StartupTaint && t.Value == "provisioning")) {
			return nil, fmt.Errorf("StartupTaintTolerationNotSupported")
		}
	}
	var profiles api.NodeProfileList
	if err := m.Reader.List(ctx, &profiles); err != nil {
		return nil, &InventoryUnavailable{Cause: err}
	}
	eligible := []string{}
	for _, q := range profiles.Items {
		if !q.DeletionTimestamp.IsZero() || policy.Profile(&q, profiles.Items, m.AllowedMirrorHosts) != nil {
			continue
		}
		for _, e := range q.Spec.RuntimeImages {
			if e.ID == id {
				eligible = append(eligible, q.Name)
				break
			}
		}
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("NoCompatibleRuntime")
	}
	warnings := []string{}
	var nodes corev1.NodeList
	if err := m.Reader.List(ctx, &nodes); err != nil {
		return nil, &InventoryUnavailable{Cause: err}
	}
	warm := false
	for _, n := range nodes.Items {
		if n.DeletionTimestamp != nil || n.Spec.Unschedulable || n.Labels[api.RuntimeLabel] != "ready" || n.Labels["brewlet.sh/runtime."+id] != "ready" || n.Labels[api.OwnerNodeUID] != string(n.UID) {
			continue
		}
		fenced := false
		for _, t := range n.Spec.Taints {
			if t.Key == api.StartupTaint && t.Effect == corev1.TaintEffectNoSchedule {
				fenced = true
			}
		}
		ready := false
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		for _, q := range profiles.Items {
			if slices.Contains(eligible, q.Name) && n.Labels[api.OwnerUID] == string(q.UID) && n.Annotations[api.OwnerName] == q.Name && ready && !fenced {
				warm = true
			}
		}
	}
	if !warm {
		slices.Sort(eligible)
		warnings = append(warnings, "runtime capacity pending; eligible NodeProfiles: "+strings.Join(eligible, ", "))
	}
	if p.Spec.SecurityContext == nil {
		p.Spec.SecurityContext = &corev1.PodSecurityContext{}
	}
	ps := p.Spec.SecurityContext
	for _, v := range []*int64{ps.RunAsUser, ps.RunAsGroup, ps.FSGroup} {
		if v != nil && (*v <= 0 || *v > 4294967294) {
			return nil, fmt.Errorf("RootNotSupported")
		}
	}
	if ps.RunAsNonRoot != nil && !*ps.RunAsNonRoot {
		return nil, fmt.Errorf("RootNotSupported")
	}
	for _, v := range ps.SupplementalGroups {
		if v <= 0 || v > 4294967294 {
			return nil, fmt.Errorf("RootNotSupported")
		}
	}
	if ps.WindowsOptions != nil || (p.Spec.OS != nil && p.Spec.OS.Name != corev1.Linux) || (p.Spec.HostUsers != nil && !*p.Spec.HostUsers) {
		return nil, fmt.Errorf("UnsupportedSecurityContext: Brewlet requires verified Linux host identities")
	}
	ps.RunAsNonRoot = ptr(true)
	// Default containers independently so explicit pod identities retain precedence.
	mutate := func(c *corev1.Container) error {
		if err := policy.Source(c.Image); err != nil {
			return fmt.Errorf("UnsupportedArtifact: %s: %w", c.Name, err)
		}
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		s := c.SecurityContext
		if s.WindowsOptions != nil {
			return fmt.Errorf("UnsupportedSecurityContext")
		}
		if s.RunAsNonRoot != nil && !*s.RunAsNonRoot {
			return fmt.Errorf("RootNotSupported")
		}
		if s.RunAsUser == nil {
			if ps.RunAsUser != nil {
				s.RunAsUser = ptr(*ps.RunAsUser)
			} else {
				s.RunAsUser = ptr(r.Spec.User.UID)
			}
		}
		if s.RunAsGroup == nil {
			if ps.RunAsGroup != nil {
				s.RunAsGroup = ptr(*ps.RunAsGroup)
			} else {
				s.RunAsGroup = ptr(r.Spec.User.GID)
			}
		}
		if *s.RunAsUser <= 0 || *s.RunAsGroup <= 0 || *s.RunAsUser > 4294967294 || *s.RunAsGroup > 4294967294 {
			return fmt.Errorf("RootNotSupported")
		}
		if s.Privileged != nil && *s.Privileged || s.AllowPrivilegeEscalation != nil && *s.AllowPrivilegeEscalation || s.Capabilities != nil && len(s.Capabilities.Add) > 0 {
			return fmt.Errorf("PrivilegeEscalationNotSupported")
		}
		s.RunAsNonRoot = ptr(true)
		s.AllowPrivilegeEscalation = ptr(false)
		if s.Capabilities == nil {
			s.Capabilities = &corev1.Capabilities{}
		}
		if !slices.Contains(s.Capabilities.Drop, corev1.Capability("ALL")) {
			s.Capabilities.Drop = append(s.Capabilities.Drop, "ALL")
		}
		for _, e := range c.Env {
			if e.Name == "PATH" {
				warnings = append(warnings, c.Name+": PATH is runtime-owned; requested override is ignored")
			}
		}
		return nil
	}
	for i := range p.Spec.Containers {
		if err := mutate(&p.Spec.Containers[i]); err != nil {
			return nil, err
		}
	}
	for i := range p.Spec.InitContainers {
		if err := mutate(&p.Spec.InitContainers[i]); err != nil {
			return nil, err
		}
	}
	for i := range p.Spec.EphemeralContainers {
		e := &p.Spec.EphemeralContainers[i]
		c := corev1.Container{Name: e.Name, Image: e.Image, Env: e.Env, SecurityContext: e.SecurityContext}
		if err := mutate(&c); err != nil {
			return nil, err
		}
		e.SecurityContext = c.SecurityContext
	}
	InjectAffinity(p, id)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	for _, k := range []string{"brewlet.sh/artifact-container", "brewlet.sh/artifact-ref", "brewlet.sh/artifact-digest"} {
		delete(p.Annotations, k)
	}
	if len(p.Spec.Containers) > 0 {
		c := p.Spec.Containers[0]
		p.Annotations["brewlet.sh/artifact-container"] = c.Name
		p.Annotations["brewlet.sh/artifact-ref"] = c.Image
		p.Annotations["brewlet.sh/artifact-digest"] = strings.SplitN(c.Image, "@", 2)[1]
	}
	return warnings, nil
}

func InjectAffinity(p *corev1.Pod, id string) {
	if p.Spec.Affinity == nil {
		p.Spec.Affinity = &corev1.Affinity{}
	}
	if p.Spec.Affinity.NodeAffinity == nil {
		p.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	a := p.Spec.Affinity.NodeAffinity
	req := corev1.NodeSelectorRequirement{Key: "brewlet.sh/runtime." + id, Operator: corev1.NodeSelectorOpIn, Values: []string{"ready", "available"}}
	if a.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		a.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{req}}}}
	} else {
		for i := range a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			t := &a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i]
			// An empty Kubernetes selector term matches nothing. Adding any expression
			// would turn that false alternative into a true one.
			if len(t.MatchExpressions) == 0 && len(t.MatchFields) == 0 {
				continue
			}
			found := false
			for _, x := range t.MatchExpressions {
				if x.Key == req.Key && x.Operator == req.Operator && slices.Equal(x.Values, req.Values) {
					found = true
				}
			}
			if !found {
				t.MatchExpressions = append(t.MatchExpressions, req)
			}
		}
	}
	preferred := corev1.PreferredSchedulingTerm{Weight: 100, Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: req.Key, Operator: corev1.NodeSelectorOpIn, Values: []string{"ready"}}}}}
	for _, x := range a.PreferredDuringSchedulingIgnoredDuringExecution {
		if x.Weight == 100 && len(x.Preference.MatchExpressions) == 1 {
			r := x.Preference.MatchExpressions[0]
			if r.Key == req.Key && r.Operator == req.Operator && slices.Equal(r.Values, []string{"ready"}) && len(x.Preference.MatchFields) == 0 {
				return
			}
		}
	}
	a.PreferredDuringSchedulingIgnoredDuringExecution = append(a.PreferredDuringSchedulingIgnoredDuringExecution, preferred)
}
