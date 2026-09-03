// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"strings"
	"testing"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testImageDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

func jdk(dist string, feature int32) nodev1alpha1.JDKRef {
	return nodev1alpha1.JDKRef{
		Distribution: dist,
		Feature:      feature,
		Source: nodev1alpha1.JDKSource{
			Image:    "registry.example.com/jdks/" + dist + "@" + testImageDigest,
			JavaHome: "/opt/jdk",
		},
	}
}

func launcher(name string) nodev1alpha1.LauncherRef {
	return nodev1alpha1.LauncherRef{
		Name: name,
		Source: nodev1alpha1.LauncherSource{
			Image: "registry.example.com/launchers/" + name + "@" + testImageDigest,
			Path:  "/usr/bin/" + name,
		},
	}
}

func profileNamed(name string, names []string, jdks ...nodev1alpha1.JDKRef) nodev1alpha1.NodeProfile {
	return nodev1alpha1.NodeProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: nodev1alpha1.NodeProfileSpec{
			NodePool: nodev1alpha1.NodePoolRef{Names: names},
			JDKs:     jdks,
		},
	}
}

func labeledNode(name string, labels map[string]string) corev1.Node {
	return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

func TestValidateNodeProfile(t *testing.T) {
	cases := []struct {
		name    string
		spec    nodev1alpha1.NodeProfileSpec
		wantErr bool
	}{
		{
			name:    "valid",
			spec:    nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 21)}},
			wantErr: false,
		},
		{
			name:    "empty jdks rejected",
			spec:    nodev1alpha1.NodeProfileSpec{JDKs: nil},
			wantErr: true,
		},
		{
			name:    "JDK without source rejected",
			spec:    nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{Distribution: "temurin", Feature: 21}}},
			wantErr: true,
		},
		{
			name: "custom distribution accepted",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: nodev1alpha1.JDKSource{
					Image:    "docker.io/library/azul-zulu@" + testImageDigest,
					JavaHome: "/usr/lib/jvm/zulu21",
				},
			}}},
			wantErr: false,
		},
		{
			name: "temurin explicit source accepted",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "temurin",
				Feature:      21,
				Source: nodev1alpha1.JDKSource{
					Image:    "registry.example.com/jdk@" + testImageDigest,
					JavaHome: "/opt/jdk",
				},
			}}},
			wantErr: false,
		},
		{
			name: "mutable custom image tag rejected",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: nodev1alpha1.JDKSource{
					Image:    "docker.io/library/azul-zulu:21",
					JavaHome: "/usr/lib/jvm/zulu21",
				},
			}}},
			wantErr: true,
		},
		{
			name: "custom image must be fully qualified",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: nodev1alpha1.JDKSource{
					Image:    "azul-zulu:21",
					JavaHome: "/usr/lib/jvm/zulu21",
				},
			}}},
			wantErr: true,
		},
		{
			name: "custom image must be a valid OCI reference",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: nodev1alpha1.JDKSource{
					Image:    "registry.example.com/jdk:bad:tag",
					JavaHome: "/usr/lib/jvm/zulu21",
				},
			}}},
			wantErr: true,
		},
		{
			name: "custom java home must be clean and absolute",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "zulu",
				Feature:      21,
				Source: nodev1alpha1.JDKSource{
					Image:    "docker.io/library/azul-zulu@" + testImageDigest,
					JavaHome: "/usr/lib/../jdk",
				},
			}}},
			wantErr: true,
		},
		{
			name: "JDK token must fit capability label",
			spec: nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{{
				Distribution: "this-distribution-name-is-far-too-long-for-a-jdk-label-xx",
				Feature:      21,
				Source: nodev1alpha1.JDKSource{
					Image:    "registry.example.com/jdk@" + testImageDigest,
					JavaHome: "/opt/jdk",
				},
			}}},
			wantErr: true,
		},
		{
			name: "launcher accepted",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs:      []nodev1alpha1.JDKRef{jdk("temurin", 21)},
				Launchers: []nodev1alpha1.LauncherRef{launcher("jaz")},
			},
			wantErr: false,
		},
		{
			name: "launcher source required",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs:      []nodev1alpha1.JDKRef{jdk("temurin", 21)},
				Launchers: []nodev1alpha1.LauncherRef{{Name: "jaz"}},
			},
			wantErr: true,
		},
		{
			name: "java launcher is implicit",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs:      []nodev1alpha1.JDKRef{jdk("temurin", 21)},
				Launchers: []nodev1alpha1.LauncherRef{launcher("java")},
			},
			wantErr: true,
		},
		{
			name: "duplicate launcher rejected",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs:      []nodev1alpha1.JDKRef{jdk("temurin", 21)},
				Launchers: []nodev1alpha1.LauncherRef{launcher("jaz"), launcher("jaz")},
			},
			wantErr: true,
		},
		{
			name: "launcher path must be clean and absolute",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 21)},
				Launchers: []nodev1alpha1.LauncherRef{{
					Name: "jaz",
					Source: nodev1alpha1.LauncherSource{
						Image: "registry.example.com/launchers/jaz@" + testImageDigest,
						Path:  "/usr/../jaz",
					},
				}},
			},
			wantErr: true,
		},
		{
			name:    "non-positive feature rejected",
			spec:    nodev1alpha1.NodeProfileSpec{JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 0)}},
			wantErr: true,
		},
		{
			name: "bad containerdRestart rejected",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs:    []nodev1alpha1.JDKRef{jdk("microsoft", 25)},
				Rollout: nodev1alpha1.RolloutSpec{ContainerdRestart: "reboot"},
			},
			wantErr: true,
		},
		{
			name: "label-only containerdRestart=none accepted",
			spec: nodev1alpha1.NodeProfileSpec{
				JDKs:    []nodev1alpha1.JDKRef{jdk("microsoft", 25)},
				Rollout: nodev1alpha1.RolloutSpec{ContainerdRestart: "none"},
			},
			wantErr: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &nodev1alpha1.NodeProfile{Spec: tc.spec}
			err := ValidateNodeProfile(p)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateNodeProfile() err=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestNodeProfilePolicyMirrors(t *testing.T) {
	profile := &nodev1alpha1.NodeProfile{Spec: nodev1alpha1.NodeProfileSpec{
		JDKs: []nodev1alpha1.JDKRef{jdk("temurin", 21)},
		Registry: &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{
			"docker.io": "registry.internal/dockerhub",
		}},
	}}
	policy := NodeProfilePolicy{AllowedSourceMirrorHosts: []string{"registry.internal"}}
	if err := policy.Validate(profile); err != nil {
		t.Fatalf("approved mirror rejected: %v", err)
	}

	cases := []struct {
		name    string
		source  string
		target  string
		allowed []string
	}{
		{name: "mirrors disabled", source: "docker.io", target: "registry.internal/cache"},
		{name: "unapproved host", source: "docker.io", target: "evil.example/cache", allowed: []string{"registry.internal"}},
		{name: "source scheme", source: "https://docker.io", target: "registry.internal/cache", allowed: []string{"registry.internal"}},
		{name: "target scheme", source: "docker.io", target: "https://registry.internal/cache", allowed: []string{"registry.internal"}},
		{name: "target whitespace", source: "docker.io", target: "registry.internal/bad path", allowed: []string{"registry.internal"}},
		{name: "uppercase mirror host", source: "docker.io", target: "REGISTRY.internal/cache", allowed: []string{"REGISTRY.internal"}},
		{name: "self mirror", source: "docker.io", target: "docker.io/cache", allowed: []string{"docker.io"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := profile.DeepCopy()
			p.Spec.Registry.Mirrors = map[string]string{tc.source: tc.target}
			if err := (NodeProfilePolicy{AllowedSourceMirrorHosts: tc.allowed}).Validate(p); err == nil {
				t.Fatal("expected mirror policy rejection")
			}
		})
	}
}

func TestParseAllowedSourceMirrorHosts(t *testing.T) {
	got, err := ParseAllowedSourceMirrorHosts("registry.internal,mirror.example.com:5000")
	if err != nil {
		t.Fatalf("ParseAllowedSourceMirrorHosts() error: %v", err)
	}
	if strings.Join(got, ",") != "registry.internal,mirror.example.com:5000" {
		t.Fatalf("parsed hosts = %v", got)
	}
	for _, value := range []string{
		"registry.internal,",
		"registry.internal,,mirror.example.com",
		"registry.internal,registry.internal",
		"https://registry.internal",
		"registry.internal/cache",
		" registry.internal",
		"REGISTRY.internal",
		"registry:5000",
		strings.Repeat("a", 64) + ".example.com",
		"registry.internal:0",
		"registry.internal:65536",
	} {
		if _, err := ParseAllowedSourceMirrorHosts(value); err == nil {
			t.Errorf("expected %q to be rejected", value)
		}
	}
}

func TestValidateNoPoolConflicts(t *testing.T) {
	batchA := profileNamed("team-a", []string{"batch"}, jdk("temurin", 21))
	batchB := profileNamed("team-b", []string{"batch", "edge"}, jdk("temurin", 21))
	edge := profileNamed("edge", []string{"edge-only"}, jdk("temurin", 21))

	if err := ValidateNoPoolConflicts(&batchB, []nodev1alpha1.NodeProfile{batchA}); err == nil {
		t.Fatal("expected conflict: two profiles naming pool 'batch'")
	}
	if err := ValidateNoPoolConflicts(&edge, []nodev1alpha1.NodeProfile{batchA}); err != nil {
		t.Fatalf("disjoint pools must not conflict: %v", err)
	}
	// A profile compared against itself in the list is not a self-conflict.
	if err := ValidateNoPoolConflicts(&batchA, []nodev1alpha1.NodeProfile{batchA}); err != nil {
		t.Fatalf("self must not conflict: %v", err)
	}
}

func TestResolvePoolKey(t *testing.T) {
	gke := []corev1.Node{
		labeledNode("n1", map[string]string{"cloud.google.com/gke-nodepool": "general"}),
	}
	p := profileNamed("p", []string{"general"}, jdk("temurin", 21))
	if got := resolvePoolKey(&p, gke); got != "cloud.google.com/gke-nodepool" {
		t.Fatalf("GKE auto-detect = %q", got)
	}

	// Explicit key overrides auto-detection.
	pk := p
	pk.Spec.NodePool.Key = "brewlet.sh/pool"
	if got := resolvePoolKey(&pk, gke); got != "brewlet.sh/pool" {
		t.Fatalf("explicit key = %q, want brewlet.sh/pool", got)
	}

	// Bare-metal (no provider labels) resolves to empty.
	bare := []corev1.Node{labeledNode("b1", map[string]string{"kubernetes.io/arch": "amd64"})}
	if got := resolvePoolKey(&p, bare); got != "" {
		t.Fatalf("bare-metal key = %q, want empty", got)
	}
}

func TestNamedPoolsExcept(t *testing.T) {
	profiles := []nodev1alpha1.NodeProfile{
		profileNamed("a", []string{"batch"}, jdk("temurin", 21)),
		profileNamed("b", []string{"edge", "batch"}, jdk("temurin", 21)),
		profileNamed("default", nil, jdk("temurin", 21)),
	}
	got := namedPoolsExcept(profiles, "a")
	// Should include edge + batch (from b), deduped; exclude a's contribution.
	set := map[string]bool{}
	for _, s := range got {
		set[s] = true
	}
	if !set["edge"] || !set["batch"] {
		t.Fatalf("namedPoolsExcept = %v, want to contain edge+batch", got)
	}
}

func TestMirrorEnv(t *testing.T) {
	p := &nodev1alpha1.NodeProfile{Spec: nodev1alpha1.NodeProfileSpec{
		Registry: &nodev1alpha1.RegistrySpec{Mirrors: map[string]string{
			"mcr.microsoft.com": "registry.internal/mcr",
			"docker.io":         "registry.internal/dockerhub",
		}},
	}}
	// Deterministic (sorted by host).
	want := "docker.io=registry.internal/dockerhub,mcr.microsoft.com=registry.internal/mcr"
	if got := mirrorEnv(p); got != want {
		t.Fatalf("mirrorEnv = %q, want %q", got, want)
	}
	if got := mirrorEnv(&nodev1alpha1.NodeProfile{}); got != "" {
		t.Fatalf("no-registry mirrorEnv = %q, want empty", got)
	}
}

func TestJDKRefToken(t *testing.T) {
	if got := jdk("temurin", 21).Token(); got != "temurin-21" {
		t.Fatalf("Token() = %q, want temurin-21", got)
	}
}

func TestJDKSourceEnv(t *testing.T) {
	p := profileNamed("custom", nil,
		jdk("temurin", 21),
		nodev1alpha1.JDKRef{
			Distribution: "zulu",
			Feature:      21,
			Source: nodev1alpha1.JDKSource{
				Image:    "docker.io/library/azul-zulu@" + testImageDigest,
				JavaHome: "/usr/lib/jvm/zulu21",
			},
		},
	)
	got := map[string]string{}
	for _, item := range jdkSourceEnv(&p) {
		got[item.Name] = item.Value
	}
	want := map[string]string{
		"JDK_SOURCE_COUNT":       "2",
		"JDK_SOURCE_0_TOKEN":     "temurin-21",
		"JDK_SOURCE_1_TOKEN":     "zulu-21",
		"JDK_SOURCE_1_IMAGE":     "docker.io/library/azul-zulu@" + testImageDigest,
		"JDK_SOURCE_1_JAVA_HOME": "/usr/lib/jvm/zulu21",
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func TestProfileDaemonSetMirrorAllowlistEnv(t *testing.T) {
	cfg := testConfig()
	cfg.AllowedSourceMirrorHosts = []string{"registry.internal", "mirror.example.com:5000"}
	p := profileNamed("mirrored", nil, jdk("temurin", 21))
	ds := buildProfileDaemonSet(cfg, &p, "", nil)
	for _, item := range ds.Spec.Template.Spec.Containers[0].Env {
		if item.Name == "SOURCE_ALLOWED_MIRROR_HOSTS" {
			if item.Value != "registry.internal,mirror.example.com:5000" {
				t.Fatalf("SOURCE_ALLOWED_MIRROR_HOSTS = %q", item.Value)
			}
			return
		}
	}
	t.Fatal("SOURCE_ALLOWED_MIRROR_HOSTS env missing")
}

func TestCleanupDaemonSetBuilder(t *testing.T) {
	cfg := testConfig()
	p := profileNamed("batch", []string{"batch"}, jdk("temurin", 21))
	ds := buildCleanupDaemonSet(cfg, &p, "cloud.google.com/gke-nodepool", nil)
	if ds.Name != brewlet.CleanupDaemonSetName("batch") {
		t.Fatalf("cleanup ds name = %q", ds.Name)
	}
	var mode string
	for _, e := range ds.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "BREWLET_MODE" {
			mode = e.Value
		}
	}
	if mode != "cleanup" {
		t.Fatalf("cleanup ds BREWLET_MODE = %q, want cleanup", mode)
	}
	if len(ds.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("cleanup ds containers = %d, want no metrics sidecar", len(ds.Spec.Template.Spec.Containers))
	}
}
