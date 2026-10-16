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
		{`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":{}}`, true},
		{`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":{"runtimeImages":[]}}`, false},
		{`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":{"runtimeImages":["approved"]}}`, true},
	} {
		before, e := ParseLaunch([]byte(test.json))
		must(t, e)
		if before.Allows("approved") != test.allows {
			t.Fatal("incorrect allow-list semantics")
		}
		encoded, e := json.Marshal(before)
		must(t, e)
		after, e := ParseLaunch(encoded)
		must(t, e)
		if after.Allows("approved") != test.allows {
			t.Fatalf("allow-list changed across marshal: %s", encoded)
		}
	}
	for _, data := range []string{
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":null}`,
		`{"schemaVersion":2,"process":{"entrypoint":["runner"]},"requires":{"runtimeImages":null}}`,
	} {
		if _, e := ParseLaunch([]byte(data)); Reason(e) != "InvalidLaunchConfig" {
			t.Fatalf("explicit null accepted: %v", e)
		}
	}
}
