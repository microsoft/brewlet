package node

import (
	"reflect"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestAbsoluteInitialAndExecWithoutPATHIgnoreInjectedCRIDefault(t *testing.T) {
	m := testMetadata()
	m.Env, m.Homes = []string{"BASE=runtime"}, nil
	p := testProcess()
	p.Args = []string{"/app/server"}
	if err := MergeProcess(&p, m); err != nil {
		t.Fatal(err)
	}
	want := []string{"BASE=runtime", "X=cri", "HOME=/tmp"}
	if !reflect.DeepEqual(p.Env, want) {
		t.Fatal("CRI PATH was adopted for a pathless runtime", p.Env)
	}
	state := InitialState{Process: p}
	exec := testProcess()
	exec.Args = []string{"/app/health"}
	if err := RewriteExec(&exec, state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exec.Env, want) {
		t.Fatal("Exec did not retain the pathless initial environment", exec.Env)
	}
	for _, args := range [][]string{{"health"}, {"./health"}} {
		exec.Args = args
		if err := RewriteExec(&exec, state); reason(err) != "RuntimePATHRequired" {
			t.Fatal("relative Exec acquired fallback PATH", err)
		}
	}
}

func TestRelativeInitialWithoutRuntimePATHFailsDespiteCRIEnvironment(t *testing.T) {
	m := testMetadata()
	m.Env = nil
	for _, args := range [][]string{{"server"}, {"./server"}} {
		p := testProcess()
		p.Args = args
		if err := MergeProcess(&p, m); reason(err) != "RuntimePATHRequired" {
			t.Fatal("relative initial acquired CRI PATH", err)
		}
	}
}

func TestPathlessExecDoesNotWeakenProcessSecurity(t *testing.T) {
	state := InitialState{Process: specs.Process{Args: []string{"/app/server"}, Cwd: "/app",
		User: specs.User{UID: 1000, GID: 1000}, NoNewPrivileges: true}}
	p := testProcess()
	p.Args = []string{"/app/health"}
	p.User.UID = 0
	if err := RewriteExec(&p, state); reason(err) != "RootNotSupported" {
		t.Fatal(err)
	}
}
