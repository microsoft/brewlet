package provisioner

import (
	"bytes"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestConfiguredRuntimeEnforcesExactVersionedPackedLayerRetention(t *testing.T) {
	for _, version := range []string{"2", "3"} {
		t.Run(version, func(t *testing.T) {
			plugin := "io.containerd.grpc.v1.cri"
			section := `plugins."` + plugin + `".containerd`
			if version == "3" {
				plugin = "io.containerd.cri.v1.runtime"
				section = `plugins."io.containerd.cri.v1.images"`
			}
			original := []byte("# preserve these exact bytes for restoration\nversion = " + version + "\n[" + section + "]\ndiscard_unpacked_layers = true\n" +
				`[plugins."` + plugin + `".containerd.runtimes.runc.options]` + "\nSystemdCgroup = true\n" +
				"[debug]\nlevel = 'warn'\n")
			before := append([]byte(nil), original...)
			desired, err := configuredRuntime(original)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, original) {
				t.Fatal("configuration mutated original backup bytes")
			}
			if err := validatePackedLayerConfig(desired); err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			if err := toml.Unmarshal(desired, &config); err != nil {
				t.Fatal(err)
			}
			if configAt(config, "debug")["level"] != "warn" || configAt(config, "plugins", plugin, "containerd", "runtimes", "brewlet", "options")["SystemdCgroup"] != true {
				t.Fatal("unrelated config or original runc options lost")
			}
			if version == "3" {
				if _, wrong := configAt(config, "plugins", plugin, "containerd")["discard_unpacked_layers"]; wrong {
					t.Fatal("retention incorrectly written into v3 runtime plugin")
				}
			}
			if _, err := configuredRuntime(desired); err == nil {
				t.Fatal("existing handler silently adopted")
			}
			for _, bad := range [][]byte{
				[]byte("version = " + version + "\n"),
				bytes.ReplaceAll(desired, []byte("discard_unpacked_layers = false"), []byte("discard_unpacked_layers = true")),
				bytes.ReplaceAll(desired, []byte("discard_unpacked_layers = false"), []byte(`discard_unpacked_layers = "false"`)),
			} {
				if err := validatePackedLayerConfig(bad); err == nil {
					t.Fatalf("unsafe or missing retention accepted: %s", bad)
				}
			}
		})
	}
}

func TestPackedLayerRetentionRejectsWrongPluginAndImplicitDefaults(t *testing.T) {
	for _, data := range []string{
		`version=3
[plugins."io.containerd.cri.v1.runtime".containerd]
discard_unpacked_layers=false`,
		`version=2
[plugins."io.containerd.cri.v1.images"]
discard_unpacked_layers=false`,
		"version=1",
	} {
		if err := validatePackedLayerConfig([]byte(data)); err == nil {
			t.Fatal("wrong retention authority accepted", data)
		}
	}
	desired, err := configuredRuntime([]byte("version=3"))
	if err != nil || !strings.Contains(string(desired), "discard_unpacked_layers = false") {
		t.Fatal("missing config did not receive explicit retention", string(desired), err)
	}
}
