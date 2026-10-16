//go:build linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/cmd/containerd-shim-runc-v2/manager"
	"github.com/containerd/containerd/v2/pkg/shim"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--brewlet-evidence" {
		if err := evidenceMain(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	shim.Run(context.Background(), manager.NewShimManager("io.containerd.brewlet.v2"))
}
