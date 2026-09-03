// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command brewlet-source-policy validates administrator-provided runtime
// source fields before the provisioner mutates a host.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/microsoft/brewlet/internal/sourcepolicy"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "validate-ref":
		err = validateRef(os.Args[2:])
	case "validate-host":
		err = validateHost(os.Args[2:])
	case "validate-mirror":
		err = validateMirror(os.Args[2:])
	case "validate-path":
		err = validatePath(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  brewlet-source-policy validate-ref --image REF
  brewlet-source-policy validate-host --host HOST
  brewlet-source-policy validate-mirror --target HOST[/PATH]
  brewlet-source-policy validate-path --path PATH`)
}

func validateRef(args []string) error {
	fs := flag.NewFlagSet("validate-ref", flag.ContinueOnError)
	image := fs.String("image", "", "digest-pinned image reference")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return sourcepolicy.ValidateDigestReference(*image)
}

func validateHost(args []string) error {
	fs := flag.NewFlagSet("validate-host", flag.ContinueOnError)
	host := fs.String("host", "", "registry host")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return sourcepolicy.ValidateRegistryHost(*host)
}

func validateMirror(args []string) error {
	fs := flag.NewFlagSet("validate-mirror", flag.ContinueOnError)
	target := fs.String("target", "", "mirror host with optional repository path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	host, err := sourcepolicy.ValidateMirrorTarget(*target)
	if err != nil {
		return err
	}
	fmt.Println(host)
	return nil
}

func validatePath(args []string) error {
	fs := flag.NewFlagSet("validate-path", flag.ContinueOnError)
	value := fs.String("path", "", "absolute source path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return sourcepolicy.ValidateAbsolutePath(*value)
}
