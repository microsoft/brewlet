package main

import (
	"fmt"
	"io"
	"runtime"
)

// Source builds deliberately do not impersonate a release.
var version = "dev"
var commit = "unknown"

const versionSynopsis = `Usage: brewlet version [-o text|json]

Prints the Brewlet version, source commit and platform.`

func writeVersion(out io.Writer, args []string) error {
	fs := newFlagSet("version", versionSynopsis, out)
	output := addOutputFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: brewlet version [-o text|json]")
	}
	if err := checkTextOutput("version", *output); err != nil {
		return err
	}
	platform := runtime.GOOS + "/" + runtime.GOARCH
	if *output == outputJSON {
		return writeJSON(out, struct {
			Version  string `json:"version"`
			Commit   string `json:"commit"`
			Platform string `json:"platform"`
		}{version, commit, platform})
	}
	_, err := fmt.Fprintf(out, "brewlet %s (commit %s, %s)\n", version, commit, platform)
	return err
}
