//go:build linux

package provisioner

import "testing"

func TestLiveCRIConfigurationRequiresHandlerAndBothAnnotations(t *testing.T) {
	valid := []byte(`{"config":{"containerd":{"runtimes":{"brewlet":{"runtimeType":"io.containerd.brewlet.v2","PodAnnotations":["brewlet.sh/runtime-image","brewlet.sh/rotated-to-generation"]}}}}}`)
	if err := validateLiveConfig(valid); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		`{}`,
		`{"config":{"containerd":{"runtimes":{"brewlet":{"runtimeType":"io.containerd.runc.v2","PodAnnotations":["brewlet.sh/*"]}}}}}`,
		`{"config":{"containerd":{"runtimes":{"brewlet":{"runtimeType":"io.containerd.brewlet.v2","PodAnnotations":["brewlet.sh/runtime-image"]}}}}}`,
	} {
		if err := validateLiveConfig([]byte(data)); err == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}
