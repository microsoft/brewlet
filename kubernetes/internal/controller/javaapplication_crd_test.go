// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestJavaApplicationCRDsMatch(t *testing.T) {
	readCRD := func(path string) unstructured.Unstructured {
		t.Helper()
		content, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		var crd unstructured.Unstructured
		if err := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096).Decode(&crd); err != nil {
			t.Fatal(err)
		}
		return crd
	}
	deploy := readCRD("deploy/javaapplication-crd.yaml")
	chart := readCRD("charts/brewlet/crds/javaapplication-crd.yaml")
	if !equality.Semantic.DeepEqual(deploy.Object, chart.Object) {
		t.Fatal("deploy and Helm JavaApplication CRDs differ")
	}
}
