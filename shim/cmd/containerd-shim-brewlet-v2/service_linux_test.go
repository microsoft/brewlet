//go:build linux

package main

import (
	"context"
	"reflect"
	"testing"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/typeurl/v2"
	"github.com/microsoft/brewlet/pkg/node"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
)

type execRecorder struct {
	taskapi.TTRPCTaskService
	process *specs.Process
	called  bool
}

func (r *execRecorder) Exec(_ context.Context, request *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	r.called = true
	value, err := typeurl.UnmarshalAny(request.Spec)
	if err != nil {
		return nil, err
	}
	r.process = value.(*specs.Process)
	return &emptypb.Empty{}, nil
}
func processAny(t *testing.T, p *specs.Process) *anypb.Any {
	t.Helper()
	a, err := typeurl.MarshalAny(p)
	if err != nil {
		t.Fatal(err)
	}
	return &anypb.Any{TypeUrl: a.GetTypeUrl(), Value: a.GetValue()}
}
func TestExecRPCActuallyRewritesBeforeRunc(t *testing.T) {
	initial := specs.Process{Args: []string{"server"}, Cwd: "/app", Env: []string{"PATH=/runtime/bin", "HOME=/home/app", "BASE=yes"}, User: specs.User{UID: 1000, GID: 1000}, NoNewPrivileges: true}
	state := node.InitialState{Lease: node.Lease{Namespace: "k8s.io"}, Process: initial}
	recorder := new(execRecorder)
	s := &service{TTRPCTaskService: recorder, loadInitial: func(string) (node.InitialState, error) { return state, nil }}
	probe := specs.Process{Args: []string{"probe", "two words"}, Cwd: "/work", Terminal: true, Env: []string{"PATH=/wrong"}, User: initial.User}
	ctx := namespaces.WithNamespace(context.Background(), "k8s.io")
	if _, err := s.Exec(ctx, &taskapi.ExecProcessRequest{ID: "task", ExecID: "probe", Spec: processAny(t, &probe)}); err != nil {
		t.Fatal(err)
	}
	got := recorder.process
	if !recorder.called || !reflect.DeepEqual(got.Env, initial.Env) || !reflect.DeepEqual(got.Args, probe.Args) || got.Cwd != "/work" || !got.Terminal || !got.NoNewPrivileges {
		t.Fatalf("RPC rewrite missing: %+v", got)
	}
}
func TestExecRPCMissingStateNeverDelegates(t *testing.T) {
	recorder := new(execRecorder)
	s := &service{TTRPCTaskService: recorder, loadInitial: func(string) (node.InitialState, error) {
		return node.InitialState{}, &node.Error{Reason: "ExecStateUnavailable"}
	}}
	if _, err := s.Exec(context.Background(), &taskapi.ExecProcessRequest{ID: "missing"}); err == nil || recorder.called {
		t.Fatal("missing state delegated to runc")
	}
}
