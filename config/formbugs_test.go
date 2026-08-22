package config

import (
	"reflect"
	"strings"
	"testing"

	_ "github.com/rossoctl/context-guru/components/all"
	"gopkg.in/yaml.v3"
)

// Two data-loss bugs in the settings save path, and the tests that fail without their fixes.
//
// Both are the same shape: a save wrote something the operator did not ask for and there was
// no test at the layer where the decision is made. The first threw away a consent the user
// had just given; the second threw away a component the client could not see.

// Bug 1: the keep-alive opt-in did not persist on a document with no `cache:` block.
//
// The one case that breaks, and the reason it went unnoticed for so long: on a document that
// ALREADY has a cache block every field round-trips correctly, because child() then returns
// the real map. Absent key, detached map, whole block discarded — and "switching keep-alive on
// for the first time" is by definition the absent-key case.
//
// Asserted through a ParseForm round trip rather than a substring. A test for
// `"keepalive: true"` in the output text would also pass on a document that persisted the
// consent and dropped something else.
func TestSavingKeepAliveOnADocumentWithNoCacheBlockKeepsIt(t *testing.T) {
	const doc = "pipeline:\n  - format\n  - extract\n"
	if strings.Contains(doc, "cache") {
		t.Fatal("the fixture must have NO cache block; that is the only case that breaks")
	}
	// Every field of CacheForm, one case each, so a field added later is covered by the same
	// table. reflect over the struct rather than a hand-listed set: a hand-listed set is how
	// the form drifted from its own declarations in the first place.
	full := CacheForm{
		KeepAlive: true, KeepAliveIdleSeconds: 280, KeepAliveMaxPings: 2,
		KeepAliveMaxUSDPerPing: 0.05, KeepAliveMinPrefixTokens: 20000,
		HeadTTL1h: true, HeadTTLMinTokens: 50000,
	}
	rt := reflect.TypeOf(full)
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		t.Run(name, func(t *testing.T) {
			// One field at a time, on an otherwise zero form: a field that only survives
			// because a neighbour happened to create the block is not a field that works.
			var one CacheForm
			reflect.ValueOf(&one).Elem().Field(i).Set(reflect.ValueOf(full).Field(i))
			out, err := ApplyForm(doc, Form{Pipeline: []string{"format", "extract"}, Cache: &one})
			if err != nil {
				t.Fatalf("ApplyForm: %v", err)
			}
			got, err := ParseForm(out)
			if err != nil {
				t.Fatalf("ParseForm: %v", err)
			}
			if got.Cache == nil {
				t.Fatalf("the whole cache block was discarded; the saved document is:\n%s", out)
			}
			if *got.Cache != one {
				t.Errorf("%s did not round-trip: sent %+v, read back %+v\nsaved document:\n%s",
					name, one, *got.Cache, out)
			}
		})
	}
}

// Bug 1's CLASS, on the helper. Three lines, and it is the assertion that generalises: the
// next block added to ApplyForm inherits it for free.
func TestChildAttachesTheBlockItCreates(t *testing.T) {
	m := map[string]any{}
	child(m, "x")["k"] = 1
	sub, ok := m["x"].(map[string]any)
	if !ok {
		t.Fatalf(`child(m,"x") did not insert a map into m; m = %#v`, m)
	}
	if sub["k"] != 1 {
		t.Errorf("the returned map is not the one in m: m[x] = %#v", sub)
	}
	// And it still adopts what is already there rather than replacing it.
	m2 := map[string]any{"y": map[string]any{"keep": true}}
	child(m2, "y")["added"] = true
	y := m2["y"].(map[string]any)
	if y["keep"] != true || y["added"] != true {
		t.Errorf("child replaced an existing block: %#v", y)
	}
}

// Bug 2: a save must never drop a component the client did not render a control for.
//
// The reported incident: `linecap` vanished from an operator's pipeline and `toon` was
// re-added at the end. The server writes the posted pipeline wholesale, so a stale render or
// a cached bundle whose /api/options predates a component produces exactly that — and the
// server could not tell it from a deliberate removal. PipelineKnown is the client CLAIMING
// what it drew, so absence from it resolves the ambiguity in favour of the stored document.
func TestASaveNeverDropsAComponentTheClientDidNotRender(t *testing.T) {
	const doc = "pipeline:\n  - format\n  - linecap\n  - extract\n"
	// The client draws format and extract, does not know linecap, and posts without it.
	out, err := ApplyForm(doc, Form{
		Pipeline:      []string{"format", "extract"},
		PipelineKnown: []string{"format", "extract"},
	})
	if err != nil {
		t.Fatalf("ApplyForm: %v", err)
	}
	if got := pipelineOf(t, out); !reflect.DeepEqual(got, []string{"format", "linecap", "extract"}) {
		t.Errorf("linecap was not preserved at its own index: got %v\n%s", got, out)
	}
	// And a DECLARED removal still removes. Without this the fix would be "the pipeline is
	// append-only", which is a different bug.
	out, err = ApplyForm(doc, Form{
		Pipeline:      []string{"format", "extract"},
		PipelineKnown: []string{"format", "linecap", "extract"},
	})
	if err != nil {
		t.Fatalf("ApplyForm (declared removal): %v", err)
	}
	if got := pipelineOf(t, out); !reflect.DeepEqual(got, []string{"format", "extract"}) {
		t.Errorf("a declared removal did not remove: got %v\n%s", got, out)
	}
}

// The old-bundle direction: a client that declares nothing removes nothing. A cached app.js
// and a hand-rolled API client are the same case, and both can still add and reorder.
func TestASaveFromAClientThatDeclaresNothingRemovesNothing(t *testing.T) {
	const doc = "pipeline:\n  - format\n  - linecap\n  - extract\n  - toon\n"
	out, err := ApplyForm(doc, Form{Pipeline: []string{"format"}})
	if err != nil {
		t.Fatalf("ApplyForm: %v", err)
	}
	got := pipelineOf(t, out)
	for _, want := range []string{"format", "linecap", "extract", "toon"} {
		if !contains(got, want) {
			t.Errorf("%s was removed by a client that declared nothing: got %v", want, got)
		}
	}
}

// A preserved component keeps its CONFIGURATION too. The component loop reads the resolved
// pipeline to decide what is switched off, and switched-off clears the declared keys — so
// preserving a name while wiping its block would leave a run order the document cannot explain.
func TestAPreservedComponentKeepsItsOwnBlock(t *testing.T) {
	const doc = `pipeline:
  - format
  - linecap
components:
  linecap:
    max_line_chars: 4000
`
	out, err := ApplyForm(doc, Form{
		Pipeline:      []string{"format"},
		PipelineKnown: []string{"format"},
		Components:    map[string]map[string]any{"linecap": {"max_line_chars": 4000}},
	})
	if err != nil {
		t.Fatalf("ApplyForm: %v", err)
	}
	if !strings.Contains(out, "max_line_chars: 4000") {
		t.Errorf("the preserved component's configuration was cleared:\n%s", out)
	}
}

// pipelineOf reads the pipeline back out of a saved document.
func pipelineOf(t *testing.T, doc string) []string {
	t.Helper()
	var d struct {
		Pipeline []string `yaml:"pipeline"`
	}
	if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
		t.Fatalf("the saved document does not parse: %v\n%s", err, doc)
	}
	return d.Pipeline
}
