package artifact

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// encoding/json accepts case-insensitive struct keys. The wire schema does not.
func exactJSONKeys(value any, typ reflect.Type) error {
	if typ == nil {
		return nil
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	unmarshaler := reflect.TypeFor[json.Unmarshaler]()
	if typ.Implements(unmarshaler) || reflect.PointerTo(typ).Implements(unmarshaler) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		fields := map[string]reflect.Type{}
		var collect func(reflect.Type)
		collect = func(t reflect.Type) {
			for i := 0; i < t.NumField(); i++ {
				field := t.Field(i)
				if field.PkgPath != "" {
					continue
				}
				tag := strings.Split(field.Tag.Get("json"), ",")[0]
				if tag == "-" {
					continue
				}
				if field.Anonymous && tag == "" {
					nested := field.Type
					for nested.Kind() == reflect.Pointer {
						nested = nested.Elem()
					}
					if nested.Kind() == reflect.Struct {
						collect(nested)
						continue
					}
				}
				if tag == "" {
					tag = field.Name
				}
				fields[tag] = field.Type
			}
		}
		collect(typ)
		for name, v := range object {
			field, ok := fields[name]
			if !ok {
				return fmt.Errorf("unknown JSON field %q", name)
			}
			if e := exactJSONKeys(v, field); e != nil {
				return e
			}
		}
	case reflect.Slice, reflect.Array:
		if items, ok := value.([]any); ok {
			for _, v := range items {
				if e := exactJSONKeys(v, typ.Elem()); e != nil {
					return e
				}
			}
		}
	case reflect.Map:
		if object, ok := value.(map[string]any); ok {
			for _, v := range object {
				if e := exactJSONKeys(v, typ.Elem()); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
