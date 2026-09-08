// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chart_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func helmCommand(t *testing.T, extra ...string) *exec.Cmd {
	t.Helper()
	helm := os.Getenv("HELM")
	if helm == "" {
		helm = "helm"
	}
	path, err := exec.LookPath(helm)
	if err != nil {
		if os.Getenv("BREWLET_REQUIRE_HELM") == "true" {
			t.Fatal(err)
		}
		t.Skip("Helm is required; make helm-lifecycle-check enforces these tests")
	}
	args := []string{"template", "test-release", "../../charts/brewlet",
		"--namespace", "release-home", "--set", "namespace=operator-home",
		"--set", "defaultProfile.enabled=false", "--set", "admission.enabled=false"}
	return exec.Command(path, append(args, extra...)...)
}

func render(t *testing.T, extra ...string) []unstructured.Unstructured {
	t.Helper()
	out, err := helmCommand(t, extra...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("helm template: %v\n%s", err, exit.Stderr)
		}
		t.Fatal(err)
	}
	return parseManifest(t, out)
}

func parseManifest(t *testing.T, out []byte) []unstructured.Unstructured {
	t.Helper()
	var objects []unstructured.Unstructured
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			return objects
		}
		if err != nil {
			t.Fatalf("decode rendered manifest: %v", err)
		}
		if object.Object != nil {
			objects = append(objects, object)
		}
	}
}

func convert[T any](t *testing.T, object unstructured.Unstructured) T {
	t.Helper()
	var result T
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func hooks(t *testing.T, objects []unstructured.Unstructured) map[string]unstructured.Unstructured {
	t.Helper()
	result := make(map[string]unstructured.Unstructured)
	for _, object := range objects {
		if object.GetAnnotations()["helm.sh/hook"] != "pre-delete" {
			continue
		}
		if _, exists := result[object.GetKind()]; exists {
			t.Fatalf("duplicate %s uninstall hook", object.GetKind())
		}
		result[object.GetKind()] = object
	}
	if len(result) != 4 {
		t.Fatalf("got %d uninstall hooks, want SA, ClusterRole, ClusterRoleBinding, Job", len(result))
	}
	return result
}

func TestUninstallHookLifecycle(t *testing.T) {
	objects := render(t)
	resources := hooks(t, objects)
	job := convert[batchv1.Job](t, resources["Job"])
	weights := map[string]int{"ServiceAccount": -30, "ClusterRole": -20,
		"ClusterRoleBinding": -10, "Job": 0}
	for kind, object := range resources {
		if object.GetName() != job.Name {
			t.Errorf("%s name = %q, want %q", kind, object.GetName(), job.Name)
		}
		if got := object.GetAnnotations()["helm.sh/hook-weight"]; got != strconv.Itoa(weights[kind]) {
			t.Errorf("%s hook weight = %q", kind, got)
		}
		if got := object.GetAnnotations()["helm.sh/hook-delete-policy"]; got != "before-hook-creation,hook-succeeded" {
			t.Errorf("%s deletion policy = %q; failures must retain evidence and support retry", kind, got)
		}
		if kind != "ClusterRole" && kind != "ClusterRoleBinding" && object.GetNamespace() != "operator-home" {
			t.Errorf("%s namespace = %q, want operator-home", kind, object.GetNamespace())
		}
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 ||
		job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 260 ||
		job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatalf("unsafe Job retry/deadline/retention policy: %+v", job.Spec)
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever || pod.ServiceAccountName != job.Name {
		t.Fatalf("wrong hook pod restart policy or service account: %+v", pod)
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC || len(pod.Volumes) != 0 {
		t.Fatal("cleanup coordinator must not access host namespaces or volumes")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("cleanup coordinator must run nonroot with the default seccomp profile")
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("got %d hook containers, want one", len(pod.Containers))
	}
	container := pod.Containers[0]
	wantArgs := []string{"--namespace=operator-home", "--uninstall-release-name=test-release",
		"--uninstall-release-namespace=release-home", "--uninstall-timeout=240s"}
	if !slices.Equal(container.Args, wantArgs) || len(container.Command) != 0 {
		t.Fatalf("cleanup must use the existing image entrypoint and release identity: %+v", container)
	}
	sc := container.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
		len(sc.Capabilities.Add) != 0 || (sc.Privileged != nil && *sc.Privileged) {
		t.Fatal("cleanup coordinator must be unprivileged and use a read-only root filesystem")
	}
	var foundOperator bool
	var keptNamespace bool
	for _, object := range objects {
		if object.GetKind() == "Namespace" && object.GetName() == "operator-home" {
			keptNamespace = object.GetAnnotations()["helm.sh/resource-policy"] == "keep"
		}
		if object.GetKind() == "Deployment" && object.GetName() == "brewlet-operator" {
			foundOperator = true
			operator := convert[appsv1.Deployment](t, object)
			if object.GetAnnotations()["helm.sh/hook"] != "" {
				t.Fatal("operator must remain a normal release resource until pre-delete succeeds")
			}
			if container.Image != operator.Spec.Template.Spec.Containers[0].Image {
				t.Fatal("hook and operator must use the same image")
			}
		}
	}
	if !foundOperator {
		t.Fatal("operator Deployment missing")
	}
	if !keptNamespace {
		t.Fatal("uninstall must not cascade-delete unrelated objects in the component namespace")
	}
	role := convert[rbacv1.ClusterRole](t, resources["ClusterRole"])
	wantRules := []rbacv1.PolicyRule{
		{APIGroups: []string{"node.brewlet.sh"}, Resources: []string{"nodeprofiles"}, Verbs: []string{"list", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"}},
		{APIGroups: []string{"apps"}, Resources: []string{"daemonsets"}, Verbs: []string{"list"}},
	}
	if !reflect.DeepEqual(role.Rules, wantRules) {
		t.Fatalf("hook must only inventory workers globally, never modify them: %+v", role.Rules)
	}
	clusterBinding := convert[rbacv1.ClusterRoleBinding](t, resources["ClusterRoleBinding"])
	wantSubjects := []rbacv1.Subject{{Kind: "ServiceAccount", Name: job.Name, Namespace: "operator-home"}}
	if !reflect.DeepEqual(clusterBinding.Subjects, wantSubjects) ||
		clusterBinding.RoleRef.Name != job.Name || clusterBinding.RoleRef.Kind != "ClusterRole" {
		t.Fatal("hook permissions are not bound exclusively to its service account")
	}
}

func TestOperatorWorkerInventoryRBAC(t *testing.T) {
	check := func(t *testing.T, objects []unstructured.Unstructured) {
		t.Helper()
		var foundClusterRead, foundNamespacedWrite bool
		for _, object := range objects {
			if object.GetName() != "brewlet-operator" {
				continue
			}
			switch object.GetKind() {
			case "ClusterRole":
				role := convert[rbacv1.ClusterRole](t, object)
				for _, rule := range role.Rules {
					if !(slices.Contains(rule.APIGroups, "apps") || slices.Contains(rule.APIGroups, "*")) ||
						!(slices.Contains(rule.Resources, "daemonsets") || slices.Contains(rule.Resources, "*")) {
						continue
					}
					if !reflect.DeepEqual(rule, rbacv1.PolicyRule{
						APIGroups: []string{"apps"}, Resources: []string{"daemonsets"}, Verbs: []string{"list"},
					}) {
						t.Fatalf("global worker inventory must be list-only: %+v", rule)
					}
					foundClusterRead = true
				}
			case "Role":
				role := convert[rbacv1.Role](t, object)
				if role.Namespace == "" {
					t.Fatal("worker mutation Role must have a namespace")
				}
				for _, rule := range role.Rules {
					if slices.Contains(rule.APIGroups, "apps") && slices.Contains(rule.Resources, "daemonsets") &&
						slices.Contains(rule.Verbs, "create") && slices.Contains(rule.Verbs, "delete") {
						foundNamespacedWrite = true
					}
				}
			}
		}
		if !foundClusterRead || !foundNamespacedWrite {
			t.Fatalf("missing global inventory or namespace-scoped mutation: read=%t write=%t",
				foundClusterRead, foundNamespacedWrite)
		}
	}
	t.Run("raw", func(t *testing.T) {
		data, err := os.ReadFile("../../config/operator.yaml")
		if err != nil {
			t.Fatal(err)
		}
		check(t, parseManifest(t, data))
	})
	t.Run("chart", func(t *testing.T) {
		check(t, render(t))
	})
}

func TestUninstallConfiguration(t *testing.T) {
	resources := hooks(t, render(t, "--set", "uninstall.timeoutSeconds=600",
		"--set", "uninstall.imagePullSecrets[0].name=registry-auth",
		"--set", "images.digests.operator=sha256:"+strings.Repeat("a", 64)))
	job := convert[batchv1.Job](t, resources["Job"])
	if *job.Spec.ActiveDeadlineSeconds != 620 ||
		!slices.Contains(job.Spec.Template.Spec.Containers[0].Args, "--uninstall-timeout=600s") ||
		job.Spec.Template.Spec.ImagePullSecrets[0].Name != "registry-auth" ||
		!strings.HasSuffix(job.Spec.Template.Spec.Containers[0].Image, "@sha256:"+strings.Repeat("a", 64)) {
		t.Fatalf("custom cleanup settings did not propagate: %+v", job.Spec)
	}
	otherNamespace := hooks(t, render(t, "--namespace", "other-release-home"))
	firstRole, otherRole := resources["ClusterRole"], otherNamespace["ClusterRole"]
	if firstRole.GetName() == otherRole.GetName() {
		t.Fatal("cluster-scoped hooks collide across release namespaces")
	}
	for _, value := range []string{"0", "-1", "1.5", "bad", "86401", "9223372036854775808"} {
		t.Run(value, func(t *testing.T) {
			out, err := helmCommand(t, "--set-string", "uninstall.timeoutSeconds="+value).CombinedOutput()
			if err == nil || !strings.Contains(string(out), "uninstall.timeoutSeconds must be a whole number") {
				t.Fatalf("invalid timeout %q was not rejected clearly: %v\n%s", value, err, out)
			}
		})
	}
}

func TestChartProfilesRequireExplicitPools(t *testing.T) {
	jdks := `[{"distribution":"temurin","feature":21,"source":{"image":"registry.example.com/jdk@sha256:` +
		strings.Repeat("a", 64) + `","javaHome":"/opt/jdk"}}]`
	base := []string{"--set-json", "profiles[0].jdks=" + jdks, "--set", "profiles[0].name=batch"}
	tests := []struct {
		name, pools, message string
	}{
		{"omitted", "", "profiles[0].pools must be a nonempty list"},
		{"null", "null", "profiles[0].pools must be a nonempty list"},
		{"empty", "[]", "profiles[0].pools must be a nonempty list"},
		{"string", `"workers"`, "profiles[0].pools must be a nonempty list"},
		{"empty name", `[""]`, "profiles[0].pools must contain only nonempty"},
		{"numeric name", `[1]`, "profiles[0].pools must contain only nonempty"},
		{"padded name", `[" workers "]`, "profiles[0].pools must not contain whitespace"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := slices.Clone(base)
			if test.pools != "" {
				args = append(args, "--set-json", "profiles[0].pools="+test.pools)
			}
			out, err := helmCommand(t, args...).CombinedOutput()
			if err == nil || !strings.Contains(string(out), test.message) {
				t.Fatalf("unsafe profile was not rejected clearly: %v\n%s", err, out)
			}
		})
	}
	args := append(slices.Clone(base), "--set", "profiles[0].pools={workers}")
	found := false
	for _, object := range render(t, args...) {
		if object.GetKind() != "NodeProfile" {
			continue
		}
		found = true
		pools, _, err := unstructured.NestedStringSlice(object.Object, "spec", "nodePool", "names")
		if err != nil || !slices.Equal(pools, []string{"workers"}) {
			t.Fatalf("wrong pool selector: %v, %v", pools, err)
		}
		if object.GetAnnotations()["meta.helm.sh/release-name"] != "test-release" ||
			object.GetAnnotations()["meta.helm.sh/release-namespace"] != "release-home" ||
			object.GetLabels()["app.kubernetes.io/managed-by"] != "Helm" {
			t.Fatal("profile release ownership does not match the uninstall coordinator")
		}
	}
	if !found {
		t.Fatal("explicitly scoped additional profile did not render")
	}
	duplicate := append(slices.Clone(args), "--set", "defaultProfile.enabled=true",
		"--set", "provisioner.pools={general}", "--set-json", "provisioner.jdks="+jdks,
		"--set", "profiles[0].name=default")
	out, err := helmCommand(t, duplicate...).CombinedOutput()
	if err == nil || !strings.Contains(string(out), `duplicate NodeProfile name "default"`) {
		t.Fatalf("duplicate profile name was not rejected: %v\n%s", err, out)
	}
}
