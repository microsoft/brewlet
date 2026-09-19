// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// This deliberately untrusted competing verifier is used only to prove that its
// success cannot replace Brewlet verification and its failure cannot veto it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/ratify-project/ratify/pkg/common"
	"github.com/ratify-project/ratify/pkg/ocispecs"
	"github.com/ratify-project/ratify/pkg/referrerstore"
	_ "github.com/ratify-project/ratify/pkg/referrerstore/oras"
	"github.com/ratify-project/ratify/pkg/verifier"
	"github.com/ratify-project/ratify/pkg/verifier/plugin/skel"
)

func verify(args *skel.CmdArgs, subject common.Reference, reference ocispecs.ReferenceDescriptor, store referrerstore.ReferrerStore) (*verifier.VerifierResult, error) {
	var input struct {
		Config struct {
			Success bool `json:"success"`
		} `json:"config"`
	}
	if err := json.Unmarshal(args.StdinData, &input); err != nil {
		return nil, err
	}
	// Fetch the real candidate rather than manufacture an executor report.
	if _, err := store.GetReferenceManifest(context.Background(), subject, reference); err != nil {
		return nil, err
	}
	return &verifier.VerifierResult{
		IsSuccess: input.Config.Success, Name: "admission-other",
		VerifierName: "admission-other", Type: "admission-other",
		VerifierType: "admission-other", Message: "live competing verifier",
	}, nil
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--probe" {
		// Ratify 1.4.5 drops errorReason in GetVerifierResult. Preserve the real
		// plugin's unmodified stdout in a supplementary subprocess diagnostic.
		command := exec.Command("/home/nonroot/.ratify/plugins/brewlet-managed-dependencies")
		command.Env = append(os.Environ(), "RATIFY_VERIFIER_COMMAND=VERIFY",
			"RATIFY_VERIFIER_VERSION=1.0.0", "RATIFY_VERIFIER_SUBJECT="+os.Args[2])
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	skel.PluginMain("admission-other", "1.0.0", verify, []string{"1.0.0"})
}
