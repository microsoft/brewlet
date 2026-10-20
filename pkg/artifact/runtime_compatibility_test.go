package artifact

import (
	"encoding/json"
	"testing"
)

func TestRuntimeAllowListRoundTrip(t *testing.T) {
	for _, test := range []struct {
		json   string
		allows bool
	}{
		{`{"schemaVersion":2,"process":{"entrypoint":["runner"]}}`, true},
		{`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"runtimeImages":[]}`, false},
		{`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"runtimeImages":["approved"]}`, true},
		{`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"runtimeImages":["other"]}`, false},
	} {
		before, e := ParseLaunch([]byte(test.json))
		must(t, e)
		if before.Allows("approved") != test.allows {
			t.Fatal("incorrect allow-list semantics")
		}
		encoded, e := json.Marshal(before)
		must(t, e)
		var wire map[string]json.RawMessage
		must(t, json.Unmarshal(encoded, &wire))
		if _, exists := wire["runtimeImages"]; exists != (before.RuntimeImages != nil) {
			t.Fatalf("omitted/explicit allow-list changed: %s", encoded)
		}
		after, e := ParseLaunch(encoded)
		must(t, e)
		if after.Allows("approved") != test.allows {
			t.Fatalf("allow-list changed across marshal: %s", encoded)
		}
	}
	for _, data := range []string{
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":null}`,
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":{"runtimeImages":null}}`,
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":{}}`,
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":{"runtimeImages":["approved"]}}`,
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"runtimeImages":null}`,
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"runtimeImages":["approved"],"requires":{}}`,
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"RuntimeImages":["approved"]}`,
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"runtimeImages":["approved"],"runtime\u0049mages":[]}`,
	} {
		if _, e := ParseLaunch([]byte(data)); Reason(e) != "InvalidLaunchConfig" {
			t.Fatalf("explicit null accepted: %v", e)
		}
	}
}
