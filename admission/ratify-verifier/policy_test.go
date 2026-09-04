// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/open-policy-agent/opa/ast"
	"github.com/open-policy-agent/opa/rego"
	"github.com/open-policy-agent/opa/types"
)

// constraintTemplatePath is the deployed Gatekeeper ConstraintTemplate. The
// policy it carries is the cluster-side admission control for managed-dependency
// attestation, so it is exercised here rather than only in a live cluster.
const constraintTemplatePath = "../deploy/40-gatekeeper-constrainttemplate.yaml"

// extractRego pulls the inline `rego: |` block out of the ConstraintTemplate.
// It is done textually so this test needs no YAML dependency.
func extractRego(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(constraintTemplatePath)
	if err != nil {
		t.Fatalf("read constraint template: %v", err)
	}

	lines := strings.Split(string(raw), "\n")
	start := -1
	keyIndent := 0
	for i, line := range lines {
		if strings.HasSuffix(strings.TrimRight(line, " \t"), "rego: |") {
			start = i + 1
			keyIndent = len(line) - len(strings.TrimLeft(line, " "))
			break
		}
	}
	if start < 0 {
		t.Fatalf("no `rego: |` block found in %s", constraintTemplatePath)
	}

	var block []string
	bodyIndent := -1
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			block = append(block, "")
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent <= keyIndent {
			break
		}
		if bodyIndent < 0 {
			bodyIndent = indent
		}
		block = append(block, line[bodyIndent:])
	}

	policy := strings.Join(block, "\n")
	if !strings.Contains(policy, "package brewletmanageddependencies") {
		t.Fatalf("extracted block is not the expected policy:\n%s", policy)
	}
	return policy
}

// evalViolations runs the policy against a pod and a canned Ratify response,
// mocking Gatekeeper's `external_data` builtin (which stock OPA does not have).
func evalViolations(t *testing.T, pod, externalData map[string]any) []string {
	t.Helper()

	mock := rego.Function1(
		&rego.Function{
			Name: "external_data",
			Decl: types.NewFunction(types.Args(types.A), types.A),
		},
		func(_ rego.BuiltinContext, _ *ast.Term) (*ast.Term, error) {
			v, err := ast.InterfaceToValue(externalData)
			if err != nil {
				return nil, err
			}
			return ast.NewTerm(v), nil
		},
	)

	query, err := rego.New(
		rego.Query("data.brewletmanageddependencies.violation"),
		rego.Module("constrainttemplate.rego", extractRego(t)),
		// The deployed template is written in Rego v0 syntax.
		rego.SetRegoVersion(ast.RegoV0),
		mock,
		rego.Input(map[string]any{"review": map[string]any{"object": pod}}),
	).PrepareForEval(context.Background())
	if err != nil {
		t.Fatalf("prepare policy: %v", err)
	}

	rs, err := query.Eval(context.Background())
	if err != nil {
		t.Fatalf("eval policy: %v", err)
	}
	if len(rs) == 0 {
		return nil
	}

	set, ok := rs[0].Expressions[0].Value.([]any)
	if !ok {
		t.Fatalf("unexpected result shape %T", rs[0].Expressions[0].Value)
	}
	var msgs []string
	for _, entry := range set {
		m, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("unexpected violation shape %T", entry)
		}
		msg, _ := m["msg"].(string)
		msgs = append(msgs, msg)
	}
	return msgs
}

func brewletPod(images ...string) map[string]any {
	containers := make([]any, 0, len(images))
	for _, img := range images {
		containers = append(containers, map[string]any{"image": img})
	}
	return map[string]any{
		"spec": map[string]any{
			"runtimeClassName": "brewlet",
			"containers":       containers,
		},
	}
}

func response(pairs ...any) map[string]any {
	return map[string]any{
		"responses":    pairs,
		"errors":       []any{},
		"system_error": "",
	}
}

func ok(img string) []any {
	return []any{img, map[string]any{"isSuccess": true}}
}

func failed(img string) []any {
	return []any{img, map[string]any{"isSuccess": false, "message": "no attestation"}}
}

func TestPolicyAdmitsVerifiedBrewletPod(t *testing.T) {
	msgs := evalViolations(t,
		brewletPod("registry.example.com/app@sha256:aaa"),
		response(ok("registry.example.com/app@sha256:aaa")),
	)
	if len(msgs) != 0 {
		t.Fatalf("expected admission, got violations: %v", msgs)
	}
}

func TestPolicyIgnoresNonBrewletPod(t *testing.T) {
	pod := map[string]any{
		"spec": map[string]any{
			"containers": []any{map[string]any{"image": "registry.example.com/other:latest"}},
		},
	}
	if msgs := evalViolations(t, pod, response()); len(msgs) != 0 {
		t.Fatalf("non-brewlet pod must pass through untouched, got: %v", msgs)
	}
}

// A provider that returns no responses at all must not result in admission.
// This is the regression guarding the fail-open defect: before the
// positive-verification rule existed, an empty responses array produced no
// violation and the pod was admitted with zero proof.
func TestPolicyDeniesWhenProviderReturnsNoResponses(t *testing.T) {
	msgs := evalViolations(t,
		brewletPod("registry.example.com/app@sha256:aaa"),
		response(),
	)
	if len(msgs) == 0 {
		t.Fatal("empty provider response must be denied, not admitted")
	}
}

func TestPolicyDeniesPartiallyVerifiedPod(t *testing.T) {
	msgs := evalViolations(t,
		brewletPod("registry.example.com/a@sha256:aaa", "registry.example.com/b@sha256:bbb"),
		response(ok("registry.example.com/a@sha256:aaa")),
	)
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one violation for the unverified image, got %v", msgs)
	}
	if !strings.Contains(msgs[0], "registry.example.com/b@sha256:bbb") {
		t.Fatalf("violation should name the unverified image, got %q", msgs[0])
	}
}

func TestPolicyDeniesOnResponseKeyMismatch(t *testing.T) {
	msgs := evalViolations(t,
		brewletPod("registry.example.com/app@sha256:aaa"),
		response(ok("registry.example.com/unrelated@sha256:zzz")),
	)
	if len(msgs) == 0 {
		t.Fatal("a success keyed to a different image must not verify this image")
	}
}

func TestPolicyDeniesExplicitVerificationFailure(t *testing.T) {
	msgs := evalViolations(t,
		brewletPod("registry.example.com/app@sha256:aaa"),
		response(failed("registry.example.com/app@sha256:aaa")),
	)
	if len(msgs) == 0 {
		t.Fatal("explicit isSuccess=false must be denied")
	}
}

func TestPolicyDeniesOnProviderSystemError(t *testing.T) {
	data := response()
	data["system_error"] = "provider unreachable"
	if msgs := evalViolations(t, brewletPod("registry.example.com/app@sha256:aaa"), data); len(msgs) == 0 {
		t.Fatal("provider system_error must fail closed")
	}
}

func TestPolicyDeniesOnProviderErrors(t *testing.T) {
	data := response()
	data["errors"] = []any{[]any{"registry.example.com/app@sha256:aaa", "boom"}}
	if msgs := evalViolations(t, brewletPod("registry.example.com/app@sha256:aaa"), data); len(msgs) == 0 {
		t.Fatal("provider errors must fail closed")
	}
}

func TestPolicyCoversInitAndEphemeralContainers(t *testing.T) {
	for _, field := range []string{"initContainers", "ephemeralContainers"} {
		t.Run(field, func(t *testing.T) {
			pod := map[string]any{
				"spec": map[string]any{
					"runtimeClassName": "brewlet",
					"containers": []any{
						map[string]any{"image": "registry.example.com/app@sha256:aaa"},
					},
					field: []any{
						map[string]any{"image": "registry.example.com/side@sha256:bbb"},
					},
				},
			}
			msgs := evalViolations(t, pod, response(ok("registry.example.com/app@sha256:aaa")))
			if len(msgs) == 0 {
				t.Fatalf("an unverified %s image must be denied", field)
			}
		})
	}
}
