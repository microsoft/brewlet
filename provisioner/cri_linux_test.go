//go:build linux

package provisioner

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const validCRIConfig = `{"containerd":{"runtimes":{"brewlet":{"runtimeType":"io.containerd.brewlet.v2","PodAnnotations":["runtime-image.brewlet.sh/*","runtime-generation.brewlet.sh/*"]}}}}`

type fakeCRIRuntime struct {
	runtimeapi.UnimplementedRuntimeServiceServer
	mu       sync.Mutex
	requests []bool
	info     map[string]string
	failAt   map[int]bool
	block    bool
}

func (f *fakeCRIRuntime) Status(ctx context.Context, request *runtimeapi.StatusRequest) (*runtimeapi.StatusResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, request.Verbose)
	fail := f.failAt[len(f.requests)]
	block := f.block
	info := f.info
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if fail {
		return nil, status.Error(codes.Unavailable, "injected CRI failure")
	}
	if !request.Verbose {
		info = nil
	}
	return &runtimeapi.StatusResponse{Info: info}, nil
}

func startFakeCRI(t *testing.T, root string) *fakeCRIRuntime {
	t.Helper()
	dir := filepath.Join(root, "run/containerd")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "containerd.sock"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeCRIRuntime{info: map[string]string{"config": validCRIConfig}, failAt: make(map[int]bool)}
	server := grpc.NewServer()
	runtimeapi.RegisterRuntimeServiceServer(server, fake)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return fake
}

func shortCRIRoot(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("", "brewlet-cri-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	return root
}

func TestHostCRIStatusUsesUnixSocketAndHonorsFailures(t *testing.T) {
	root := shortCRIRoot(t)
	fake := startFakeCRI(t, root)
	response, err := hostCRIStatus(context.Background(), root, true)
	if err != nil || response.Info["config"] != validCRIConfig {
		t.Fatalf("verbose CRI status: %v, %v", response, err)
	}
	fake.mu.Lock()
	fake.failAt[2] = true
	fake.mu.Unlock()
	if _, err := hostCRIStatus(context.Background(), root, false); err == nil || !strings.Contains(err.Error(), "injected CRI failure") {
		t.Fatalf("CRI RPC failure not surfaced: %v", err)
	}
	fake.mu.Lock()
	fake.block = true
	fake.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := hostCRIStatus(ctx, root, true); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("CRI deadline not honored: %v", err)
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := hostCRIStatus(cancelled, root, false); status.Code(err) != codes.Canceled {
		t.Fatalf("CRI cancellation not honored: %v", err)
	}
	if _, err := hostCRIStatus(context.Background(), shortCRIRoot(t), false); err == nil || !strings.Contains(err.Error(), "containerd.sock") {
		t.Fatalf("missing socket not reported: %v", err)
	}
}

func activationWithCRI(t *testing.T) (HostActivation, []byte, *fakeCRIRuntime) {
	t.Helper()
	a, original, _ := activationFixture(t)
	a.runCRI = nil
	a.validateBaseline = nil
	a.runHost = func(_ context.Context, args ...string) error {
		if args[0] == "crictl" {
			return errors.New("crictl unavailable on host PATH")
		}
		return nil
	}
	bin := filepath.Join("testdata", "cri-bin")
	if _, err := os.Stat(filepath.Join(bin, "nsenter")); err != nil {
		bin = filepath.Join("/src/provisioner", bin)
	}
	bin, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("BREWLET_TEST_CONFIG", a.config())
	return a, original, startFakeCRI(t, a.HostRoot)
}

func TestActivationAndRestorationWithoutHostCrictl(t *testing.T) {
	a, original, fake := activationWithCRI(t)
	ctx := context.Background()
	fence := func(context.Context) error { return nil }
	if err := a.Ensure(ctx, "validated", fence); err != nil {
		t.Fatal(err)
	}
	if err := a.Restore(ctx, fence); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(a.config())
	if err != nil || string(config) != string(original) {
		t.Fatalf("original config not restored: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 3 || fake.requests[0] || !fake.requests[1] || !fake.requests[2] {
		t.Fatalf("unexpected preflight, baseline, and restoration requests: %v", fake.requests)
	}
}

func TestActivationCRIFailuresPreserveTransaction(t *testing.T) {
	t.Run("preflight", func(t *testing.T) {
		a, original, fake := activationWithCRI(t)
		fake.mu.Lock()
		fake.failAt[1] = true
		fake.mu.Unlock()
		err := a.Ensure(context.Background(), "validated", func(context.Context) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "injected CRI failure") {
			t.Fatalf("preflight failure ignored: %v", err)
		}
		config, readErr := os.ReadFile(a.config())
		if readErr != nil || string(config) != string(original) {
			t.Fatalf("preflight changed host config: %v", readErr)
		}
		if _, err := os.Lstat(a.journal()); !os.IsNotExist(err) {
			t.Fatalf("preflight created journal: %v", err)
		}
	})
	t.Run("invalid live config rolls back", func(t *testing.T) {
		a, original, fake := activationWithCRI(t)
		fake.mu.Lock()
		fake.info = map[string]string{"config": `{}`}
		fake.mu.Unlock()
		err := a.Ensure(context.Background(), "validated", func(context.Context) error { return nil })
		if err == nil || !strings.Contains(err.Error(), "rolled back") {
			t.Fatalf("invalid live config accepted: %v", err)
		}
		config, readErr := os.ReadFile(a.config())
		if readErr != nil || string(config) != string(original) {
			t.Fatalf("rollback failed to restore config: %v", readErr)
		}
	})
	t.Run("restore retries failed CRI check", func(t *testing.T) {
		a, original, fake := activationWithCRI(t)
		ctx := context.Background()
		fence := func(context.Context) error { return nil }
		if err := a.Ensure(ctx, "validated", fence); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		fake.failAt[3] = true
		fake.mu.Unlock()
		if err := a.Restore(ctx, fence); err == nil || !strings.Contains(err.Error(), "injected CRI failure") {
			t.Fatalf("restore ignored failed CRI check: %v", err)
		}
		if _, err := os.Stat(a.journal()); err != nil {
			t.Fatalf("restore discarded retry journal: %v", err)
		}
		if err := a.Restore(ctx, fence); err != nil {
			t.Fatalf("restore retry: %v", err)
		}
		config, err := os.ReadFile(a.config())
		if err != nil || string(config) != string(original) {
			t.Fatalf("retry failed to restore config: %v", err)
		}
	})
}
