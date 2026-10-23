package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"testing"
)

// Rules the published schema documents as enforced only by the shared parser.
var parserOnlyLaunchCases = map[string]string{
	"duplicate-json-member": "JSON Schema operates on decoded values and cannot see duplicate members",
	"duplicate-env":         "env name uniqueness is enforced by the shared parser",
}

// TestLaunchSchemaMatchesConformance binds conformance/launch.schema.json to the
// shared fixtures so the published schema cannot drift from ParseLaunch (§6.3, §11.1).
func TestLaunchSchemaMatchesConformance(t *testing.T) {
	var schema map[string]any
	readJSON(t, "../../conformance/launch.schema.json", &schema)
	var fixtures struct {
		Cases []Fixture `json:"cases"`
	}
	readJSON(t, "../../conformance/artifact-v1.json", &fixtures)
	checked, exceptions := 0, 0
	for _, f := range fixtures.Cases {
		if f.Kind != "launch" && f.Kind != "compatibility" {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			var doc any
			if err := json.Unmarshal(f.Launch, &doc); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			schemaErr := validateSchema(schema, doc, "$")
			_, parseErr := ParseLaunch(f.Launch)
			if (schemaErr == nil) != (parseErr == nil) {
				if why, ok := parserOnlyLaunchCases[f.Name]; ok && schemaErr == nil {
					exceptions++
					t.Logf("parser-only rule: %s", why)
					return
				}
				t.Fatalf("schema/parser disagreement: schema=%v parser=%v", schemaErr, parseErr)
			}
			if _, ok := parserOnlyLaunchCases[f.Name]; ok {
				t.Fatalf("listed parser-only case is now enforced by the schema; remove the exception")
			}
			checked++
		})
	}
	if checked == 0 || exceptions != len(parserOnlyLaunchCases) {
		t.Fatalf("checked %d cases with %d/%d expected parser-only exceptions", checked, exceptions, len(parserOnlyLaunchCases))
	}
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

var jsonUnicodeEscape = regexp.MustCompile(`\\u([0-9a-fA-F]{4})`)

// validateSchema implements exactly the JSON Schema 2020-12 keywords used by
// launch.schema.json and fails on any other keyword so coverage cannot silently shrink.
func validateSchema(s map[string]any, v any, at string) error {
	for k, kw := range s {
		var err error
		switch k {
		case "$schema", "$id", "title", "description":
		case "type":
			if !hasType(v, kw.(string)) {
				err = fmt.Errorf("not %s", kw)
			}
		case "const":
			if !reflect.DeepEqual(v, kw) {
				err = fmt.Errorf("must equal %v", kw)
			}
		case "not":
			if validateSchema(kw.(map[string]any), v, at) == nil {
				err = fmt.Errorf("matches forbidden schema")
			}
		case "required":
			if o, ok := v.(map[string]any); ok {
				for _, name := range kw.([]any) {
					if _, ok := o[name.(string)]; !ok {
						err = fmt.Errorf("missing %s", name)
					}
				}
			}
		case "properties":
			if o, ok := v.(map[string]any); ok {
				for name, sub := range kw.(map[string]any) {
					if pv, ok := o[name]; ok {
						if e := validateSchema(sub.(map[string]any), pv, at+"."+name); e != nil {
							return e
						}
					}
				}
			}
		case "additionalProperties":
			if o, ok := v.(map[string]any); ok && kw == false {
				props, _ := s["properties"].(map[string]any)
				for name := range o {
					if _, ok := props[name]; !ok {
						err = fmt.Errorf("unexpected property %s", name)
					}
				}
			}
		case "minItems":
			if a, ok := v.([]any); ok && float64(len(a)) < kw.(float64) {
				err = fmt.Errorf("fewer than %v items", kw)
			}
		case "uniqueItems":
			if a, ok := v.([]any); ok && kw == true {
				for i := range a {
					for j := i + 1; j < len(a); j++ {
						if reflect.DeepEqual(a[i], a[j]) {
							err = fmt.Errorf("duplicate items")
						}
					}
				}
			}
		case "prefixItems", "items":
			if a, ok := v.([]any); ok {
				prefix, _ := s["prefixItems"].([]any)
				for i, item := range a {
					var sub map[string]any
					if i < len(prefix) {
						if k == "items" {
							continue
						}
						sub = prefix[i].(map[string]any)
					} else if k == "items" {
						sub = kw.(map[string]any)
					} else {
						continue
					}
					if e := validateSchema(sub, item, fmt.Sprintf("%s[%d]", at, i)); e != nil {
						return e
					}
				}
			}
		case "minLength", "maxLength":
			if str, ok := v.(string); ok {
				n := float64(len([]rune(str)))
				if (k == "minLength" && n < kw.(float64)) || (k == "maxLength" && n > kw.(float64)) {
					err = fmt.Errorf("%s %v violated", k, kw)
				}
			}
		case "pattern":
			if str, ok := v.(string); ok {
				re := regexp.MustCompile(jsonUnicodeEscape.ReplaceAllString(kw.(string), `\x{$1}`))
				if !re.MatchString(str) {
					err = fmt.Errorf("does not match %s", kw)
				}
			}
		default:
			panic(fmt.Sprintf("launch schema uses unsupported keyword %q at %s; extend validateSchema", k, at))
		}
		if err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
	}
	return nil
}

func hasType(v any, typ string) bool {
	switch typ {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "integer":
		n, ok := v.(float64)
		return ok && n == float64(int64(n))
	}
	panic("launch schema uses unsupported type " + typ)
}
