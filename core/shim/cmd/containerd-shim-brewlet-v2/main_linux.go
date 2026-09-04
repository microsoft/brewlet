// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

// Production entrypoint for the Brewlet containerd Runtime v2 shim.
//
// Like containerd's own `containerd-shim-runc-v2`, we run the containerd shim
// framework's manager loop. containerd invokes this binary per task; the
// framework forks/serves the TTRPC Task service and dispatches
// Create/Start/Kill/Delete/Exec/Wait to it.
//
// Brewlet reuses containerd's battle-tested runc manager (process forking,
// socket setup) and runc-backed Task service for the entire container
// lifecycle, and layers in ONE Brewlet-specific step: at Create() we disassemble
// the OCI artifact and assemble the `java -jar` OCI bundle (see
// service_linux.go). Importing the blank plugin below registers that decorated
// task service into the shim's plugin graph.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/manager"
	"github.com/containerd/containerd/v2/pkg/shim"
	// The Brewlet task service (a decorator over the runc task service) is
	// registered as the shim's TTRPC "task" plugin from service_linux.go's
	// init(). We deliberately do NOT import runc's own task/plugin, which would
	// collide on the same plugin ID.
	//
	// containerd 1.7's runc shim also registered a no-op TTRPC *sandbox*
	// service (runtime/v2/runc/pause), which we blank-imported here. containerd
	// 2.x deleted that package: pod-sandbox handling moved daemon-side, and the
	// CRI plugin defaults every runtime handler that does not set `sandboxer`
	// to the in-daemon "podsandbox" controller
	// (internal/cri/config/config.go). That controller creates the pause
	// container through this shim's ordinary Task service, which Create()
	// already recognizes and passes through untouched (see isSandboxBundle).
	// The provisioner's [plugins…runtimes.brewlet] block sets no `sandboxer`,
	// so no shim-side sandbox service is required.

	// Register the CRI runtime-options proto type ("runtimeoptions.v1.Options")
	// with the global proto/typeurl registry. containerd's CRI plugin hands the
	// shim its RuntimeClass options wrapped in a typeurl.Any of this type; the
	// runc task service unmarshals it during Create(). Without this blank import
	// the shim aborts sandbox creation with
	// `type with url runtimeoptions.v1.Options: not found`, so no pod using
	// runtimeClassName: brewlet can ever start under kubelet/CRI. Upstream's
	// containerd-shim-runc-v2 pulls this in transitively via its task/plugin
	// import, which we intentionally omit (see above), so we register it here.
	_ "github.com/containerd/containerd/api/types/runtimeoptions/v1"
)

// runtimeName matches the containerd runtime the provisioner wires into
// /etc/containerd/config.toml (https://github.com/microsoft/brewlet/tree/main/specs §5.2) and the RuntimeClass
// handler (§7).
const runtimeName = "io.containerd.brewlet.v2"

func main() {
	// The e2e harness drives the portable bundle-assembly core directly; keep
	// that subcommand available on Linux too so `make e2e-linux` works without
	// a full containerd.
	if len(os.Args) >= 2 && os.Args[1] == "prepare-bundle" {
		if err := prepareBundle(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "shim:", err)
			os.Exit(1)
		}
		return
	}

	shim.Run(context.Background(), manager.NewShimManager(runtimeName))
}
