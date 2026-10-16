package node

import (
	"strings"
	"testing"
)

func TestRuntimeHOMEIsSynthesizedForActualUID(t *testing.T) {
	m := testMetadata()
	m.UID = 65532
	m.GID = 65532
	m.Env = append(m.Env, "HOME=/home/app")
	m.Homes = map[uint32]string{65532: "/home/app"}
	for _, test := range []struct {
		name           string
		uid            uint32
		explicit, want string
	}{
		{"unknown custom UID", 2000, "", "/tmp"},
		{"default app UID", 65532, "", "/home/app"},
		{"explicit CRI HOME", 2000, "/custom/home", "/custom/home"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := testProcess()
			p.User.UID = test.uid
			p.User.GID = 65532
			if test.explicit != "" {
				p.Env = append(p.Env, "HOME="+test.explicit)
			}
			if err := MergeProcess(&p, m); err != nil {
				t.Fatal(err)
			}
			home := ""
			count := 0
			for _, env := range p.Env {
				if strings.HasPrefix(env, "HOME=") {
					home = strings.TrimPrefix(env, "HOME=")
					count++
				}
			}
			if home != test.want || count != 1 {
				t.Fatal(p.Env)
			}
			exec := testProcess()
			exec.User = p.User
			if err := RewriteExec(&exec, InitialState{Process: p}); err != nil {
				t.Fatal(err)
			}
			for _, env := range exec.Env {
				if strings.HasPrefix(env, "HOME=") && env != "HOME="+test.want {
					t.Fatal("exec HOME differs", exec.Env)
				}
			}
		})
	}
}
