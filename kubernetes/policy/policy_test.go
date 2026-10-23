package policy

import (
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func image() string { return "registry.example.com/golden/python@sha256:" + strings.Repeat("a", 64) }
func TestIDAndSource(t *testing.T) {
	for _, id := range []string{"python-3.12", "java-21", "static", strings.Repeat("a", 55)} {
		if !ValidID(id) {
			t.Errorf("valid ID rejected %q", id)
		}
	}
	for _, id := range []string{"", "A", "x..y", ".x", "x.", "x/y", "x:tag", "x@sha256:a", strings.Repeat("a", 56), "-x", "x-"} {
		if ValidID(id) {
			t.Errorf("invalid ID accepted %q", id)
		}
	}
	if err := Source(image()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"python", "python@sha256:" + strings.Repeat("a", 64), "registry.example.com/golden/python:tag@sha256:" + strings.Repeat("a", 64), "registry.example.com/x:latest", "registry.example.com/x@sha256:" + strings.Repeat("A", 64), "https://registry.example.com/x@sha256:" + strings.Repeat("a", 64)} {
		if Source(s) == nil {
			t.Errorf("invalid source accepted %q", s)
		}
	}
}
func TestImmutableUserAndCompatibility(t *testing.T) {
	r := &api.RuntimeImage{ObjectMeta: metav1.ObjectMeta{Name: "python-3.12"}, Spec: api.RuntimeImageSpec{Source: api.Source{Image: image()}, User: api.User{UID: 1000, GID: 1000}}}
	if err := Runtime(r, nil); err != nil {
		t.Fatal(err)
	}
	old := r.DeepCopy()
	r.Spec.User.GID = 0
	if Runtime(r, nil) == nil {
		t.Fatal("root GID accepted")
	}
	r = old.DeepCopy()
	r.Spec.User.UID = 1001
	if Runtime(r, old) == nil {
		t.Fatal("identity update accepted")
	}
	r = old.DeepCopy()
	r.Spec.Compatibility.Ecosystem = "changed"
	if Runtime(r, old) == nil {
		t.Fatal("compatibility update accepted")
	}
	r = old.DeepCopy()
	r.Spec.VolumeMountPaths = []string{"/tmp"}
	if Runtime(r, old) == nil {
		t.Fatal("mount policy update accepted within an inventory epoch")
	}
	if err := Runtime(r, nil); err != nil {
		t.Fatalf("new explicit data mount policy rejected: %v", err)
	}
}
func TestProfilePolicy(t *testing.T) {
	base := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: api.NodeProfileSpec{NodePool: api.NodePool{Names: []string{"web"}, Key: "agentpool"}, RuntimeImages: []api.RuntimeEntry{{ID: "python-3.12"}}}}
	if err := Profile(base, nil, nil); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*api.NodeProfile){
		"duplicate":  func(p *api.NodeProfile) { p.Spec.RuntimeImages = append(p.Spec.RuntimeImages, p.Spec.RuntimeImages[0]) },
		"idle eager": func(p *api.NodeProfile) { p.Spec.RuntimeImages[0].IdleTTL = "1h" },
		"invalid duration": func(p *api.NodeProfile) {
			p.Spec.RuntimeImages[0].Install = "OnDemand"
			p.Spec.RuntimeImages[0].IdleTTL = "-1s"
		},
		"restart": func(p *api.NodeProfile) { p.Spec.Rollout.ContainerdRestart = "always" },
		"mirror": func(p *api.NodeProfile) {
			p.Spec.Registry.Mirrors = map[string]string{"registry.example.com": "evil.example.com/x"}
		},
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			p := base.DeepCopy()
			fn(p)
			if Profile(p, nil, nil) == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
	q := base.DeepCopy()
	q.Name = "other"
	if Profile(base, []api.NodeProfile{*q}, nil) == nil {
		t.Fatal("overlap accepted")
	}
	q.Spec.NodePool.Names = []string{"other"}
	q.Spec.NodePool.Key = ""
	if Profile(base, []api.NodeProfile{*q}, nil) == nil {
		t.Fatal("mixed explicit/auto accepted")
	}
	if MirrorPolicy(map[string]string{"registry.example.com": "mirror.internal/base"}, []string{"mirror.internal"}) != nil {
		t.Fatal("valid mirror rejected")
	}
	if MirrorPolicy(map[string]string{"registry.example.com": "mirror.internal.evil/base"}, []string{"mirror.internal"}) == nil {
		t.Fatal("suffix allowlist bypass")
	}
}

func TestRuntimeEntryInstallModesAndIdleTTL(t *testing.T) {
	for _, tc := range []struct {
		mode, ttl string
		valid     bool
	}{
		{"", "", true},
		{"Eager", "", true},
		{"OnDemand", "", true},
		{"OnDemand", "1h30m", true},
		{"", "1h", false},
		{"Eager", "1h", false},
		{"OnDemand", "0s", false},
		{"OnDemand", "-1s", false},
		{"OnDemand", "never", false},
		{"ondemand", "", false},
		{"Lazy", "", false},
	} {
		t.Run(tc.mode+"/"+tc.ttl, func(t *testing.T) {
			entry := api.RuntimeEntry{ID: "static", Install: tc.mode, IdleTTL: tc.ttl}
			profile := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "profile"},
				Spec: api.NodeProfileSpec{RuntimeImages: []api.RuntimeEntry{entry}}}
			if err := Profile(profile, nil, nil); (err == nil) != tc.valid {
				t.Fatalf("install=%q idleTTL=%q: %v", tc.mode, tc.ttl, err)
			}
			if tc.mode == "" && entry.Mode() != "Eager" {
				t.Fatal("omitted install mode does not default to Eager")
			}
		})
	}
}

func TestMirrorPolicyRejectsInvalidConfigurationBeforeProvisioning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mirrors map[string]string
		allowed []string
	}{
		{"self mirror", map[string]string{"registry.example.com": "registry.example.com/base"}, []string{"registry.example.com"}},
		{"duplicate allowlist", nil, []string{"mirror.internal", "mirror.internal"}},
		{"empty allowlist host", nil, []string{""}},
		{"allowlist whitespace", nil, []string{" mirror.internal"}},
		{"allowlist scheme", nil, []string{"https://mirror.internal"}},
		{"allowlist path", nil, []string{"mirror.internal/base"}},
		{"allowlist digest", nil, []string{"mirror.internal@sha256:abc"}},
		{"invalid allowlist authority", nil, []string{"mirror.internal:invalid"}},
		{"source path", map[string]string{"registry.example.com/base": "mirror.internal/base"}, []string{"mirror.internal"}},
		{"destination tag", map[string]string{"registry.example.com": "mirror.internal/base:tag"}, []string{"mirror.internal"}},
		{"destination digest", map[string]string{"registry.example.com": "mirror.internal/base@sha256:abc"}, []string{"mirror.internal"}},
		{"suffix bypass", map[string]string{"registry.example.com": "mirror.internal.evil/base"}, []string{"mirror.internal"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &api.NodeProfile{Spec: api.NodeProfileSpec{
				RuntimeImages: []api.RuntimeEntry{{ID: "static"}},
				Registry:      api.Registry{Mirrors: tc.mirrors},
			}}
			if err := MirrorPolicy(tc.mirrors, tc.allowed); err == nil {
				t.Fatal("invalid mirror configuration accepted")
			}
			if err := Profile(p, nil, tc.allowed); err == nil {
				t.Fatal("profile bypassed shared mirror validation")
			}
		})
	}
	for _, host := range []string{"mirror.internal", "mirror.internal:5000", "[::1]:5000", "docker.io", "localhost:5000"} {
		if err := MirrorPolicy(map[string]string{"registry.example.com": host + "/base"}, []string{host}); err != nil {
			t.Fatalf("valid registry authority %q rejected: %v", host, err)
		}
	}

}

func TestRetainedProfilePlacementAndRolloutPolicy(t *testing.T) {
	base := &api.NodeProfile{ObjectMeta: metav1.ObjectMeta{Name: "web"}, Spec: api.NodeProfileSpec{
		NodePool:      api.NodePool{Names: []string{"web"}, Key: "agentpool"},
		RuntimeImages: []api.RuntimeEntry{{ID: "static"}},
	}}
	for _, tc := range []struct {
		name  string
		alter func(*api.NodeProfile)
		valid bool
	}{
		{"validated default", func(*api.NodeProfile) {}, true},
		{"validated explicit", func(p *api.NodeProfile) { p.Spec.Rollout.ContainerdRestart = "validated" }, true},
		{"none", func(p *api.NodeProfile) { p.Spec.Rollout.ContainerdRestart = "none" }, true},
		{"disabled", func(p *api.NodeProfile) { p.Spec.Rollout.ContainerdRestart = "disabled" }, false},
		{"negative availability", func(p *api.NodeProfile) { p.Spec.Rollout.MaxUnavailable = -1 }, false},
		{"explicit toleration", func(p *api.NodeProfile) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Value: "web", Effect: corev1.TaintEffectNoSchedule}}
		}, true},
		{"keyed Exists", func(p *api.NodeProfile) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}
		}, true},
		{"wildcard toleration", func(p *api.NodeProfile) {
			p.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
		}, false},
		{"Exists with value", func(p *api.NodeProfile) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists, Value: "web"}}
		}, false},
		{"invalid effect", func(p *api.NodeProfile) {
			p.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Effect: "Always"}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := base.DeepCopy()
			tc.alter(p)
			if err := Profile(p, nil, nil); (err == nil) != tc.valid {
				t.Fatalf("policy validity should be %t: %v", tc.valid, err)
			}
		})
	}
	for _, tc := range []struct {
		name        string
		pool, other api.NodePool
		valid       bool
	}{
		{"multiple catchalls", api.NodePool{}, api.NodePool{}, false},
		{"catchall and named", api.NodePool{}, base.Spec.NodePool, true},
		{"named and catchall", base.Spec.NodePool, api.NodePool{}, true},
		{"overlap", base.Spec.NodePool, base.Spec.NodePool, false},
		{"disjoint explicit", base.Spec.NodePool, api.NodePool{Key: "agentpool", Names: []string{"batch"}}, true},
		{"disjoint auto", api.NodePool{Names: []string{"web"}}, api.NodePool{Names: []string{"batch"}}, true},
		{"mixed keys", base.Spec.NodePool, api.NodePool{Key: "other", Names: []string{"batch"}}, false},
		{"explicit and auto", base.Spec.NodePool, api.NodePool{Names: []string{"batch"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, q := base.DeepCopy(), base.DeepCopy()
			p.Spec.NodePool, q.Spec.NodePool = tc.pool, tc.other
			q.Name = "other"
			if err := Profile(p, []api.NodeProfile{*q}, nil); (err == nil) != tc.valid {
				t.Fatalf("pool validity should be %t: %v", tc.valid, err)
			}
		})
	}
}
