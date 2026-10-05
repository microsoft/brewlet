// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"io"
	"os"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestNodeProfileCompletionReadinessProbe(t *testing.T) {
	cfg := testConfig()
	cfg.MetricsEnabled = true
	p := profileNamed("completion", []string{"workers"}, jdk("temurin", 21))
	for name, ds := range map[string]*appsv1.DaemonSet{
		"provisioner": buildProfileDaemonSet(cfg, &p, "agentpool", nil),
		"cleanup":     buildCleanupDaemonSet(cfg, &p, "agentpool", nil),
	} {
		t.Run(name, func(t *testing.T) {
			assertCompletionReadinessProbe(t, ds.Spec.Template.Spec.Containers[0])
		})
	}
}

func TestManagedProvisionerRawPrerequisites(t *testing.T) {
	file, err := os.Open("../../deploy/provisioner-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	seen := map[string]bool{}
	for {
		var doc *struct {
			metav1.TypeMeta `json:",inline"`
			Metadata        metav1.ObjectMeta   `json:"metadata"`
			Rules           []rbacv1.PolicyRule `json:"rules"`
			RoleRef         rbacv1.RoleRef      `json:"roleRef"`
			Subjects        []rbacv1.Subject    `json:"subjects"`
		}
		if err := decoder.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if doc == nil {
			continue
		}
		if seen[doc.Kind] {
			t.Fatalf("duplicate prerequisite kind %s", doc.Kind)
		}
		seen[doc.Kind] = true
		wantName, wantNamespace, wantAPI := "brewlet-node-provisioner", "", "v1"
		switch doc.Kind {
		case "Namespace":
			wantName = "brewlet"
		case "ServiceAccount":
			wantNamespace = "brewlet"
		case "ClusterRole":
			wantAPI = rbacv1.SchemeGroupVersion.String()
			want := []rbacv1.PolicyRule{
				{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get", "list", "watch", "patch", "update"}},
				{APIGroups: []string{"node.brewlet.sh"}, Resources: []string{"nodeprofiles"}, Verbs: []string{"get"}},
			}
			if !reflect.DeepEqual(doc.Rules, want) {
				t.Fatalf("unexpected provisioner permissions: %+v", doc.Rules)
			}
		case "ClusterRoleBinding":
			wantAPI = rbacv1.SchemeGroupVersion.String()
			wantRef := rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: wantName}
			wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: wantName, Namespace: "brewlet"}}
			if doc.RoleRef != wantRef || !reflect.DeepEqual(doc.Subjects, wantSubjects) {
				t.Fatalf("incorrect provisioner binding: %+v", doc)
			}
		default:
			t.Fatalf("unexpected raw prerequisite %q; no standalone workloads may be shipped", doc.Kind)
		}
		if doc.Metadata.Name != wantName || doc.Metadata.Namespace != wantNamespace || doc.APIVersion != wantAPI {
			t.Fatalf("incorrect prerequisite identity: %+v", doc)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("missing raw prerequisites: %v", seen)
	}
}

func assertCompletionReadinessProbe(t *testing.T, container corev1.Container) {
	t.Helper()
	probe := container.ReadinessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatal("completion readiness exec probe missing")
	}
	want := []string{"/usr/bin/test", "-f", "/tmp/brewlet-complete"}
	if !reflect.DeepEqual(probe.Exec.Command, want) {
		t.Fatalf("readiness command = %v, want %v", probe.Exec.Command, want)
	}
	if probe.FailureThreshold != 1 || probe.SuccessThreshold != 1 || probe.PeriodSeconds != 2 || probe.TimeoutSeconds != 1 {
		t.Fatalf("unexpected readiness timing: %+v", probe)
	}
	if container.StartupProbe != nil || container.LivenessProbe != nil {
		t.Fatal("long-running provisioning must not be killed by a startup/liveness timeout")
	}
}

func TestCleanupTemplateRevisionTracksDesiredPod(t *testing.T) {
	cfg := testConfig()
	p := profileNamed("revision", []string{"workers"}, jdk("temurin", 21))
	revision := func(cfg Config, key string) string {
		return buildCleanupDaemonSet(cfg, &p, key, nil).Spec.Template.Annotations[cleanupTemplateAnnotation]
	}
	first := revision(cfg, "agentpool")
	if len(first) != 64 || first != revision(cfg, "agentpool") {
		t.Fatal("cleanup revision must be nonempty and deterministic")
	}
	cfg.ProvisionerImage += "-new"
	if first == revision(cfg, "agentpool") {
		t.Fatal("new provisioner image must change cleanup revision")
	}
	if revision(cfg, "agentpool") == revision(cfg, "example.com/pool") {
		t.Fatal("new affinity must change cleanup revision")
	}
}
