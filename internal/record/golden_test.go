package record

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// This file is the compatibility contract against the REAL corpus rather than against
// anyone's reading of it.
//
// testdata/corpus_schema.json is extracted from all 67 existing claude-record trace files
// (5,680 `chat *` spans): every span key, attribute key, JSON type and span kind the corpus
// actually uses, with counts. The tests below assert that this package emits what those
// files contain.
//
// This is not decoration. Writing it immediately caught a real omission: a 12-file sample
// had shown 8 inference attributes, and the full 67-file scan has NINE -- the missing one
// being gen_ai.tool.definitions, present on 5,037 spans, which is what makes a tool_use
// block in a response interpretable at all. A hand-written fixture would have carried the
// same blind spot as the hand-written emitter.
//
// When the corpus schema changes, regenerate the fixture rather than editing it.

type corpusSchema struct {
	FilesScanned   int                       `json:"files_scanned"`
	ChatSpans      int                       `json:"chat_spans"`
	TopLevelKeys   map[string]int            `json:"top_level_keys"`
	SpanKeys       map[string]int            `json:"span_keys"`
	Kinds          map[string]int            `json:"kinds"`
	StatusKeys     map[string]int            `json:"status_keys"`
	ResourceAttrs  map[string]int            `json:"resource_attributes"`
	AttributeKeys  map[string]int            `json:"attribute_keys"`
	AttributeTypes map[string]map[string]int `json:"attribute_types"`
	TimestampsSeen []string                  `json:"timestamp_samples"`
}

func loadCorpusSchema(t *testing.T) corpusSchema {
	t.Helper()
	b, err := os.ReadFile("testdata/corpus_schema.json")
	if err != nil {
		t.Fatalf("read corpus fixture: %v", err)
	}
	var cs corpusSchema
	if err := json.Unmarshal(b, &cs); err != nil {
		t.Fatalf("parse corpus fixture: %v", err)
	}
	if cs.ChatSpans == 0 {
		t.Fatal("fixture has no chat spans; it is not ground truth")
	}
	return cs
}

// richTurn sets every field, so a "can this package emit X" test measures the emitter and
// not a sparse fixture.
func richTurn() Turn {
	turn := sampleTurn()
	turn.OutputSchema = `{"type":"object"}`
	turn.ToolDefs = `[{"name":"Read","input_schema":{"type":"object"}}]`
	return turn
}

// TestEmitterCoversEveryCorpusAttribute is the central golden assertion: for every
// attribute the real files use on a chat span, this package must be able to produce it.
// A gap here is a column that silently disappears from the dataset when a session is
// recorded through the proxy instead of the collector.
func TestEmitterCoversEveryCorpusAttribute(t *testing.T) {
	cs := loadCorpusSchema(t)
	at := richTurn().ToSpan().Attributes

	var missing []string
	for key, count := range cs.AttributeKeys {
		if _, ok := at[key]; !ok {
			missing = append(missing, key+" (on "+itoa(count)+" corpus spans)")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("cannot emit %d attribute(s) the corpus uses:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	t.Logf("covered %d/%d corpus attributes across %d spans in %d files",
		len(cs.AttributeKeys)-len(missing), len(cs.AttributeKeys), cs.ChatSpans, cs.FilesScanned)
}

// TestEmittedAttributeTypesMatchTheCorpus. The scripts do arithmetic on the token counts and
// json.loads on the bodies, so a number emitted as a string is a TypeError in someone's
// analysis rather than a visible schema error.
func TestEmittedAttributeTypesMatchTheCorpus(t *testing.T) {
	cs := loadCorpusSchema(t)
	// Round-trip through JSON so the types checked are the ON-DISK types.
	var at map[string]any
	b, err := json.Marshal(richTurn().ToSpan().Attributes)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(b, &at); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for key, types := range cs.AttributeTypes {
		got, ok := at[key]
		if !ok {
			continue // reported by TestEmitterCoversEveryCorpusAttribute
		}
		wantNumber := types["int"] > 0 || types["float"] > 0
		wantString := types["str"] > 0
		switch got.(type) {
		case float64:
			if !wantNumber {
				t.Errorf("%s: emitted a number, corpus has %v", key, keysOfInt(types))
			}
		case string:
			if !wantString {
				t.Errorf("%s: emitted a string, corpus has %v", key, keysOfInt(types))
			}
		default:
			t.Errorf("%s: emitted %T, corpus has %v", key, got, keysOfInt(types))
		}
	}
}

// TestSpanEnvelopeMatchesTheCorpusExactly checks the ten span keys and the nested shapes,
// read from the fixture rather than retyped.
func TestSpanEnvelopeMatchesTheCorpusExactly(t *testing.T) {
	cs := loadCorpusSchema(t)
	m := mustJSON(t, richTurn().ToSpan())

	for key := range cs.SpanKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("missing span key %q (on all %d corpus spans)", key, cs.SpanKeys[key])
		}
	}
	if len(m) != len(cs.SpanKeys) {
		t.Errorf("emitted %d span keys, corpus has %d", len(m), len(cs.SpanKeys))
	}

	// The corpus uses exactly one kind. Emitting a second would be a new value for every
	// consumer to handle.
	if len(cs.Kinds) != 1 {
		t.Fatalf("fixture has %d kinds; this test assumes one", len(cs.Kinds))
	}
	for kind := range cs.Kinds {
		if m["kind"] != kind {
			t.Errorf("kind = %v, corpus uses only %q", m["kind"], kind)
		}
	}

	st, _ := m["status"].(map[string]any)
	for key := range cs.StatusKeys {
		if _, ok := st[key]; !ok {
			t.Errorf("status missing %q, which the corpus always writes", key)
		}
	}
	ra, _ := m["resource_attributes"].(map[string]any)
	for key := range cs.ResourceAttrs {
		if _, ok := ra[key]; !ok {
			t.Errorf("resource_attributes missing %q", key)
		}
	}
}

// TestTopLevelEnvelopeMatchesTheCorpus. collector_version appears on 31 of 67 files (older
// ones predate it); we always write it, which is a superset and is what makes a mixed
// corpus attributable per file.
func TestTopLevelEnvelopeMatchesTheCorpus(t *testing.T) {
	cs := loadCorpusSchema(t)
	m := mustJSON(t, NewFile("t", []Span{richTurn().ToSpan()}, time.Now()))
	for key := range cs.TopLevelKeys {
		if _, ok := m[key]; !ok {
			t.Errorf("missing top-level key %q (on %d corpus files)", key, cs.TopLevelKeys[key])
		}
	}
	if len(m) != len(cs.TopLevelKeys) {
		t.Errorf("emitted %d top-level keys, corpus has %d: %v", len(m), len(cs.TopLevelKeys), keysOf(m))
	}
}

// TestOurTimestampsParseAsTheCorpusOnesDo compares format against real samples rather than
// against the format string, which is what would catch a change to either.
func TestOurTimestampsParseAsTheCorpusOnesDo(t *testing.T) {
	cs := loadCorpusSchema(t)
	if len(cs.TimestampsSeen) == 0 {
		t.Fatal("fixture carries no timestamp samples")
	}
	ours := FormatTime(time.Date(2026, 9, 10, 7, 18, 20, 896000000, time.UTC))
	for _, sample := range cs.TimestampsSeen {
		if len(sample) != len(ours) {
			t.Errorf("length differs from corpus: ours %q (%d), corpus %q (%d)",
				ours, len(ours), sample, len(sample))
		}
		// Both must parse with the same layout, which is the operational property.
		if _, err := time.Parse(timeFormat, sample); err != nil {
			t.Errorf("corpus timestamp %q does not parse with our layout: %v", sample, err)
		}
	}
	if _, err := time.Parse(timeFormat, ours); err != nil {
		t.Errorf("our own timestamp %q does not parse with our layout: %v", ours, err)
	}
}

// TestCGAttributesAreNamespaced. The additive columns must be unmistakable: a reader has to
// be able to tell "the proxy knew this" from "the OTel semconv says this", and an analysis
// that wants strict claude-record parity must be able to drop them by prefix.
func TestCGAttributesAreNamespaced(t *testing.T) {
	cs := loadCorpusSchema(t)
	at := richTurn().ToSpan().Attributes

	for key := range at {
		_, inCorpus := cs.AttributeKeys[key]
		if inCorpus {
			continue
		}
		if !strings.HasPrefix(key, "cg.") {
			t.Errorf("new attribute %q is neither in the corpus nor namespaced under cg.", key)
		}
	}

	// And the ones that carry the reason this recorder exists must actually be present.
	for _, key := range []string{AttrCacheRead, AttrCacheWrite, AttrFreshInput} {
		if _, ok := at[key]; !ok {
			t.Errorf("%s absent; the cache split is the mooncake unblock", key)
		}
	}
}

// TestStrictParityViewIsExactlyTheCorpusSchema proves the promise that recording through
// the proxy is a SUPERSET: drop every cg.* key and what remains is the corpus schema, no
// more and no less. That is the claim being made to the Eval team, so it is a test.
func TestStrictParityViewIsExactlyTheCorpusSchema(t *testing.T) {
	cs := loadCorpusSchema(t)
	at := richTurn().ToSpan().Attributes

	strict := map[string]any{}
	for k, v := range at {
		if !strings.HasPrefix(k, "cg.") {
			strict[k] = v
		}
	}
	for key := range cs.AttributeKeys {
		if _, ok := strict[key]; !ok {
			t.Errorf("strict view is missing corpus attribute %q", key)
		}
	}
	for key := range strict {
		if _, ok := cs.AttributeKeys[key]; !ok {
			t.Errorf("strict view has %q, which the corpus does not; it should be cg.-namespaced", key)
		}
	}
	if len(strict) != len(cs.AttributeKeys) {
		t.Errorf("strict view has %d attributes, corpus has %d", len(strict), len(cs.AttributeKeys))
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func keysOfInt(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
