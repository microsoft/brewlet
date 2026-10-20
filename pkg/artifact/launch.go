// Package artifact implements the language-neutral Brewlet 2.0 wire contract.
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
type Process struct {
	Entrypoint []string `json:"entrypoint"`
	Args       []string `json:"args,omitempty"`
	WorkingDir string   `json:"workingDir,omitempty"`
	Env        []Env    `json:"env,omitempty"`
}
type Launch struct {
	SchemaVersion int      `json:"schemaVersion"`
	Process       Process  `json:"process"`
	RuntimeImages []string `json:"runtimeImages,omitempty"`
}

// Preserve an explicit deny-all [] rather than dropping it through omitempty.
func (l Launch) MarshalJSON() ([]byte, error) {
	var images *[]string
	if l.RuntimeImages != nil {
		images = &l.RuntimeImages
	}
	return json.Marshal(struct {
		SchemaVersion int       `json:"schemaVersion"`
		Process       Process   `json:"process"`
		RuntimeImages *[]string `json:"runtimeImages,omitempty"`
	}{l.SchemaVersion, l.Process, images})
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
	if l.SchemaVersion != 2 || len(l.Process.Entrypoint) == 0 || l.Process.Entrypoint[0] == "" {
		return nil, fail("InvalidLaunchConfig", "schemaVersion 2 and nonempty entrypoint required")
	}
	for _, s := range append(slices.Clone(l.Process.Entrypoint), l.Process.Args...) {
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
	seen = map[string]bool{}
	for _, id := range l.RuntimeImages {
		if err := ValidateRuntimeImageID(id); err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, fail("InvalidLaunchConfig", "duplicate runtime ID")
		}
		seen[id] = true
	}
	return &l, nil
}
func (l *Launch) Allows(id string) bool {
	return l.RuntimeImages == nil || slices.Contains(l.RuntimeImages, id)
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
	return v1.ImageConfig{Entrypoint: l.Process.Entrypoint, Cmd: l.Process.Args, WorkingDir: wd, Env: env, Labels: map[string]string{"sh.brewlet.app": "2"}}
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
