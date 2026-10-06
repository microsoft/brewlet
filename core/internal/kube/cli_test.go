// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/brewlet/internal/progress"
)

const testImage = "docker.io/library/eclipse-temurin@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const secondImage = "docker.io/library/eclipse-temurin@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func rawJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func objectJSON(t *testing.T, raw string) object {
	t.Helper()
	var obj object
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func listJSON(t *testing.T, items ...object) []byte {
	t.Helper()
	if items == nil {
		items = []object{}
	}
	return rawJSON(t, struct {
		Items []object `json:"items"`
	}{items})
}

func fixtureProfile() string {
	return `{
	  "apiVersion":"node.brewlet.sh/v1alpha1","kind":"NodeProfile",
	  "metadata":{"name":"workers","uid":"profile-uid","resourceVersion":"42","generation":3,
	    "labels":{"team":"platform"},"finalizers":["node.brewlet.sh/cleanup"]},
	  "spec":{"nodePool":{"names":["java"]},"jdks":[{"distribution":"temurin","feature":21,
	    "source":{"image":"` + testImage + `","javaHome":"/opt/java/openjdk"}}],
	    "launchers":[{"name":"jaz","source":{"image":"` + testImage + `","path":"/usr/bin/jaz"}}],
	    "rollout":{"maxUnavailable":1},"appCDS":{"regenerationEnabled":true}},
	  "status":{"observedGeneration":3,"assignedNodes":1,"readyNodes":1,
	    "conditions":[{"type":"Ready","status":"True","observedGeneration":3,"reason":"AllNodesProvisioned"}]}
	}`
}

func addArgs(extra ...string) []string {
	return append([]string{"jdk", "add", "--profile", "workers", "--distribution", "temurin",
		"--feature", "25", "--image", testImage, "--java-home", "/opt/java/openjdk"}, extra...)
}

func dryRunArgs(extra ...string) []string {
	return addArgs(append([]string{"--dry-run"}, extra...)...)
}

func runTest(t *testing.T, args []string, execute executor) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := run(context.Background(), args, &out, &stderr, execute)
	return out.String(), stderr.String(), err
}

func noExecution(t *testing.T) executor {
	t.Helper()
	return func(_ context.Context, program string, args []string, _ []byte) ([]byte, error) {
		t.Fatalf("unexpected execution: %s %v", program, args)
		return nil, nil
	}
}

func writeFixture(t *testing.T, value string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "input.yaml")
	if err := os.WriteFile(file, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

func hasArgs(args []string, want ...string) bool {
	for i := range args {
		if i+len(want) <= len(args) && reflect.DeepEqual(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	for i, arg := range args {
		if arg == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("missing %s in %v", flag, args)
	return ""
}

func TestCommandValidationBeforeExecution(t *testing.T) {
	cases := [][]string{
		{"unknown"}, {"jdk", "remove"}, {"debug", "app", "orders"},
		{"jdk", "render"}, {"launcher", "render"},
		{"status", "extra"}, {"inspect", "app"}, {"profile", "inspect", "--bad"},
		{"profile", "inspect", "../bad"}, {"jdk", "list", "--output", "yaml"},
		{"status", "--output", "bogus"}, {"status", "--timeout", "0"},
		{"jdk", "list", "--timeout", "-1s"}, {"launcher", "list", "--oops"},
		addArgs("--apply"), addArgs("--file", "missing"), addArgs("--values", "missing"),
		dryRunArgs("--values", "missing", "--file", "missing"),
		dryRunArgs("--apply"), addArgs("--output", "table"), dryRunArgs("--output", "table"),
		addArgs("--dry-run=invalid"), addArgs("--dry-run=false"), addArgs("--dry-run="),
		addArgs("--dry-run=server", "--file", "missing"), addArgs("--dry-run=server", "--values", "missing"),
		addArgs("--feature", "0"), addArgs("--feature", "2147483648"),
		addArgs("--distribution", "../jdk"), addArgs("--distribution", "com.example"),
		addArgs("--image", "temurin:21"), addArgs("--image", "docker.io/library/eclipse-temurin:21@sha256:"+strings.Repeat("a", 64)),
		addArgs("--java-home", "/"), addArgs("--java-home", "/opt/../jdk"),
		{"launcher", "add", "--profile", "workers", "--name", "java", "--image", testImage, "--path", "/usr/bin/java"},
		{"install", "--version", "latest", "-f", "missing"},
		{"install", "--version", "1.2.3"},
		{"install", "--version", "1.2.3", "-f", "missing", "--wait-timeout", "-1s"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if out, _, err := runTest(t, args, noExecution(t)); err == nil || out != "" {
				t.Fatalf("expected usage/validation error with empty stdout, got %q, %v", out, err)
			}
		})
	}
	for _, args := range [][]string{nil, {"--help"}, {"jdk", "--help"}, {"jdk", "add", "--help"}, {"launcher", "add", "--help"}} {
		out, _, err := runTest(t, args, noExecution(t))
		if err != nil || !strings.Contains(out, "Brewlet Kubernetes operations") {
			t.Fatalf("help = %q, %v", out, err)
		}
	}
}

func TestInventoryAndConnectionFlags(t *testing.T) {
	node := objectJSON(t, `{"kind":"Node","metadata":{"name":"worker-a","annotations":{
		"brewlet.sh/jdks-info":"[{\"distribution\":\"temurin\",\"vendor\":\"Adoptium\",\"feature\":21,\"version\":\"21.0.5\",\"arch\":\"amd64\"}]",
		"brewlet.sh/launchers":"jaz,jaz, custom"}}}`)
	for _, command := range []string{"jdk", "launcher"} {
		t.Run(command, func(t *testing.T) {
			out, _, err := runTest(t, []string{"--context", "staging", command, "list",
				"--kubeconfig", "/config with spaces", "--selector", "pool=java", "--timeout", "2s", "--output", "json"},
				func(ctx context.Context, program string, args []string, _ []byte) ([]byte, error) {
					if program != "kubectl" || !hasArgs(args, "--context", "staging") ||
						!hasArgs(args, "--kubeconfig", "/config with spaces") || !hasArgs(args, "--selector", "pool=java") ||
						!hasArgs(args, "--request-timeout", "2s") {
						t.Fatalf("connection/selector not forwarded: %s %v", program, args)
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 2*time.Second {
						t.Fatal("missing command deadline")
					}
					return listJSON(t, node), nil
				})
			if err != nil || !json.Valid([]byte(out)) || !strings.Contains(out, "worker-a") {
				t.Fatalf("inventory = %s, %v", out, err)
			}
			if command == "launcher" {
				var rows []launcherInventory
				if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 2 || len(rows[1].Nodes) != 1 {
					t.Fatalf("launcher deduplication: %s %v", out, err)
				}
			}
		})
	}
	for _, format := range []string{"table", "wide"} {
		for _, command := range []struct{ name, value, header string }{
			{"launcher", "jaz", "LAUNCHER"},
			{"jdk", "temurin", "DISTRIBUTION"},
		} {
			out, _, err := runTest(t, []string{command.name, "list", "--output", format},
				func(context.Context, string, []string, []byte) ([]byte, error) { return listJSON(t, node), nil })
			if err != nil || !strings.Contains(out, command.value) || !strings.Contains(out, command.header) {
				t.Fatalf("%s %s = %s %v", command.name, format, out, err)
			}
			if format == "wide" && !strings.Contains(out, "worker-a") {
				t.Fatalf("%s wide inventory missing node: %s", command.name, out)
			}
		}
	}
}

func TestReadErrorsAreNotEmptySuccess(t *testing.T) {
	for _, command := range [][]string{{"profile", "list"}, {"jdk", "list"}, {"launcher", "list"}, {"status"}} {
		for _, response := range []string{`broken`, `{}`, `{"items":null}`} {
			if _, _, err := runTest(t, command, func(context.Context, string, []string, []byte) ([]byte, error) {
				return []byte(response), nil
			}); err == nil {
				t.Fatalf("%v accepted %s", command, response)
			}
		}
		if _, _, err := runTest(t, command, func(context.Context, string, []string, []byte) ([]byte, error) {
			return nil, errors.New("Forbidden: RBAC denied")
		}); err == nil || !strings.Contains(err.Error(), "Forbidden") {
			t.Fatalf("%v hid API failure: %v", command, err)
		}
	}
}

func TestJDKListRequiresStructuredInventory(t *testing.T) {
	for _, format := range []string{"table", "wide", "json"} {
		for _, raw := range []string{"", " \t", "[]", "null", "{broken", `[{"feature":"21"}]`} {
			t.Run(format+"/"+raw, func(t *testing.T) {
				node := objectJSON(t, `{"kind":"Node","metadata":{"name":"compact-node","annotations":{"brewlet.sh/jdks":"temurin-21"}}}`)
				if raw != "" {
					node.Metadata.Annotations["brewlet.sh/jdks-info"] = raw
				}
				out, _, err := runTest(t, []string{"jdk", "list", "--output", format},
					func(context.Context, string, []string, []byte) ([]byte, error) { return listJSON(t, node), nil })
				if raw == "{broken" || raw == `[{"feature":"21"}]` {
					if err == nil || !strings.Contains(err.Error(), `node "compact-node"`) ||
						!strings.Contains(err.Error(), "brewlet.sh/jdks-info") || out != "" {
						t.Fatalf("malformed metadata must fail without output: %q, %v", out, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if format == "json" {
					if strings.TrimSpace(out) != "[]" {
						t.Fatalf("empty JSON inventory = %q", out)
					}
				} else if !strings.Contains(out, "No Brewlet JDK inventory") ||
					!strings.Contains(out, "brewlet.sh/jdks-info") || !strings.Contains(out, "node-provisioner") {
					t.Fatalf("missing structured-inventory guidance: %q", out)
				}
				if strings.Contains(out, "temurin") || strings.Contains(out, "compact-node") {
					t.Fatalf("fabricated detailed inventory: %q", out)
				}
			})
		}
	}
}

func TestProfileInspectionAndStaleReadiness(t *testing.T) {
	profile := objectJSON(t, fixtureProfile())
	for _, mutate := range []func(*object){
		func(o *object) { o.Status.ObservedGeneration-- },
		func(o *object) { o.Status.Conditions[0].ObservedGeneration-- },
		func(o *object) { o.Metadata.DeletionTimestamp = "2026-09-22T00:00:00Z" },
	} {
		obj := objectJSON(t, fixtureProfile())
		mutate(&obj)
		if ready, _ := readyCondition(obj); ready {
			t.Fatal("stale/terminating profile reported ready")
		}
	}
	node := objectJSON(t, `{"kind":"Node","metadata":{"name":"worker-a","uid":"node-uid",
		"labels":{"brewlet.sh/owner-uid":"profile-uid","brewlet.sh/owner-node-uid":"node-uid","brewlet.sh/runtime":"ready"},
		"annotations":{"brewlet.sh/owner-name":"workers","brewlet.sh/profile-generation":"3","brewlet.sh/jdks":"temurin-21"}},
		"spec":{},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	foreign := node
	foreign.Metadata.UID = "recreated-node"
	out, _, err := runTest(t, []string{"profile", "inspect", "workers", "--output", "json"},
		func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
			if hasArgs(args, "get", profilesResource) {
				return rawJSON(t, profile), nil
			}
			if !hasArgs(args, "--selector", "brewlet.sh/owner-uid=profile-uid") {
				t.Fatalf("profile node query not scoped: %v", args)
			}
			return listJSON(t, node, foreign), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Profile profileSummary `json:"profile"`
		Nodes   []nodeSummary  `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil || len(report.Nodes) != 1 || !report.Profile.Ready {
		t.Fatalf("profile report = %s %v", out, err)
	}
	if report.Nodes[0].JDKs != "temurin-21" {
		t.Fatalf("compact inventory missing from profile inspection: %s", out)
	}
}

func healthyDeployment(t *testing.T) object {
	return objectJSON(t, `{"kind":"Deployment","metadata":{"name":"brewlet-operator","generation":2},
		"spec":{"replicas":1},"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}`)
}

func TestStatusDistinguishesOldReplicasAndMissingComponents(t *testing.T) {
	for _, state := range []string{"ready", "old replicas", "operator missing", "admission missing", "node failed", "no nodes", "no profiles"} {
		t.Run(state, func(t *testing.T) {
			dep := healthyDeployment(t)
			if state == "old replicas" {
				dep.Status.UpdatedReplicas = 0
			}
			node := objectJSON(t, `{"kind":"Node","metadata":{"name":"worker","labels":{"brewlet.sh/runtime":"ready"},
				"annotations":{"brewlet.sh/jdks":"temurin-21"}},
				"spec":{},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
			if state == "node failed" {
				node.Metadata.Annotations = map[string]string{"brewlet.sh/provision-error": "jdk-copy-failed"}
			}
			out, _, err := runTest(t, []string{"status", "--output", "json", "--namespace", "runtime-system"},
				func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
					switch {
					case hasArgs(args, "get", "deployments"):
						if !hasArgs(args, "--namespace", "runtime-system") {
							t.Fatal("wrong component namespace")
						}
						if state == "operator missing" {
							return listJSON(t), nil
						}
						if state == "admission missing" {
							return listJSON(t, dep), nil
						}
						admission := healthyDeployment(t)
						admission.Metadata.Name = "brewlet-admission"
						return listJSON(t, dep, admission), nil
					case hasArgs(args, "get", profilesResource):
						if state == "no profiles" {
							return listJSON(t), nil
						}
						return listJSON(t, objectJSON(t, fixtureProfile())), nil
					default:
						if state == "no nodes" {
							return listJSON(t), nil
						}
						return listJSON(t, node), nil
					}
				})
			var report statusReport
			if decodeErr := json.Unmarshal([]byte(out), &report); decodeErr != nil {
				t.Fatal(decodeErr, out)
			}
			if report.Healthy != (state == "ready") || (err == nil) != report.Healthy {
				t.Fatalf("status %s: %s, %v", state, out, err)
			}
			if state == "ready" && (len(report.Nodes) != 1 || report.Nodes[0].JDKs != "temurin-21") {
				t.Fatalf("compact inventory missing from status: %s", out)
			}
			if state == "admission missing" && report.Components[1].Present {
				t.Fatal("absent admission should be explicit")
			}
			if report.Namespace != "runtime-system" {
				t.Fatalf("report namespace = %q", report.Namespace)
			}
		})
	}
}

func TestAppInspectionFollowsOwnershipAndOmitsEnvironment(t *testing.T) {
	app := objectJSON(t, `{"apiVersion":"apps.brewlet.sh/v1alpha1","kind":"JavaApplication",
		"metadata":{"name":"orders","namespace":"team","uid":"app-uid","generation":3},
		"spec":{"artifact":{"image":"registry.example/orders@sha256:abc"},"jvm":{"version":21},"env":[{"name":"TOKEN","value":"secret-value"}]},
		"status":{"observedGeneration":3,"conditions":[{"type":"Ready","status":"True","observedGeneration":3,"reason":"Reconciled"}]}}`)
	dep := healthyDeployment(t)
	dep.Metadata.Name, dep.Metadata.UID = "orders", "dep-uid"
	dep.Metadata.OwnerReferences = []ownerReference{{UID: "app-uid", Controller: true}}
	dep.Status.UpdatedReplicas = 0
	rs := objectJSON(t, `{"kind":"ReplicaSet","metadata":{"name":"orders-rs","uid":"rs-uid",
		"ownerReferences":[{"uid":"dep-uid","controller":true}]}}`)
	pod := objectJSON(t, `{"kind":"Pod","metadata":{"name":"orders-pod","uid":"pod-uid",
		"ownerReferences":[{"uid":"rs-uid","controller":true}],"annotations":{"brewlet.sh/jdk":"21"}},
		"spec":{"nodeName":"worker","containers":[{"env":[{"name":"TOKEN","value":"secret-value"}]}]},
		"status":{"phase":"Pending","containerStatuses":[{"name":"app","state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`)
	foreign := objectJSON(t, `{"kind":"Pod","metadata":{"name":"foreign","uid":"foreign-uid","ownerReferences":[{"uid":"other-app","controller":true}]},"spec":{}}`)
	event := objectJSON(t, `{"involvedObject":{"kind":"Pod","name":"orders-pod","uid":"pod-uid"},"reason":"Failed","message":"Cannot pull image","type":"Warning"}`)
	foreignEvent := objectJSON(t, `{"involvedObject":{"kind":"Pod","name":"foreign","uid":"foreign-uid"},"message":"unrelated event"}`)
	out, _, err := runTest(t, []string{"inspect", "app", "orders", "--namespace", "team", "--output", "json"},
		func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
			if !hasArgs(args, "--namespace", "team") {
				t.Fatalf("missing namespace: %v", args)
			}
			switch {
			case hasArgs(args, "get", appsResource):
				return rawJSON(t, app), nil
			case hasArgs(args, "get", "events"):
				return listJSON(t, event, foreignEvent), nil
			default:
				return listJSON(t, pod, foreign, rs, dep), nil
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	var report appReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Ready || report.JDKRequest != "21" || len(report.Pods) != 1 || len(report.Events) != 1 ||
		len(report.Pods[0].Problems) != 1 || strings.Contains(out, "secret-value") || strings.Contains(out, "foreign") {
		t.Fatalf("unsafe or incorrect app inspection: %s", out)
	}
}

func TestOfflineProfileAndValuesUpdates(t *testing.T) {
	profile := writeFixture(t, fixtureProfile())
	out, _, err := runTest(t, dryRunArgs("--file", profile, "--output", "json"), noExecution(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "resourceVersion") || strings.Contains(out, "finalizers") ||
		strings.Contains(out, `"status"`) || !strings.Contains(out, `"maxUnavailable"`) ||
		!strings.Contains(out, `"regenerationEnabled"`) || !strings.Contains(out, `"jaz"`) {
		t.Fatalf("manifest did not preserve configuration/strip live metadata: %s", out)
	}
	doc, err := decodeDocument([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := decodeDocument(doc["spec"])
	if err != nil {
		t.Fatal(err)
	}
	var jdks []jdkSource
	if err := json.Unmarshal(spec["jdks"], &jdks); err != nil || len(jdks) != 2 {
		t.Fatalf("inventory: %s %v", spec["jdks"], err)
	}
	original, err := os.ReadFile(profile)
	if err != nil || string(original) != fixtureProfile() {
		t.Fatal("source file was overwritten")
	}
	values := writeFixture(t, `namespace: brewlet
provisioner:
  pools: [java]
  jdks: []
  rollout: {maxUnavailable: 1}
profiles:
  - name: workers
    pools: [batch]
    jdks: []
  - name: unrelated
    pools: [other]
    jdks: []
metrics: {enabled: true}
`)
	for _, name := range []string{"default", "workers"} {
		out, _, err := runTest(t, dryRunArgs("--values", values, "--profile", name), noExecution(t))
		if err != nil || !strings.Contains(out, "metrics:") || !strings.Contains(out, "name: unrelated") || !strings.Contains(out, "feature: 25") {
			t.Fatalf("values update: %s, %v", out, err)
		}
	}
	for _, invalid := range []string{"a: 1\na: 2", "spec: {}\n---\nspec: {}", "null", "provisioner: ["} {
		if out, _, err := runTest(t, dryRunArgs("--file", writeFixture(t, invalid)), noExecution(t)); err == nil || out != "" {
			t.Fatalf("invalid source %s must fail without output, got %q, %v", invalid, out, err)
		}
	}
}

func TestInventoryReplacementAndLimits(t *testing.T) {
	file := writeFixture(t, fixtureProfile())
	out, _, err := runTest(t, dryRunArgs("--file", file, "--feature", "21", "--output", "json"), noExecution(t))
	if err != nil || strings.Count(out, `"distribution"`) != 1 {
		t.Fatalf("identical add is not idempotent: %s %v", out, err)
	}
	args := dryRunArgs("--file", file, "--feature", "21", "--image", secondImage)
	if _, _, err := runTest(t, args, noExecution(t)); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("implicit replacement allowed: %v", err)
	}
	out, _, err = runTest(t, append(args, "--replace"), noExecution(t))
	if err != nil || !strings.Contains(out, secondImage) {
		t.Fatalf("explicit replacement failed: %s %v", out, err)
	}
	var entries []jdkSource
	for n := 1; n <= 32; n++ {
		jdk := jdkSource{Distribution: "temurin", Feature: int64(n)}
		jdk.Source.Image, jdk.Source.JavaHome = testImage, "/opt/java"
		entries = append(entries, jdk)
	}
	spec := document{"jdks": rawJSON(t, entries)}
	u := updateOptions{jdk: true, distribution: "temurin", feature: 99, image: testImage, path: "/opt/java"}
	field, entry, err := u.entry()
	if err != nil {
		t.Fatal(err)
	}
	if err := updateInventory(spec, field, entry, false); err == nil {
		t.Fatal("allowed inventory larger than CRD maxItems")
	}
	spec["jdks"] = rawJSON(t, []jdkSource{entries[0], entries[0]})
	if err := updateInventory(spec, field, entry, false); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatal("duplicate inventory was not rejected", err)
	}
}

func TestManagedProfilesCannotBePatched(t *testing.T) {
	cases := []string{
		`"labels":{"app.kubernetes.io/managed-by":"Helm"}`,
		`"annotations":{"meta.helm.sh/release-name":"brewlet"}`,
		`"annotations":{"argocd.argoproj.io/tracking-id":"apps:profile/workers"}`,
		`"labels":{"kustomize.toolkit.fluxcd.io/name":"runtime"}`,
		`"ownerReferences":[{"uid":"controller","controller":true}]`,
		`"managedFields":[{"manager":"custom-gitops","fieldsV1":{"f:spec":{}}}]`,
	}
	for _, ownership := range cases {
		t.Run(ownership, func(t *testing.T) {
			raw := strings.Replace(fixtureProfile(), `"labels":{"team":"platform"}`, ownership, 1)
			executor := func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
				if !hasArgs(args, "get", profilesResource) || !hasArgs(args, "--show-managed-fields=true") {
					t.Fatalf("attempted mutation or missed ownership fields: %v", args)
				}
				return []byte(raw), nil
			}
			for _, args := range [][]string{addArgs(), addArgs("--dry-run=server")} {
				if out, _, err := runTest(t, args, executor); err == nil || !strings.Contains(err.Error(), "managed by") || out != "" {
					t.Fatalf("managed profile must fail without output, got %q, %v", out, err)
				}
			}
			out, stderr, err := runTest(t, dryRunArgs(), executor)
			if err != nil || !strings.Contains(stderr, "source of truth") || out == "" {
				t.Fatalf("managed declaration must remain available: %s %s %v", out, stderr, err)
			}
		})
	}
}

func TestAddMutatesByDefaultAndDryRunUsesConditionalPatch(t *testing.T) {
	for _, mode := range []string{"apply", "dry-run", "conflict"} {
		t.Run(mode, func(t *testing.T) {
			var patchPath string
			calls := 0
			args := addArgs("--output", "json")
			if mode == "dry-run" {
				args = append(args, "--dry-run=server")
			}
			out, stderr, err := runTest(t, args, func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
				calls++
				if hasArgs(args, "get", profilesResource) {
					return []byte(fixtureProfile()), nil
				}
				if !hasArgs(args, "patch", profilesResource, "workers") || !hasArgs(args, "--type=json") {
					t.Fatalf("unexpected update: %v", args)
				}
				if hasArgs(args, "--dry-run=server") != (mode == "dry-run") {
					t.Fatalf("wrong dry-run mode: %v", args)
				}
				patchPath = flagValue(t, args, "--patch-file")
				info, err := os.Stat(patchPath)
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("patch file is not private: %v", err)
				}
				raw, err := os.ReadFile(patchPath)
				if err != nil {
					t.Fatal(err)
				}
				var patch []struct {
					Op, Path string
					Value    json.RawMessage
				}
				if err := json.Unmarshal(raw, &patch); err != nil || len(patch) != 3 {
					t.Fatalf("patch: %s %v", raw, err)
				}
				if patch[0].Op != "test" || patch[0].Path != "/metadata/uid" || string(patch[0].Value) != `"profile-uid"` ||
					patch[1].Op != "test" || patch[1].Path != "/metadata/resourceVersion" || string(patch[1].Value) != `"42"` ||
					patch[2].Op != "add" || patch[2].Path != "/spec/jdks" {
					t.Fatalf("patch is not identity/version fenced and narrow: %s", raw)
				}
				if mode == "conflict" {
					return nil, errors.New("Conflict: resourceVersion changed")
				}
				doc, err := decodeDocument([]byte(fixtureProfile()))
				if err != nil {
					t.Fatal(err)
				}
				spec, err := decodeDocument(doc["spec"])
				if err != nil {
					t.Fatal(err)
				}
				spec["jdks"] = patch[2].Value
				doc["spec"] = rawJSON(t, spec)
				return rawJSON(t, doc), nil
			})
			if calls != 2 {
				t.Fatalf("unexpected retries: %d", calls)
			}
			if mode == "conflict" {
				if err == nil || !strings.Contains(err.Error(), "Conflict") || out != "" || stderr != "" {
					t.Fatalf("conflict concealed: %s %s %v", out, stderr, err)
				}
			} else {
				if err != nil || !json.Valid([]byte(out)) || !strings.Contains(out, `"feature": 25`) {
					t.Fatalf("add result: %s %s %v", out, stderr, err)
				}
				want := "Profile update accepted; provisioning is asynchronous."
				if mode == "dry-run" {
					want = "Server dry run succeeded; no inventory was changed."
				}
				if !strings.Contains(stderr, want) {
					t.Fatalf("missing mutation/preview distinction: %s", stderr)
				}
			}
			if _, err := os.Stat(patchPath); !os.IsNotExist(err) {
				t.Fatal("temporary patch file leaked", err)
			}
		})
	}
}

func TestFailedServerDryRunDoesNotRender(t *testing.T) {
	for _, kind := range []string{"jdk", "launcher"} {
		for _, format := range []string{"yaml", "json"} {
			for _, failure := range []string{"read", "validation", "conflict", "decode", "metadata"} {
				t.Run(kind+"/"+format+"/"+failure, func(t *testing.T) {
					args := addArgs("--dry-run=server", "--output", format)
					if kind == "launcher" {
						args = []string{"launcher", "add", "--profile", "workers", "--name", "custom",
							"--image", testImage, "--path", "/usr/bin/custom", "--dry-run=server", "--output", format}
					}
					calls := 0
					var patchFile string
					out, stderr, err := runTest(t, args, func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
						calls++
						if hasArgs(args, "get", profilesResource) {
							if failure == "read" {
								return []byte(fixtureProfile()), errors.New("Forbidden: cannot read profile")
							}
							return []byte(fixtureProfile()), nil
						}
						if !hasArgs(args, "patch", profilesResource, "workers") || !hasArgs(args, "--dry-run=server") {
							t.Fatalf("dry run attempted a non-preview operation: %v", args)
						}
						patchFile = flagValue(t, args, "--patch-file")
						switch failure {
						case "validation":
							return []byte(fixtureProfile()), errors.New("admission rejected the source")
						case "conflict":
							return []byte(fixtureProfile()), errors.New("Conflict: resourceVersion changed")
						case "decode":
							return []byte("invalid JSON"), nil
						case "metadata":
							return []byte(`{"metadata":false}`), nil
						default:
							t.Fatal("unexpected patch after failed read")
							return nil, nil
						}
					})
					if err == nil || out != "" || strings.Contains(stderr, "succeeded") || strings.Contains(stderr, "accepted") {
						t.Fatalf("failed dry run rendered output or claimed success: stdout=%q stderr=%q err=%v", out, stderr, err)
					}
					wantCalls := 2
					if failure == "read" {
						wantCalls = 1
					}
					if calls != wantCalls {
						t.Fatalf("unexpected fallback/retry after failure: got %d calls, want %d", calls, wantCalls)
					}
					if patchFile != "" {
						if _, err := os.Stat(patchFile); !os.IsNotExist(err) {
							t.Fatal("failed dry run left a temporary patch file", err)
						}
					}
				})
			}
		}
	}
}

func TestClientDryRunModesAndFailures(t *testing.T) {
	file := writeFixture(t, fixtureProfile())
	for _, flag := range []string{"--dry-run", "--dry-run=client"} {
		for _, format := range []string{"yaml", "json"} {
			t.Run(flag+"/"+format, func(t *testing.T) {
				out, _, err := runTest(t, addArgs(flag, "--file", file, "--output", format), noExecution(t))
				if err != nil || out == "" || !strings.Contains(out, "25") {
					t.Fatalf("client preview failed: %s %v", out, err)
				}
				for _, invalid := range [][]string{
					{"--image", "temurin:25"},
					{"--feature", "21", "--image", secondImage},
					{"--file", filepath.Join(t.TempDir(), "missing.yaml")},
				} {
					args := append(addArgs(flag, "--file", file, "--output", format), invalid...)
					out, stderr, err := runTest(t, args, noExecution(t))
					if err == nil || out != "" || strings.Contains(stderr, "succeeded") {
						t.Fatalf("failed client dry run rendered output: %q %q %v", out, stderr, err)
					}
				}
				out, _, err = runTest(t, addArgs(flag, "--output", format),
					func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
						if !hasArgs(args, "get", profilesResource) {
							t.Fatalf("client dry run attempted a write: %v", args)
						}
						return []byte(fixtureProfile()), nil
					})
				if err != nil || out == "" {
					t.Fatalf("live-profile client preview failed: %q %v", out, err)
				}
			})
		}
	}
}

func TestFailedValuesDryRunDoesNotRender(t *testing.T) {
	values := writeFixture(t, `provisioner:
  jdks: []
profiles:
  - name: workers
    jdks: []
  - name: workers
    jdks: []
`)
	out, stderr, err := runTest(t, dryRunArgs("--values", values), noExecution(t))
	if err == nil || !strings.Contains(err.Error(), "duplicate profile") || out != "" || stderr != "" {
		t.Fatalf("partially processed values must not be rendered: %q %q %v", out, stderr, err)
	}
}

func TestKubectlLocalPatchContract(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not installed")
	}
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint(stale), func(t *testing.T) {
			snapshot := fixtureProfile()
			if stale {
				snapshot = strings.Replace(snapshot, `"resourceVersion":"42"`, `"resourceVersion":"43"`, 1)
			}
			file := writeFixture(t, snapshot)
			out, _, err := runTest(t, addArgs("--output", "json"),
				func(ctx context.Context, _ string, args []string, _ []byte) ([]byte, error) {
					if hasArgs(args, "get", profilesResource) {
						return []byte(fixtureProfile()), nil
					}
					return execute(ctx, "kubectl", []string{"patch", "--local", "-f", file, "--type=json",
						"--patch-file", flagValue(t, args, "--patch-file"), "-o", "json"}, nil)
				})
			if stale {
				if err == nil {
					t.Fatal("kubectl accepted a stale resourceVersion")
				}
			} else if err != nil || !strings.Contains(out, `"feature": 25`) || !strings.Contains(out, `"maxUnavailable": 1`) {
				t.Fatalf("kubectl rejected or lost fields: %s %v", out, err)
			}
		})
	}
}

func TestInstallDelegatesToPinnedHelmChart(t *testing.T) {
	file := writeFixture(t, "provisioner:\n  pools: [workers]\n  jdks: []\n")
	for _, dryRun := range []bool{false, true} {
		args := []string{"--context", "staging", "install", "--kubeconfig", "/kubeconfig", "--version", "0.1.0-rc.1",
			"-f", file, "--values", file, "--namespace", "runtime-system", "--wait-timeout", "2m"}
		if dryRun {
			args = append(args, "--dry-run")
		}
		out, _, err := runTest(t, args,
			func(ctx context.Context, program string, args []string, _ []byte) ([]byte, error) {
				if program == "kubectl" {
					if dryRun || !hasArgs(args, "get", "customresourcedefinitions") ||
						!hasArgs(args, profilesResource, appsResource, "--ignore-not-found") {
						t.Fatalf("unexpected install preflight: %v", args)
					}
					// kubectl writes no JSON when all explicitly named resources are absent.
					return nil, nil
				}
				if program != "helm" || !hasArgs(args, "--version", "0.1.0-rc.1") ||
					!hasArgs(args, "--namespace", "runtime-system") || !hasArgs(args, "--set-string", "namespace=runtime-system") ||
					!hasArgs(args, "--kube-context", "staging") || !hasArgs(args, "--kubeconfig", "/kubeconfig") {
					t.Fatalf("unsafe install command: %s %v", program, args)
				}
				if dryRun {
					if !hasArgs(args, "template", "brewlet", chart) || hasArgs(args, "--wait") {
						t.Fatalf("dry-run should only template: %v", args)
					}
				} else if !hasArgs(args, "install", "brewlet", chart) || !hasArgs(args, "--wait") {
					t.Fatalf("install did not use Helm: %v", args)
				}
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) < 2*time.Minute {
					t.Fatal("Helm deadline does not account for rollout time")
				}
				return []byte("helm output\n"), nil
			})
		if err != nil || out != "helm output\n" {
			t.Fatalf("install: %s %v", out, err)
		}
	}
	out, stderr, err := runTest(t, []string{"install", "--version", "1.2.3", "-f", file, "--dry-run"},
		func(context.Context, string, []string, []byte) ([]byte, error) {
			return []byte("partially rendered Helm manifests"), errors.New("Helm release failed")
		})
	if err == nil || !strings.Contains(err.Error(), "Helm release failed") || out != "" || stderr != "" {
		t.Fatalf("Helm dry-run failure must not render partial output: %q %q %v", out, stderr, err)
	}
	_, _, err = runTest(t, []string{"install", "--version", "1.2.3", "-f", file},
		func(_ context.Context, program string, _ []string, _ []byte) ([]byte, error) {
			if program != "kubectl" {
				t.Fatal("attempted to install over existing CRDs")
			}
			return listJSON(t, objectJSON(t, `{"kind":"CustomResourceDefinition","metadata":{"name":"nodeprofiles.node.brewlet.sh"}}`)), nil
		})
	if err == nil || !strings.Contains(err.Error(), "CRDs already exist") {
		t.Fatal("existing CRDs were not protected", err)
	}
	if !strings.Contains(err.Error(), "fresh-install-only") ||
		!strings.Contains(err.Error(), "safe teardown and retained-resource review") ||
		!strings.Contains(err.Error(), "docs/installation.md#upgrading") {
		t.Fatalf("missing fresh-install teardown guidance: %v", err)
	}
}

func TestInstallReportsStepsAndRolloutProgress(t *testing.T) {
	saved := progress.PollInterval
	progress.PollInterval = 5 * time.Millisecond
	t.Cleanup(func() { progress.PollInterval = saved })
	file := writeFixture(t, "provisioner:\n  pools: [workers]\n  jdks: []\n")
	rollout := listJSON(t,
		objectJSON(t, `{"kind":"Deployment","metadata":{"name":"brewlet-operator","generation":1},
		  "spec":{"replicas":1},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}`),
		objectJSON(t, `{"kind":"Deployment","metadata":{"name":"brewlet-admission","generation":1},
		  "spec":{"replicas":2},"status":{"observedGeneration":1,"replicas":2,"updatedReplicas":2}}`),
		objectJSON(t, `{"kind":"Pod","metadata":{"name":"brewlet-admission-abc"},
		  "status":{"containerStatuses":[{"name":"webhook","state":{"waiting":{"reason":"ImagePullBackOff"}}}]}}`))
	for _, helmErr := range []error{nil, errors.New("helm: context deadline exceeded")} {
		polled := make(chan struct{}, 1)
		_, stderr, err := runTest(t, []string{"install", "--version", "1.2.3", "-f", file, "--release", "rel", "--namespace", "sys"},
			func(ctx context.Context, program string, args []string, _ []byte) ([]byte, error) {
				if program == "kubectl" && hasArgs(args, "get", "customresourcedefinitions") {
					return nil, nil
				}
				if program == "kubectl" {
					if !hasArgs(args, "get", "deployments,pods") || !hasArgs(args, "--namespace", "sys") ||
						!hasArgs(args, "--selector", "app.kubernetes.io/name=brewlet,app.kubernetes.io/instance=rel") {
						t.Errorf("unexpected rollout poll: %v", args)
					}
					select {
					case polled <- struct{}{}:
					default:
					}
					return rollout, nil
				}
				// Keep Helm "waiting" until the rollout has been polled and reported.
				<-polled
				time.Sleep(50 * time.Millisecond)
				return []byte("helm output\n"), helmErr
			})
		for _, want := range []string{
			`Installing Brewlet chart 1.2.3 as release "rel" in namespace "sys" (current context)`,
			"[1/3] Validated 1 values file(s)",
			"[2/3] Checking the cluster for existing Brewlet CRDs...",
			"[3/3] Pulling " + chart + " and waiting for rollout (timeout 5m0s)...",
			"1/2 deployments ready (brewlet-operator 1/1 available, brewlet-admission 0/2 available); brewlet-admission-abc: ImagePullBackOff",
		} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr missing %q:\n%s", want, stderr)
			}
		}
		if strings.Contains(stderr, "\r") {
			t.Errorf("non-terminal progress must not redraw lines: %q", stderr)
		}
		if helmErr == nil && (err != nil || !strings.Contains(stderr, `Helm release "rel" deployed and rolled out in`)) {
			t.Fatalf("install: %s %v", stderr, err)
		}
		if helmErr != nil && (err == nil || !strings.Contains(stderr, "inspect with: brewlet k8s status --namespace sys")) {
			t.Fatalf("failed install should point at status: %s %v", stderr, err)
		}
	}
}

func TestExecuteErrorsAndCancellation(t *testing.T) {
	if _, err := execute(context.Background(), "brewlet-test-nonexistent-program", nil, nil); err == nil {
		t.Fatal("missing binary should fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := execute(ctx, "kubectl", []string{"version", "--client"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not propagated: %v", err)
	}
}

func TestDoctorDelegatesChecksAndPropagatesFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		for _, format := range []string{"table", "json", "yaml"} {
			out, _, err := runTest(t, []string{"--kubeconfig", "/config with spaces", "doctor", "--context", "staging", "--namespace", "team", "--output", format},
				func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
					if !hasArgs(args, "--context", "staging") || !hasArgs(args, "--kubeconfig", "/config with spaces") {
						t.Fatalf("connection flags missing: %v", args)
					}
					switch {
					case hasArgs(args, "config", "current-context"):
						return []byte("staging"), nil
					case hasArgs(args, "get", "nodes"):
						return []byte(`{"items":[{"metadata":{"name":"node","labels":{"brewlet.sh/runtime":"ready"},
								"annotations":{"brewlet.sh/jdks":"temurin-21",
								"brewlet.sh/jdks-info":"[{\"distribution\":\"temurin\",\"vendor\":\"Adoptium\",\"feature\":21,\"version\":\"21.0.5\",\"arch\":\"amd64\"}]"}},"spec":{},
								"status":{"nodeInfo":{"containerRuntimeVersion":"containerd://2.0.0"}}}]}`), nil
					case hasArgs(args, "auth", "can-i"):
						if !hasArgs(args, "-n", "team") {
							t.Fatal("doctor used the wrong namespace")
						}
						if fail {
							return []byte("no"), nil
						}
						return []byte("yes"), nil
					default:
						return []byte("ok"), nil
					}
				})
			if (err != nil) != fail || !strings.Contains(out, "developer-rbac") {
				t.Fatalf("doctor fail=%t format=%s: %s %v", fail, format, out, err)
			}
		}
	}
}

func TestProfileTableIncludesDesiredInventory(t *testing.T) {
	out, _, err := runTest(t, []string{"profile", "list"},
		func(context.Context, string, []string, []byte) ([]byte, error) {
			return listJSON(t, objectJSON(t, fixtureProfile())), nil
		})
	for _, text := range []string{"DESIRED JDKS", "DESIRED LAUNCHERS", "temurin-21", "jaz", "AllNodesProvisioned"} {
		if !strings.Contains(out, text) {
			t.Fatalf("profile table lacks %q: %s", text, out)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestLauncherAdditionAndTerminatingProfile(t *testing.T) {
	profile := fixtureProfile()
	args := []string{"launcher", "add", "--profile", "workers", "--name", "custom",
		"--image", testImage, "--path", "/usr/bin/custom", "--output", "json"}
	for _, mode := range []string{"add", "server", "client", "bare"} {
		t.Run(mode, func(t *testing.T) {
			command := append([]string(nil), args...)
			if mode == "bare" {
				command = append(command, "--dry-run")
			} else if mode != "add" {
				command = append(command, "--dry-run="+mode)
			}
			patches := 0
			out, _, err := runTest(t, command, func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
				if hasArgs(args, "get", profilesResource) {
					return []byte(profile), nil
				}
				if mode == "client" || mode == "bare" || !hasArgs(args, "patch", profilesResource, "workers") {
					t.Fatalf("unexpected command: %v", args)
				}
				patches++
				if hasArgs(args, "--dry-run=server") != (mode == "server") {
					t.Fatalf("incorrect launcher mutation/preview mode: %v", args)
				}
				raw, err := os.ReadFile(flagValue(t, args, "--patch-file"))
				if err != nil {
					t.Fatal(err)
				}
				var patch []struct {
					Op, Path string
					Value    json.RawMessage
				}
				if err := json.Unmarshal(raw, &patch); err != nil || len(patch) != 3 ||
					patch[0].Path != "/metadata/uid" || patch[1].Path != "/metadata/resourceVersion" ||
					patch[2].Path != "/spec/launchers" || patch[2].Op != "add" {
					t.Fatalf("launcher patch is not conditional and narrow: %s %v", raw, err)
				}
				doc, err := decodeDocument([]byte(profile))
				if err != nil {
					t.Fatal(err)
				}
				spec, err := decodeDocument(doc["spec"])
				if err != nil {
					t.Fatal(err)
				}
				spec["launchers"] = patch[2].Value
				doc["spec"] = rawJSON(t, spec)
				return rawJSON(t, doc), nil
			})
			if err != nil || !strings.Contains(out, `"name": "custom"`) || !strings.Contains(out, `"name": "jaz"`) ||
				!strings.Contains(out, `"feature": 21`) {
				t.Fatalf("launcher %s: %s %v", mode, out, err)
			}
			if (mode == "add" || mode == "server") && patches != 1 {
				t.Fatalf("launcher add did not issue exactly one patch: %d", patches)
			}
		})
	}
	for _, field := range []string{`"deletionTimestamp":"2026-09-22T00:00:00Z"`, `"namespace":"forbidden"`} {
		invalid := strings.Replace(profile, `"generation":3`, `"generation":3,`+field, 1)
		if _, _, err := runTest(t, args, func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
			if hasArgs(args, "patch") {
				t.Fatal("mutated an invalid/terminating profile")
			}
			return []byte(invalid), nil
		}); err == nil {
			t.Fatal("invalid/terminating profile accepted")
		}
	}
}

func TestHelmPreviewAndValuesContract(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("Helm not installed")
	}
	localChart, err := filepath.Abs(filepath.Join("..", "..", "..", "kubernetes", "charts", "brewlet"))
	if err != nil {
		t.Fatal(err)
	}
	values := writeFixture(t, `provisioner:
  pools: [java-workers]
  jdks:
    - distribution: temurin
      feature: 21
      source:
        image: `+testImage+`
        javaHome: /opt/java/openjdk
  rollout: {maxUnavailable: 1}
profiles:
  - name: batch
    pools: [batch-workers]
    jdks:
      - distribution: temurin
        feature: 21
        source:
          image: `+testImage+`
          javaHome: /opt/java/openjdk
`)
	updated, _, err := runTest(t, dryRunArgs("--values", values, "--profile", "default"), noExecution(t))
	if err != nil {
		t.Fatal(err)
	}
	nextValues := writeFixture(t, updated)
	out, _, err := runTest(t, []string{"install", "--version", "1.2.3", "--values", nextValues, "--namespace", "runtime-system", "--dry-run"},
		func(ctx context.Context, program string, args []string, input []byte) ([]byte, error) {
			if program != "helm" || args[0] != "template" {
				t.Fatalf("preview attempted cluster access: %s %v", program, args)
			}
			for i, arg := range args {
				if arg == chart {
					args[i] = localChart
				}
			}
			return execute(ctx, program, args, input)
		})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"kind: NodeProfile", "kind: CustomResourceDefinition", "runtime-system",
		"java-workers", "batch-workers", "feature: 21", "feature: 25", testImage, "maxUnavailable: 1",
	} {
		if !strings.Contains(out, text) {
			t.Fatalf("Helm render missing %q", text)
		}
	}
}

func TestStatusResolvesControlPlaneNamespace(t *testing.T) {
	controlPlane := func(name, namespace string) object {
		dep := healthyDeployment(t)
		dep.Metadata.Name = name
		dep.Metadata.Namespace = namespace
		return dep
	}
	terminatingControlPlane := func(name, namespace string) object {
		dep := controlPlane(name, namespace)
		dep.Metadata.DeletionTimestamp = "2026-10-01T00:00:00Z"
		return dep
	}
	node := objectJSON(t, `{"kind":"Node","metadata":{"name":"worker","labels":{"brewlet.sh/runtime":"ready"}},
		"spec":{},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	for _, tc := range []struct {
		name       string
		args       []string
		discovered []object
		discover   error
		want       string
		wantErr    string
	}{
		{name: "explicit", args: []string{"--namespace", "devoxx"}, want: "devoxx"},
		{name: "explicit before command", args: []string{"--namespace", "devoxx"}, want: "devoxx"},
		{name: "discovered", want: "devoxx", discovered: []object{
			controlPlane("brewlet-operator", "devoxx"), controlPlane("brewlet-admission", "devoxx"),
			controlPlane("brewlet", "apps")}},
		{name: "terminating ignored", want: "new", discovered: []object{
			terminatingControlPlane("brewlet-operator", "old"), controlPlane("brewlet-operator", "new")}},
		{name: "only terminating", want: "old", discovered: []object{terminatingControlPlane("brewlet-operator", "old")}},
		{name: "none discovered", want: "brewlet", wantErr: `no Brewlet control plane found in namespace "brewlet"; pass --namespace`},
		{name: "forbidden", want: "brewlet", discover: errors.New(`kubectl: exit status 1: Error from server (Forbidden): deployments.apps is forbidden`)},
		{name: "multiple", wantErr: "multiple Brewlet control planes found in namespaces a, b; pass --namespace", discovered: []object{
			controlPlane("brewlet-operator", "b"), controlPlane("brewlet-operator", "a")}},
		{name: "discovery failure", wantErr: "discover Brewlet control plane: boom", discover: errors.New("boom")},
		{name: "invalid", args: []string{"--namespace", "Bad_NS"}, wantErr: "--namespace:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"status", "--output", "json"}, tc.args...)
			if tc.name == "explicit before command" {
				args = append(append([]string{}, tc.args...), "status", "--output", "json")
			}
			out, _, err := runTest(t, args, func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
				switch {
				case hasArgs(args, "get", "deployments") && hasArgs(args, "--all-namespaces"):
					if len(tc.args) > 0 {
						t.Fatal("explicit namespace must skip discovery")
					}
					if !hasArgs(args, "--selector", "app.kubernetes.io/name=brewlet") {
						t.Fatalf("discovery selector missing: %v", args)
					}
					if tc.discover != nil {
						return nil, tc.discover
					}
					return listJSON(t, tc.discovered...), nil
				case hasArgs(args, "get", "deployments"):
					if !hasArgs(args, "--namespace", tc.want) {
						t.Fatalf("wrong control-plane namespace: %v", args)
					}
					if tc.name == "none discovered" {
						return listJSON(t), nil
					}
					return listJSON(t, controlPlane("brewlet-operator", tc.want), controlPlane("brewlet-admission", tc.want)), nil
				case hasArgs(args, "get", profilesResource):
					return listJSON(t, objectJSON(t, fixtureProfile())), nil
				default:
					return listJSON(t, node), nil
				}
			})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("status: %v: %s", err, out)
			}
			var report statusReport
			if err := json.Unmarshal([]byte(out), &report); err != nil || report.Namespace != tc.want || !report.Healthy {
				t.Fatalf("report: %v %s", err, out)
			}
		})
	}
}

func TestClusterScopedCommandsRejectNamespace(t *testing.T) {
	fail := func(_ context.Context, _ string, args []string, _ []byte) ([]byte, error) {
		t.Fatalf("unexpected kubectl call: %v", args)
		return nil, nil
	}
	for _, args := range [][]string{
		{"jdk", "list", "--namespace", "x"},
		{"--namespace", "x", "launcher", "list"},
		{"profile", "list", "--namespace", "x"},
		{"profile", "inspect", "workers", "--namespace", "x"},
		addArgs("--namespace", "x"),
		{"launcher", "add", "--profile", "workers", "--name", "jaz", "--image", testImage, "--path", "/usr/bin/jaz", "--namespace", "x"},
	} {
		_, _, err := runTest(t, args, fail)
		if err == nil || !strings.Contains(err.Error(), "--namespace is not supported") || !strings.Contains(err.Error(), "cluster-scoped") {
			t.Fatalf("%v: err = %v", args, err)
		}
	}
	if _, _, err := runTest(t, []string{"status", "--system-namespace", "x"}, fail); err == nil {
		t.Fatal("--system-namespace must be rejected")
	}
}

type deleteCluster struct {
	t         *testing.T
	profile   string
	snapshots []string
	pods      []object
	podsErr   error
	cleanup   []object
	deletes   []map[string]any
	polls     int
}

func deletingProfile(reason, message string, extra string) string {
	return `{"apiVersion":"node.brewlet.sh/v1alpha1","kind":"NodeProfile",
	  "metadata":{"name":"workers","uid":"profile-uid","resourceVersion":"43","deletionTimestamp":"2026-01-01T00:00:00Z"},
	  "status":{"targets":[{"name":"node-a","uid":"a"},{"name":"node-b","uid":"b"}]` + extra + `,
	    "conditions":[{"type":"Ready","status":"False","reason":"` + reason + `","message":"` + message + `"}]}}`
}

func (f *deleteCluster) exec(_ context.Context, program string, args []string, input []byte) ([]byte, error) {
	t := f.t
	if program != "kubectl" {
		t.Fatalf("unexpected %s %v", program, args)
	}
	switch {
	case hasArgs(args, "get", profilesResource, "workers", "-o", "json", "--show-managed-fields=true"):
		return []byte(f.profile), nil
	case hasArgs(args, "get", profilesResource, "workers", "-o", "json", "--ignore-not-found"):
		f.polls++
		if len(f.snapshots) == 0 {
			t.Fatal("unexpected profile poll")
		}
		next := f.snapshots[0]
		if len(f.snapshots) > 1 {
			f.snapshots = f.snapshots[1:]
		}
		return []byte(next), nil
	case hasArgs(args, "get", "nodes") && hasArgs(args, "--selector", "brewlet.sh/owner-uid=profile-uid"):
		return listJSON(t, objectJSON(t, `{"kind":"Node","metadata":{"name":"node-a"}}`)), nil
	case hasArgs(args, "get", "pods", "-o", "json", "--all-namespaces"):
		if f.podsErr != nil {
			return nil, f.podsErr
		}
		return listJSON(t, f.pods...), nil
	case hasArgs(args, "get", "deployments") && hasArgs(args, "--all-namespaces"):
		return listJSON(t, objectJSON(t, `{"kind":"Deployment","metadata":{"name":"brewlet-operator","namespace":"brewlet-system"}}`)), nil
	case hasArgs(args, "get", "pods") && hasArgs(args, "--namespace", "brewlet-system"):
		if !hasArgs(args, "--selector", "app=brewlet-cleanup,brewlet.sh/nodeprofile=workers") {
			t.Errorf("unexpected cleanup pod selector: %v", args)
		}
		return listJSON(t, f.cleanup...), nil
	case hasArgs(args, "delete", "--raw", "/apis/node.brewlet.sh/v1alpha1/nodeprofiles/workers"):
		if flagValue(t, args, "-f") != "-" {
			t.Errorf("delete options must be passed on stdin: %v", args)
		}
		var body map[string]any
		if err := json.Unmarshal(input, &body); err != nil {
			t.Fatal(err)
		}
		f.deletes = append(f.deletes, body)
		return []byte(f.profile), nil
	}
	t.Fatalf("unexpected kubectl %v", args)
	return nil, nil
}

func javaPod(t *testing.T, name, node string) object {
	return objectJSON(t, `{"kind":"Pod","metadata":{"name":"`+name+`","namespace":"shop"},
	  "spec":{"nodeName":"`+node+`","runtimeClassName":"brewlet"},"status":{"phase":"Running"}}`)
}

func terminatingJavaPod(t *testing.T, name, node string) object {
	return objectJSON(t, `{"kind":"Pod","metadata":{"name":"`+name+`","namespace":"shop","deletionTimestamp":"2026-10-04T19:00:00Z"},
	  "spec":{"nodeName":"`+node+`","runtimeClassName":"brewlet"},"status":{"phase":"Running"}}`)
}

func fastPolling(t *testing.T) {
	saved := progress.PollInterval
	progress.PollInterval = 2 * time.Millisecond
	t.Cleanup(func() { progress.PollInterval = saved })
}

func TestProfileDeleteValidation(t *testing.T) {
	out, _, err := runTest(t, []string{"profile", "delete", "--help"}, noExecution(t))
	if err != nil || !strings.Contains(out, "Exits non-zero on CleanupBlocked or timeout.") {
		t.Fatalf("delete help missing blocked-cleanup exit status: %q, %v", out, err)
	}
	for _, args := range [][]string{
		{"profile", "delete"},
		{"profile", "delete", "Bad_Name"},
		{"profile", "delete", "workers", "--namespace", "x"},
		{"profile", "delete", "workers", "--wait-timeout", "1m"},
		{"profile", "delete", "workers", "--wait", "--dry-run"},
		{"profile", "delete", "workers", "--wait", "--wait-timeout", "0s"},
		{"profile", "delete", "workers", "--output", "wide"},
	} {
		if _, _, err := runTest(t, args, noExecution(t)); err == nil {
			t.Errorf("%v: expected validation error", args)
		}
	}
}

func TestProfileDeleteRefusesManagedProfiles(t *testing.T) {
	for _, meta := range []string{
		`"labels":{"app.kubernetes.io/managed-by":"Helm"}`,
		`"annotations":{"argocd.argoproj.io/tracking-id":"x"}`,
		`"ownerReferences":[{"uid":"owner"}]`,
	} {
		f := &deleteCluster{t: t, profile: `{"apiVersion":"node.brewlet.sh/v1alpha1","kind":"NodeProfile",
		  "metadata":{"name":"workers","uid":"profile-uid","resourceVersion":"42",` + meta + `}}`}
		_, _, err := runTest(t, []string{"profile", "delete", "workers"}, f.exec)
		if err == nil || !strings.Contains(err.Error(), "source of truth") || len(f.deletes) != 0 {
			t.Fatalf("%s: managed profile was not refused: %v %v", meta, err, f.deletes)
		}
	}
}

func TestProfileDeleteWorkloadGuardAndPreconditions(t *testing.T) {
	profile := `{"apiVersion":"node.brewlet.sh/v1alpha1","kind":"NodeProfile",
	  "metadata":{"name":"workers","uid":"profile-uid","resourceVersion":"42"},
	  "status":{"targets":[{"name":"node-b","uid":"b"}],"conditions":[{"type":"Ready","status":"True","reason":"AllNodesProvisioned"}]}}`
	other := objectJSON(t, `{"kind":"Pod","metadata":{"name":"plain","namespace":"shop"},"spec":{"nodeName":"node-a"},"status":{"phase":"Running"}}`)
	elsewhere := javaPod(t, "elsewhere", "node-z")
	f := &deleteCluster{t: t, profile: profile, pods: []object{javaPod(t, "api", "node-a"), javaPod(t, "batch", "node-b"), terminatingJavaPod(t, "shutdown", "node-b"), other, elsewhere}}
	_, _, err := runTest(t, []string{"profile", "delete", "workers"}, f.exec)
	if err == nil || !strings.Contains(err.Error(), "3 Java workload pod(s)") || !strings.Contains(err.Error(), "shop/api (node node-a)") ||
		!strings.Contains(err.Error(), "shop/shutdown (node node-b, terminating)") ||
		strings.Contains(err.Error(), "elsewhere") || len(f.deletes) != 0 {
		t.Fatalf("workloads did not block deletion: %v", err)
	}
	f.podsErr = errors.New("pods is forbidden")
	if _, _, err = runTest(t, []string{"profile", "delete", "workers"}, f.exec); err == nil ||
		!strings.Contains(err.Error(), "cannot verify") || len(f.deletes) != 0 {
		t.Fatalf("pod list failure must fail closed: %v", err)
	}
	f.podsErr = nil
	out, stderr, err := runTest(t, []string{"profile", "delete", "workers", "--yes", "--output", "json"}, f.exec)
	if err != nil || len(f.deletes) != 1 || !strings.Contains(stderr, "Warning") || !strings.Contains(stderr, "asynchronous") {
		t.Fatalf("--yes deletion: %v %q %v", err, stderr, f.deletes)
	}
	pre := f.deletes[0]["preconditions"].(map[string]any)
	if pre["uid"] != "profile-uid" || pre["resourceVersion"] != "42" || f.deletes[0]["dryRun"] != nil {
		t.Fatalf("missing preconditions: %v", f.deletes[0])
	}
	var report deleteReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || !report.DeletionRequested || report.Deleted ||
		!reflect.DeepEqual(report.ClaimedNodes, []string{"node-a", "node-b"}) || len(report.JavaWorkloads) != 3 ||
		!report.JavaWorkloads[2].Terminating || report.Reason != "" {
		t.Fatalf("unexpected report: %s %v", out, err)
	}

	f = &deleteCluster{t: t, profile: profile}
	out, stderr, err = runTest(t, []string{"profile", "delete", "workers", "--dry-run"}, f.exec)
	if err != nil || len(f.deletes) != 0 || !strings.Contains(out, "client dry run") || !strings.Contains(stderr, "not deleted") {
		t.Fatalf("client dry run: %q %q %v", out, stderr, err)
	}
	_, _, err = runTest(t, []string{"profile", "delete", "workers", "--dry-run=server"}, f.exec)
	if err != nil || len(f.deletes) != 1 || !reflect.DeepEqual(f.deletes[0]["dryRun"], []any{"All"}) {
		t.Fatalf("server dry run: %v %v", err, f.deletes)
	}
}

func TestProfileDeleteWaitFollowsCleanup(t *testing.T) {
	fastPolling(t)
	profile := `{"apiVersion":"node.brewlet.sh/v1alpha1","kind":"NodeProfile",
	  "metadata":{"name":"workers","uid":"profile-uid","resourceVersion":"42"}}`
	cleanPod := func(node string, ready bool, waiting string) object {
		status := `"phase":"Running","conditions":[{"type":"Ready","status":"True"}]`
		if !ready {
			status = `"phase":"Pending","containerStatuses":[{"name":"c","state":{"waiting":{"reason":"` + waiting + `"}}}]`
		}
		return objectJSON(t, `{"kind":"Pod","metadata":{"name":"cleanup-`+node+`"},"spec":{"nodeName":"`+node+`"},"status":{`+status+`}}`)
	}
	f := &deleteCluster{t: t, profile: profile, snapshots: []string{
		deletingProfile("CleanupPending", "waiting for host cleanup", ""),
		deletingProfile("CleanupTeardown", "host cleanup complete", ""),
		"",
	}, cleanup: []object{cleanPod("node-a", true, ""), cleanPod("node-b", false, "ImagePullBackOff")}}
	out, stderr, err := runTest(t, []string{"profile", "delete", "workers", "--wait", "--output", "json"}, f.exec)
	if err != nil || len(f.deletes) != 1 {
		t.Fatalf("wait failed: %v %s", err, stderr)
	}
	for _, want := range []string{`Waiting for NodeProfile "workers"`, "node-a: cleaned", "node-b: cleaning (ImagePullBackOff)", `NodeProfile "workers" deleted`} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr)
		}
	}
	var report deleteReport
	if err := json.Unmarshal([]byte(out), &report); err != nil || !report.Deleted {
		t.Fatalf("report: %s %v", out, err)
	}
	f.snapshots, f.deletes = []string{""}, nil
	out, _, err = runTest(t, []string{"profile", "delete", "workers", "--wait"}, f.exec)
	if err != nil || !strings.Contains(out, "action: deleted\nstate: deleted") {
		t.Fatalf("table after wait: %v\n%s", err, out)
	}
	if got := summarizeCleanup("CleanupPending", "", []cleanupNode{{Name: "a", State: "cleaned"}, {Name: "b", State: "cleaning", Detail: "ImagePullBackOff"}}); got != "CleanupPending: 1/2 nodes cleaned (b: ImagePullBackOff)" {
		t.Fatalf("summary: %q", got)
	}

	// A profile recreated under the same name means the original is gone.
	for _, reason := range []string{"CleanupPending", "CleanupBlocked"} {
		f = &deleteCluster{t: t, profile: profile, snapshots: []string{strings.Replace(deletingProfile(reason, "", ""), "profile-uid", "new-uid", 1)}}
		if _, _, err := runTest(t, []string{"profile", "delete", "workers", "--wait"}, f.exec); err != nil {
			t.Fatalf("UID change (%s): %v", reason, err)
		}
	}
}

func TestProfileDeleteWaitFailures(t *testing.T) {
	fastPolling(t)
	profile := `{"apiVersion":"node.brewlet.sh/v1alpha1","kind":"NodeProfile",
	  "metadata":{"name":"workers","uid":"profile-uid","resourceVersion":"42"}}`
	f := &deleteCluster{t: t, profile: profile, snapshots: []string{deletingProfile("CleanupBlocked", "invalid source policy", "")}}
	out, _, err := runTest(t, []string{"profile", "delete", "workers", "--wait"}, f.exec)
	if err == nil || !strings.Contains(err.Error(), "invalid source policy") || !strings.Contains(err.Error(), "troubleshooting.md") ||
		!strings.Contains(err.Error(), "Never remove finalizers") || !strings.Contains(out, "CleanupBlocked") {
		t.Fatalf("blocked cleanup: %q %v", out, err)
	}
	f = &deleteCluster{t: t, profile: profile, snapshots: []string{deletingProfile("CleanupPending", "", "")}}
	_, _, err = runTest(t, []string{"profile", "delete", "workers", "--wait", "--wait-timeout", "30ms"}, f.exec)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "CleanupPending") {
		t.Fatalf("timeout: %v", err)
	}
}

func TestProfileDeleteBlockedReasons(t *testing.T) {
	fastPolling(t)
	for _, tc := range []struct {
		reason, message, recovery string
	}{
		{"CleanupBlocked", "invalid source policy", "Repair the profile spec"},
	} {
		for _, mode := range []string{"attach", "attach-wait", "delete-wait"} {
			t.Run(tc.reason+"/"+mode, func(t *testing.T) {
				blocked := deletingProfile(tc.reason, tc.message, "")
				f := &deleteCluster{t: t, profile: blocked, snapshots: []string{blocked}}
				args := []string{"profile", "delete", "workers", "--output", "json"}
				wantDeletes, wantPolls := 0, 0
				if mode != "attach" {
					args = append(args, "--wait", "--wait-timeout", "30ms")
					wantPolls = 1
				}
				if mode == "delete-wait" {
					f.profile = `{"apiVersion":"node.brewlet.sh/v1alpha1","kind":"NodeProfile",
					  "metadata":{"name":"workers","uid":"profile-uid","resourceVersion":"42"}}`
					wantDeletes = 1
				}
				out, stderr, err := runTest(t, args, f.exec)
				if err == nil {
					t.Fatalf("expected refusal, got success: %s\n%s", out, stderr)
				}
				for _, want := range []string{"Ready=False/" + tc.reason, tc.message, tc.recovery, "Never remove finalizers", cleanupTroubleshooting} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error missing %q: %v", want, err)
					}
				}
				for _, unwanted := range []string{"timed out", "cleanup continues in the background", "Follow cleanup with"} {
					if strings.Contains(err.Error()+stderr, unwanted) {
						t.Errorf("misleading diagnostic %q: %v\n%s", unwanted, err, stderr)
					}
				}
				var report deleteReport
				if decodeErr := json.Unmarshal([]byte(out), &report); decodeErr != nil ||
					report.Reason != tc.reason || report.Message != tc.message || report.Deleted ||
					report.AlreadyDeleting != (mode != "delete-wait") || report.DeletionRequested != (mode == "delete-wait") {
					t.Errorf("refusal report: %s (%v)", out, decodeErr)
				}
				if len(f.deletes) != wantDeletes || f.polls != wantPolls {
					t.Errorf("expected %d deletes and %d polls, got %d and %d", wantDeletes, wantPolls, len(f.deletes), f.polls)
				}
			})
		}
	}
}

func TestProfileDeleteAttachesToTerminatingProfile(t *testing.T) {
	fastPolling(t)
	// Helm-owned but already deleting: following cleanup mutates nothing.
	terminating := strings.Replace(deletingProfile("CleanupPending", "", ""), `"resourceVersion":"43"`,
		`"resourceVersion":"43","labels":{"app.kubernetes.io/managed-by":"Helm"}`, 1)
	f := &deleteCluster{t: t, profile: terminating, snapshots: []string{""}, podsErr: errors.New("must not list pods")}
	_, stderr, err := runTest(t, []string{"profile", "delete", "workers", "--wait"}, f.exec)
	if err != nil || len(f.deletes) != 0 || !strings.Contains(stderr, "already deleting") {
		t.Fatalf("attach: %v %q %v", err, stderr, f.deletes)
	}
	f = &deleteCluster{t: t, profile: deletingProfile("CleanupBlocked", "pool conflict", "")}
	out, _, err := runTest(t, []string{"profile", "delete", "workers"}, f.exec)
	if err == nil || !strings.Contains(err.Error(), "pool conflict") || len(f.deletes) != 0 || !strings.Contains(out, "already deleting") {
		t.Fatalf("blocked terminating profile: %q %v", out, err)
	}
}
