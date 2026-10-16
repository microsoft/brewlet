package v2alpha1

import (
	"bytes"
	"context"
	"os"
	"testing"

	ext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	extvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"sigs.k8s.io/yaml"
)

func TestDeployAndChartCRDsAreIdenticalAndValid(t *testing.T) {
	for _, name := range []string{"runtimeimages.yaml", "nodeprofiles.yaml", "noderetirementevidences.yaml"} {
		t.Run(name, func(t *testing.T) {
			deploy, err := os.ReadFile("../../deploy/crds/" + name)
			if err != nil {
				t.Fatal(err)
			}
			chart, err := os.ReadFile("../../charts/brewlet/crds/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(deploy, chart) {
				t.Fatal("chart and deploy CRD schemas drifted")
			}
			v1 := new(extv1.CustomResourceDefinition)
			if err := yaml.UnmarshalStrict(deploy, v1); err != nil {
				t.Fatal(err)
			}
			extv1.SetDefaults_CustomResourceDefinition(v1)
			crd := new(ext.CustomResourceDefinition)
			if err := extv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(v1, crd, nil); err != nil {
				t.Fatal(err)
			}
			if errs := extvalidation.ValidateCustomResourceDefinition(context.Background(), crd); len(errs) > 0 {
				t.Fatal(errs.ToAggregate())
			}
		})
	}
}
