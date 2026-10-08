// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chart_test

import (
	"bytes"
	"os"
	"reflect"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

func TestRetirementEvidenceSchemaAndRBACParity(t *testing.T) {
	for _, name := range []string{"nodeprofile-crd.yaml", "noderetirementevidence-crd.yaml"} {
		raw, err := os.ReadFile("../../deploy/" + name)
		if err != nil {
			t.Fatal(err)
		}
		chart, err := os.ReadFile("../../charts/brewlet/crds/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, chart) {
			t.Fatalf("raw/chart schemas differ: %s", name)
		}
	}
	raw, err := os.ReadFile("../../deploy/retirement-recovery-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	expected := convert[rbacv1.ClusterRole](t, parseManifest(t, raw)[0])
	found := false
	for _, obj := range render(t) {
		if obj.GetKind() == "ClusterRole" && obj.GetName() == expected.Name {
			got := convert[rbacv1.ClusterRole](t, obj)
			if !reflect.DeepEqual(got.Rules, expected.Rules) || got.AggregationRule != nil {
				t.Fatal("recovery role must match restricted, unaggregated raw permissions")
			}
			found = true
		}
		if obj.GetKind() == "ClusterRoleBinding" {
			binding := convert[rbacv1.ClusterRoleBinding](t, obj)
			if binding.RoleRef.Name == expected.Name {
				t.Fatal("recovery authority must not be automatically bound")
			}
		}
	}
	if !found {
		t.Fatal("dedicated recovery submitter role missing")
	}
}
