// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package doctor

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const healthyNodes = `{
  "items": [{
    "metadata": {
      "name": "node-1",
      "labels": {"brewlet.sh/runtime": "ready"},
      "annotations": {"brewlet.sh/jdks": "temurin-21"}
    },
    "spec": {"unschedulable": false},
    "status": {"nodeInfo": {"containerRuntimeVersion": "containerd://2.1.3"}}
  }]
}`

func TestRunHealthyCluster(t *testing.T) {
	exec := func(args ...string) ([]byte, error) {
		switch command(args) {
		case "config current-context":
			return []byte("kind-brewlet\n"), nil
		case "get --raw=/readyz":
			return []byte("ok\n"), nil
		case "get runtimeclass brewlet -o name":
			return []byte("runtimeclass.node.k8s.io/brewlet\n"), nil
		case "get crd javaapplications.apps.brewlet.sh -o name":
			return []byte("customresourcedefinition.apiextensions.k8s.io/javaapplications.apps.brewlet.sh\n"), nil
		case "get nodes -o json":
			return []byte(healthyNodes), nil
		case "auth can-i create javaapplications.apps.brewlet.sh -n apps":
			return []byte("yes\n"), nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return nil, nil
		}
	}

	var streamed []Check
	report := Run(exec, Options{Namespace: "apps", OnCheck: func(check Check) { streamed = append(streamed, check) }})
	if !report.OK() {
		t.Fatalf("report should be healthy: %+v", report)
	}
	if len(report.Checks) != 7 {
		t.Fatalf("checks = %d, want 7", len(report.Checks))
	}
	if !reflect.DeepEqual(streamed, report.Checks) {
		t.Fatalf("OnCheck must stream every check in report order: %+v", streamed)
	}
}

func TestRunReportsMissingPlatform(t *testing.T) {
	exec := func(args ...string) ([]byte, error) {
		switch command(args) {
		case "config current-context":
			return []byte("dev\n"), nil
		case "get --raw=/readyz":
			return []byte("ok\n"), nil
		case "get runtimeclass brewlet -o name":
			return []byte(`Error from server (NotFound): runtimeclasses.node.k8s.io "brewlet" not found`), errors.New("exit status 1")
		case "get crd javaapplications.apps.brewlet.sh -o name":
			return []byte("not found"), errors.New("exit status 1")
		case "get nodes -o json":
			return []byte(`{"items":[]}`), nil
		case "config view --minify -o jsonpath={.contexts[0].context.namespace}":
			return nil, nil
		case "auth can-i create javaapplications.apps.brewlet.sh -n default":
			return []byte("no\n"), nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return nil, nil
		}
	}

	report := Run(exec, Options{})
	if report.OK() {
		t.Fatalf("report should contain failures: %+v", report)
	}
	failures := 0
	for _, check := range report.Checks {
		if check.Status == Fail {
			failures++
		}
	}
	if failures != 5 {
		t.Fatalf("failures = %d, want 5: %+v", failures, report)
	}
}

func TestRunPassesKubectlOptions(t *testing.T) {
	exec := func(args ...string) ([]byte, error) {
		prefix := "--kubeconfig /tmp/k --context prod "
		got := command(args)
		if !strings.HasPrefix(got, prefix) {
			t.Fatalf("command %q missing prefix %q", got, prefix)
		}
		switch strings.TrimPrefix(got, prefix) {
		case "config current-context":
			return []byte("prod\n"), nil
		case "get --raw=/readyz":
			return []byte("ok\n"), nil
		case "get runtimeclass brewlet -o name", "get crd javaapplications.apps.brewlet.sh -o name":
			return []byte("resource\n"), nil
		case "get nodes -o json":
			return []byte(healthyNodes), nil
		case "auth can-i create javaapplications.apps.brewlet.sh -n apps":
			return []byte("yes\n"), nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return nil, nil
		}
	}

	if report := Run(exec, Options{Kubeconfig: "/tmp/k", Context: "prod", Namespace: "apps"}); !report.OK() {
		t.Fatalf("report should be healthy: %+v", report)
	}
}

func command(args []string) string {
	return strings.Join(args, " ")
}

func TestRunDefaultsToContextNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, namespace, want string
		err                   error
	}{
		{name: "context namespace", namespace: "apps\n", want: "apps"},
		{name: "unset", want: "default"},
		{name: "lookup failure", err: errors.New("exit status 1"), want: "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var canI string
			exec := func(args ...string) ([]byte, error) {
				switch got := command(args); {
				case got == "config view --minify -o jsonpath={.contexts[0].context.namespace}":
					return []byte(tc.namespace), tc.err
				case strings.HasPrefix(got, "auth can-i"):
					canI = got
					return []byte("yes\n"), nil
				case got == "get nodes -o json":
					return []byte(`{"items":[]}`), nil
				default:
					return []byte("ok\n"), nil
				}
			}
			Run(exec, Options{})
			if want := "auth can-i create javaapplications.apps.brewlet.sh -n " + tc.want; canI != want {
				t.Fatalf("can-i = %q, want %q", canI, want)
			}
		})
	}
}
