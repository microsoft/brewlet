package v1alpha1_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/microsoft/brewlet/pkg/node"
	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func crdSchema(t *testing.T, name string) *ext.JSONSchemaProps {
	t.Helper()
	data, err := os.ReadFile("../../deploy/crds/" + name)
	if err != nil {
		t.Fatal(err)
	}
	v1 := new(extv1.CustomResourceDefinition)
	if err := yaml.UnmarshalStrict(data, v1); err != nil {
		t.Fatal(err)
	}
	crd := new(ext.CustomResourceDefinition)
	if err := extv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(v1, crd, nil); err != nil {
		t.Fatal(err)
	}
	if crd.Spec.Validation != nil {
		return crd.Spec.Validation.OpenAPIV3Schema
	}
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema
}

// schemaAccepts applies both OpenAPI and CEL create-time validation to one value.
func schemaAccepts(t *testing.T, props *ext.JSONSchemaProps, value any) bool {
	t.Helper()
	openapi, _, err := validation.NewSchemaValidator(props)
	if err != nil {
		t.Fatal(err)
	}
	if errs := validation.ValidateCustomResource(field.NewPath("value"), value, openapi); len(errs) > 0 {
		return false
	}
	structural, err := schema.NewStructural(props)
	if err != nil {
		t.Fatal(err)
	}
	errs, _ := cel.NewValidator(structural, false, celconfig.PerCallLimit).
		Validate(context.Background(), field.NewPath("value"), structural, value, nil, celconfig.RuntimeCELCostBudget)
	return len(errs) == 0
}

func TestCRDVolumeMountPathsMatchNodePolicy(t *testing.T) {
	props := crdSchema(t, "runtimeimages.yaml").Properties["spec"].Properties["volumeMountPaths"]
	for _, paths := range [][]string{
		{}, {"/data"}, {"/data", "/tmp"}, {"/srv/cache", "/var/lib/app"},
		{"/"}, {"data"}, {"/data/"}, {"/data//x"}, {"/data/./x"}, {"/data/.."}, {"/data/../x"},
		{"/a\\b"}, {"/a\x01b"}, {"/tmp", "/data"}, {"/data", "/data"}, {"/data", "/data/x"},
		{"/app"}, {"/app/data"}, {"/dev/shm"}, {"/proc"}, {"/sys/fs"}, {"/etc"}, {"/etc/hosts"},
		{"/etc/resolv.conf/x"}, {"/applications"},
	} {
		value := make([]any, len(paths))
		for i, p := range paths {
			value[i] = p
		}
		want := node.ValidateVolumeMountPaths(paths) == nil
		if got := schemaAccepts(t, &props, value); got != want {
			t.Errorf("%q: CRD accepts=%v, node policy accepts=%v", paths, got, want)
		}
	}
}

func TestCRDIdleTTLMatchesControllerPolicy(t *testing.T) {
	props := crdSchema(t, "nodeprofiles.yaml").Properties["spec"].Properties["runtimeImages"].Items.Schema.Properties["idleTTL"]
	for _, value := range []string{"72h", "1h30m", "90s", "1.5h", "300ms", "", "0", "0s", "-1h", "72", "h", "1d", "forever"} {
		d, err := time.ParseDuration(value)
		want := err == nil && d > 0
		if got := schemaAccepts(t, &props, value); got != want {
			t.Errorf("%q: CRD accepts=%v, controller accepts=%v", value, got, want)
		}
	}
}
