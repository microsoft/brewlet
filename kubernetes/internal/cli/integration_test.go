// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	nodeapi "brewlet-operator/api/nodeprofile/v1alpha1"
	appapi "brewlet-operator/api/v1alpha1"
	"brewlet-operator/internal/controller"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const sourceImage = "registry.example.com/jdk@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const replacementImage = "registry.example.com/jdk@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type fixture struct {
	ctx              context.Context
	api              client.Client
	scheme           *runtime.Scheme
	config           *rest.Config
	server           *envtest.Environment
	binary, helm     string
	kubeconfig, work string
	env              []string
}

type result struct {
	stdout, stderr string
	code           int
}

// These are API/process integration tests, not node-runtime E2E: envtest has no
// kubelet or workload controllers. Only fixture readiness/inventory is simulated.
func TestCLIIntegration(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		if os.Getenv("BREWLET_REQUIRE_CLI_INTEGRATION") == "true" {
			t.Fatal("KUBEBUILDER_ASSETS is required; run make test-cli-integration")
		}
		t.Skip("requires envtest assets; run make test-cli-integration")
	}
	assets, err := filepath.Abs(assets)
	must(t, err)
	for _, name := range []string{"kube-apiserver", "etcd", "kubectl"} {
		info, err := os.Stat(filepath.Join(assets, name))
		if err != nil || info.IsDir() {
			t.Fatalf("missing envtest binary %s: %v", name, err)
		}
	}
	helm, err := exec.LookPath("helm")
	must(t, err)
	work := t.TempDir()
	binary := filepath.Join(work, "brewlet")
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "-C", "../../../core", "build", "-o", binary, "./cmd/brewlet")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	scheme := runtime.NewScheme()
	must(t, clientgoscheme.AddToScheme(scheme))
	must(t, nodeapi.AddToScheme(scheme))
	must(t, appapi.AddToScheme(scheme))
	server := &envtest.Environment{
		UseExistingCluster:    ptr.To(false),
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{"../../charts/brewlet/crds"},
		ErrorIfCRDPathMissing: true,
	}
	server.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := server.Start()
	must(t, err)
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop isolated API server: %v", err)
		}
	})
	api, err := client.New(config, client.Options{Scheme: scheme})
	must(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	f := &fixture{ctx: ctx, api: api, scheme: scheme, config: config, server: server,
		work: work, binary: binary, helm: helm}
	f.kubeconfig = f.writeConfig(t, "admin", config)
	// Poison defaults so a missed explicit context/path fails rather than
	// accidentally using the user's current cluster.
	poison := filepath.Join(work, "default-kubeconfig")
	must(t, os.WriteFile(poison, []byte("not a kubeconfig\n"), 0o600))
	f.env = append(os.Environ(),
		"KUBECONFIG="+poison,
		"PATH="+assets+string(os.PathListSeparator)+filepath.Dir(helm)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HELM_CACHE_HOME="+filepath.Join(work, "helm-cache"),
		"HELM_CONFIG_HOME="+filepath.Join(work, "helm-config"),
		"HELM_DATA_HOME="+filepath.Join(work, "helm-data"),
	)
	for _, name := range []string{"runtime-system", "team", "other-team"} {
		must(t, api.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}))
	}
	must(t, api.Create(ctx, &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "brewlet"}, Handler: "brewlet"}))
	t.Run("inventory-status-doctor", f.testReadCommands)
	t.Run("live-and-dry-run-inventory-updates", f.testInventoryUpdates)
	t.Run("helm-owned-profile-and-offline-values", f.testHelmOwnership)
	t.Run("rbac-denied-dry-run", f.testRBAC)
	t.Run("admission-denied-dry-run", f.testAdmission)
	t.Run("concurrent-updates", f.testConflicts)
	t.Run("application-inspection", f.testAppInspection)
	t.Run("install-refuses-existing-crds", f.testInstallGuard)
	unchanged, err := os.ReadFile(poison)
	must(t, err)
	if string(unchanged) != "not a kubeconfig\n" {
		t.Fatal("CLI changed the default kubeconfig")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) writeConfig(t *testing.T, name string, config *rest.Config) string {
	t.Helper()
	file := filepath.Join(f.work, name+"-kubeconfig")
	must(t, clientcmd.WriteToFile(clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{"isolated": {
			Server: config.Host, CertificateAuthorityData: config.CAData, CertificateAuthority: config.CAFile,
		}},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{"isolated": {
			ClientCertificateData: config.CertData, ClientKeyData: config.KeyData,
			ClientCertificate: config.CertFile, ClientKey: config.KeyFile,
		}},
		Contexts: map[string]*clientcmdapi.Context{
			"selected": {Cluster: "isolated", AuthInfo: "isolated", Namespace: "other-team"},
			"wrong":    {Cluster: "missing", AuthInfo: "missing"},
		},
		CurrentContext: "wrong",
	}, file))
	return file
}

func (f *fixture) command(t *testing.T, program string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.WaitDelay = time.Second
	cmd.Env = f.env
	cmd.Dir = f.work
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s %v timed out: %v\n%s", program, args, ctx.Err(), stderr.String())
	}
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("start %s: %v", program, err)
		}
		code = exit.ExitCode()
	}
	return result{stdout.String(), stderr.String(), code}
}

func (f *fixture) cli(t *testing.T, args ...string) result {
	t.Helper()
	return f.cliConfig(t, f.kubeconfig, args...)
}

func (f *fixture) cliConfig(t *testing.T, config string, args ...string) result {
	t.Helper()
	base := []string{"k8s", "--kubeconfig", config, "--context", "selected", "--timeout", "10s"}
	return f.command(t, f.binary, append(base, args...)...)
}

func (r result) success(t *testing.T) string {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("CLI failed with exit %d\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	return r.stdout
}

func (r result) failure(t *testing.T, reason string) {
	t.Helper()
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, reason) {
		t.Fatalf("expected exit 1, empty stdout, and %q; got %+v", reason, r)
	}
}

func decode[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	must(t, json.Unmarshal([]byte(raw), &value))
	return value
}

func (f *fixture) profile(t *testing.T, name string) *nodeapi.NodeProfile {
	t.Helper()
	p := &nodeapi.NodeProfile{
		TypeMeta:   metav1.TypeMeta{APIVersion: nodeapi.GroupVersion.String(), Kind: "NodeProfile"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"fixture": name}},
		Spec: nodeapi.NodeProfileSpec{
			NodePool: nodeapi.NodePoolRef{Names: []string{name}, Key: "example.com/pool"},
			JDKs: []nodeapi.JDKRef{{Distribution: "temurin", Feature: 21,
				Source: nodeapi.JDKSource{Image: sourceImage, JavaHome: "/opt/java/openjdk"}}},
			Launchers: []nodeapi.LauncherRef{{Name: "jaz",
				Source: nodeapi.LauncherSource{Image: sourceImage, Path: "/usr/bin/jaz"}}},
			Rollout: nodeapi.RolloutSpec{MaxUnavailable: ptr.To(intstr.FromInt(1)), Validate: ptr.To(true)},
		},
	}
	must(t, f.api.Create(f.ctx, p, client.FieldOwner("kubectl")))
	return p
}

func (f *fixture) getProfile(t *testing.T, name string) *nodeapi.NodeProfile {
	t.Helper()
	p := &nodeapi.NodeProfile{}
	must(t, f.api.Get(f.ctx, client.ObjectKey{Name: name}, p))
	return p
}

func (f *fixture) assertUnchanged(t *testing.T, before *nodeapi.NodeProfile) {
	t.Helper()
	after := f.getProfile(t, before.Name)
	if after.ResourceVersion != before.ResourceVersion || !reflect.DeepEqual(after.Spec, before.Spec) {
		t.Fatalf("dry run or refused update persisted changes: before=%+v after=%+v", before, after)
	}
}

func jdkArgs(profile string, extra ...string) []string {
	return append([]string{"jdk", "add", "--profile", profile, "--distribution", "temurin",
		"--feature", "25", "--image", sourceImage, "--java-home", "/opt/java/openjdk", "--output", "json"}, extra...)
}

func launcherArgs(profile string, extra ...string) []string {
	return append([]string{"launcher", "add", "--profile", profile, "--name", "custom",
		"--image", sourceImage, "--path", "/usr/bin/custom", "--output", "json"}, extra...)
}

func (f *fixture) node(t *testing.T, p *nodeapi.NodeProfile) *corev1.Node {
	t.Helper()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: p.Name + "-node", Labels: map[string]string{
		"example.com/pool": p.Name, "kubernetes.io/arch": "amd64",
		"brewlet.sh/runtime": "ready", "brewlet.sh/owner-uid": string(p.UID),
	}, Annotations: map[string]string{
		"brewlet.sh/owner-name": p.Name, "brewlet.sh/profile-generation": fmt.Sprint(p.Generation),
		"brewlet.sh/jdks": "temurin-21", "brewlet.sh/launchers": "jaz",
		"brewlet.sh/jdks-info": `[{"distribution":"temurin","vendor":"Adoptium","feature":21,"version":"21.0.5","arch":"amd64"}]`,
	}}}
	must(t, f.api.Create(f.ctx, node))
	node.Labels["brewlet.sh/owner-node-uid"] = string(node.UID)
	must(t, f.api.Update(f.ctx, node))
	node.Status.NodeInfo.ContainerRuntimeVersion = "containerd://2.0.0"
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	must(t, f.api.Status().Update(f.ctx, node))
	// No node controller is present to remove the initial NotReady taint.
	node.Spec.Taints = nil
	must(t, f.api.Update(f.ctx, node))
	return node
}

func (f *fixture) deployment(t *testing.T, name string) *appsv1.Deployment {
	t.Helper()
	labels := map[string]string{"app": name}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "runtime-system"},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fixture", Image: sourceImage}}}},
		}}
	must(t, f.api.Create(f.ctx, d))
	f.readyDeployment(t, d)
	return d
}

func (f *fixture) readyDeployment(t *testing.T, d *appsv1.Deployment) {
	t.Helper()
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1,
		UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	must(t, f.api.Status().Update(f.ctx, d))
}

func (f *fixture) testReadCommands(t *testing.T) {
	p := f.profile(t, "inventory")
	f.node(t, p)
	p.Status = nodeapi.NodeProfileStatus{ObservedGeneration: p.Generation, AssignedNodes: 1, ReadyNodes: 1,
		Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "AllNodesProvisioned",
			Message: "fixture readiness", ObservedGeneration: p.Generation, LastTransitionTime: metav1.Now()}}}
	must(t, f.api.Status().Update(f.ctx, p))
	operator := f.deployment(t, "brewlet-operator")
	f.deployment(t, "brewlet-admission")
	jdks := f.cli(t, "jdk", "list", "--selector", "example.com/pool=inventory", "--output", "json").success(t)
	rows := decode[[]struct {
		Distribution, Version, Arch string
		Nodes                       []string
	}](t, jdks)
	if len(rows) != 1 || rows[0].Distribution != "temurin" || rows[0].Version != "21.0.5" ||
		rows[0].Arch != "amd64" || !reflect.DeepEqual(rows[0].Nodes, []string{"inventory-node"}) {
		t.Fatalf("unexpected advertised inventory: %s", jdks)
	}
	aliases := f.command(t, f.binary, "jdks", "--kubeconfig", f.kubeconfig, "--context", "selected",
		"--selector", "example.com/pool=inventory", "--output", "json").success(t)
	if aliases != jdks {
		t.Fatal("legacy jdks alias differs from k8s inventory")
	}
	empty := f.cli(t, "jdk", "list", "--selector", "example.com/pool=absent", "--output", "json").success(t)
	if len(decode[[]json.RawMessage](t, empty)) != 0 {
		t.Fatalf("node selector was ignored: %s", empty)
	}
	launchers := f.cli(t, "launcher", "list", "--output", "json").success(t)
	if !strings.Contains(launchers, `"jaz"`) || !strings.Contains(launchers, `"inventory-node"`) {
		t.Fatal(launchers)
	}
	for _, command := range []string{"jdk", "launcher"} {
		if out := f.cli(t, command, "list", "--output", "wide").success(t); !strings.Contains(out, "inventory-node") {
			t.Fatalf("%s per-node inventory missing node: %s", command, out)
		}
	}
	if out := f.cli(t, "profile", "list").success(t); !strings.Contains(out, "temurin-21") {
		t.Fatal(out)
	}
	profile := decode[struct {
		Profile struct{ Ready bool }
		Nodes   []struct{ Name string }
	}](t, f.cli(t, "profile", "inspect", p.Name, "--output", "json").success(t))
	if !profile.Profile.Ready || len(profile.Nodes) != 1 || profile.Nodes[0].Name != "inventory-node" {
		t.Fatalf("profile inspection: %+v", profile)
	}
	statusArgs := []string{"status", "--system-namespace", "runtime-system", "--output", "json"}
	status := decode[struct{ Healthy bool }](t, f.cli(t, statusArgs...).success(t))
	if !status.Healthy {
		t.Fatal("ready fixtures reported unhealthy")
	}
	f.cli(t, "doctor", "--namespace", "team", "--output", "json").success(t)
	f.command(t, f.binary, "doctor", "--kubeconfig", f.kubeconfig, "--context", "selected",
		"--namespace", "team", "--output", "json").success(t)

	operator.Status.UpdatedReplicas = 0
	must(t, f.api.Status().Update(f.ctx, operator))
	unready := f.cli(t, statusArgs...)
	if unready.code != 1 || decode[struct{ Healthy bool }](t, unready.stdout).Healthy {
		t.Fatalf("old available replicas counted as a current rollout: %+v", unready)
	}
	f.readyDeployment(t, operator)
	p.Status.Conditions[0].ObservedGeneration--
	must(t, f.api.Status().Update(f.ctx, p))
	unready = f.cli(t, statusArgs...)
	if unready.code != 1 || decode[struct{ Healthy bool }](t, unready.stdout).Healthy {
		t.Fatalf("stale profile condition counted as ready: %+v", unready)
	}
}

func (f *fixture) testInventoryUpdates(t *testing.T) {
	p := f.profile(t, "updates")
	for _, args := range [][]string{jdkArgs(p.Name, "--image", "invalid:source", "--dry-run"),
		launcherArgs(p.Name, "--path", "/usr/../bin/custom", "--dry-run")} {
		r := f.cli(t, args...)
		if r.code != 1 || r.stdout != "" || r.stderr == "" {
			t.Fatalf("invalid client dry run should fail with empty output: %+v", r)
		}
		f.assertUnchanged(t, p)
	}
	for _, flags := range [][]string{{"--dry-run"}, {"--dry-run=client"}, {"--dry-run=server"}} {
		for _, args := range [][]string{jdkArgs(p.Name, flags...), launcherArgs(p.Name, flags...)} {
			proposal := decode[nodeapi.NodeProfile](t, f.cli(t, args...).success(t))
			if len(proposal.Spec.JDKs)+len(proposal.Spec.Launchers) != 3 {
				t.Fatalf("dry run did not render addition: %+v", proposal.Spec)
			}
			f.assertUnchanged(t, p)
		}
	}
	f.cli(t, jdkArgs(p.Name)...).success(t)
	f.cli(t, launcherArgs(p.Name)...).success(t)
	updated := f.getProfile(t, p.Name)
	if len(updated.Spec.JDKs) != 2 || len(updated.Spec.Launchers) != 2 ||
		!reflect.DeepEqual(updated.Spec.Rollout, p.Spec.Rollout) || updated.Labels["fixture"] != p.Name ||
		updated.Generation <= p.Generation {
		t.Fatalf("live updates did not preserve/apply configuration: %+v", updated)
	}
	f.cli(t, jdkArgs(p.Name)...).success(t)
	repeated := f.getProfile(t, p.Name)
	if repeated.Generation != updated.Generation || !reflect.DeepEqual(repeated.Spec, updated.Spec) {
		t.Fatal("identical add was not idempotent")
	}
	f.cli(t, jdkArgs(p.Name, "--image", replacementImage)...).failure(t, "--replace")
	f.assertUnchanged(t, repeated)
	f.cli(t, jdkArgs(p.Name, "--image", replacementImage, "--replace")...).success(t)
	if got := f.getProfile(t, p.Name).Spec.JDKs[1].Source.Image; got != replacementImage {
		t.Fatalf("explicit replacement was not saved: %s", got)
	}
	// Exercise the real profile reconciler without starting any host worker.
	r := &controller.NodeProfileReconciler{Client: f.api, Recorder: record.NewFakeRecorder(100),
		Config: controller.Config{Namespace: "runtime-system", ProvisionerImage: "registry.invalid/never-executed:cli-test"}}
	for range 2 {
		_, err := r.Reconcile(f.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: p.Name}})
		must(t, err)
	}
	ds := &appsv1.DaemonSet{}
	must(t, f.api.Get(f.ctx, client.ObjectKey{Namespace: "runtime-system", Name: "brewlet-node-provisioner-" + p.Name}, ds))
	env := map[string]string{}
	for _, entry := range ds.Spec.Template.Spec.Containers[0].Env {
		env[entry.Name] = entry.Value
	}
	if env["JDK_SOURCE_COUNT"] != "2" || env["LAUNCHER_SOURCE_COUNT"] != "2" {
		t.Fatalf("controller did not consume CLI additions: %+v", env)
	}
	inspected := f.cli(t, "profile", "inspect", p.Name, "--output", "json").success(t)
	if !strings.Contains(inspected, replacementImage) || !strings.Contains(inspected, "custom") {
		t.Fatal("profile inspection did not show the reconciled desired inventory", inspected)
	}
}

func (f *fixture) testHelmOwnership(t *testing.T) {
	values := filepath.Join(f.work, "values.json")
	must(t, os.WriteFile(values, []byte(fmt.Sprintf(`{"provisioner":{"pools":["helm-pool"],"jdks":[
		{"distribution":"temurin","feature":21,"source":{"image":%q,"javaHome":"/opt/java/openjdk"}}]}}`, sourceImage)), 0o600))
	chart, err := filepath.Abs("../../charts/brewlet")
	must(t, err)
	rendered := f.command(t, f.helm, "template", "brewlet", chart, "--namespace", "runtime-system",
		"--values", values, "--show-only", "templates/nodeprofiles.yaml").success(t)
	manifest := filepath.Join(f.work, "helm-profile.yaml")
	must(t, os.WriteFile(manifest, []byte(rendered), 0o600))
	f.command(t, "kubectl", "--kubeconfig", f.kubeconfig, "--context", "selected",
		"apply", "-f", manifest).success(t)
	p := f.getProfile(t, "default")
	for _, args := range [][]string{jdkArgs("default"), launcherArgs("default"), jdkArgs("default", "--dry-run=server")} {
		f.cli(t, args...).failure(t, "managed by")
		f.assertUnchanged(t, p)
	}
	proposal := f.cli(t, jdkArgs("default", "--dry-run", "--values", values)...).success(t)
	next := filepath.Join(f.work, "values-next.json")
	must(t, os.WriteFile(next, []byte(proposal), 0o600))
	rendered = f.command(t, f.helm, "template", "brewlet", chart, "--namespace", "runtime-system",
		"--values", next, "--show-only", "templates/nodeprofiles.yaml").success(t)
	if !strings.Contains(rendered, "feature: 25") || !strings.Contains(rendered, "feature: 21") {
		t.Fatal("CLI-generated Helm values lost or failed to add inventory", rendered)
	}
	f.assertUnchanged(t, p)
	offline := f.cli(t, launcherArgs("default", "--dry-run", "--file", manifest)...).success(t)
	if len(decode[nodeapi.NodeProfile](t, offline).Spec.Launchers) != 1 {
		t.Fatal("offline profile proposal omitted launcher")
	}
	f.assertUnchanged(t, p)
}

func (f *fixture) testRBAC(t *testing.T) {
	p := f.profile(t, "read-only")
	user, err := f.server.AddUser(envtest.User{Name: "cli-reader"}, f.config)
	must(t, err)
	config := f.writeConfig(t, "reader", user.Config())
	must(t, f.api.Create(f.ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "cli-reader"},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{"node.brewlet.sh"}, Resources: []string{"nodeprofiles"},
			ResourceNames: []string{p.Name}, Verbs: []string{"get"}}}}))
	must(t, f.api.Create(f.ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "cli-reader"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cli-reader"},
		Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: "cli-reader"}}}))
	must(t, wait.PollUntilContextTimeout(f.ctx, 100*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		r := f.command(t, "kubectl", "--kubeconfig", config, "--context", "selected", "auth", "can-i",
			"get", "nodeprofiles.node.brewlet.sh/"+p.Name)
		return r.code == 0 && strings.TrimSpace(r.stdout) == "yes", nil
	}))
	f.cliConfig(t, config, jdkArgs(p.Name, "--dry-run")...).success(t)
	for _, args := range [][]string{jdkArgs(p.Name), jdkArgs(p.Name, "--dry-run=server")} {
		f.cliConfig(t, config, args...).failure(t, "forbidden")
		f.assertUnchanged(t, p)
	}
	f.cliConfig(t, config, "jdk", "list", "--output", "json").failure(t, "forbidden")
}

func (f *fixture) testAdmission(t *testing.T) {
	p := f.profile(t, "policy")
	fail := admissionv1.Fail
	policy := &admissionv1.ValidatingAdmissionPolicy{ObjectMeta: metav1.ObjectMeta{Name: "cli-test-source-policy"},
		Spec: admissionv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionv1.MatchResources{
				ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"fixture": p.Name}},
				ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
					Operations: []admissionv1.OperationType{admissionv1.Update},
					Rule:       admissionv1.Rule{APIGroups: []string{"node.brewlet.sh"}, APIVersions: []string{"v1alpha1"}, Resources: []string{"nodeprofiles"}},
				}}},
			},
			Validations: []admissionv1.Validation{{Expression: "!object.spec.jdks.exists(j, j.distribution == 'blocked')",
				Message: "CLI integration source policy denied this distribution"}},
		}}
	must(t, f.api.Create(f.ctx, policy))
	must(t, f.api.Create(f.ctx, &admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: policy.Name},
		Spec:       admissionv1.ValidatingAdmissionPolicyBindingSpec{PolicyName: policy.Name, ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny}},
	}))
	// Wait for admission registration using an API dry run; never persist the
	// deliberately disallowed inventory while the policy cache is warming.
	must(t, wait.PollUntilContextTimeout(f.ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		candidate := p.DeepCopy()
		candidate.Spec.JDKs[0].Distribution = "blocked"
		err := f.api.Update(ctx, candidate, client.DryRunAll)
		if err == nil {
			return false, nil
		}
		if strings.Contains(err.Error(), "CLI integration source policy denied") {
			return true, nil
		}
		return false, err
	}))
	for _, format := range []string{"json", "yaml"} {
		for _, dryRun := range []bool{false, true} {
			args := jdkArgs(p.Name, "--distribution", "blocked", "--output", format)
			if dryRun {
				args = append(args, "--dry-run=server")
			}
			f.cli(t, args...).failure(t, "CLI integration source policy denied")
			f.assertUnchanged(t, p)
		}
	}
	f.cli(t, jdkArgs(p.Name, "--distribution", "blocked", "--dry-run")...).success(t)
	f.assertUnchanged(t, p)
}

func (f *fixture) testConflicts(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("recreate=%t/dry-run=%t", replace, dryRun), func(t *testing.T) {
				p := f.profile(t, fmt.Sprintf("conflict-%t-%t", replace, dryRun))
				upstream, err := url.Parse(f.config.Host)
				must(t, err)
				proxy := httputil.NewSingleHostReverseProxy(upstream)
				proxy.Transport, err = rest.TransportFor(f.config)
				must(t, err)
				var once sync.Once
				mutations := make(chan error, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/nodeprofiles/"+p.Name) {
						once.Do(func() {
							current := &nodeapi.NodeProfile{}
							err := f.api.Get(r.Context(), client.ObjectKey{Name: p.Name}, current)
							if err == nil && replace {
								err = f.api.Delete(r.Context(), current)
								if err == nil {
									err = f.api.Create(r.Context(), &nodeapi.NodeProfile{
										ObjectMeta: metav1.ObjectMeta{Name: p.Name}, Spec: current.Spec})
								}
							} else if err == nil {
								current.Annotations = map[string]string{"concurrent-writer": "preserve-me"}
								err = f.api.Update(r.Context(), current)
							}
							mutations <- err
						})
					}
					proxy.ServeHTTP(w, r)
				}))
				defer server.Close()
				config := f.writeConfig(t, p.Name, &rest.Config{Host: server.URL})
				args := jdkArgs(p.Name)
				if dryRun {
					args = append(args, "--dry-run=server")
				}
				r := f.cliConfig(t, config, args...)
				if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "profile update failed") {
					t.Fatalf("stale patch was not rejected with empty output: %+v", r)
				}
				select {
				case err := <-mutations:
					must(t, err)
				default:
					t.Fatal("test did not inject a concurrent write")
				}
				current := f.getProfile(t, p.Name)
				if !reflect.DeepEqual(current.Spec, p.Spec) {
					t.Fatal("stale patch overwrote the current profile")
				}
				if replace && current.UID == p.UID {
					t.Fatal("replacement did not change the UID")
				}
				if !replace && current.Annotations["concurrent-writer"] != "preserve-me" {
					t.Fatal("concurrent writer's change was lost")
				}
			})
		}
	}
}

func (f *fixture) testAppInspection(t *testing.T) {
	app := &appapi.JavaApplication{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team"},
		Spec: appapi.JavaApplicationSpec{Artifact: appapi.ArtifactSpec{Image: sourceImage},
			JVM: appapi.JVMSpec{Version: 21}, Replicas: ptr.To(int32(1)),
			Env: []corev1.EnvVar{{Name: "TOKEN", Value: "fixture-secret-must-not-appear"}}}}
	must(t, f.api.Create(f.ctx, app))
	reconciler := &controller.JavaApplicationReconciler{Client: f.api, APIReader: f.api, Scheme: f.scheme,
		Recorder: record.NewFakeRecorder(100)}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)}
	_, err := reconciler.Reconcile(f.ctx, request)
	must(t, err)
	d := &appsv1.Deployment{}
	must(t, f.api.Get(f.ctx, request.NamespacedName, d))
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "orders-rs", Namespace: "team", Labels: d.Spec.Template.Labels,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(d, appsv1.SchemeGroupVersion.WithKind("Deployment"))}},
		Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(1)), Selector: d.Spec.Selector, Template: d.Spec.Template}}
	must(t, f.api.Create(f.ctx, rs))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orders-pod", Namespace: "team", Labels: d.Spec.Template.Labels,
		Annotations:     d.Spec.Template.Annotations,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}},
		Spec: d.Spec.Template.Spec}
	must(t, f.api.Create(f.ctx, pod))
	pod.Status.Phase = corev1.PodPending
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: pod.Spec.Containers[0].Name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}
	must(t, f.api.Status().Update(f.ctx, pod))
	foreign := pod.DeepCopy()
	foreign.ObjectMeta = metav1.ObjectMeta{Name: "foreign", Namespace: "team", Labels: pod.Labels}
	foreign.Status = corev1.PodStatus{}
	must(t, f.api.Create(f.ctx, foreign))
	for _, target := range []*corev1.Pod{pod, foreign} {
		must(t, f.api.Create(f.ctx, &corev1.Event{ObjectMeta: metav1.ObjectMeta{Name: target.Name + "-event", Namespace: "team"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: target.Name, Namespace: "team", UID: target.UID},
			Type:           "Warning", Reason: "Failed", Message: "fixture image unavailable"}))
	}
	args := []string{"inspect", "app", "orders", "--namespace", "team", "--output", "json"}
	raw := f.cli(t, args...).success(t)
	report := decode[struct {
		Ready      bool
		JDKRequest string
		Pods       []struct{ Name string }
		Events     []struct{ Object string }
	}](t, raw)
	if report.Ready || report.JDKRequest != "21" || len(report.Pods) != 1 || len(report.Events) != 1 ||
		report.Pods[0].Name != pod.Name || strings.Contains(raw, "fixture-secret") || strings.Contains(raw, "foreign") ||
		!strings.Contains(raw, "ImagePullBackOff") {
		t.Fatalf("wrong ownership, readiness, or sensitive data in report: %s", raw)
	}
	f.readyDeployment(t, d)
	_, err = reconciler.Reconcile(f.ctx, request)
	must(t, err)
	if report := decode[struct{ Ready bool }](t, f.cli(t, args...).success(t)); !report.Ready {
		t.Fatal("current completed deployment rollout was not reported ready")
	}
	f.cli(t, "inspect", "app", "orders", "--output", "json").failure(t, "not found")
}

func (f *fixture) testInstallGuard(t *testing.T) {
	values := filepath.Join(f.work, "no-profiles.json")
	must(t, os.WriteFile(values, []byte(`{"defaultProfile":{"enabled":false}}`), 0o600))
	f.cli(t, "install", "--version", "0.0.0", "--values", values).
		failure(t, "Brewlet CRDs already exist")
}
