package v2alpha1

import (
	"bytes"
	"io"
	"os/exec"
	"slices"
	"testing"

	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestPodWebhookSeparatesCreationFromEphemeralSubresource(t *testing.T) {
	requireHelm(t)
	raw, err := exec.Command("helm", "template", "test", "../../charts/brewlet", "--namespace", "brewlet-system").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	found := false
	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if doc["kind"] != "MutatingWebhookConfiguration" {
			continue
		}
		for _, item := range doc["webhooks"].([]any) {
			hook := item.(map[string]any)
			if hook["name"] != "pods.brewlet.sh" {
				continue
			}
			found = true
			rules := hook["rules"].([]any)
			if len(rules) != 2 {
				t.Fatal("pod webhook must have separate executable operation rules")
			}
			for _, item := range rules {
				rule := item.(map[string]any)
				operations := rule["operations"].([]any)
				resources := rule["resources"].([]any)
				if slices.Contains(resources, any("pods")) && (!slices.Equal(operations, []any{"CREATE"}) || len(resources) != 1) {
					t.Fatal("ordinary pod UPDATE matched the webhook")
				}
				if slices.Contains(resources, any("pods/ephemeralcontainers")) && (!slices.Equal(operations, []any{"UPDATE"}) || len(resources) != 1) {
					t.Fatal("ephemeral operation was not separately scoped")
				}
			}
		}
	}
	if !found {
		t.Fatal("pod webhook missing")
	}
}
