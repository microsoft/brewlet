package admission

import (
	"context"
	"fmt"
	"slices"
	"strings"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	"github.com/microsoft/brewlet/kubernetes/policy"
	"github.com/microsoft/brewlet/pkg/node"
	"github.com/microsoft/brewlet/pkg/workload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type PodMutator struct {
	Reader             client.Reader
	AllowedMirrorHosts []string
}

func ptr[T any](v T) *T { return &v }

func containerNames(p *corev1.Pod) []string {
	names := make([]string, 0, len(p.Spec.Containers)+len(p.Spec.InitContainers)+len(p.Spec.EphemeralContainers))
	for _, c := range p.Spec.Containers {
		names = append(names, c.Name)
	}
	for _, c := range p.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range p.Spec.EphemeralContainers {
		names = append(names, c.Name)
	}
	return names
}

func (m *PodMutator) Mutate(ctx context.Context, p *corev1.Pod) ([]string, error) {
	if p.Spec.RuntimeClassName == nil || *p.Spec.RuntimeClassName != "brewlet" {
		return nil, nil
	}
	selections, err := workload.ActiveSelections(p.Annotations, containerNames(p))
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"brewlet.sh/jdk", "brewlet.sh/jvm-args", "brewlet.sh/arch"} {
		if _, ok := p.Annotations[k]; ok {
			return nil, fmt.Errorf("UnsupportedAnnotation: %s", k)
		}
	}
	ids := workload.DistinctRuntimeIDs(selections)
	if len(ids) == 0 {
		return nil, nil
	}
	runtimes := make(map[string]*api.RuntimeImage, len(ids))
	for _, id := range ids {
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
		if raw, ok := p.Annotations[workload.RuntimeGenerationAnnotation(id)]; ok {
			b, err := node.ParseBarrier(raw, id)
			if err != nil || b == nil {
				return nil, fmt.Errorf("InvalidRuntimeGeneration")
			}
			if b.InventoryUID != string(r.UID) {
				return nil, fmt.Errorf("RuntimeImageEpochMismatch")
			}
		}
		runtimes[id] = r
	}
	for _, t := range p.Spec.Tolerations {
		if (t.Effect == "" || t.Effect == corev1.TaintEffectNoSchedule) &&
			((t.Operator == corev1.TolerationOpExists && (t.Key == "" || t.Key == api.StartupTaint)) ||
				((t.Operator == "" || t.Operator == corev1.TolerationOpEqual) && t.Key == api.StartupTaint && t.Value == "provisioning")) {
			return nil, fmt.Errorf("StartupTaintTolerationNotSupported")
		}
	}
	var profiles api.NodeProfileList
	if err := m.Reader.List(ctx, &profiles); err != nil {
		return nil, &InventoryUnavailable{Cause: err}
	}
	eligible := make([]string, 0, len(profiles.Items))
	validProfile := make(map[string]bool, len(profiles.Items))
	for i := range profiles.Items {
		q := &profiles.Items[i]
		if !q.DeletionTimestamp.IsZero() || policy.Profile(q, profiles.Items, m.AllowedMirrorHosts) != nil {
			continue
		}
		offered := make(map[string]bool, len(q.Spec.RuntimeImages))
		for _, entry := range q.Spec.RuntimeImages {
			offered[entry.ID] = true
		}
		all := true
		for _, id := range ids {
			all = all && offered[id]
		}
		if all {
			eligible = append(eligible, q.Name)
			validProfile[q.Name] = true
		}
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("NoCompatibleRuntime")
	}
	var nodes corev1.NodeList
	if err := m.Reader.List(ctx, &nodes); err != nil {
		return nil, &InventoryUnavailable{Cause: err}
	}
	if p.Spec.NodeName != "" {
		var bound *corev1.Node
		for i := range nodes.Items {
			if nodes.Items[i].Name == p.Spec.NodeName {
				bound = &nodes.Items[i]
				break
			}
		}
		if bound == nil || bound.DeletionTimestamp != nil ||
			bound.Labels[api.RuntimeLabel] != "ready" ||
			bound.Labels[api.OwnerNodeUID] != string(bound.UID) {
			return nil, fmt.Errorf("NoCompatibleRuntime: bound node is not Brewlet-ready")
		}
		ready, fenced := false, false
		for _, condition := range bound.Status.Conditions {
			ready = ready || condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue
		}
		for _, taint := range bound.Spec.Taints {
			fenced = fenced || taint.Key == api.StartupTaint && taint.Effect == corev1.TaintEffectNoSchedule
		}
		if !ready || fenced {
			return nil, fmt.Errorf("NoCompatibleRuntime: bound node is not schedulable for Brewlet")
		}
		owned := false
		for _, q := range profiles.Items {
			if !validProfile[q.Name] || bound.Labels[api.OwnerUID] != string(q.UID) ||
				bound.Annotations[api.OwnerName] != q.Name {
				continue
			}
			offered := make(map[string]bool, len(q.Spec.RuntimeImages))
			for _, entry := range q.Spec.RuntimeImages {
				offered[entry.ID] = true
			}
			if slices.ContainsFunc(ids, func(id string) bool { return !offered[id] }) {
				continue
			}
			if slices.ContainsFunc(ids, func(id string) bool {
				state := bound.Labels["brewlet.sh/runtime."+id]
				return state != "ready" && state != "available"
			}) {
				continue
			}
			owned = true
			break
		}
		if !owned {
			return nil, fmt.Errorf("NoCompatibleRuntime: bound node does not offer every selected runtime")
		}
	}
	warm := false
	for _, n := range nodes.Items {
		if n.DeletionTimestamp != nil || n.Spec.Unschedulable || n.Labels[api.RuntimeLabel] != "ready" ||
			n.Labels[api.OwnerNodeUID] != string(n.UID) {
			continue
		}
		ready, fenced := false, false
		for _, condition := range n.Status.Conditions {
			ready = ready || condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue
		}
		for _, taint := range n.Spec.Taints {
			fenced = fenced || taint.Key == api.StartupTaint && taint.Effect == corev1.TaintEffectNoSchedule
		}
		if !ready || fenced {
			continue
		}
		for _, id := range ids {
			if n.Labels["brewlet.sh/runtime."+id] != "ready" {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		for _, q := range profiles.Items {
			if validProfile[q.Name] && n.Labels[api.OwnerUID] == string(q.UID) && n.Annotations[api.OwnerName] == q.Name {
				warm = true
				break
			}
		}
		if warm {
			break
		}
	}
	warnings := []string{}
	if !warm {
		slices.Sort(eligible)
		warnings = append(warnings, "runtime capacity pending for IDs "+strings.Join(ids, ", ")+"; eligible NodeProfiles: "+strings.Join(eligible, ", "))
	}
	ps := p.Spec.SecurityContext
	if ps != nil {
		if ps.FSGroup != nil && (*ps.FSGroup <= 0 || *ps.FSGroup > 4294967294) {
			return nil, fmt.Errorf("RootNotSupported")
		}
		for _, v := range ps.SupplementalGroups {
			if v <= 0 || v > 4294967294 {
				return nil, fmt.Errorf("RootNotSupported")
			}
		}
		if ps.WindowsOptions != nil {
			return nil, fmt.Errorf("UnsupportedSecurityContext: Brewlet requires verified Linux host identities")
		}
	}
	if (p.Spec.OS != nil && p.Spec.OS.Name != corev1.Linux) || (p.Spec.HostUsers != nil && !*p.Spec.HostUsers) {
		return nil, fmt.Errorf("UnsupportedSecurityContext: Brewlet requires verified Linux host identities")
	}
	mutate := func(c *corev1.Container) error {
		id, selected, err := workload.RuntimeIDForContainer(p.Annotations, c.Name)
		if err != nil || !selected {
			return err
		}
		r := runtimes[id]
		if err := policy.Source(c.Image); err != nil {
			return fmt.Errorf("UnsupportedArtifact: %s: %w", c.Name, err)
		}
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		s := c.SecurityContext
		if s.ReadOnlyRootFilesystem != nil && !*s.ReadOnlyRootFilesystem {
			return fmt.Errorf("ReadOnlyRootRequired")
		}
		s.ReadOnlyRootFilesystem = ptr(true)
		var destinations []string
		for _, mount := range c.VolumeMounts {
			if err := node.ValidateWorkloadMount(mount.MountPath, mount.ReadOnly, r.Spec.VolumeMountPaths); err != nil {
				return err
			}
			destinations = append(destinations, mount.MountPath)
		}
		for _, device := range c.VolumeDevices {
			if err := node.ValidateWorkloadMount(device.DevicePath, false, r.Spec.VolumeMountPaths); err != nil {
				return err
			}
			destinations = append(destinations, device.DevicePath)
		}
		if c.TerminationMessagePath != "" && c.TerminationMessagePath != "/dev/termination-log" {
			if err := node.ValidateWorkloadMount(c.TerminationMessagePath, false, r.Spec.VolumeMountPaths); err != nil {
				return err
			}
			destinations = append(destinations, c.TerminationMessagePath)
		}
		if err := node.ValidateMountDestinations(destinations); err != nil {
			return err
		}
		if s.WindowsOptions != nil {
			return fmt.Errorf("UnsupportedSecurityContext")
		}
		if s.RunAsNonRoot != nil && !*s.RunAsNonRoot {
			return fmt.Errorf("RootNotSupported")
		}
		if ps != nil && ps.RunAsNonRoot != nil && !*ps.RunAsNonRoot && s.RunAsNonRoot == nil {
			return fmt.Errorf("RootNotSupported")
		}
		if s.RunAsUser == nil {
			if ps != nil && ps.RunAsUser != nil {
				s.RunAsUser = ptr(*ps.RunAsUser)
			} else {
				s.RunAsUser = ptr(r.Spec.User.UID)
			}
		}
		if s.RunAsGroup == nil {
			if ps != nil && ps.RunAsGroup != nil {
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
		if len(c.Command) > 0 {
			warnings = append(warnings, c.Name+": command is ignored; the runtime image ENTRYPOINT always runs first (use args to replace the CMD)")
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
		c := corev1.Container{Name: e.Name, Image: e.Image, Env: e.Env, SecurityContext: e.SecurityContext,
			VolumeMounts: e.VolumeMounts, VolumeDevices: e.VolumeDevices, TerminationMessagePath: e.TerminationMessagePath}
		if err := mutate(&c); err != nil {
			return nil, err
		}
		e.SecurityContext = c.SecurityContext
	}
	InjectAffinities(p, ids)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	for _, k := range []string{"brewlet.sh/artifact-container", "brewlet.sh/artifact-ref", "brewlet.sh/artifact-digest"} {
		delete(p.Annotations, k)
	}
	for _, c := range p.Spec.Containers {
		_, selected, _ := workload.RuntimeIDForContainer(p.Annotations, c.Name)
		if !selected {
			continue
		}
		p.Annotations["brewlet.sh/artifact-container"] = c.Name
		p.Annotations["brewlet.sh/artifact-ref"] = c.Image
		p.Annotations["brewlet.sh/artifact-digest"] = strings.SplitN(c.Image, "@", 2)[1]
		break
	}
	return warnings, nil
}

func InjectAffinity(p *corev1.Pod, id string) { InjectAffinities(p, []string{id}) }

func InjectAffinities(p *corev1.Pod, ids []string) {
	if len(ids) == 0 {
		return
	}
	if p.Spec.Affinity == nil {
		p.Spec.Affinity = &corev1.Affinity{}
	}
	if p.Spec.Affinity.NodeAffinity == nil {
		p.Spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}
	a := p.Spec.Affinity.NodeAffinity
	createdSelector := a.RequiredDuringSchedulingIgnoredDuringExecution == nil
	if createdSelector {
		a.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{}
		for _, id := range ids {
			req := corev1.NodeSelectorRequirement{Key: "brewlet.sh/runtime." + id, Operator: corev1.NodeSelectorOpIn, Values: []string{"ready", "available"}}
			if len(a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {
				a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = []corev1.NodeSelectorTerm{{}}
			}
			a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions = append(a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions, req)
		}
	}
	for _, id := range ids {
		req := corev1.NodeSelectorRequirement{Key: "brewlet.sh/runtime." + id, Operator: corev1.NodeSelectorOpIn, Values: []string{"ready", "available"}}
		for i := range a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			t := &a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[i]
			if createdSelector || len(t.MatchExpressions) == 0 && len(t.MatchFields) == 0 {
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
		preferred := corev1.PreferredSchedulingTerm{Weight: 100, Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: req.Key, Operator: corev1.NodeSelectorOpIn, Values: []string{"ready"}}}}}
		exists := false
		for _, x := range a.PreferredDuringSchedulingIgnoredDuringExecution {
			if x.Weight == 100 && len(x.Preference.MatchExpressions) == 1 {
				r := x.Preference.MatchExpressions[0]
				exists = exists || r.Key == req.Key && r.Operator == req.Operator && slices.Equal(r.Values, []string{"ready"}) && len(x.Preference.MatchFields) == 0
			}
		}
		if !exists {
			a.PreferredDuringSchedulingIgnoredDuringExecution = append(a.PreferredDuringSchedulingIgnoredDuringExecution, preferred)
		}
	}
}
