package v1alpha1

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
			if v1.Spec.Group != "node.brewlet.sh" || len(v1.Spec.Versions) != 1 ||
				v1.Spec.Versions[0].Name != "v1alpha1" || !v1.Spec.Versions[0].Served ||
				!v1.Spec.Versions[0].Storage {
				t.Fatal("CRDs must serve and store only node.brewlet.sh/v1alpha1")
			}
			if GroupVersion.String() != "node.brewlet.sh/v1alpha1" {
				t.Fatal("API scheme and CRD version disagree")
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
