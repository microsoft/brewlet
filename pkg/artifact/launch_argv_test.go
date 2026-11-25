package artifact

import (
	"slices"
	"testing"
)

func TestArgvComposesLikeDockerfileEntrypointAndCmd(t *testing.T) {
	entrypoint := []string{"/opt/runtime/bin/launcher", "--secure"}
	runtimeCmd := []string{"default", "two words"}
	for _, tc := range []struct {
		name, launch              string
		entrypoint, cmd           []string
		podCommand, podArgs, want []string
		reason                    string
	}{
		{name: "application command appended to runtime entrypoint", launch: `{"command":["java"],"args":["-jar","/app/app.jar"]}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure", "java", "-jar", "/app/app.jar"}},
		{name: "omitted application CMD inherits runtime CMD", launch: `{}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure", "default", "two words"}},
		{name: "args-only application CMD replaces runtime CMD", launch: `{"args":["--port",""]}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure", "--port", ""}},
		{name: "application command without args does not inherit runtime CMD", launch: `{"command":["java"]}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure", "java"}},
		{name: "explicitly empty application CMD clears runtime CMD", launch: `{"command":[],"args":[]}`, entrypoint: entrypoint, cmd: runtimeCmd, want: []string{"/opt/runtime/bin/launcher", "--secure"}},
		{name: "Pod command replaces application command and drops omitted args", launch: `{"command":["java"],"args":["-jar","/app/app.jar"]}`, entrypoint: entrypoint, cmd: runtimeCmd, podCommand: []string{"python"}, want: []string{"/opt/runtime/bin/launcher", "--secure", "python"}},
		{name: "Pod command and args replace application fields", launch: `{"command":["java"],"args":["-jar","/app/app.jar"]}`, entrypoint: entrypoint, cmd: runtimeCmd, podCommand: []string{"python"}, podArgs: []string{"app.py"}, want: []string{"/opt/runtime/bin/launcher", "--secure", "python", "app.py"}},
		{name: "Pod args replace launch args and preserve launch command", launch: `{"command":["java"],"args":["-jar","/app/app.jar"]}`, entrypoint: entrypoint, cmd: runtimeCmd, podArgs: []string{"--debug"}, want: []string{"/opt/runtime/bin/launcher", "--secure", "java", "--debug"}},
		{name: "Pod args replace runtime CMD when launch fields are omitted", launch: `{}`, entrypoint: entrypoint, cmd: runtimeCmd, podArgs: []string{"--debug"}, want: []string{"/opt/runtime/bin/launcher", "--secure", "--debug"}},
		{name: "no runtime entrypoint executes application command", launch: `{"command":["/app/server"],"args":["x"]}`, cmd: runtimeCmd, want: []string{"/app/server", "x"}},
		{name: "no runtime entrypoint and inherited runtime CMD", launch: `{}`, cmd: []string{"/usr/bin/server"}, want: []string{"/usr/bin/server"}},
		{name: "no runtime entrypoint and Pod command", launch: `{"command":["/app/server"]}`, podCommand: []string{"/bin/sh"}, podArgs: []string{"-c", "echo"}, want: []string{"/bin/sh", "-c", "echo"}},
		{name: "no runtime entrypoint and Pod args", launch: `{"command":["/app/server"],"args":["old"]}`, podArgs: []string{"new"}, want: []string{"/app/server", "new"}},
		{name: "nothing to execute", launch: `{}`, reason: "LaunchCommandRequired"},
		{name: "cleared CMD without runtime entrypoint", launch: `{"command":[]}`, cmd: runtimeCmd, reason: "LaunchCommandRequired"},
		{name: "empty inherited executable", launch: `{}`, cmd: []string{""}, reason: "LaunchCommandRequired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := ParseLaunch([]byte(`{"schemaVersion":1,"process":` + tc.launch + `}`))
			if err != nil {
				t.Fatal(err)
			}
			got, err := l.Process.Argv(tc.entrypoint, tc.cmd, tc.podCommand, tc.podArgs)
			if Reason(err) != tc.reason || (tc.reason == "" && !slices.Equal(got, tc.want)) {
				t.Fatalf("argv=%q err=%v want %q reason %q", got, err, tc.want, tc.reason)
			}
		})
	}
}
