package controller

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	validation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	kjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

func TestProducedPreAuthorizationStatusConformsToRealCRDSchema(t *testing.T) {
	r, im, _, _ := revocationSetup(t)
	ctx := context.Background()
	revokeStep(t, r, im)
	if err := r.Get(ctx, client.ObjectKeyFromObject(im), im); err != nil {
		t.Fatal(err)
	}
	if len(im.Status.RevocationTargets) != 1 || im.Status.RevocationTargets[0].Generation != im.Status.Generation || im.Status.RevocationTargets[0].Digest != im.Status.Digest {
		t.Fatal("producer did not bind exact generation and digest before worker authorization")
	}
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
	schema, _, err := validation.NewSchemaValidator(props)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(im)
	var object map[string]any
	if err := kjson.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if errs := validation.ValidateCustomResource(field.NewPath("runtimeimage"), object, schema); len(errs) > 0 {
		t.Fatalf("actual controller-produced status violates CRD: %v", errs)
	}
	target := object["status"].(map[string]any)["revocationTargets"].([]any)[0].(map[string]any)
	delete(target, "generation")
	delete(target, "digest")
	if errs := validation.ValidateCustomResource(field.NewPath("runtimeimage"), object, schema); len(errs) == 0 {
		t.Fatal("schema silently accepted the formerly broken missing generation/digest payload")
	}
}
