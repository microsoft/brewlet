//go:build linux

package provisioner

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func hostCRIStatus(ctx context.Context, hostRoot string, verbose bool) (*runtimeapi.StatusResponse, error) {
	socket := filepath.Join(hostRoot, "run/containerd/containerd.sock")
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	conn, err := grpc.NewClient("passthrough:///containerd", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}))
	if err != nil {
		return nil, fmt.Errorf("host CRI at %s: %w", socket, err)
	}
	defer conn.Close()
	response, err := runtimeapi.NewRuntimeServiceClient(conn).Status(callCtx, &runtimeapi.StatusRequest{Verbose: verbose})
	if err != nil {
		return nil, fmt.Errorf("host CRI status at %s: %w", socket, err)
	}
	return response, nil
}
