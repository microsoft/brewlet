package provisioner

import (
	"testing"

	"github.com/microsoft/brewlet/pkg/node"
)

func TestBindingUsesActualLeasedEpochGenerationAndProtectedPodIdentity(t *testing.T) {
	lease := node.Lease{TaskID: "container-one", Namespace: "k8s.io", Metadata: node.Metadata{ID: "static", InventoryUID: "old-epoch", Generation: 7, SourceDigest: "sha256:old"}}
	annotations := map[string]string{"io.kubernetes.cri.sandbox-uid": "pod-one", "io.kubernetes.cri.sandbox-name": "workload", "io.kubernetes.cri.sandbox-namespace": "apps", "io.kubernetes.cri.container-name": "main", "brewlet.sh/runtime-image": "static"}
	binding, err := bindingFromAnnotations(lease, annotations, "running")
	if err != nil {
		t.Fatal(err)
	}
	if binding.InventoryUID != "old-epoch" || binding.Generation != 7 || binding.PodUID != "pod-one" || binding.ContainerID != "container-one" {
		t.Fatal(binding)
	}
	delete(annotations, "io.kubernetes.cri.sandbox-uid")
	if _, err := bindingFromAnnotations(lease, annotations, "running"); err == nil {
		t.Fatal("missing protected Pod identity inferred")
	}
	annotations["io.kubernetes.cri.sandbox-uid"] = "pod-one"
	annotations["brewlet.sh/runtime-image"] = "different"
	if _, err := bindingFromAnnotations(lease, annotations, "running"); err == nil {
		t.Fatal("inconsistent actual runtime selection accepted")
	}
}
