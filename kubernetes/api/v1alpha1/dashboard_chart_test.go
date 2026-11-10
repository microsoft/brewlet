package v1alpha1

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestDashboardChart(t *testing.T) {
	requireHelm(t)
	render := func(args ...string) ([]byte, error) {
		return exec.Command("helm", append([]string{"template", "test", "../../charts/brewlet", "-n", "brewlet-system"}, args...)...).CombinedOutput()
	}
	for _, write := range []bool{false, true} {
		args := []string{"--set", "dashboard.enabled=true", "--set", "dashboard.publicURL=https://runtimes.example.com"}
		if write {
			args = append(args, "--set", "dashboard.allowWrite=true")
		}
		raw, err := render(args...)
		if err != nil {
			t.Fatalf("render: %v: %s", err, raw)
		}
		decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
		var deployment appsv1.Deployment
		var service corev1.Service
		var account corev1.ServiceAccount
		roles := map[string]rbacv1.ClusterRole{}
		for {
			var doc json.RawMessage
			if err := decoder.Decode(&doc); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			var item struct {
				Kind     string
				Metadata struct{ Name string }
			}
			if err := json.Unmarshal(doc, &item); err != nil {
				t.Fatal(err)
			}
			switch {
			case item.Kind == "Deployment" && item.Metadata.Name == "brewlet-dashboard":
				if err := json.Unmarshal(doc, &deployment); err != nil {
					t.Fatal(err)
				}
			case item.Kind == "Service" && item.Metadata.Name == "brewlet-dashboard":
				if err := json.Unmarshal(doc, &service); err != nil {
					t.Fatal(err)
				}
			case item.Kind == "ServiceAccount" && item.Metadata.Name == "brewlet-dashboard":
				if err := json.Unmarshal(doc, &account); err != nil {
					t.Fatal(err)
				}
			case item.Kind == "ClusterRole" && strings.Contains(item.Metadata.Name, "dashboard"):
				var role rbacv1.ClusterRole
				if err := json.Unmarshal(doc, &role); err != nil {
					t.Fatal(err)
				}
				roles[role.Name] = role
			case item.Kind == "ClusterRoleBinding" || item.Kind == "RoleBinding":
				var binding rbacv1.ClusterRoleBinding
				if err := json.Unmarshal(doc, &binding); err != nil {
					t.Fatal(err)
				}
				for _, subject := range binding.Subjects {
					if subject.Kind == "ServiceAccount" && subject.Name == "brewlet-dashboard" {
						t.Fatal("dashboard pod must not receive inventory or write privileges")
					}
				}
			}
		}
		if service.Spec.Type != corev1.ServiceTypeClusterIP || service.Spec.Ports[0].Port != 8088 || account.Name != "brewlet-dashboard" {
			t.Fatal("dashboard needs a private Service and separate ServiceAccount")
		}
		pod := deployment.Spec.Template.Spec
		if pod.ServiceAccountName != account.Name || *deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
			t.Fatal("deployment identity or single-process ticket lifecycle changed")
		}
		c := pod.Containers[0]
		if pod.SecurityContext == nil || !*pod.SecurityContext.RunAsNonRoot ||
			pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault ||
			!*c.SecurityContext.ReadOnlyRootFilesystem || *c.SecurityContext.AllowPrivilegeEscalation ||
			!slices.Contains(c.SecurityContext.Capabilities.Drop, corev1.Capability("ALL")) ||
			c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.LivenessProbe.HTTPGet.Path != "/healthz" ||
			len(c.Resources.Requests) == 0 || len(c.Resources.Limits) == 0 {
			t.Fatal("dashboard container hardening, resources or probes missing")
		}
		if !strings.HasPrefix(c.Image, "ghcr.io/microsoft/brewlet-dashboard:") ||
			!slices.Contains(c.Args, "--in-cluster") || !slices.Contains(c.Args, "--namespace=brewlet-system") ||
			!slices.Contains(c.Args, "--public-url=https://runtimes.example.com") || slices.Contains(c.Args, "--allow-write") != write {
			t.Fatalf("dashboard configuration not wired: %+v", c)
		}
		viewer, ok := roles["test-dashboard-viewer"]
		if !ok || len(viewer.Rules) != 3 {
			t.Fatal("viewer role missing")
		}
		for _, rule := range viewer.Rules {
			for _, verb := range rule.Verbs {
				if verb != "get" && verb != "list" {
					t.Fatalf("viewer can mutate: %+v", rule)
				}
			}
		}
		operator, ok := roles["test-dashboard-operator"]
		if ok != write || (write && (len(operator.Rules) != 1 || !slices.Equal(operator.Rules[0].Resources, []string{"runtimeimages"}) || !slices.Equal(operator.Rules[0].Verbs, []string{"patch"}))) {
			t.Fatal("rotation role must grant only RuntimeImage patch")
		}
	}
	for _, option := range []string{"dashboard.enabled=yes", "dashboard.allowWrite=yes", "dashboard.publicURL=http://insecure.example", "dashboard.publicURL=https://example.com/path", "dashboard.image=", "dashboard.imagePullPolicy=Sometimes"} {
		if raw, err := render("--set", option); err == nil {
			t.Fatalf("invalid configuration accepted: %s: %s", option, raw)
		}
	}
}
