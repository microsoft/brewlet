// Package artifact implements the language-neutral Brewlet wire contract.
package artifact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"

	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type Error struct{ Reason, Detail string }

func (e *Error) Error() string { return e.Reason + ": " + e.Detail }
func fail(reason, format string, args ...any) error {
	return &Error{reason, fmt.Sprintf(format, args...)}
}
func Reason(err error) string {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e.Reason
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return ""
}

type Env struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Process is the application's downstream launch request. Like CMD in a
// Dockerfile built FROM the runtime image, Command and Args are appended to the
// runtime image ENTRYPOINT. Nil means omitted (inherit the runtime image CMD);
// a non-nil empty slice is an explicit, empty value.
type Process struct {
	Command    *[]string `json:"command,omitempty"`
	Args       *[]string `json:"args,omitempty"`
	WorkingDir string    `json:"workingDir,omitempty"`
	Env        []Env     `json:"env,omitempty"`
}

// RuntimeEntrypointPlaceholder is mirrored as the application image ENTRYPOINT.
// Brewlet replaces it on the node with the approved runtime image ENTRYPOINT; it
// is never executed.
const RuntimeEntrypointPlaceholder = "/.brewlet/runtime-entrypoint"

// CMD returns the application's downstream CMD (command followed by args) and
// whether the descriptor declares one. Only when both fields are omitted does
// the application inherit the runtime image CMD.
func (p Process) CMD() ([]string, bool) {
	if p.Command == nil && p.Args == nil {
		return nil, false
	}
	var cmd []string
	if p.Command != nil {
		cmd = append(cmd, (*p.Command)...)
	}
	if p.Args != nil {
		cmd = append(cmd, (*p.Args)...)
	}
	return cmd, true
}

// Argv composes the initial process argv with Dockerfile ENTRYPOINT/CMD
// semantics: runtime ENTRYPOINT + effective CMD. The effective CMD is
// override when nonempty (Pod command followed by args, or local run arguments),
// otherwise the application CMD when declared, otherwise the runtime image CMD.
func (p Process) Argv(runtimeEntrypoint, runtimeCmd, override []string) ([]string, error) {
	cmd := runtimeCmd
	if len(override) > 0 {
		cmd = override
	} else if app, declared := p.CMD(); declared {
		cmd = app
	}
	argv := append(slices.Clone(runtimeEntrypoint), cmd...)
	if len(argv) == 0 || argv[0] == "" {
		return nil, fail("LaunchCommandRequired", "runtime image ENTRYPOINT and the effective CMD are both empty; set process.command in launch.json or an ENTRYPOINT/CMD in the runtime image")
	}
	for _, s := range argv {
		if strings.ContainsRune(s, 0) {
			return nil, fail("InvalidLaunchConfig", "NUL in argv")
		}
	}
	return argv, nil
}

type Launch struct {
	SchemaVersion int     `json:"schemaVersion"`
	Process       Process `json:"process"`
}

// StrictJSON rejects unknown fields, duplicate object members, and trailing data.
func StrictJSON(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var visit func() error
	visit = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, err := d.Token()
					if err != nil {
						return err
					}
					s := k.(string)
					if seen[s] {
						return fmt.Errorf("duplicate JSON member %q", s)
					}
					seen[s] = true
					if err := visit(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err := visit(); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("unexpected delimiter")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := visit(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return err
	}
	return exactJSONKeys(generic, reflect.TypeOf(dst))
}

var idPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func ValidateRuntimeImageID(id string) error {
	if len(id) > 55 || !idPattern.MatchString(id) {
		return fail("InvalidRuntimeImageID", "%q", id)
	}
	return nil
}
func ParseLaunch(data []byte) (*Launch, error) {
	var l Launch
	if err := StrictJSON(data, &l); err != nil {
		return nil, fail("InvalidLaunchConfig", "%v", err)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(data, &raw)
	if _, ok := raw["process"]; !ok {
		return nil, fail("InvalidLaunchConfig", "process is required")
	}
	var process map[string]json.RawMessage
	_ = json.Unmarshal(raw["process"], &process)
	if wd, ok := process["workingDir"]; ok && string(wd) == `""` {
		return nil, fail("InvalidLaunchConfig", "workingDir must be absolute")
	}
	var rejectNull func(any) bool
	rejectNull = func(v any) bool {
		switch x := v.(type) {
		case nil:
			return true
		case map[string]any:
			for _, value := range x {
				if rejectNull(value) {
					return true
				}
			}
		case []any:
			for _, value := range x {
				if rejectNull(value) {
					return true
				}
			}
		}
		return false
	}
	var generic any
	_ = json.Unmarshal(data, &generic)
	if rejectNull(generic) {
		return nil, fail("InvalidLaunchConfig", "null launch fields are not allowed")
	}
	var envs []map[string]json.RawMessage
	_ = json.Unmarshal(process["env"], &envs)
	for _, env := range envs {
		if _, ok := env["value"]; !ok {
			return nil, fail("InvalidLaunchConfig", "environment value is required")
		}
	}
	if l.SchemaVersion != 1 {
		return nil, fail("InvalidLaunchConfig", "schemaVersion 1 required")
	}
	if l.Process.Command != nil && len(*l.Process.Command) > 0 && (*l.Process.Command)[0] == "" {
		return nil, fail("InvalidLaunchConfig", "command executable must not be empty")
	}
	cmd, _ := l.Process.CMD()
	for _, s := range cmd {
		if strings.ContainsRune(s, 0) {
			return nil, fail("InvalidLaunchConfig", "NUL in argv")
		}
	}
	if l.Process.WorkingDir == "" {
		l.Process.WorkingDir = "/app"
	}
	if !path.IsAbs(l.Process.WorkingDir) || strings.ContainsRune(l.Process.WorkingDir, 0) {
		return nil, fail("InvalidLaunchConfig", "workingDir must be absolute")
	}
	seen := map[string]bool{}
	for _, e := range l.Process.Env {
		if e.Name == "" || e.Name == "PATH" || strings.ContainsAny(e.Name, "=\x00") || strings.ContainsRune(e.Value, 0) || seen[e.Name] {
			return nil, fail("InvalidLaunchConfig", "invalid or duplicate env %q", e.Name)
		}
		seen[e.Name] = true
	}
	return &l, nil
}
func (l *Launch) ImageConfig() v1.ImageConfig {
	env := make([]string, 0, len(l.Process.Env))
	for _, e := range l.Process.Env {
		env = append(env, e.Name+"="+e.Value)
	}
	wd := l.Process.WorkingDir
	if wd == "" {
		wd = "/app"
	}
	cmd, _ := l.Process.CMD()
	return v1.ImageConfig{Entrypoint: []string{RuntimeEntrypointPlaceholder}, Cmd: cmd, WorkingDir: wd, Env: env, Labels: map[string]string{"sh.brewlet.app": "1"}}
}
func ValidateMirror(l *Launch, image v1.Image) error {
	want, got := l.ImageConfig(), image.Config
	if !slices.Equal(want.Entrypoint, got.Entrypoint) || !slices.Equal(want.Cmd, got.Cmd) || !slices.Equal(want.Env, got.Env) || want.WorkingDir != got.WorkingDir || got.User != "" || len(got.ExposedPorts) != 0 || len(got.Volumes) != 0 || got.StopSignal != "" || got.ArgsEscaped {
		return fail("LaunchConfigMismatch", "image config differs from launch.json")
	}
	return nil
}

// ParseImage validates image execution fields which OCI's Image type intentionally
// does not model (for example Docker Healthcheck) before typed decoding drops them.
func ParseImage(data []byte) (v1.Image, error) {
	var image v1.Image
	var raw map[string]json.RawMessage
	if err := StrictJSON(data, &raw); err != nil {
		return image, fail("LaunchConfigMismatch", "%v", err)
	}
	var config map[string]json.RawMessage
	for key, value := range raw {
		if strings.EqualFold(key, "config") {
			if err := json.Unmarshal(value, &config); err != nil {
				return image, fail("LaunchConfigMismatch", "invalid config")
			}
		}
	}
	for k, x := range config {
		if strings.EqualFold(k, "Healthcheck") || strings.EqualFold(k, "Shell") || strings.EqualFold(k, "OnBuild") {
			if string(x) != "null" && string(x) != "{}" && string(x) != "[]" && string(x) != `""` {
				return image, fail("LaunchConfigMismatch", "unsupported config %s", k)
			}
		}
	}
	if err := json.Unmarshal(data, &image); err != nil {
		return image, fail("LaunchConfigMismatch", "%v", err)
	}
	return image, nil
}
func ParsePlatform(s string) (v1.Platform, error) {
	p := strings.Split(s, "/")
	valid := regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
	if len(p) < 2 || len(p) > 3 {
		return v1.Platform{}, fail("InvalidPlatform", "expected os/arch[/variant]")
	}
	for _, x := range p {
		if !valid.MatchString(x) {
			return v1.Platform{}, fail("InvalidPlatform", "invalid component %q", x)
		}
	}
	v := v1.Platform{OS: p[0], Architecture: p[1]}
	if len(p) == 3 {
		v.Variant = p[2]
	}
	return v, nil
}
