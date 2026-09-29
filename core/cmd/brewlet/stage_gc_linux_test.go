// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestStageGCCancelsOnSIGTERM(t *testing.T) {
	if os.Getenv("BREWLET_TEST_GC_CHILD") == "true" {
		err := cmdStageGC([]string{
			"--stage-root", os.Getenv("BREWLET_TEST_GC_ROOT"),
			"--address", os.Getenv("BREWLET_TEST_GC_SOCKET"),
		})
		if status.Code(err) != codes.Canceled {
			t.Fatalf("expected canceled metadata inspection, got %v", err)
		}
		return
	}

	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "immutable-v2"), 0o755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "containerd.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connected := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		connected <- err
		if err == nil {
			defer conn.Close()
			// Leave the gRPC handshake pending until the child is signaled.
			<-ctx.Done()
		}
	}()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStageGCCancelsOnSIGTERM$")
	cmd.Env = append(os.Environ(), "BREWLET_TEST_GC_CHILD=true",
		"BREWLET_TEST_GC_ROOT="+root, "BREWLET_TEST_GC_SOCKET="+socket)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	defer func() { cancel(); <-done }()
	select {
	case err := <-connected:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("stage GC did not connect to containerd")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	<-done
	if waitErr != nil {
		t.Fatalf("stage GC did not cancel cleanly: %v\n%s", waitErr, output.String())
	}
}
