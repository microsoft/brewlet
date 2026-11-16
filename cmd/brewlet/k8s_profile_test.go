package main

import (
	"errors"
	"flag"
	"strings"
	"testing"

	api "github.com/microsoft/brewlet/kubernetes/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func k8sPooledProfile(name string, pools []string, entries ...api.RuntimeEntry) *api.NodeProfile {
	p := k8sProfile(name, entries...)
	p.Spec.NodePool.Names = pools
	return p
}

func TestK8sProfileHelp(t *testing.T) {
	out, _, err := k8sRun("profile")
	if err != nil || !strings.Contains(out, "add-runtime") || !strings.Contains(out, "remove-runtime") {
		t.Fatalf("profile help: %v\n%s", err, out)
	}
	if out, _, err := k8sRun(); err != nil || !strings.Contains(out, "profile create") {
		t.Fatalf("k8s help missing profile commands: %v\n%s", err, out)
	}
	out, _, err = k8sRun("np", "create", "--help")
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(out, "--all-nodes") || !strings.Contains(out, "Examples:") {
		t.Fatalf("create help: %v\n%s", err, out)
	}
	if _, _, err := k8sRun("profile", "bogus"); err == nil {
		t.Fatal("unknown profile command accepted")
	}
}

func TestK8sProfileListAndStatus(t *testing.T) {
	web := k8sPooledProfile("web", []string{"userpool"}, api.RuntimeEntry{ID: "python-3.12"}, api.RuntimeEntry{ID: "node-22", Install: "OnDemand", IdleTTL: "72h"})
	web.Status.AssignedNodes, web.Status.ReadyNodes = 1, 1
	k8sFakeClient(t, web, k8sProfile("rest", api.RuntimeEntry{ID: "java-21"}), k8sApproved("python-3.12", k8sOldSource), k8sNode("node-a", "amd64"))

	out, _, err := k8sRun("profile", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "web", "userpool", "python-3.12,node-22(OnDemand)", "1/1", "rest", "all nodes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("list missing %q:\n%s", want, out)
		}
	}
	if out, _, err := k8sRun("profile", "list", "-o", "json"); err != nil || !strings.Contains(out, `"kind": "NodeProfileList"`) {
		t.Fatalf("list json: %v\n%s", err, out)
	}

	out, _, err = k8sRun("profile", "status", "web")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Pool:", "userpool", "Ready:", "AllNodesProvisioned", "python-3.12", "Missing", "node-22", "72h", "node-a"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	if _, _, err := k8sRun("profile", "status", "nope"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing profile: %v", err)
	}
	if _, _, err := k8sRun("profile", "status"); err == nil || !strings.Contains(err.Error(), "profile name required") {
		t.Fatalf("missing name: %v", err)
	}
}

func TestK8sProfileCreate(t *testing.T) {
	c := k8sFakeClient(t, k8sApproved("python-3.12", k8sOldSource))

	out, stderr, err := k8sRun("profile", "create", "web", "--pool", "userpool", "--pool", "burst", "--runtime", "python-3.12", "--runtime", "node-22:OnDemand:72h",
		"--toleration", "dedicated=brewlet:NoSchedule", "--toleration", "gpu:NoExecute", "--max-unavailable", "2", "--dry-run")
	if err != nil {
		t.Fatal(err, stderr)
	}
	for _, want := range []string{"kind: NodeProfile", "- userpool", "- burst", "id: python-3.12", "install: OnDemand", "idleTTL: 72h", "operator: Equal", "operator: Exists", "maxUnavailable: 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("manifest missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(stderr, "dry run") || !strings.Contains(stderr, "RuntimeImage node-22 does not exist") {
		t.Fatalf("dry run stderr: %s", stderr)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "web"}, new(api.NodeProfile)); !apierrors.IsNotFound(err) {
		t.Fatalf("dry run created the profile: %v", err)
	}

	out, _, err = k8sRun("profile", "create", "web", "--pool", "userpool", "--runtime", "python-3.12")
	if err != nil || !strings.Contains(out, "nodeprofile.node.brewlet.sh/web created") {
		t.Fatalf("create: %v\n%s", err, out)
	}
	p := new(api.NodeProfile)
	if err := c.Get(t.Context(), client.ObjectKey{Name: "web"}, p); err != nil {
		t.Fatal(err)
	}
	if len(p.Spec.NodePool.Names) != 1 || p.Spec.NodePool.Names[0] != "userpool" || len(p.Spec.RuntimeImages) != 1 || p.Spec.RuntimeImages[0].ID != "python-3.12" {
		t.Fatalf("unexpected spec: %+v", p.Spec)
	}
}

func TestK8sProfileCreateRejects(t *testing.T) {
	k8sFakeClient(t, k8sPooledProfile("web", []string{"userpool"}, api.RuntimeEntry{ID: "python-3.12"}), k8sProfile("rest", api.RuntimeEntry{ID: "python-3.12"}))
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no runtime":      {[]string{"new", "--pool", "p"}, "at least one --runtime"},
		"no selection":    {[]string{"new", "--runtime", "python-3.12"}, "--pool NAME, or --all-nodes"},
		"both selections": {[]string{"new", "--pool", "p", "--all-nodes", "--runtime", "python-3.12"}, "mutually exclusive"},
		"pool key":        {[]string{"new", "--all-nodes", "--pool-key", "agentpool", "--runtime", "python-3.12"}, "--pool-key requires --pool"},
		"bad name":        {[]string{"New_Profile", "--pool", "p", "--runtime", "python-3.12"}, "invalid NodeProfile name"},
		"bad id":          {[]string{"new", "--pool", "p", "--runtime", "Python"}, "invalid --runtime"},
		"bad install":     {[]string{"new", "--pool", "p", "--runtime", "python-3.12:Lazy"}, "Eager or OnDemand"},
		"ttl needs mode":  {[]string{"new", "--pool", "p", "--runtime", "python-3.12:Eager:1h"}, "OnDemand"},
		"duplicate id":    {[]string{"new", "--pool", "p", "--runtime", "python-3.12", "--runtime", "python-3.12"}, "duplicate"},
		"bad toleration":  {[]string{"new", "--pool", "p", "--runtime", "python-3.12", "--toleration", "k=v:Bogus"}, "effect must be"},
		"pool conflict":   {[]string{"new", "--pool", "userpool", "--runtime", "python-3.12"}, "PoolConflict"},
		"second catchall": {[]string{"new", "--all-nodes", "--runtime", "python-3.12"}, "catch-all"},
		"exists":          {[]string{"web", "--pool", "p", "--runtime", "python-3.12"}, "already exists"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := k8sRun(append([]string{"profile", "create"}, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestK8sProfileDelete(t *testing.T) {
	objs := append(k8sWorkloads("python-3.12", false), k8sPooledProfile("web", []string{"userpool"}, api.RuntimeEntry{ID: "python-3.12"}), k8sApproved("python-3.12", k8sOldSource))
	c := k8sFakeClient(t, objs...)
	exists := func() bool {
		return c.Get(t.Context(), client.ObjectKey{Name: "web"}, new(api.NodeProfile)) == nil
	}

	out, stderr, err := k8sRun("profile", "delete", "web", "--dry-run")
	if err != nil || !strings.Contains(out, "kind: NodeProfile") || !strings.Contains(stderr, "apps/web-abc") || !strings.Contains(stderr, "dry run") {
		t.Fatalf("dry run: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	out, _, err = k8sRun("profile", "delete", "web")
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(out, "node-a") || !exists() {
		t.Fatalf("delete without --yes: %v\n%s", err, out)
	}
	if out, _, err = k8sRun("profile", "delete", "web", "--yes"); err != nil || !strings.Contains(out, "deleted") || exists() {
		t.Fatalf("delete --yes: %v\n%s", err, out)
	}
}

func TestK8sProfileDeleteUnassignedNeedsNoConfirmation(t *testing.T) {
	p := k8sPooledProfile("idle", []string{"p"}, api.RuntimeEntry{ID: "python-3.12"})
	p.Status.Targets = nil
	c := k8sFakeClient(t, p)
	if out, _, err := k8sRun("profile", "delete", "idle"); err != nil || !strings.Contains(out, "No nodes or pods are affected") {
		t.Fatalf("delete: %v\n%s", err, out)
	}
	if err := c.Get(t.Context(), client.ObjectKey{Name: "idle"}, new(api.NodeProfile)); !apierrors.IsNotFound(err) {
		t.Fatalf("profile still exists: %v", err)
	}
}

func TestK8sProfileAddAndRemoveRuntime(t *testing.T) {
	web := k8sPooledProfile("web", []string{"userpool"}, api.RuntimeEntry{ID: "python-3.12"})
	objs := append(k8sWorkloads("python-3.12", false), web, k8sApproved("python-3.12", k8sOldSource), k8sApproved("node-22", k8sOldSource))
	c := k8sFakeClient(t, objs...)
	entries := func() []api.RuntimeEntry {
		p := new(api.NodeProfile)
		if err := c.Get(t.Context(), client.ObjectKey{Name: "web"}, p); err != nil {
			t.Fatal(err)
		}
		return p.Spec.RuntimeImages
	}

	if out, _, err := k8sRun("profile", "add-runtime", "web", "node-22", "--install", "OnDemand", "--idle-ttl", "72h", "--dry-run"); err != nil || !strings.Contains(out, "id: node-22") || len(entries()) != 1 {
		t.Fatalf("add dry run: %v\n%s", err, out)
	}
	if out, _, err := k8sRun("profile", "add-runtime", "web", "node-22", "--install", "OnDemand", "--idle-ttl", "72h"); err != nil || !strings.Contains(out, "now offers node-22 (OnDemand)") {
		t.Fatalf("add: %v\n%s", err, out)
	}
	if got := entries(); len(got) != 2 || got[1] != (api.RuntimeEntry{ID: "node-22", Install: "OnDemand", IdleTTL: "72h"}) {
		t.Fatalf("entries after add: %+v", got)
	}
	if out, _, err := k8sRun("profile", "add-runtime", "web", "node-22", "--install", "OnDemand", "--idle-ttl", "72h"); err != nil || !strings.Contains(out, "nothing to do") {
		t.Fatalf("idempotent add: %v\n%s", err, out)
	}
	if _, _, err := k8sRun("profile", "add-runtime", "web", "node-22"); err == nil || !strings.Contains(err.Error(), "already lists") {
		t.Fatalf("conflicting add: %v", err)
	}
	if _, _, err := k8sRun("profile", "add-runtime", "web", "java-21", "--idle-ttl", "1h"); err == nil {
		t.Fatal("idle TTL without OnDemand accepted")
	}
	if _, _, err := k8sRun("profile", "add-runtime", "web"); err == nil || !strings.Contains(err.Error(), "runtime ID required") {
		t.Fatalf("missing ID: %v", err)
	}

	out, _, err := k8sRun("profile", "remove-runtime", "web", "python-3.12")
	if err == nil || !strings.Contains(err.Error(), "--yes") || !strings.Contains(out, "apps/web-abc") || len(entries()) != 2 {
		t.Fatalf("remove with live pods: %v\n%s", err, out)
	}
	if out, _, err := k8sRun("profile", "remove-runtime", "web", "python-3.12", "--yes"); err != nil || !strings.Contains(out, "no longer offers python-3.12") {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if got := entries(); len(got) != 1 || got[0].ID != "node-22" {
		t.Fatalf("entries after remove: %+v", got)
	}
	if _, _, err := k8sRun("profile", "remove-runtime", "web", "node-22"); err == nil || !strings.Contains(err.Error(), "cannot be empty") {
		t.Fatalf("emptying a profile: %v", err)
	}
	if _, _, err := k8sRun("profile", "remove-runtime", "web", "ruby-3"); err == nil || !strings.Contains(err.Error(), "does not offer") {
		t.Fatalf("unknown runtime: %v", err)
	}
}

func TestK8sProfileHelmManagedWarning(t *testing.T) {
	p := k8sPooledProfile("chart", []string{"p"}, api.RuntimeEntry{ID: "python-3.12"})
	p.Labels = map[string]string{"app.kubernetes.io/managed-by": "Helm"}
	k8sFakeClient(t, p, k8sApproved("node-22", k8sOldSource))
	_, stderr, err := k8sRun("profile", "add-runtime", "chart", "node-22", "--dry-run")
	if err != nil || !strings.Contains(stderr, "managed by Helm") {
		t.Fatalf("expected Helm warning: %v\n%s", err, stderr)
	}
}

func TestParseProfileToleration(t *testing.T) {
	for in, want := range map[string]corev1.Toleration{
		"dedicated=brewlet:NoSchedule": {Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "brewlet", Effect: corev1.TaintEffectNoSchedule},
		"gpu":                          {Key: "gpu", Operator: corev1.TolerationOpExists},
		"example.com/x:NoExecute":      {Key: "example.com/x", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	} {
		got, err := parseProfileToleration(in)
		if err != nil || got != want {
			t.Fatalf("%q: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "=v", ":NoSchedule", "k:Bad"} {
		if _, err := parseProfileToleration(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}
