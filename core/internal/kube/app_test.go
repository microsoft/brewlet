// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/brewlet/internal/progress"
)

// appStatusJSON mirrors DeployMojoTest.status so both tools are tested against
// the same JavaApplication shapes.
func appStatusJSON(generation, observed int, ready, reason, message string) string {
	return fmt.Sprintf(`{"kind":"JavaApplication","metadata":{"name":"orders","namespace":"apps","uid":"app-uid","generation":%d},
	  "status":{"observedGeneration":%d,"readyReplicas":1,"selectedJdk":"temurin-21","conditions":[{
	  "type":"Ready","status":"%s","reason":"%s","message":"%s","observedGeneration":%d}]}}`,
		generation, observed, ready, reason, message, observed)
}

func TestAppReadinessMatchesDeployMojo(t *testing.T) {
	for _, tc := range []struct {
		name, raw, key, phase string
		ready                 bool
	}{
		{"unobserved generation", `{"metadata":{"generation":2},"status":{"observedGeneration":1}}`,
			"Pending: waiting for the Brewlet operator to reconcile generation 2", "Pending", false},
		{"stale ready condition", strings.Replace(appStatusJSON(2, 2, "True", "Reconciled", "old"),
			`"observedGeneration":2}]`, `"observedGeneration":1}]`, 1), "Reconciled: old", "Pending", false},
		{"progressing", appStatusJSON(2, 2, "False", "Progressing", "Rollout 0/1 ready"),
			"Progressing: Rollout 0/1 ready", "Progressing", false},
		{"reconcile error", appStatusJSON(1, 1, "False", "ReconcileError", "no node offers temurin-25"),
			"ReconcileError: no node offers temurin-25", "Failed", false},
		{"ready", appStatusJSON(2, 2, "True", "Reconciled", "Rollout complete"), "Reconciled: Rollout complete", "Ready", true},
		{"no ready condition", `{"metadata":{"generation":1},"status":{"observedGeneration":1}}`,
			"Pending: no Ready condition reported yet", "Pending", false},
		{"condition without generation", `{"metadata":{"generation":1},"status":{"observedGeneration":1,
			"conditions":[{"type":"Ready","status":"True"}]}}`, "Unknown", "Ready", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := objectJSON(t, tc.raw)
			state := appReadiness(app)
			if state.Ready != tc.ready || state.key() != tc.key || appPhase(app, state) != tc.phase {
				t.Fatalf("got ready=%t key=%q phase=%q", state.Ready, state.key(), appPhase(app, state))
			}
		})
	}
	ready := appReadiness(objectJSON(t, appStatusJSON(1, 1, "True", "Reconciled", "done")))
	if ready.detail() != "Reconciled: done (ready replicas: 1)" || ready.SelectedJDK != "temurin-21" {
		t.Fatalf("unexpected detail: %+v", ready)
	}
}

func appStatusFixtures(t *testing.T) (object, []object, []object) {
	app := objectJSON(t, appStatusJSON(3, 3, "False", "Progressing", "Rollout 1/2 ready"))
	dep := objectJSON(t, `{"kind":"Deployment","metadata":{"name":"orders","uid":"dep-uid",
		"ownerReferences":[{"uid":"app-uid","controller":true}]},"spec":{}}`)
	rs := objectJSON(t, `{"kind":"ReplicaSet","metadata":{"name":"orders-rs","uid":"rs-uid",
		"ownerReferences":[{"uid":"dep-uid","controller":true}]}}`)
	pod := func(name, node, ready string) object {
		return objectJSON(t, `{"kind":"Pod","metadata":{"name":"`+name+`","uid":"`+name+`-uid",
			"ownerReferences":[{"uid":"rs-uid","controller":true}],"annotations":{"brewlet.sh/jdk":"temurin-21"}},
			"spec":{"nodeName":"`+node+`"},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"`+ready+`"}]}}`)
	}
	foreign := objectJSON(t, `{"kind":"Pod","metadata":{"name":"foreign","uid":"foreign-uid",
		"ownerReferences":[{"uid":"other","controller":true}]},"spec":{"nodeName":"elsewhere"}}`)
	workloads := []object{pod("orders-b", "worker-2", "False"), pod("orders-a", "worker-1", "True"), pod("orders-c", "worker-1", "True"), foreign, rs, dep}
	var events []object
	for i := 0; i < 12; i++ {
		events = append(events, objectJSON(t, fmt.Sprintf(`{"involvedObject":{"kind":"Pod","name":"orders-a","uid":"orders-a-uid"},
			"type":"Normal","reason":"R%02d","message":"event %d","lastTimestamp":"2026-10-02T10:%02d:00Z"}`, i, i, 59-i)))
	}
	events = append(events,
		objectJSON(t, `{"involvedObject":{"kind":"JavaApplication","name":"orders","uid":"app-uid"},
			"type":"Warning","reason":"Newest","message":"latest","eventTime":"2026-10-02T11:00:00.123456Z"}`),
		objectJSON(t, `{"involvedObject":{"kind":"Pod","name":"foreign","uid":"foreign-uid"},
			"reason":"Foreign","lastTimestamp":"2026-10-02T12:00:00Z"}`))
	return app, workloads, events
}

func appStatusExecutor(t *testing.T, app object, workloads, events []object) executor {
	return func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
		switch {
		case hasArgs(args, "get", appsResource, "orders"):
			return rawJSON(t, app), nil
		case hasArgs(args, "get", "events"):
			if !hasArgs(args, "--namespace", "apps") {
				t.Errorf("events not read from the app namespace: %v", args)
			}
			return listJSON(t, events...), nil
		case hasArgs(args, "get", "deployments,replicasets,pods"):
			return listJSON(t, workloads...), nil
		}
		t.Fatalf("unexpected kubectl call: %v", args)
		return nil, nil
	}
}

func TestAppStatusReportsReadinessNodesAndRecentEvents(t *testing.T) {
	app, workloads, events := appStatusFixtures(t)
	out, _, err := runTest(t, []string{"app", "status", "orders", "--output", "json"}, appStatusExecutor(t, app, workloads, events))
	if err != nil {
		t.Fatal(err)
	}
	var report appStatusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Ready || report.Phase != "Progressing" || report.Reason != "Progressing" || report.Generation != 3 ||
		report.ObservedGeneration != 3 || report.SelectedJDK != "temurin-21" || report.Namespace != "apps" {
		t.Fatalf("unexpected readiness: %+v", report)
	}
	if strings.Join(report.Nodes, ",") != "worker-1,worker-2" || len(report.Pods) != 3 || report.Pods[0].Name != "orders-a" {
		t.Fatalf("unexpected pods/nodes: %+v", report)
	}
	if len(report.Events) != maxRecentEvents || report.Events[len(report.Events)-1].Reason != "Newest" ||
		report.Events[0].Reason != "R08" || strings.Contains(out, "Foreign") || strings.Contains(out, "foreign") {
		t.Fatalf("events must be the most recent owned ones, oldest first: %+v", report.Events)
	}

	out, _, err = runTest(t, []string{"--context", "aks", "app", "status", "orders"}, appStatusExecutor(t, app, workloads, events))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Phase:", "Progressing", "Ready:", "False (Progressing: Rollout 1/2 ready)",
		"Generation:", "3 (observed 3)", "Selected JDK:", "temurin-21", "worker-1, worker-2",
		"CONDITIONS", "PODS", "RECENT EVENTS", "2026-10-02T11:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}

	// A not-ready application is still a successful read.
	notFound := func(context.Context, string, []string, []byte) ([]byte, error) {
		return nil, errors.New(`kubectl: exit status 1: Error from server (NotFound): javaapplications "orders" not found`)
	}
	if _, _, err := runTest(t, []string{"app", "status", "orders"}, notFound); err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Fatalf("missing application must fail: %v", err)
	}
}

func fastProgress(t *testing.T) {
	t.Helper()
	poll, heartbeat := progress.PollInterval, progress.HeartbeatInterval
	progress.PollInterval, progress.HeartbeatInterval = time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { progress.PollInterval, progress.HeartbeatInterval = poll, heartbeat })
}

type scriptedApp struct {
	mu        sync.Mutex
	responses []func() ([]byte, error)
	calls     [][]string
}

func (s *scriptedApp) exec(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, args)
	next := s.responses[0]
	if len(s.responses) > 1 {
		s.responses = s.responses[1:]
	}
	return next()
}

func respondJSON(raw string) func() ([]byte, error) {
	return func() ([]byte, error) { return []byte(raw), nil }
}

func TestAppWaitReportsChangesUntilReadyForCurrentGeneration(t *testing.T) {
	fastProgress(t)
	script := &scriptedApp{responses: []func() ([]byte, error){
		func() ([]byte, error) {
			return nil, errors.New(`kubectl: exit status 1: Error from server (NotFound): javaapplications "orders" not found`)
		},
		respondJSON(`{"kind":"JavaApplication","metadata":{"name":"orders","namespace":"apps","generation":2},"status":{"observedGeneration":1}}`),
		respondJSON(strings.Replace(appStatusJSON(2, 2, "True", "Reconciled", "old"), `"observedGeneration":2}]`, `"observedGeneration":1}]`, 1)),
		respondJSON(appStatusJSON(2, 2, "False", "Progressing", "Rollout 0/1 ready")),
		respondJSON(appStatusJSON(2, 2, "False", "Progressing", "Rollout 0/1 ready")),
		respondJSON(appStatusJSON(2, 2, "False", "ReconcileError", "no node offers temurin-25")),
		respondJSON(appStatusJSON(2, 2, "True", "Reconciled", "Rollout complete")),
	}}
	out, stderr, err := runTest(t, []string{"--kubeconfig", "/tmp/kc", "--context", "aks", "app", "wait", "orders",
		"--namespace", "apps", "--wait-timeout", "10s"}, script.exec)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr)
	}
	for _, want := range []string{
		"Waiting up to 10s for JavaApplication apps/orders to become Ready...",
		"NotFound",
		"Pending: waiting for the Brewlet operator to reconcile generation 2",
		"Reconciled: old",
		"ReconcileError: no node offers temurin-25",
		"Ready: Reconciled: Rollout complete (ready replicas: 1)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	if n := strings.Count(stderr, "Progressing: Rollout 0/1 ready"); n != 1 {
		t.Errorf("unchanged status must be logged once, got %d:\n%s", n, stderr)
	}
	if strings.TrimSpace(out) != "orders is Ready in namespace apps (JDK temurin-21)" {
		t.Errorf("unexpected stdout %q", out)
	}
	for _, args := range script.calls {
		if !hasArgs(args, "--kubeconfig", "/tmp/kc") || !hasArgs(args, "--context", "aks") ||
			!hasArgs(args, "get", appsResource, "orders", "-o", "json") || !hasArgs(args, "--namespace", "apps") {
			t.Fatalf("unexpected kubectl call: %v", args)
		}
	}
}

func TestAppWaitTimesOutWithLastStatusAndDescribeHint(t *testing.T) {
	fastProgress(t)
	script := &scriptedApp{responses: []func() ([]byte, error){respondJSON(appStatusJSON(1, 1, "False", "Progressing", "x"))}}
	out, stderr, err := runTest(t, []string{"--context", "aks", "app", "wait", "orders", "--wait-timeout", "60ms"}, script.exec)
	if err == nil {
		t.Fatal("timeout must fail")
	}
	for _, want := range []string{
		"JavaApplication apps/orders was not Ready after 60ms: Progressing: x (ready replicas: 1)",
		"kubectl --context aks describe javaapplication orders -n apps",
		"brewlet k8s --context aks app status orders --namespace apps",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
	if out != "" {
		t.Errorf("timeout must not print a Ready line: %q", out)
	}
	if !strings.Contains(stderr, "... waiting for orders to become Ready") {
		t.Errorf("non-terminal waits must print a heartbeat:\n%s", stderr)
	}
	if strings.Contains(stderr, "\r") {
		t.Errorf("non-terminal progress must not redraw lines: %q", stderr)
	}
}

func TestDescribeHintQuotesValues(t *testing.T) {
	c := &client{opts: options{kubeconfig: "/tmp/my kube/config", context: "it's prod"}}
	got := c.describeHint("apps", "orders")
	want := `kubectl --kubeconfig '/tmp/my kube/config' --context 'it'"'"'s prod' describe javaapplication orders -n apps` +
		` (or brewlet k8s --kubeconfig '/tmp/my kube/config' --context 'it'"'"'s prod' app status orders --namespace apps)`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if shellQuote("arn:aws:eks:us-east-1:1:cluster/a_b") != "arn:aws:eks:us-east-1:1:cluster/a_b" {
		t.Fatal("safe values should stay unquoted")
	}
}

func TestAppWaitStopsOnCancellation(t *testing.T) {
	fastProgress(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	exec := func(context.Context, string, []string, []byte) ([]byte, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return []byte(appStatusJSON(1, 1, "False", "Progressing", "x")), nil
	}
	var out, stderr strings.Builder
	err := run(ctx, []string{"app", "wait", "orders"}, &out, &stderr, exec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
}

func TestAppCommandValidation(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"app", "wait"}, "expects 1 positional"},
		{[]string{"app", "status"}, "expects 1 positional"},
		{[]string{"app", "status", "Bad_Name"}, ""},
		{[]string{"app", "wait", "orders", "--output", "json"}, "flag provided but not defined"},
		{[]string{"app", "wait", "orders", "--wait-timeout", "0s"}, "--wait-timeout must be positive"},
		{[]string{"app", "status", "orders", "--wait-timeout", "1m"}, "flag provided but not defined"},
		{[]string{"app", "status", "orders", "--output", "wide"}, "invalid --output"},
		{[]string{"app", "restart", "orders"}, "unknown Kubernetes command"},
	} {
		_, _, err := runTest(t, tc.args, noExecution(t))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
	out, _, err := runTest(t, []string{"app"}, noExecution(t))
	if err != nil || !strings.Contains(out, "app wait NAME") || !strings.Contains(out, "--wait-timeout 5m") {
		t.Fatalf("app without a subcommand should print help: %v\n%s", err, out)
	}
}
