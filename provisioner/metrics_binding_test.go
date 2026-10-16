package provisioner

import (
	"testing"

	"github.com/microsoft/brewlet/pkg/node"
	dto "github.com/prometheus/client_model/go"
)

func TestRuntimeInfoMetricIdentifiesActualPodContainerAndRuntime(t *testing.T) {
	binding := node.RuntimeBinding{PodNamespace: "apps", PodName: "api", PodUID: "pod-one", ContainerName: "main", ContainerID: "container-one", ID: "static", InventoryUID: "epoch-one", Generation: 7, Digest: "sha256:actual"}
	data := new(dto.Metric)
	if err := runtimeBindingMetric(binding).Write(data); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{}
	for _, label := range data.Label {
		labels[label.GetName()] = label.GetValue()
	}
	for name, value := range map[string]string{"namespace": "apps", "pod": "api", "pod_uid": "pod-one", "container": "main", "container_id": "container-one", "id": "static", "inventory_uid": "epoch-one", "generation": "7", "digest": "sha256:actual"} {
		if labels[name] != value {
			t.Fatal("runtime info cannot be joined to actual Pod/container", name, labels)
		}
	}
	if data.GetGauge().GetValue() != 1 {
		t.Fatal("info metric is not per actual container")
	}
}
