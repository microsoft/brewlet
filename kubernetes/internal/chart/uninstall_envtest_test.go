// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chart_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envtest has no Job controller or kubelet. Simulating only Job terminal status
// exercises Helm's real hook/RBAC/delete ordering without running host workers.
func TestHelmUninstallGatesControlPlaneRemoval(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires envtest control-plane assets; run make test-envtest")
	}
	helmPath := helmCommand(t).Path
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../charts/brewlet/crds"},
		ErrorIfCRDPathMissing: true,
	}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop isolated control plane: %v", err)
		}
	})
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	kubeconfig := filepath.Join(work, "kubeconfig")
	err = clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"isolated": {
			Server: config.Host, CertificateAuthorityData: config.CAData, CertificateAuthority: config.CAFile,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"isolated": {
			ClientCertificateData: config.CertData, ClientKeyData: config.KeyData,
			ClientCertificate: config.CertFile, ClientKey: config.KeyFile,
		}},
		Contexts:       map[string]*clientcmdapi.Context{"isolated": {Cluster: "isolated", AuthInfo: "isolated"}},
		CurrentContext: "isolated",
	}, kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	command := func(args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, helmPath, args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig,
			"HELM_CACHE_HOME="+filepath.Join(work, "cache"),
			"HELM_CONFIG_HOME="+filepath.Join(work, "config"),
			"HELM_DATA_HOME="+filepath.Join(work, "data"))
		return cmd
	}
	out, err := command("install", "test-release", "../../charts/brewlet",
		"--namespace", "brewlet", "--create-namespace", "--skip-crds",
		"--set", "defaultProfile.enabled=false", "--set", "admission.enabled=false").CombinedOutput()
	if err != nil {
		t.Fatalf("install into isolated API server: %v\n%s", err, out)
	}
	rendered := hooks(t, render(t, "--namespace", "brewlet", "--set", "namespace=brewlet"))
	hook := rendered["Job"]
	jobKey := client.ObjectKey{Namespace: "brewlet", Name: hook.GetName()}

	assertControlPlanePresent := func() {
		t.Helper()
		if err := c.Get(ctx, client.ObjectKey{Namespace: "brewlet", Name: "brewlet-operator"}, &appsv1.Deployment{}); err != nil {
			t.Fatalf("operator was removed before cleanup succeeded: %v", err)
		}
		if err := c.Get(ctx, client.ObjectKey{Name: "brewlet-operator"}, &rbacv1.ClusterRole{}); err != nil {
			t.Fatalf("operator RBAC was removed before cleanup succeeded: %v", err)
		}
	}
	type result struct {
		output []byte
		err    error
	}
	startUninstall := func() <-chan result {
		done := make(chan result, 1)
		go func() {
			out, err := command("uninstall", "test-release", "--namespace", "brewlet", "--timeout", "30s").CombinedOutput()
			done <- result{output: out, err: err}
			close(done)
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("Helm process did not stop after cancellation")
			}
		})
		return done
	}
	waitForJob := func(previous types.UID) *batchv1.Job {
		t.Helper()
		job := &batchv1.Job{}
		err := wait.PollUntilContextTimeout(ctx, 25*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, jobKey, job); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			return job.UID != previous && job.DeletionTimestamp.IsZero(), nil
		})
		if err != nil {
			t.Fatalf("waiting for cleanup hook Job: %v", err)
		}
		return job
	}

	first := startUninstall()
	job := waitForJob("")
	assertControlPlanePresent()
	if err := c.Get(ctx, jobKey, &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("hook service account must survive while the Job runs: %v", err)
	}
	checkPermission := func(account string, attributes authorizationv1.ResourceAttributes, allowed bool) {
		t.Helper()
		err := wait.PollUntilContextTimeout(ctx, 25*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
			review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
				User:               "system:serviceaccount:brewlet:" + account,
				ResourceAttributes: &attributes,
			}}
			if err := c.Create(ctx, review); err != nil {
				return false, err
			}
			return review.Status.Allowed == allowed, nil
		})
		if err != nil {
			t.Fatalf("%s permission %+v, want allowed=%t: %v", account, attributes, allowed, err)
		}
	}
	for _, permission := range []struct {
		group, resource, verb, namespace string
		allowed                          bool
	}{
		{"node.brewlet.sh", "nodeprofiles", "delete", "", true},
		{"node.brewlet.sh", "nodeprofiles/finalizers", "update", "", false},
		{"", "pods", "list", "brewlet", true},
		{"apps", "daemonsets", "list", "brewlet", true},
		{"", "pods", "list", "", true},
		{"", "pods", "list", "other-namespace", true},
		{"apps", "daemonsets", "list", "", true},
		{"apps", "daemonsets", "delete", "other-namespace", false},
		{"", "pods", "delete", "brewlet", false},
		{"", "nodes", "patch", "", false},
	} {
		resource, subresource, _ := strings.Cut(permission.resource, "/")
		checkPermission(jobKey.Name, authorizationv1.ResourceAttributes{
			Group: permission.group, Resource: resource, Subresource: subresource,
			Verb: permission.verb, Namespace: permission.namespace,
		}, permission.allowed)
	}
	for _, namespace := range []string{"", "brewlet", "other-namespace"} {
		checkPermission("brewlet-operator", authorizationv1.ResourceAttributes{
			Group: "apps", Resource: "daemonsets", Verb: "list", Namespace: namespace,
		}, true)
		checkPermission("brewlet-operator", authorizationv1.ResourceAttributes{
			Group: "apps", Resource: "daemonsets", Verb: "delete", Namespace: namespace,
		}, namespace == "brewlet")
	}
	select {
	case result := <-first:
		t.Fatalf("Helm returned while cleanup was incomplete: %v\n%s", result.err, result.output)
	default:
	}
	firstUID := job.UID
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "SimulatedCleanupFailure", LastTransitionTime: now,
	}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	failed := <-first
	if failed.err == nil {
		t.Fatalf("failed cleanup allowed uninstall: %s", failed.output)
	}
	assertControlPlanePresent()
	if err := c.Get(ctx, jobKey, &batchv1.Job{}); err != nil {
		t.Fatalf("failed hook evidence was discarded: %v", err)
	}
	second := startUninstall()
	job = waitForJob(firstUID)
	assertControlPlanePresent()
	if err := c.Get(ctx, client.ObjectKey{Name: jobKey.Name}, &rbacv1.ClusterRole{}); err != nil {
		t.Fatalf("retry did not recreate the hook RBAC: %v", err)
	}
	now = metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Succeeded = 1
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "SimulatedCleanupSuccess", LastTransitionTime: now,
	}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	succeeded := <-second
	if succeeded.err != nil {
		t.Fatalf("retry after successful cleanup: %v\n%s", succeeded.err, succeeded.output)
	}
	for _, object := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "brewlet-operator", Namespace: "brewlet"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "brewlet-operator"}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: "brewlet"}},
		&rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: jobKey.Name, Namespace: "brewlet"}},
	} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Errorf("%T %s survived successful uninstall: %v", object, object.GetName(), err)
		}
	}
	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "brewlet"}, &namespace); err != nil || !namespace.DeletionTimestamp.IsZero() {
		t.Fatalf("uninstall must retain the component namespace: %v", err)
	}
}
