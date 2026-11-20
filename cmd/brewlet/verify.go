package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/microsoft/brewlet/pkg/artifact"
)

const verifySynopsis = `Usage: brewlet verify REF@sha256:... --platform OS/ARCH [flags]

Verifies every blob in a pinned OCI graph and the selected platform's Brewlet
application contract, including layer paths and explicit launch metadata.
REF may be a registry reference or a local OCI layout DIR@sha256:DIGEST.
This is structural verification, not publisher authentication or a vulnerability scan.`

func runVerify(ctx context.Context, args []string, stdout, _ io.Writer) error {
	fs := newFlagSet("verify", verifySynopsis, stdout)
	var platforms repeated
	fs.Var(&platforms, "platform", "one explicit linux/arch[/variant] (required)")
	authFile := fs.String("registry-auth", "", "JSON credentials keyed by exact scheme://host[:port]")
	insecure := fs.Bool("insecure", false, "explicitly allow HTTP registries")
	scheme := fs.String("registry-scheme", "", "explicit https for loopback TLS")
	output := addOutputFlag(fs)
	ref, err := parseRefCommand(fs, args)
	if err != nil {
		return err
	}
	if err = checkTextOutput("verify", *output); err != nil {
		return err
	}
	if len(platforms) != 1 {
		return fmt.Errorf("verify requires exactly one --platform")
	}
	if *scheme != "" && *scheme != "https" || *scheme == "https" && *insecure {
		return fmt.Errorf("invalid/conflicting registry scheme")
	}
	platform, err := artifact.ParsePlatform(platforms[0])
	if err != nil {
		return err
	}
	registries, err := loadRegistries(*authFile, *insecure, *scheme == "https")
	if err != nil {
		return err
	}
	temp, err := os.MkdirTemp("", "brewlet-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	store := artifact.Store{Dir: temp}
	target, err := pinnedInput(ctx, store, ref, registries)
	if err != nil {
		return err
	}
	result, err := artifact.InspectApplication(ctx, store, target, platform)
	if err != nil {
		return err
	}
	if *output == outputJSON {
		return writeJSON(stdout, result)
	}
	fmt.Fprintf(stdout, "Verified application structure %s\n", ref)
	return writeFields(stdout, "  ",
		field{"Platform", platformLabel(&result.Platform)},
		field{"Manifest", result.Manifest.Digest.String()},
		field{"Config", result.Config.Digest.String()})
}
