//go:build linux

package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/containerd/containerd/v2/client"
	"github.com/microsoft/brewlet/pkg/node"
)

func evidenceMain() error {
	var request node.EvidenceRequest
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return err
	}
	c, err := client.New("/run/containerd/containerd.sock")
	if err != nil {
		return err
	}
	defer c.Close()
	evidence := node.LinuxEvidence{Client: c, RuntimeRoot: inventoryRoot}
	var response node.EvidenceResponse
	if request.Path != "" {
		response.Gone, err = evidence.MountsGone(context.Background(), request.Path)
	} else if request.AllRuntime {
		response.Gone, err = evidence.RuntimeGone(context.Background())
	} else {
		response.Gone, err = evidence.Gone(context.Background(), request.Lease)
	}
	if err != nil {
		response.Error = err.Error()
	}
	return json.NewEncoder(os.Stdout).Encode(response)
}
