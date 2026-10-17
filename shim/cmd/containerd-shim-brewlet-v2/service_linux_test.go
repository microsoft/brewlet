//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"reflect"
	"strings"
	"testing"

	taskapi "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/typeurl/v2"
	"github.com/microsoft/brewlet/pkg/node"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
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

type startRecorder struct {
	taskapi.TTRPCTaskService
	err error
}

func (r *startRecorder) Start(context.Context, *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	if r.err != nil {
		return nil, r.err
	}
	return &taskapi.StartResponse{Pid: 42}, nil
}

type eventRecorder struct {
	topic string
	event *structpb.Struct
	err   error
}

func (r *eventRecorder) Publish(_ context.Context, topic string, event events.Event) error {
	r.topic, r.event = topic, event.(*structpb.Struct)
	return r.err
}
func (*eventRecorder) Close() error { return nil }

func TestStartPublishesExactRuntimeIdentityOnlyForSuccessfulInitialProcess(t *testing.T) {
	for _, tc := range []struct {
		name    string
		execID  string
		failure error
		publish bool
	}{
		{name: "initial", publish: true},
		{name: "exec", execID: "probe"},
		{name: "failed", failure: errors.New("runc start failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := new(eventRecorder)
			state := node.InitialState{Lease: node.Lease{Metadata: node.Metadata{
				ID: "runtime", InventoryUID: "epoch", Generation: 9007199254740993, SourceDigest: "sha256:source",
			}}}
			loads := 0
			s := &service{
				TTRPCTaskService: &startRecorder{err: tc.failure}, publisher: recorder,
				loadInitial: func(id string) (node.InitialState, error) {
					if id != "task" {
						t.Fatalf("wrong state lookup: %s", id)
					}
					loads++
					return state, nil
				},
			}
			response, err := s.Start(context.Background(), &taskapi.StartRequest{ID: "task", ExecID: tc.execID})
			if !errors.Is(err, tc.failure) || (err == nil && response.Pid != 42) {
				t.Fatalf("changed actual start result: %v, %v", response, err)
			}
			if !tc.publish {
				if recorder.event != nil || loads != 0 {
					t.Fatal("failed or exec start emitted a runtime resolution")
				}
				return
			}
			want := map[string]any{"reason": "RuntimeImageResolved", "containerID": "task",
				"id": "runtime", "inventoryUID": "epoch", "generation": "9007199254740993", "digest": "sha256:source"}
			if loads != 1 || recorder.topic != "/brewlet/runtime-image" || recorder.event == nil ||
				!reflect.DeepEqual(recorder.event.AsMap(), want) {
				t.Fatalf("missing or lossy runtime resolution: %+v", recorder)
			}
		})
	}
}

func TestRuntimeEventFailuresAreReportedWithoutHidingActualStart(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	publisher := &eventRecorder{err: errors.New("event service unavailable")}
	s := &service{
		TTRPCTaskService: &startRecorder{}, publisher: publisher,
		loadInitial: func(string) (node.InitialState, error) { return node.InitialState{}, nil },
	}
	response, err := s.Start(context.Background(), &taskapi.StartRequest{ID: "task"})
	if err != nil || response.Pid != 42 || !strings.Contains(output.String(), "publish: event service unavailable") {
		t.Fatalf("event failure concealed or real start changed: %v, %v, %s", response, err, output.String())
	}
	output.Reset()
	s.runtimeEvent(context.Background(), "RuntimeImageInstalling", "task", node.Metadata{ID: "\xff"})
	if !strings.Contains(output.String(), "encode:") {
		t.Fatal("invalid event encoding failed silently")
	}
	output.Reset()
	s.publisher = nil
	s.runtimeEvent(context.Background(), "RuntimeImageInstalling", "task", node.Metadata{})
	if !strings.Contains(output.String(), "publisher unavailable") {
		t.Fatal("missing publisher failed silently")
	}
}
