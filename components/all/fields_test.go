package all_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
)

// This is the test the settings form exists on top of.
//
// The form is generated from each component's declared components.Field list, so the only
// way it can be wrong is by the declaration disagreeing with the struct the component
// actually unmarshals its YAML into. Nothing else in the tree can notice that: the old
// hand-written form exposed 18 keys of about a hundred and nobody found out from a test,
// they found out from an account whose extract_llm ran 251 times and acted zero times
// because the two keys that decided it were not on the page.
//
// Both directions matter. A struct key with no descriptor is a knob the form cannot reach.
// A descriptor with no struct key is worse now that per-component blocks decode STRICTLY:
// the form would write a key the component rejects, so saving would produce a document the
// proxy refuses to build.
func TestEveryComponentDeclaresExactlyItsConfigurableKeys(t *testing.T) {
	for _, name := range components.Names() {
		t.Run(name, func(t *testing.T) {
			proto, ok := components.ConfigProto(name)
			if !ok {
				t.Fatalf("%s registers a constructor but declares no fields, so the settings "+
					"page cannot configure it at all", name)
			}
			want := yamlKeys(reflect.TypeOf(proto), "")
			got := map[string]components.Field{}
			for _, fd := range components.Fields(name) {
				if _, dup := got[fd.Key]; dup {
					t.Errorf("%s declares %q twice", name, fd.Key)
				}
				got[fd.Key] = fd
			}
			for key, kind := range want {
				fd, ok := got[key]
				if !ok {
					t.Errorf("%s reads %q from its config but does not declare it, so the "+
						"settings page cannot set it", name, key)
					continue
				}
				if fd.Type == "" {
					t.Errorf("%s.%s declares no type", name, key)
				}
				if bad := typeMismatch(fd.Type, kind); bad != "" {
					t.Errorf("%s.%s is declared %s but the struct field is %s (%s)",
						name, key, fd.Type, kind, bad)
				}
				if fd.Type == components.FieldEnum && len(fd.Options) == 0 {
					t.Errorf("%s.%s is an enum with no options", name, key)
				}
				if fd.Type == components.FieldEnum && fd.Default != nil &&
					!optionsContain(fd.Options, fd.Default) {
					t.Errorf("%s.%s defaults to %v, which is not one of its options %v",
						name, key, fd.Default, fd.Options)
				}
			}
			for key := range got {
				if _, ok := want[key]; !ok {
					t.Errorf("%s declares %q, which no key of its config struct reads — "+
						"the form would write a key the component now REJECTS", name, key)
				}
			}
		})
	}
}

func optionsContain(xs []string, v any) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// typeMismatch reports why a declared field type cannot describe a Go kind.
func typeMismatch(declared string, kind reflect.Kind) string {
	switch declared {
	case components.FieldBool:
		if kind != reflect.Bool {
			return "want bool"
		}
	case components.FieldInt:
		if kind != reflect.Int && kind != reflect.Int64 {
			return "want an int"
		}
	case components.FieldFloat:
		if kind != reflect.Float64 && kind != reflect.Float32 {
			return "want a float"
		}
	case components.FieldEnum, components.FieldString:
		if kind != reflect.String {
			return "want a string"
		}
	case components.FieldStrings:
		if kind != reflect.Slice {
			return "want a slice"
		}
	default:
		return "unknown field type"
	}
	return ""
}

// yamlKeys is the set of dotted key paths a config struct unmarshals, i.e. exactly what
// components.Decode now accepts and everything else rejects. Nested blocks recurse, so
// `trigger.min_messages` is one key here just as it is one Field.
func yamlKeys(t reflect.Type, prefix string) map[string]reflect.Kind {
	out := map[string]reflect.Kind{}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue // unexported: yaml never sees it
		}
		name := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name) // yaml.v3's own fallback
		}
		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			for k, v := range yamlKeys(ft, prefix+name+".") {
				out[k] = v
			}
			continue
		}
		out[prefix+name] = ft.Kind()
	}
	return out
}

// A typo in a per-component block used to be silence: the blocks were unmarshalled
// non-strictly, so `min_tokns: 5000` parsed, changed nothing and reported nothing. That is
// also what made a descriptor-driven form untrustworthy — it could not be checked against
// what the component really reads.
func TestAMistypedComponentKeyIsRejected(t *testing.T) {
	for _, doc := range []string{
		"pipeline: [dedup]\ncomponents:\n  dedup:\n    min_tokns: 5000\n",
		"pipeline: [extract_llm]\ncomponents:\n  extract_llm:\n    cold_cache:\n      enabled: true\n      min_tokenz: 10\n",
		// cachesplit takes no configuration, and used to accept and discard whatever was
		// written under it.
		"pipeline: [cachesplit]\ncomponents:\n  cachesplit:\n    ttl: 1h\n",
	} {
		if err := config.Validate([]byte(doc)); err == nil {
			t.Errorf("accepted a mistyped component key:\n%s", doc)
		}
	}
}
