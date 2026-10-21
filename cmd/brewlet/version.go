package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
)

// Source builds deliberately do not impersonate a release.
var version = "dev"
var commit = "unknown"

func writeVersion(out io.Writer, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: brewlet version")
	}
	return json.NewEncoder(out).Encode(struct {
		Version  string `json:"version"`
		Commit   string `json:"commit"`
		Platform string `json:"platform"`
	}{version, commit, runtime.GOOS + "/" + runtime.GOARCH})
}
