// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chart_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	kubeSystemExclusion = metav1.LabelSelectorRequirement{
		Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"kube-system"},
	}
	// The expressions the AKS Admissions Enforcer injects into every webhook's
	// namespaceSelector.
	aksEnforcerExpressions = []metav1.LabelSelectorRequirement{
		{Key: "control-plane", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"true"}},
		{Key: "kubernetes.azure.com/managedby", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"aks"}},
	}
)

type webhookConfigs struct {
	pods         admissionregistrationv1.MutatingWebhookConfiguration
	nodeProfiles admissionregistrationv1.ValidatingWebhookConfiguration
}

func renderWebhooks(t *testing.T, extra ...string) webhookConfigs {
	t.Helper()
	var result webhookConfigs
	var foundPods, foundNodeProfiles bool
	for _, object := range render(t, append([]string{"--set", "admission.enabled=true"}, extra...)...) {
		switch object.GetKind() {
		case "MutatingWebhookConfiguration":
			result.pods = convert[admissionregistrationv1.MutatingWebhookConfiguration](t, object)
			foundPods = true
		case "ValidatingWebhookConfiguration":
			result.nodeProfiles = convert[admissionregistrationv1.ValidatingWebhookConfiguration](t, object)
			foundNodeProfiles = true
		}
	}
	if !foundPods || !foundNodeProfiles {
		t.Fatalf("rendered pod webhook %v, NodeProfile webhook %v; want both", foundPods, foundNodeProfiles)
	}
	return result
}

func TestWebhookNamespaceSelectorsIncludeAKSEnforcerExclusions(t *testing.T) {
	webhooks := renderWebhooks(t)
	want := append([]metav1.LabelSelectorRequirement{kubeSystemExclusion}, aksEnforcerExpressions...)
	if got := webhooks.pods.Webhooks[0].NamespaceSelector; got == nil || !reflect.DeepEqual(got.MatchExpressions, want) {
		t.Errorf("pod webhook namespaceSelector = %+v, want expressions %+v", got, want)
	}
	if got := webhooks.nodeProfiles.Webhooks[0].NamespaceSelector; got == nil || !reflect.DeepEqual(got.MatchExpressions, aksEnforcerExpressions) {
		t.Errorf("NodeProfile webhook namespaceSelector = %+v, want expressions %+v", got, aksEnforcerExpressions)
	}
	for _, labels := range []map[string]string{webhooks.pods.Labels, webhooks.nodeProfiles.Labels, webhooks.pods.Annotations, webhooks.nodeProfiles.Annotations} {
		if _, ok := labels["admissions.enforcer/disabled"]; ok {
			t.Error("default render opted out of the AKS Admissions Enforcer")
		}
	}
}

func TestWebhookNamespaceSelectorMergesUserSelector(t *testing.T) {
	values := filepath.Join(t.TempDir(), "values.yaml")
	err := os.WriteFile(values, []byte(`admission:
  namespaceSelector:
    matchLabels: {team: java}
    matchExpressions:
      - key: control-plane
        operator: NotIn
        values: ["true"]
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	got := renderWebhooks(t, "-f", values).pods.Webhooks[0].NamespaceSelector
	want := &metav1.LabelSelector{MatchLabels: map[string]string{"team": "java"}, MatchExpressions: aksEnforcerExpressions}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("namespaceSelector = %+v, want %+v without duplicated expressions", got, want)
	}

	got = renderWebhooks(t, "--set", "admission.namespaceSelector=null").pods.Webhooks[0].NamespaceSelector
	if got == nil || !reflect.DeepEqual(got.MatchExpressions, aksEnforcerExpressions) {
		t.Errorf("empty user selector rendered %+v, want only the platform exclusions", got)
	}
}

func TestWebhookPlatformExclusionsCanBeDisabled(t *testing.T) {
	webhooks := renderWebhooks(t, "--set", "admission.platformNamespaceExclusions=null")
	if got := webhooks.pods.Webhooks[0].NamespaceSelector; got == nil || !reflect.DeepEqual(got.MatchExpressions, []metav1.LabelSelectorRequirement{kubeSystemExclusion}) {
		t.Errorf("pod webhook namespaceSelector = %+v, want only kube-system exclusion", got)
	}
	if got := webhooks.nodeProfiles.Webhooks[0].NamespaceSelector; got != nil {
		t.Errorf("NodeProfile webhook rendered namespaceSelector %+v, want none", got)
	}
	webhooks = renderWebhooks(t, "--set", "admission.platformNamespaceExclusions=null", "--set", "admission.namespaceSelector=null")
	if got := webhooks.pods.Webhooks[0].NamespaceSelector; got != nil {
		t.Errorf("pod webhook rendered namespaceSelector %+v, want none", got)
	}
}

func TestWebhookAKSAdmissionsEnforcerOptOut(t *testing.T) {
	webhooks := renderWebhooks(t, "--set", "admission.disableAKSAdmissionsEnforcer=true",
		"--set", "admission.certManager.enabled=true", "--set", "admission.certManager.createSelfSignedIssuer=true")
	for name, meta := range map[string]metav1.ObjectMeta{"pods": webhooks.pods.ObjectMeta, "nodeprofiles": webhooks.nodeProfiles.ObjectMeta} {
		if meta.Labels["admissions.enforcer/disabled"] != "true" || meta.Annotations["admissions.enforcer/disabled"] != "true" {
			t.Errorf("%s webhook is missing the admissions.enforcer/disabled label or annotation: %v %v", name, meta.Labels, meta.Annotations)
		}
		if meta.Annotations["cert-manager.io/inject-ca-from"] == "" {
			t.Errorf("%s webhook lost the cert-manager CA injection annotation", name)
		}
		if meta.Labels["app.kubernetes.io/managed-by"] != "Helm" {
			t.Errorf("%s webhook lost the standard labels: %v", name, meta.Labels)
		}
	}
}

// AKS's Admissions Enforcer patches every webhook's namespaceSelector after it
// is created. LabelSelector is atomic for server-side apply, so the enforcer
// then owns the whole selector and a Helm 4 (server-side apply) upgrade that
// renders a different selector fails with a field conflict. This simulates the
// enforcer with a second field manager against an isolated API server.
func TestHelmServerSideUpgradeToleratesAKSAdmissionsEnforcer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires envtest control-plane assets; run make test-envtest")
	}
	helmPath := helmCommand(t).Path
	if version, err := exec.Command(helmPath, "version", "--template", "{{.Version}}").Output(); err != nil || strings.HasPrefix(string(version), "v3.") {
		t.Skipf("requires Helm 4 server-side apply, have %q (%v)", version, err)
	}
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../charts/brewlet/crds"},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop isolated control plane: %v", err)
		}
	})
	c, err := client.New(config, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	kubeconfig := filepath.Join(work, "kubeconfig")
	err = clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"isolated": {
			Server: config.Host, CertificateAuthorityData: config.CAData,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"isolated": {
			ClientCertificateData: config.CertData, ClientKeyData: config.KeyData,
		}},
		Contexts:       map[string]*clientcmdapi.Context{"isolated": {Cluster: "isolated", AuthInfo: "isolated"}},
		CurrentContext: "isolated",
	}, kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	helm := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, helmPath, args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig,
			"HELM_CACHE_HOME="+filepath.Join(work, "cache"),
			"HELM_CONFIG_HOME="+filepath.Join(work, "config"),
			"HELM_DATA_HOME="+filepath.Join(work, "data"))
		return cmd.CombinedOutput()
	}
	// The enforcer appends any of its expressions a selector lacks and then
	// owns the resulting (atomic) selector.
	enforce := func(kind, name string) {
		t.Helper()
		current := &unstructured.Unstructured{}
		current.SetAPIVersion("admissionregistration.k8s.io/v1")
		current.SetKind(kind)
		if err := c.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
			t.Fatal(err)
		}
		webhooks, _, _ := unstructured.NestedSlice(current.Object, "webhooks")
		applied := make([]any, 0, len(webhooks))
		for _, raw := range webhooks {
			webhook := raw.(map[string]any)
			selector, _, _ := unstructured.NestedMap(webhook, "namespaceSelector")
			if selector == nil {
				selector = map[string]any{}
			}
			expressions, _, _ := unstructured.NestedSlice(selector, "matchExpressions")
			for _, injected := range aksEnforcerExpressions {
				expression := map[string]any{"key": injected.Key, "operator": string(injected.Operator), "values": []any{injected.Values[0]}}
				if !containsExpression(expressions, expression) {
					expressions = append(expressions, expression)
				}
			}
			selector["matchExpressions"] = expressions
			applied = append(applied, map[string]any{"name": webhook["name"], "namespaceSelector": selector})
		}
		patch := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "admissionregistration.k8s.io/v1", "kind": kind,
			"metadata": map[string]any{"name": name}, "webhooks": applied,
		}}
		if err := c.Patch(ctx, patch, client.Apply, client.FieldOwner("admissionsenforcer"), client.ForceOwnership); err != nil {
			t.Fatalf("simulate AKS Admissions Enforcer on %s: %v", name, err)
		}
	}
	install := func(release, namespace string, extra ...string) {
		t.Helper()
		args := append([]string{"upgrade", "--install", release, "../../charts/brewlet",
			"--namespace", namespace, "--create-namespace", "--skip-crds", "--server-side=true",
			"--set", "namespace=" + namespace, "--set", "defaultProfile.enabled=false"}, extra...)
		if out, err := helm(args...); err != nil {
			t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	// Before the fix: the chart's selector lacks the enforcer's expressions.
	install("brewlet", "brewlet", "--set", "admission.platformNamespaceExclusions=null")
	enforce("MutatingWebhookConfiguration", "brewlet-admission")
	enforce("ValidatingWebhookConfiguration", "brewlet-nodeprofiles")
	out, err := helm("upgrade", "brewlet", "../../charts/brewlet", "--namespace", "brewlet", "--skip-crds",
		"--server-side=true", "--set", "namespace=brewlet", "--set", "defaultProfile.enabled=false",
		"--set", "admission.platformNamespaceExclusions=null")
	if err == nil || !strings.Contains(string(out), `conflict with "admissionsenforcer"`) {
		t.Fatalf("expected the pre-fix chart to conflict with the enforcer, got %v\n%s", err, out)
	}

	// With the default exclusions the upgrade is conflict-free, even repeatedly.
	install("brewlet", "brewlet")
	enforce("MutatingWebhookConfiguration", "brewlet-admission")
	enforce("ValidatingWebhookConfiguration", "brewlet-nodeprofiles")
	install("brewlet", "brewlet")
	install("brewlet", "brewlet", "--set", "admission.failurePolicy=Fail")

	pods := &admissionregistrationv1.MutatingWebhookConfiguration{}
	if err := c.Get(ctx, client.ObjectKey{Name: "brewlet-admission"}, pods); err != nil {
		t.Fatal(err)
	}
	want := append([]metav1.LabelSelectorRequirement{kubeSystemExclusion}, aksEnforcerExpressions...)
	if got := pods.Webhooks[0].NamespaceSelector; got == nil || !reflect.DeepEqual(got.MatchExpressions, want) {
		t.Errorf("namespaceSelector after upgrade = %+v, want %+v", got, want)
	}
	if got := *pods.Webhooks[0].FailurePolicy; got != admissionregistrationv1.Fail {
		t.Errorf("upgrade did not apply failurePolicy: got %s", got)
	}
}

func containsExpression(expressions []any, want map[string]any) bool {
	for _, expression := range expressions {
		if reflect.DeepEqual(expression, want) {
			return true
		}
	}
	return false
}
