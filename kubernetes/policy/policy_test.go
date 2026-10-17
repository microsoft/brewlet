package policy

import (
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v2alpha1"
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
