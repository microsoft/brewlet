package provisioner

import (
	"fmt"

	"github.com/pelletier/go-toml/v2"
)

func mapAt(parent map[string]any, key string) map[string]any {
	child, _ := parent[key].(map[string]any)
	if child == nil {
		child = map[string]any{}
		parent[key] = child
	}
	return child
}

func runtimePlugin(version int64) (string, error) {
	switch version {
	case 2:
		return "io.containerd.grpc.v1.cri", nil
	case 3:
		return "io.containerd.cri.v1.runtime", nil
	default:
		return "", fmt.Errorf("containerd config version 2 or 3 required")
	}
}

func configuredRuntime(original []byte) ([]byte, error) {
	var config map[string]any
	if err := toml.Unmarshal(original, &config); err != nil {
		return nil, err
	}
	version, _ := config["version"].(int64)
	plugin, err := runtimePlugin(version)
	if err != nil {
		return nil, err
	}
	plugins := mapAt(config, "plugins")
	containerd := mapAt(mapAt(plugins, plugin), "containerd")
	runtimes := mapAt(containerd, "runtimes")
	if _, exists := runtimes["brewlet"]; exists {
		return nil, fmt.Errorf("unmanaged brewlet handler cannot be adopted")
	}
	options := map[string]any{}
	if runc, ok := runtimes["runc"].(map[string]any); ok {
		if existing, ok := runc["options"].(map[string]any); ok {
			for k, v := range existing {
				options[k] = v
			}
		}
	}
	runtimes["brewlet"] = map[string]any{
		"runtime_type":    "io.containerd.brewlet.v2",
		"pod_annotations": []string{"brewlet.sh/runtime-image", "brewlet.sh/rotated-to-generation"},
		"options":         options,
	}
	if version == 2 {
		containerd["discard_unpacked_layers"] = false
	} else {
		mapAt(plugins, "io.containerd.cri.v1.images")["discard_unpacked_layers"] = false
	}
	return toml.Marshal(config)
}

func configAt(parent map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		parent, _ = parent[key].(map[string]any)
	}
	return parent
}

func validatePackedLayerRetention(config map[string]any) error {
	version, _ := config["version"].(int64)
	plugin, err := runtimePlugin(version)
	if err != nil {
		return err
	}
	imageConfig := configAt(config, "plugins", plugin, "containerd")
	if version == 3 {
		imageConfig = configAt(config, "plugins", "io.containerd.cri.v1.images")
	}
	discard, exposed := imageConfig["discard_unpacked_layers"].(bool)
	if !exposed || discard {
		return fmt.Errorf("effective packed-layer retention requires explicit discard_unpacked_layers=false")
	}
	return nil
}

func validatePackedLayerConfig(data []byte) error {
	var config map[string]any
	if err := toml.Unmarshal(data, &config); err != nil {
		return err
	}
	return validatePackedLayerRetention(config)
}
