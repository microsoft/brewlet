package admission

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setup(t *testing.T) (*PodMutator, *corev1.Pod) {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = api.AddToScheme(s)
	digest := "sha256:" + strings.Repeat("a", 64)
	source := "registry.example.com/runtime@" + digest
	r := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12", UID: "inventory", Generation: 1}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: source}, User: api.User{UID: 1000, GID: 2000}}, Status: api.RuntimeImageStatus{Generation: 7, Digest: digest, SourceImage: source, Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	p := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web", UID: "profile"}, Spec: api.NodeProfileSpec{NodePool: api.NodePool{Names: []string{"web"}, Key: "agentpool"}, RuntimeImages: []api.RuntimeEntry{{ID: r.Name, Install: "OnDemand"}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(r, p).Build()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{api.RuntimeAnnotation: r.Name, "brewlet.sh/artifact-ref": "untrusted"}}, Spec: corev1.PodSpec{RuntimeClassName: ptr("brewlet"), Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/app@" + digest}}, InitContainers: []corev1.Container{{Name: "init", Image: "registry.example.com/init@" + digest}}, EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "registry.example.com/debug@" + digest}}}}}
	return &PodMutator{Reader: c}, pod
}
func TestZeroNodeAndAllContainers(t *testing.T) {
	m, p := setup(t)
	p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr(int64(1234))}
	p.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "PATH", Value: "/bad"}}
	w, err := m.Mutate(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 2 || !strings.Contains(w[0], "web") {
		t.Fatalf("missing zero-node/PATH warnings: %v", w)
	}
	if *p.Spec.Containers[0].SecurityContext.RunAsUser != 1234 || *p.Spec.Containers[0].SecurityContext.RunAsGroup != 2000 {
		t.Fatal("independent UID/GID defaulting broken")
	}
	for _, s := range []*corev1.SecurityContext{p.Spec.Containers[0].SecurityContext, p.Spec.InitContainers[0].SecurityContext, p.Spec.EphemeralContainers[0].SecurityContext} {
		if s == nil || !*s.RunAsNonRoot || *s.AllowPrivilegeEscalation || len(s.Capabilities.Drop) != 1 || s.Capabilities.Drop[0] != "ALL" {
			t.Fatal("security defaults missing")
		}
	}
	if p.Annotations["brewlet.sh/artifact-ref"] != p.Spec.Containers[0].Image || p.Annotations["brewlet.sh/artifact-container"] != "app" {
		t.Fatal("compatibility hints not replaced")
	}
	before := p.DeepCopy()
	if _, err := m.Mutate(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, p) {
		t.Fatal("mutation not idempotent")
	}
}
func TestSecurityAndSelectionDenials(t *testing.T) {
	cases := map[string]struct {
		mutate func(*corev1.Pod)
		reason string
	}{
		"missing":        {func(p *corev1.Pod) { delete(p.Annotations, api.RuntimeAnnotation) }, "RuntimeImageRequired"},
		"digest runtime": {func(p *corev1.Pod) { p.Annotations[api.RuntimeAnnotation] = "python@sha256:a" }, "InvalidRuntimeImageID"},
		"legacy":         {func(p *corev1.Pod) { p.Annotations["brewlet.sh/jdk"] = "21" }, "UnsupportedAnnotation"},
		"root":           {func(p *corev1.Pod) { p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr(int64(0))} }, "RootNotSupported"},
		"supplemental root": {func(p *corev1.Pod) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{SupplementalGroups: []int64{0}}
		}, "RootNotSupported"},
		"init privilege": {func(p *corev1.Pod) {
			p.Spec.InitContainers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr(true)}
		}, "PrivilegeEscalationNotSupported"},
		"ephemeral cap": {func(p *corev1.Pod) {
			p.Spec.EphemeralContainers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW"}}}
		}, "PrivilegeEscalationNotSupported"},
		"escalation": {func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: ptr(true)}
		}, "PrivilegeEscalationNotSupported"},
		"tag init":            {func(p *corev1.Pod) { p.Spec.InitContainers[0].Image = "registry.example.com/init:tag" }, "UnsupportedArtifact"},
		"wildcard toleration": {func(p *corev1.Pod) { p.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}} }, "StartupTaintTolerationNotSupported"},
		"key toleration": {func(p *corev1.Pod) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: api.StartupTaint, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
		}, "StartupTaintTolerationNotSupported"},
		"equal toleration": {func(p *corev1.Pod) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: api.StartupTaint, Value: "provisioning"}}
		}, "StartupTaintTolerationNotSupported"},
		"old barrier": {func(p *corev1.Pod) {
			p.Annotations[api.BarrierAnnotation] = `{"id":"python-3.12","inventoryUID":"old","generation":7}`
		}, "RuntimeImageEpochMismatch"},
		"malformed barrier": {func(p *corev1.Pod) {
			p.Annotations[api.BarrierAnnotation] = `{"id":"python-3.12","inventoryUID":"inventory","generation":0}`
		}, "InvalidRuntimeGeneration"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m, p := setup(t)
			tc.mutate(p)
			_, err := m.Mutate(context.Background(), p)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want %s, got %v", tc.reason, err)
			}
		})
	}
}
func TestAffinityConjunctionAndEmptySelectors(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{true: "empty-selector", false: "multiple-terms"}[empty], func(t *testing.T) {
			_, p := setup(t)
			p.Spec.NodeSelector = map[string]string{"kubernetes.io/arch": "arm64"}
			a := &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{}, PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{Weight: 2, Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpExists}}}}}}
			if !empty {
				a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"}}}}, {MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"node"}}}}, {}}
			}
			p.Spec.Affinity = &corev1.Affinity{NodeAffinity: a}
			InjectAffinity(p, "python-3.12")
			if empty && len(a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) != 0 {
				t.Fatal("empty selector became satisfiable")
			}
			if !empty {
				terms := a.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
				if len(terms) != 3 || len(terms[0].MatchExpressions) != 2 || len(terms[1].MatchFields) != 1 || len(terms[1].MatchExpressions) != 1 || len(terms[2].MatchExpressions) != 0 {
					t.Fatal("author constraints changed")
				}
			}
			if len(a.PreferredDuringSchedulingIgnoredDuringExecution) != 2 || p.Spec.NodeSelector["kubernetes.io/arch"] != "arm64" {
				t.Fatal("preferences or nodeSelector lost")
			}
		})
	}
}
func TestIneligibleProfileAndStartupFencedWarmNode(t *testing.T) {
	m, p := setup(t)
	c := m.Reader.(client.Client)
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n", UID: "node", Labels: map[string]string{api.OwnerUID: "profile", api.OwnerNodeUID: "node", api.RuntimeLabel: "ready", "brewlet.sh/runtime.python-3.12": "ready"}, Annotations: map[string]string{api.OwnerName: "web"}}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{{Key: api.StartupTaint, Value: "provisioning", Effect: corev1.TaintEffectNoSchedule}}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	if err := c.Create(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	w, err := m.Mutate(context.Background(), p)
	if err != nil || len(w) == 0 {
		t.Fatalf("startup-tainted node counted warm: %v, %v", w, err)
	}
	q := new(api.NodeProfile)
	_ = c.Get(context.Background(), client.ObjectKey{Name: "web"}, q)
	q.Spec.Registry.Mirrors = map[string]string{"registry.example.com": "bad.internal"}
	_ = c.Update(context.Background(), q)
	_, err = m.Mutate(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "NoCompatibleRuntime") {
		t.Fatalf("invalid profile authorized placement: %v", err)
	}
	b, _ := json.Marshal(p)
	if len(b) == 0 {
		t.Fatal("marshal failed")
	}
}

func TestDeletingAndConflictingProfilesDoNotOfferCapacity(t *testing.T) {
	for _, deleting := range []bool{true, false} {
		t.Run(map[bool]string{true: "deleting", false: "pool-conflict"}[deleting], func(t *testing.T) {
			m, p := setup(t)
			c := m.Reader.(client.Client)
			ctx := context.Background()
			q := new(api.NodeProfile)
			_ = c.Get(ctx, client.ObjectKey{Name: "web"}, q)
			if deleting {
				q.Finalizers = []string{"hold"}
				_ = c.Update(ctx, q)
				_ = c.Delete(ctx, q)
			} else {
				q.Name = "competing"
				q.UID = "other"
				q.ResourceVersion = ""
				if err := c.Create(ctx, q); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := m.Mutate(ctx, p); err == nil || !strings.Contains(err.Error(), "NoCompatibleRuntime") {
				t.Fatalf("ineligible inventory counted: %v", err)
			}
		})
	}
}
