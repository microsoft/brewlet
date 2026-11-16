package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"
)

func TestVersion(t *testing.T) {
	platform := runtime.GOOS + "/" + runtime.GOARCH

	// A non-terminal writer still gets text: JSON is never inferred.
	var text bytes.Buffer
	if err := writeVersion(&text, nil); err != nil {
		t.Fatal(err)
	}
	if want := "brewlet " + version + " (commit " + commit + ", " + platform + ")\n"; text.String() != want {
		t.Fatalf("version text = %q, want %q", text.String(), want)
	}

	for _, args := range [][]string{{"-o", "json"}, {"--output=json"}} {
		var out bytes.Buffer
		if err := writeVersion(&out, args); err != nil {
			t.Fatal(err)
		}
		var value map[string]string
		if err := json.Unmarshal(out.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if len(value) != 3 || value["version"] != version || value["commit"] != commit || value["platform"] != platform {
			t.Fatalf("wrong build identity: %s", &out)
		}
	}

	for _, args := range [][]string{{"unexpected"}, {"-o", "yaml"}} {
		if err := writeVersion(&bytes.Buffer{}, args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}
