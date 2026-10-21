package main

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"
)

func TestVersion(t *testing.T) {
	var out bytes.Buffer
	if err := writeVersion(&out, nil); err != nil {
		t.Fatal(err)
	}
	var value map[string]string
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if len(value) != 3 || value["version"] != version || value["commit"] != commit ||
		value["platform"] != runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("wrong build identity: %s", &out)
	}
	if err := writeVersion(&out, []string{"unexpected"}); err == nil {
		t.Fatal("accepted unexpected argument")
	}
}
