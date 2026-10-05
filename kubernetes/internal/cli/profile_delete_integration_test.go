// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	nodeapi "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Deterministic API-layer coverage for `brewlet k8s profile delete`. envtest has
// no NodeProfile controller, kubelet or host provisioner: finalizers, cleanup
// status and terminating Pods are fixture-controlled, and concurrent writes are
// injected synchronously by a proxy in front of the API server. The production
// cleanup lifecycle on a real node is proven by integration-tests/e2e/live/workflows.py.

type profileDeleteReport struct {
	Profile, UID, DryRun, Reason, Message       string
	AlreadyDeleting, DeletionRequested, Deleted bool
	ClaimedNodes                                []string
	JavaWorkloads                               []struct {
		Namespace, Name, Node string
		Terminating           bool
	}
}

func (f *fixture) testProfileDeletion(t *testing.T) {
	t.Run("workload-guards-and-dry-runs", f.testDeleteWorkloadGuards)
	t.Run("source-of-truth-ownership", f.testDeleteOwnership)
	t.Run("restricted-pod-list", f.testDeleteRestricted)
	t.Run("uid-resource-version-preconditions", f.testDeletePreconditions)
	t.Run("blocked-timeout-and-attach", f.testDeleteLifecycle)
	t.Run("unsupported-pre-claim-refusal", f.testDeleteUnsupportedPreClaim)
}

func deleteArgs(name string, extra ...string) []string {
	return append([]string{"profile", "delete", name}, extra...)
}

// assertNotDeleted proves a refused or dry-run deletion left the exact object untouched.
func (f *fixture) assertNotDeleted(t *testing.T, before *nodeapi.NodeProfile) {
	t.Helper()
	after := f.getProfile(t, before.Name)
	if after.UID != before.UID || after.ResourceVersion != before.ResourceVersion || after.DeletionTimestamp != nil {
		t.Fatalf("profile changed: before uid=%s rv=%s, after uid=%s rv=%s deletion=%v",
			before.UID, before.ResourceVersion, after.UID, after.ResourceVersion, after.DeletionTimestamp)
	}
}

// proxyConfig returns a kubeconfig for a reverse proxy to the isolated API
// server that runs hook synchronously before forwarding each request.
func (f *fixture) proxyConfig(t *testing.T, name string, hook func(*http.Request)) string {
	t.Helper()
	upstream, err := url.Parse(f.config.Host)
	must(t, err)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport, err = rest.TransportFor(f.config)
	must(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hook(r)
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return f.writeConfig(t, name, &rest.Config{Host: server.URL})
}

func isProfileRequest(r *http.Request, name, method string) bool {
	return r.Method == method && strings.HasSuffix(r.URL.Path, "/nodeprofiles/"+name)
}

func (f *fixture) removePod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	current := &corev1.Pod{}
	if err := f.api.Get(f.ctx, client.ObjectKeyFromObject(pod), current); apierrors.IsNotFound(err) {
		return
	} else {
		must(t, err)
	}
	if len(current.Finalizers) > 0 {
		current.Finalizers = nil
		must(t, f.api.Update(f.ctx, current))
	}
	must(t, client.IgnoreNotFound(f.api.Delete(f.ctx, current, client.GracePeriodSeconds(0))))
}

func (f *fixture) javaPod(t *testing.T, name, node string, runtimeClass *string, phase corev1.PodPhase, finalizers ...string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team", Finalizers: finalizers},
		Spec: corev1.PodSpec{NodeName: node, RuntimeClassName: runtimeClass,
			TerminationGracePeriodSeconds: ptr.To(int64(300)),
			Containers:                    []corev1.Container{{Name: "app", Image: sourceImage}}}}
	must(t, f.api.Create(f.ctx, pod))
	pod.Status.Phase = phase
	must(t, f.api.Status().Update(f.ctx, pod))
	t.Cleanup(func() { f.removePod(t, pod) })
	return pod
}

func (f *fixture) testDeleteWorkloadGuards(t *testing.T) {
	p := f.profile(t, "delete-guards")
	node := f.node(t, p)
	terminating := f.javaPod(t, "terminating-java", node.Name, ptr.To("brewlet"), corev1.PodRunning, "example.com/hold")
	// Neither ordinary Pods nor completed Brewlet Pods on the node hold the runtime.
	f.javaPod(t, "ordinary", node.Name, nil, corev1.PodRunning)
	f.javaPod(t, "completed-java", node.Name, ptr.To("brewlet"), corev1.PodSucceeded)
	// Graceful deletion of a bound Pod: without a kubelet it stays inside its
	// 300s grace period with a deletion timestamp.
	must(t, f.api.Delete(f.ctx, terminating))
	current := &corev1.Pod{}
	must(t, f.api.Get(f.ctx, client.ObjectKeyFromObject(terminating), current))
	if current.DeletionTimestamp == nil || current.Status.Phase != corev1.PodRunning {
		t.Fatalf("fixture Pod is not terminating within its grace period: %+v", current.ObjectMeta)
	}
	before := f.getProfile(t, p.Name)
	guard := fmt.Sprintf("1 Java workload pod(s) still run or are terminating on nodes claimed by profile %q", p.Name)
	listed := "team/terminating-java (node " + node.Name + ", terminating)"
	for _, flags := range [][]string{nil, {"--dry-run"}, {"--dry-run=server"}, {"--wait"}} {
		r := f.cli(t, deleteArgs(p.Name, flags...)...)
		r.failure(t, guard)
		if !strings.Contains(r.stderr, listed) {
			t.Fatalf("terminating workload not identified: %s", r.stderr)
		}
		f.assertNotDeleted(t, before)
	}
	r := f.cli(t, deleteArgs(p.Name, "--yes", "--dry-run=server", "--output", "json")...)
	report := decode[profileDeleteReport](t, r.success(t))
	if report.DryRun != "server" || report.DeletionRequested || len(report.JavaWorkloads) != 1 ||
		!report.JavaWorkloads[0].Terminating || !strings.Contains(r.stderr, "Warning:") ||
		!strings.Contains(r.stderr, "Server dry run succeeded") {
		t.Fatalf("--yes server dry run did not report the overridden guard: %+v\n%s", report, r.stderr)
	}
	f.assertNotDeleted(t, before)

	f.removePod(t, terminating)
	r = f.cli(t, deleteArgs(p.Name, "--dry-run", "--output", "json")...)
	report = decode[profileDeleteReport](t, r.success(t))
	if report.DryRun != "client" || report.UID != string(p.UID) || report.DeletionRequested || report.Deleted ||
		!reflect.DeepEqual(report.ClaimedNodes, []string{node.Name}) || len(report.JavaWorkloads) != 0 ||
		!strings.Contains(r.stderr, "Server validation was not performed") {
		t.Fatalf("client dry-run plan: %+v\n%s", report, r.stderr)
	}
	f.assertNotDeleted(t, before)
	r = f.cli(t, deleteArgs(p.Name, "--dry-run=server", "--output", "yaml")...)
	if out := r.success(t); !strings.Contains(out, "dryRun: server") || !strings.Contains(out, "uid: "+string(p.UID)) {
		t.Fatalf("server dry-run plan: %s", out)
	}
	f.assertNotDeleted(t, before)
	f.cli(t, deleteArgs(p.Name, "--wait", "--dry-run")...).failure(t, "--wait cannot be combined with --dry-run")
	f.cli(t, deleteArgs(p.Name, "--wait-timeout", "1s")...).failure(t, "--wait-timeout requires --wait")
	f.assertNotDeleted(t, before)
}

func (f *fixture) testDeleteOwnership(t *testing.T) {
	for _, tc := range []struct {
		name                string
		labels, annotations map[string]string
		reason              string
	}{
		{"delete-helm", map[string]string{"app.kubernetes.io/managed-by": "Helm"},
			map[string]string{"meta.helm.sh/release-name": "brewlet"}, "is managed by Helm"},
		{"delete-gitops", nil, map[string]string{"argocd.argoproj.io/tracking-id": "apps:node.brewlet.sh/NodeProfile:/delete-gitops"},
			"is managed by argocd.argoproj.io/tracking-id="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := f.profile(t, tc.name)
			for key, value := range tc.labels {
				p.Labels[key] = value
			}
			p.Annotations = tc.annotations
			must(t, f.api.Update(f.ctx, p))
			before := f.getProfile(t, p.Name)
			for _, flags := range [][]string{nil, {"--yes"}, {"--dry-run"}, {"--dry-run=server"}, {"--wait"}} {
				f.cli(t, deleteArgs(p.Name, flags...)...).failure(t, tc.reason)
				f.assertNotDeleted(t, before)
			}
		})
	}
}

func (f *fixture) testDeleteRestricted(t *testing.T) {
	p := f.profile(t, "delete-restricted")
	f.node(t, p)
	user, err := f.server.AddUser(envtest.User{Name: "profile-deleter"}, f.config)
	must(t, err)
	config := f.writeConfig(t, "profile-deleter", user.Config())
	must(t, f.api.Create(f.ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "profile-deleter"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"node.brewlet.sh"}, Resources: []string{"nodeprofiles"},
				ResourceNames: []string{p.Name}, Verbs: []string{"get", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"list"}},
		}}))
	must(t, f.api.Create(f.ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "profile-deleter"},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "profile-deleter"},
		Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: "profile-deleter"}}}))
	must(t, wait.PollUntilContextTimeout(f.ctx, 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		r := f.command(t, "kubectl", "--kubeconfig", config, "--context", "selected", "auth", "can-i",
			"delete", "nodeprofiles.node.brewlet.sh/"+p.Name)
		return r.code == 0 && strings.TrimSpace(r.stdout) == "yes", nil
	}))
	before := f.getProfile(t, p.Name)
	for _, flags := range [][]string{nil, {"--dry-run"}, {"--dry-run=server"}} {
		r := f.cliConfig(t, config, deleteArgs(p.Name, flags...)...)
		r.failure(t, "cannot verify that no Java workloads run on the profile's nodes")
		if !strings.Contains(r.stderr, "forbidden") {
			t.Fatalf("fail-closed error hid the authorization cause: %s", r.stderr)
		}
		f.assertNotDeleted(t, before)
	}
	// --yes is the explicit override; prove it through a server dry run only.
	r := f.cliConfig(t, config, deleteArgs(p.Name, "--yes", "--dry-run=server", "--output", "json")...)
	if report := decode[profileDeleteReport](t, r.success(t)); report.DryRun != "server" || report.DeletionRequested {
		t.Fatalf("restricted --yes server dry run: %+v", report)
	}
	f.assertNotDeleted(t, before)
}

func (f *fixture) testDeletePreconditions(t *testing.T) {
	for _, replace := range []bool{false, true} {
		for _, dryRun := range []bool{false, true} {
			t.Run(fmt.Sprintf("recreate=%t/dry-run=%t", replace, dryRun), func(t *testing.T) {
				p := f.profile(t, fmt.Sprintf("delete-conflict-%t-%t", replace, dryRun))
				var once sync.Once
				mutations := make(chan error, 1)
				config := f.proxyConfig(t, p.Name, func(r *http.Request) {
					if !isProfileRequest(r, p.Name, http.MethodDelete) {
						return
					}
					// Runs after the CLI read and guarded the object, before its
					// precondition-bearing DELETE reaches the API server.
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
				})
				args := deleteArgs(p.Name)
				if dryRun {
					args = append(args, "--dry-run=server")
				}
				r := f.cliConfig(t, config, args...)
				if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "profile deletion failed") {
					t.Fatalf("stale deletion was not rejected with empty output: %+v", r)
				}
				select {
				case err := <-mutations:
					must(t, err)
				default:
					t.Fatal("test did not inject a concurrent write")
				}
				current := f.getProfile(t, p.Name)
				if current.DeletionTimestamp != nil {
					t.Fatal("stale deletion marked the changed profile for deletion")
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

// deletionState is the evidence a timeout or blocked cleanup must preserve.
type deletionState struct {
	Finalizers []string
	Deletion   *metav1.Time
	Status     nodeapi.NodeProfileStatus
	NodeLabels map[string]string
}

func (f *fixture) deletionState(t *testing.T, profile, node string) deletionState {
	t.Helper()
	p := f.getProfile(t, profile)
	n := &corev1.Node{}
	must(t, f.api.Get(f.ctx, client.ObjectKey{Name: node}, n))
	return deletionState{p.Finalizers, p.DeletionTimestamp, p.Status, n.Labels}
}

func (f *fixture) setReady(t *testing.T, name, reason, message string) {
	t.Helper()
	p := f.getProfile(t, name)
	p.Status.ObservedGeneration = p.Generation
	p.Status.Conditions = []metav1.Condition{{Type: nodeapi.ConditionReady, Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: p.Generation, LastTransitionTime: metav1.Now()}}
	if reason == nodeapi.ReasonAllNodesProvisioned {
		p.Status.Conditions[0].Status = metav1.ConditionTrue
	}
	must(t, f.api.Status().Update(f.ctx, p))
}

// finishCleanup stands in for the operator after host cleanup: it releases the
// node claim and then the cleanup finalizer.
func (f *fixture) finishCleanup(ctx context.Context, name, node string) error {
	n := &corev1.Node{}
	if err := f.api.Get(ctx, client.ObjectKey{Name: node}, n); err != nil {
		return err
	}
	delete(n.Labels, "brewlet.sh/owner-uid")
	delete(n.Labels, "brewlet.sh/owner-node-uid")
	if err := f.api.Update(ctx, n); err != nil {
		return err
	}
	p := &nodeapi.NodeProfile{}
	if err := f.api.Get(ctx, client.ObjectKey{Name: name}, p); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(p, brewlet.FinalizerCleanup)
	return f.api.Update(ctx, p)
}

func (f *fixture) cleanupProfile(t *testing.T, name string) (*nodeapi.NodeProfile, *corev1.Node) {
	t.Helper()
	p := f.profile(t, name)
	controllerutil.AddFinalizer(p, brewlet.FinalizerCleanup)
	must(t, f.api.Update(f.ctx, p))
	node := f.node(t, p)
	p = f.getProfile(t, name)
	p.Status.Targets = []nodeapi.NodeTarget{{Name: node.Name, UID: node.UID, Claimed: true}}
	must(t, f.api.Status().Update(f.ctx, p))
	f.setReady(t, name, nodeapi.ReasonAllNodesProvisioned, "fixture readiness")
	return f.getProfile(t, name), node
}

func (f *fixture) testDeleteUnsupportedPreClaim(t *testing.T) {
	p, node := f.cleanupProfile(t, "delete-pre-claim")
	must(t, f.api.Delete(f.ctx, p))
	message := "restore the original release to finish cleanup"
	f.setReady(t, p.Name, nodeapi.ReasonUnsupportedPreClaimState, message)
	before := f.getProfile(t, p.Name)
	evidence := f.deletionState(t, p.Name, node.Name)
	for _, flags := range [][]string{nil, {"--wait", "--wait-timeout", "20s"}} {
		start := time.Now()
		r := f.cli(t, deleteArgs(p.Name, append(flags, "--output", "json")...)...)
		if r.code != 1 || !strings.Contains(r.stderr, "cleanup is blocked (Ready=False/UnsupportedPreClaimState)") {
			t.Fatalf("pre-claim refusal %v: %+v", flags, r)
		}
		for _, want := range []string{message, "Restore the original release's compatible", "Never remove finalizers"} {
			if !strings.Contains(r.stderr, want) {
				t.Errorf("pre-claim refusal %v missing %q: %+v", flags, want, r)
			}
		}
		for _, unwanted := range []string{"timed out", "cleanup continues in the background", "Repair the profile spec", "Ready=False/CleanupBlocked", "Follow cleanup with"} {
			if strings.Contains(r.stderr, unwanted) {
				t.Errorf("pre-claim refusal %v has misleading %q: %+v", flags, unwanted, r)
			}
		}
		if elapsed := time.Since(start); elapsed > 15*time.Second {
			t.Fatalf("pre-claim refusal was not reported promptly: %s", elapsed)
		}
		report := decode[profileDeleteReport](t, r.stdout)
		if report.Reason != nodeapi.ReasonUnsupportedPreClaimState || report.Message != message ||
			!report.AlreadyDeleting || report.DeletionRequested || report.Deleted {
			t.Fatalf("pre-claim refusal report: %+v", report)
		}
		if after := f.getProfile(t, p.Name); !reflect.DeepEqual(after, before) {
			t.Fatalf("pre-claim refusal mutated the profile:\nbefore %+v\nafter %+v", before, after)
		}
		if after := f.deletionState(t, p.Name, node.Name); !reflect.DeepEqual(after, evidence) {
			t.Fatalf("pre-claim refusal changed cleanup evidence:\nbefore %+v\nafter %+v", evidence, after)
		}
	}
}

func (f *fixture) testDeleteLifecycle(t *testing.T) {
	p, node := f.cleanupProfile(t, "delete-lifecycle")
	r := f.cli(t, deleteArgs(p.Name, "--output", "json")...)
	report := decode[profileDeleteReport](t, r.success(t))
	if !report.DeletionRequested || report.Deleted || report.AlreadyDeleting ||
		!strings.Contains(r.stderr, "Deletion requested; host cleanup is asynchronous") {
		t.Fatalf("asynchronous deletion report: %+v\n%s", report, r.stderr)
	}
	if got := f.getProfile(t, p.Name); got.DeletionTimestamp == nil || !controllerutil.ContainsFinalizer(got, brewlet.FinalizerCleanup) {
		t.Fatal("deletion was not requested or the cleanup finalizer was removed")
	}
	f.setReady(t, p.Name, nodeapi.ReasonCleanupPending, "waiting for provisioners to stop and host cleanup to complete")
	before := f.deletionState(t, p.Name, node.Name)

	start := time.Now()
	r = f.cli(t, deleteArgs(p.Name, "--wait", "--wait-timeout", "2s")...)
	if r.code != 1 || !strings.Contains(r.stderr, "timed out after 2s") ||
		!strings.Contains(r.stderr, "cleanup continues in the background") ||
		!strings.Contains(r.stderr, nodeapi.ReasonCleanupPending) || !strings.Contains(r.stdout, "already deleting") {
		t.Fatalf("bounded --wait timeout diagnostics: %+v", r)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("--wait-timeout 2s took %s", elapsed)
	}
	if after := f.deletionState(t, p.Name, node.Name); !reflect.DeepEqual(after, before) {
		t.Fatalf("timeout changed cleanup evidence:\nbefore %+v\nafter  %+v", before, after)
	}
	r = f.cli(t, deleteArgs(p.Name)...)
	if r.success(t); !strings.Contains(r.stdout, "action: already deleting") ||
		!strings.Contains(r.stderr, "is already deleting. Follow cleanup with") {
		t.Fatalf("attach without --wait: %+v", r)
	}

	f.setReady(t, p.Name, nodeapi.ReasonCleanupBlocked, "fixture mirror policy denies the cleanup source")
	blocked := f.deletionState(t, p.Name, node.Name)
	for _, flags := range [][]string{nil, {"--wait", "--wait-timeout", "20s"}} {
		start = time.Now()
		r = f.cli(t, deleteArgs(p.Name, flags...)...)
		if r.code != 1 || !strings.Contains(r.stderr, "cleanup is blocked (Ready=False/CleanupBlocked)") ||
			!strings.Contains(r.stderr, "fixture mirror policy denies the cleanup source") ||
			!strings.Contains(r.stderr, "Never remove finalizers") {
			t.Fatalf("blocked cleanup %v: %+v", flags, r)
		}
		if elapsed := time.Since(start); elapsed > 15*time.Second {
			t.Fatalf("blocked cleanup was not reported promptly: %s", elapsed)
		}
		if after := f.deletionState(t, p.Name, node.Name); !reflect.DeepEqual(after, blocked) {
			t.Fatalf("blocked cleanup changed evidence:\nbefore %+v\nafter  %+v", blocked, after)
		}
	}

	f.setReady(t, p.Name, nodeapi.ReasonCleanupPending, "waiting for provisioners to stop and host cleanup to complete")
	var gets int
	var mu sync.Mutex
	finished := make(chan error, 1)
	config := f.proxyConfig(t, p.Name+"-attach", func(r *http.Request) {
		if !isProfileRequest(r, p.Name, http.MethodGet) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		// The first GET is the guarded read; the second is the first --wait poll.
		if gets++; gets == 2 {
			finished <- f.finishCleanup(r.Context(), p.Name, node.Name)
		}
	})
	r = f.cliConfig(t, config, deleteArgs(p.Name, "--wait", "--wait-timeout", "30s", "--output", "json")...)
	report = decode[profileDeleteReport](t, r.success(t))
	must(t, <-finished)
	if !report.Deleted || !report.AlreadyDeleting || report.DeletionRequested ||
		!strings.Contains(r.stderr, "is already deleting; following its cleanup") ||
		!strings.Contains(r.stderr, "deleted after") {
		t.Fatalf("attached --wait: %+v\n%s", report, r.stderr)
	}
	if err := f.api.Get(f.ctx, client.ObjectKey{Name: p.Name}, &nodeapi.NodeProfile{}); !apierrors.IsNotFound(err) {
		t.Fatalf("profile still exists after reported deletion: %v", err)
	}

	fresh, freshNode := f.cleanupProfile(t, "delete-wait")
	var deleted bool
	finished = make(chan error, 1)
	config = f.proxyConfig(t, fresh.Name, func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case isProfileRequest(r, fresh.Name, http.MethodDelete):
			deleted = true
		case deleted && isProfileRequest(r, fresh.Name, http.MethodGet):
			deleted = false
			finished <- f.finishCleanup(r.Context(), fresh.Name, freshNode.Name)
		}
	})
	r = f.cliConfig(t, config, deleteArgs(fresh.Name, "--wait", "--wait-timeout", "30s", "--output", "json")...)
	report = decode[profileDeleteReport](t, r.success(t))
	must(t, <-finished)
	if !report.Deleted || !report.DeletionRequested || report.AlreadyDeleting {
		t.Fatalf("fresh --wait deletion: %+v\n%s", report, r.stderr)
	}
}
