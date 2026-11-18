package admission

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
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
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"runtime-image.brewlet.sh/app": r.Name, "runtime-image.brewlet.sh/init": r.Name,
		"runtime-image.brewlet.sh/debug": r.Name, "brewlet.sh/artifact-ref": "untrusted",
	}}, Spec: corev1.PodSpec{RuntimeClassName: ptr("brewlet"), Containers: []corev1.Container{{Name: "app", Image: "registry.example.com/app@" + digest}}, InitContainers: []corev1.Container{{Name: "init", Image: "registry.example.com/init@" + digest}}, EphemeralContainers: []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debug", Image: "registry.example.com/debug@" + digest}}}}}
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
		if s == nil || !*s.RunAsNonRoot || *s.AllowPrivilegeEscalation || s.ReadOnlyRootFilesystem == nil || !*s.ReadOnlyRootFilesystem || len(s.Capabilities.Drop) != 1 || s.Capabilities.Drop[0] != "ALL" {
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
func TestMixedPodSelectsIndependentRuntimesAndPreservesOCIContainers(t *testing.T) {
	m, p := setup(t)
	ctx := context.Background()
	c := m.Reader.(client.Client)
	source := "registry.example.com/java@sha256:" + strings.Repeat("b", 64)
	java := &api.RuntimeImage{
		ObjectMeta: metav1.ObjectMeta{Name: "java-25", UID: "java-inventory", Generation: 1},
		Spec:       api.RuntimeImageSpec{Source: api.Source{Image: source}, User: api.User{UID: 3000, GID: 4000}},
		Status: api.RuntimeImageStatus{Generation: 2, Digest: "sha256:" + strings.Repeat("b", 64), SourceImage: source,
			Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}},
	}
	if err := c.Create(ctx, java); err != nil {
		t.Fatal(err)
	}
	profile := new(api.NodeProfile)
	if err := c.Get(ctx, client.ObjectKey{Name: "web"}, profile); err != nil {
		t.Fatal(err)
	}
	profile.Spec.RuntimeImages = append(profile.Spec.RuntimeImages, api.RuntimeEntry{ID: java.Name, Install: "Eager"})
	if err := c.Update(ctx, profile); err != nil {
		t.Fatal(err)
	}
	delete(p.Annotations, "runtime-image.brewlet.sh/init")
	delete(p.Annotations, "runtime-image.brewlet.sh/debug")
	p.Annotations["runtime-image.brewlet.sh/app"] = java.Name
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "worker", Image: "registry.example.com/worker@sha256:" + strings.Repeat("c", 64)})
	p.Annotations["runtime-image.brewlet.sh/worker"] = "python-3.12"
	p.Spec.InitContainers[0].Image = "registry.example.com/migrator:latest"
	p.Spec.InitContainers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr(true), ReadOnlyRootFilesystem: ptr(false)}
	p.Spec.EphemeralContainers[0].Image = "registry.example.com/debug:latest"
	p.Spec.EphemeralContainers[0].SecurityContext = &corev1.SecurityContext{RunAsUser: ptr(int64(0)), Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW"}}}
	ociInitBefore := p.Spec.InitContainers[0].DeepCopy()
	ociDebugBefore := p.Spec.EphemeralContainers[0].DeepCopy()
	warnings, err := m.Mutate(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "runtime capacity pending") {
		t.Fatalf("unexpected capacity warning: %v", warnings)
	}
	for _, container := range p.Spec.Containers {
		security := container.SecurityContext
		expectedUID, expectedGID := int64(3000), int64(4000)
		if container.Name == "worker" {
			expectedUID, expectedGID = 1000, 2000
		}
		if security == nil || security.RunAsUser == nil || *security.RunAsUser != expectedUID ||
			security.RunAsGroup == nil || *security.RunAsGroup != expectedGID ||
			security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem {
			t.Fatalf("managed container %q did not receive its own runtime defaults: %+v", container.Name, security)
		}
	}
	if !reflect.DeepEqual(&p.Spec.InitContainers[0], ociInitBefore) ||
		!reflect.DeepEqual(&p.Spec.EphemeralContainers[0], ociDebugBefore) {
		t.Fatal("ordinary OCI containers were mutated")
	}
	if p.Annotations["brewlet.sh/artifact-container"] != "app" ||
		p.Annotations["brewlet.sh/artifact-digest"] != "sha256:"+strings.Repeat("a", 64) {
		t.Fatal("artifact hints were not tied to a selected container")
	}
	terms := p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 {
		t.Fatalf("unexpected affinity terms: %+v", terms)
	}
	keys := map[string]bool{}
	for _, requirement := range terms[0].MatchExpressions {
		keys[requirement.Key] = true
	}
	if !keys["brewlet.sh/runtime.java-25"] || !keys["brewlet.sh/runtime.python-3.12"] {
		t.Fatalf("joint placement does not require both runtime IDs: %+v", terms[0].MatchExpressions)
	}
}

func TestMixedPodRejectsDisjointRuntimeProfiles(t *testing.T) {
	m, p := setup(t)
	p.Annotations["runtime-image.brewlet.sh/worker"] = "java-25"
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "worker", Image: "registry.example.com/worker@sha256:" + strings.Repeat("b", 64)})
	source := "registry.example.com/java@sha256:" + strings.Repeat("b", 64)
	java := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "java-25", UID: "java-inventory", Generation: 1},
		Spec: api.RuntimeImageSpec{Source: api.Source{Image: source}, User: api.User{UID: 3000, GID: 4000}},
		Status: api.RuntimeImageStatus{Generation: 2, Digest: "sha256:" + strings.Repeat("b", 64), SourceImage: source,
			Conditions: []metav1.Condition{{Type: "Approved", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	if err := m.Reader.(client.Client).Create(context.Background(), java); err != nil {
		t.Fatal(err)
	}
	profile := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "java", UID: "java-profile"},
		Spec: api.NodeProfileSpec{NodePool: api.NodePool{Names: []string{"java"}, Key: "agentpool"}, RuntimeImages: []api.RuntimeEntry{{ID: java.Name, Install: "Eager"}}}}
	if err := m.Reader.(client.Client).Create(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Mutate(context.Background(), p); err == nil || !strings.Contains(err.Error(), "NoCompatibleRuntime") {
		t.Fatalf("disjoint profiles jointly authorized a Pod: %v", err)
	}
}

func TestBoundPodRequiresEverySelectedRuntimeOnItsOwnedNode(t *testing.T) {
	m, p := setup(t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker", UID: "node-uid",
			Labels: map[string]string{
				api.RuntimeLabel:                 "ready",
				api.OwnerUID:                     "profile",
				api.OwnerNodeUID:                 "node-uid",
				"brewlet.sh/runtime.python-3.12": "available",
			},
			Annotations: map[string]string{api.OwnerName: "web"},
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionTrue,
		}}},
	}
	if err := m.Reader.(client.Client).Create(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	p.Spec.NodeName = node.Name
	if _, err := m.Mutate(context.Background(), p); err != nil {
		t.Fatalf("bound node offering the selected OnDemand runtime was rejected: %v", err)
	}
	node.Labels["brewlet.sh/runtime.python-3.12"] = ""
	if err := m.Reader.(client.Client).Update(context.Background(), node); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Mutate(context.Background(), p); err == nil || !strings.Contains(err.Error(), "NoCompatibleRuntime") {
		t.Fatalf("bound node without the selected runtime was accepted: %v", err)
	}
}

func TestSecurityAndSelectionDenials(t *testing.T) {
	cases := map[string]struct {
		mutate func(*corev1.Pod)
		reason string
	}{
		"empty runtime":  {func(p *corev1.Pod) { p.Annotations["runtime-image.brewlet.sh/app"] = "" }, "InvalidRuntimeImageID"},
		"digest runtime": {func(p *corev1.Pod) { p.Annotations["runtime-image.brewlet.sh/app"] = "python@sha256:a" }, "InvalidRuntimeImageID"},
		"legacy":         {func(p *corev1.Pod) { p.Annotations["brewlet.sh/jdk"] = "21" }, "UnsupportedAnnotation"},
		"root":           {func(p *corev1.Pod) { p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: ptr(int64(0))} }, "RootNotSupported"},
		"primary root group": {func(p *corev1.Pod) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsGroup: ptr(int64(0))}
		}, "RootNotSupported"},
		"pod permits root": {func(p *corev1.Pod) {
			p.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr(false)}
		}, "RootNotSupported"},
		"container permits root": {func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{RunAsNonRoot: ptr(false)}
		}, "RootNotSupported"},
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
			p.Annotations["runtime-generation.brewlet.sh/python-3.12"] = `{"id":"python-3.12","inventoryUID":"old","generation":7}`
		}, "RuntimeImageEpochMismatch"},
		"malformed barrier": {func(p *corev1.Pod) {
			p.Annotations["runtime-generation.brewlet.sh/python-3.12"] = `{"id":"python-3.12","inventoryUID":"inventory","generation":0}`
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

func TestAdmissionRejectsAmbiguousGenerationBarriersLikeShim(t *testing.T) {
	for _, raw := range []string{
		``,
		`{"id":"foreign","id":"python-3.12","inventoryUID":"inventory","generation":7}`,
		`{"id":"python-3.12","inventoryUID":"old","inventoryUID":"inventory","generation":7}`,
		`{"id":"python-3.12","inventoryUID":"inventory","generation":99,"generation":7}`,
		`{"id":"python-3.12","inventoryUID":"inventory","generation":99,"generat\u0069on":7}`,
		`{"id":"python-3.12","inventoryUID":"inventory","generation":7.0}`,
		`{"id":"python-3.12","inventoryUID":"inventory","generation":7e0}`,
		`{"id":"python-3.12","inventoryUID":"inventory","generation":null}`,
		`{"id":"python-3.12","inventoryUID":"inventory","generation":9223372036854775808}`,
	} {
		t.Run(raw, func(t *testing.T) {
			m, pod := setup(t)
			pod.Annotations["runtime-generation.brewlet.sh/python-3.12"] = raw
			before := pod.DeepCopy()
			if _, err := m.Mutate(context.Background(), pod); err == nil ||
				!strings.HasPrefix(err.Error(), "InvalidRuntimeGeneration") {
				t.Fatalf("ambiguous barrier did not fail with shim reason: %v", err)
			}
			if !reflect.DeepEqual(pod, before) {
				t.Fatal("invalid barrier mutated the Pod")
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
