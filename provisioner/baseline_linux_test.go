//go:build linux

package provisioner

import (
	"bytes"
	"fmt"
	"testing"
)

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
func TestEffectiveBaselineDeniesMissingOrUnsafePackedLayerRetention(t *testing.T) {
	for _, version := range []string{"2", "3"} {
		desired, err := configuredRuntime([]byte("version=" + version))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateEffectiveConfig(desired); err != nil {
			t.Fatal(err)
		}
		for _, replacement := range []string{"discard_unpacked_layers = true", "# retention omitted", `discard_unpacked_layers = "false"`} {
			bad := bytes.ReplaceAll(desired, []byte("discard_unpacked_layers = false"), []byte(replacement))
			if err := validateEffectiveConfig(bad); err == nil {
				t.Fatal("baseline claimed ready without packed-layer retention", string(bad))
			}
		}
	}
}

func TestLiveCRIRetentionCheckedOnlyWhenExposed(t *testing.T) {
	base := `{"config":{"containerd":{"runtimes":{"brewlet":{"runtimeType":"io.containerd.brewlet.v2","PodAnnotations":["brewlet.sh/*"]}}%s}%s}}`
	for _, section := range []string{"containerd", "images"} {
		for _, value := range []string{"false", "true", "null", `"false"`} {
			field := `,"discardUnpackedLayers":` + value
			data := base
			if section == "containerd" {
				data = fmt.Sprintf(base, field, "")
			} else {
				data = fmt.Sprintf(base, "", field)
			}
			err := validateLiveConfig([]byte(data))
			if (err == nil) != (value == "false") {
				t.Fatalf("live %s value %s: %v", section, value, err)
			}
		}
	}
}
