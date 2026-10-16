package v2alpha1

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

func TestRuntimeMetadataUpdateCanonicalizesOmittedCompatibility(t *testing.T) {
	data, err := os.ReadFile("../../deploy/crds/runtimeimages.yaml")
	if err != nil {
		t.Fatal(err)
	}
	crd := new(extv1.CustomResourceDefinition)
	if err := yaml.UnmarshalStrict(data, crd); err != nil {
		t.Fatal(err)
	}
	props := new(ext.JSONSchemaProps)
	if err := extv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, props, nil); err != nil {
		t.Fatal(err)
	}
	schema, err := structuralschema.NewStructural(props)
	if err != nil {
		t.Fatal(err)
	}
	validate := celvalidation.NewValidator(schema, true, celconfig.PerCallLimit)
	oldRaw := []byte(`{"apiVersion":"node.brewlet.sh/v2alpha1","kind":"RuntimeImage","metadata":{"name":"python-3.12","uid":"epoch","resourceVersion":"1"},"spec":{"source":{"image":"registry.example.com/python@sha256:` + strings.Repeat("a", 64) + `"},"user":{"uid":65532,"gid":65532}}}`)
	var old map[string]any
	_ = kjson.Unmarshal(oldRaw, &old)
	var typed RuntimeImage
	_ = json.Unmarshal(oldRaw, &typed)
	typed.Finalizers = []string{CleanupFinalizer}
	normalized, _ := json.Marshal(typed)
	var current map[string]any
	_ = kjson.Unmarshal(normalized, &current)
	if errs, _ := validate.Validate(context.Background(), field.NewPath("runtimeimage"), schema, current, old, 10000000); len(errs) > 0 {
		t.Fatalf("unchanged typed/finalizer update rejected: %v", errs)
	}
	reverse, _ := json.Marshal(old)
	var omitted map[string]any
	_ = kjson.Unmarshal(reverse, &omitted)
	if errs, _ := validate.Validate(context.Background(), field.NewPath("runtimeimage"), schema, omitted, current, 10000000); len(errs) > 0 {
		t.Fatalf("empty-to-omitted normalization rejected: %v", errs)
	}
	current["spec"].(map[string]any)["compatibility"] = map[string]any{"ecosystem": "python"}
	if errs, _ := validate.Validate(context.Background(), field.NewPath("runtimeimage"), schema, current, old, 10000000); len(errs) == 0 {
		t.Fatal("real compatibility identity change accepted")
	}
}
