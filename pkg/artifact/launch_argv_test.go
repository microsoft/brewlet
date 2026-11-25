package artifact

import (
	"slices"
	"testing"
)

func TestArgvComposesLikeDockerfileEntrypointAndCmd(t *testing.T) {
	entrypoint := []string{"/opt/runtime/bin/launcher", "--secure"}
	runtimeCmd := []string{"default", "two words"}
	for _, tc := range []struct {
		name, launch    string
		entrypoint, cmd []string
		override, want  []string
		reason          string
	}{
		{name: "application command appended to runtime entrypoint", launch: `{"command":["java"],"args":["-jar","/app/app.jar"]}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure", "java", "-jar", "/app/app.jar"}},
		{name: "omitted application CMD inherits runtime CMD", launch: `{}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure", "default", "two words"}},
		{name: "args-only application CMD replaces runtime CMD", launch: `{"args":["--port",""]}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure", "--port", ""}},
		{name: "explicitly empty application CMD clears runtime CMD", launch: `{"command":[],"args":[]}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure"}},
		{name: "Kubernetes args replace the whole CMD", launch: `{"command":["java"],"args":["-jar","/app/app.jar"]}`, entrypoint: entrypoint, cmd: runtimeCmd, override: []string{"--debug"}, want: []string{"/opt/runtime/bin/launcher", "--secure", "--debug"}},
		{name: "no runtime entrypoint executes application command", launch: `{"command":["/app/server"],"args":["x"]}`, cmd: runtimeCmd, want: []string{"/app/server", "x"}},
		{name: "no runtime entrypoint and inherited runtime CMD", launch: `{}`, cmd: []string{"/usr/bin/server"}, want: []string{"/usr/bin/server"}},
		{name: "no runtime entrypoint and Kubernetes args", launch: `{"command":["/app/server"]}`, override: []string{"/bin/sh", "-c", "echo"}, want: []string{"/bin/sh", "-c", "echo"}},
		{name: "nothing to execute", launch: `{}`, reason: "LaunchCommandRequired"},
		{name: "cleared CMD without runtime entrypoint", launch: `{"command":[]}`, cmd: runtimeCmd, reason: "LaunchCommandRequired"},
		{name: "empty inherited executable", launch: `{}`, cmd: []string{""}, reason: "LaunchCommandRequired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := ParseLaunch([]byte(`{"schemaVersion":1,"process":` + tc.launch + `}`))
			if err != nil {
				t.Fatal(err)
			}
			got, err := l.Process.Argv(tc.entrypoint, tc.cmd, tc.override)
			if Reason(err) != tc.reason || (tc.reason == "" && !slices.Equal(got, tc.want)) {
				t.Fatalf("argv=%q err=%v want %q reason %q", got, err, tc.want, tc.reason)
			}
		})
	}
}
