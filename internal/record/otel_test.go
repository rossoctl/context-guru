package record

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func sampleTurn() Turn {
	start := time.Date(2026, 9, 10, 7, 18, 20, 896000000, time.UTC)
	return Turn{
		SessionID:    "068e40b8-4ff7-46a3-bb94-621359465a97",
		SpanID:       "065c1247752161c5",
		ParentID:     "561fa38bf42511df",
		Model:        "claude-sonnet-5",
		System:       "anthropic",
		Start:        start,
		End:          start.Add(2315*time.Millisecond + 858*time.Microsecond),
		MaxTokens:    64000,
		Effort:       "high",
		OutputTy:     "json",
		RequestBody:  `[{"role":"user","content":"hello"}]`,
		ResponseBody: `[{"role":"assistant","content":"hi"}]`,
		FreshInput:   1200,
		CacheRead:    18000,
		CacheWrite:   400,
		Output:       350,
		StopReason:   "end_turn",
		UpstreamMS:   2280,
		FirstByteMS:  410,
		Rounds:       1,
		ResponseSize: 4096,
	}
}

// TestSpanShapeMatchesTheCorpus is the compatibility contract. Every key asserted here was
// read out of a real claude-record trace file, and the analysis scripts index these names
// directly, so a rename is a silently-empty column rather than an error anyone would see.
func TestSpanShapeMatchesTheCorpus(t *testing.T) {
	m := mustJSON(t, sampleTurn().ToSpan())

	// The span envelope: exactly these ten keys, no more and no fewer. An extra key at this
	// level would be the first thing to diverge from the corpus, so the count is pinned too.
	want := []string{
		"trace_id", "span_id", "parent_span_id", "name", "kind",
		"start_time", "end_time", "attributes", "resource_attributes", "status",
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing span key %q, which the corpus has", k)
		}
	}
	if len(m) != len(want) {
		t.Errorf("span has %d keys, corpus has %d: %v", len(m), len(want), keysOf(m))
	}

	if got := m["kind"]; got != "SPAN_KIND_CLIENT" {
		t.Errorf("kind = %v, corpus uses SPAN_KIND_CLIENT", got)
	}
	// The trap this project has already hit once: the name must carry the model, because the
	// scripts select on the `chat ` PREFIX and would drop a span named plain `chat`.
	if got, _ := m["name"].(string); got != "chat claude-sonnet-5" {
		t.Errorf("name = %q, want \"chat claude-sonnet-5\"", got)
	}
	if !strings.HasPrefix(m["name"].(string), "chat ") {
		t.Error("name lost the 'chat ' prefix the analysis scripts match on")
	}

	// status is a nested object with both fields present, including an empty message on
	// success -- the corpus writes `{"code": 1, "message": ""}`, not a bare code.
	st, ok := m["status"].(map[string]any)
	if !ok {
		t.Fatalf("status is %T, corpus has an object", m["status"])
	}
	if st["code"] != float64(StatusOK) {
		t.Errorf("status.code = %v, want %d", st["code"], StatusOK)
	}
	if _, ok := st["message"]; !ok {
		t.Error("status.message absent; corpus always writes it, empty on success")
	}

	ra, ok := m["resource_attributes"].(map[string]any)
	if !ok || ra["service.name"] != "claude-code" {
		t.Errorf("resource_attributes = %v, corpus has service.name=claude-code", m["resource_attributes"])
	}

	// attributes is a flat OBJECT. This is the single biggest divergence from real OTLP,
	// where it is a list of {key, value:{stringValue}}. The scripts do `attributes[k]`.
	at, ok := m["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("attributes is %T, corpus has a flat object", m["attributes"])
	}
	for _, k := range []string{
		AttrOperation, AttrSystem, AttrModel, AttrMaxTokens, AttrEffort,
		AttrOutputType, AttrInputMsgs, AttrOutputMsgs, AttrInputTokens, AttrOutputTokens,
	} {
		if _, ok := at[k]; !ok {
			t.Errorf("missing attribute %q, which the corpus has", k)
		}
	}
	if at[AttrOperation] != "chat" {
		t.Errorf("%s = %v, want chat", AttrOperation, at[AttrOperation])
	}
}

// TestTimestampFormatIsByteIdenticalToTheCorpus. Go's RFC3339Nano trims trailing zeros and
// writes `Z`; the corpus writes six fixed digits and `+00:00`, and Python's
// datetime.fromisoformat rejects `Z` before 3.11. So this is pinned as an exact string.
func TestTimestampFormatIsByteIdenticalToTheCorpus(t *testing.T) {
	sp := sampleTurn().ToSpan()
	const want = "2026-09-10T07:18:20.896000+00:00"
	if sp.StartTime != want {
		t.Errorf("start_time = %q, corpus format is %q", sp.StartTime, want)
	}
	if strings.HasSuffix(sp.StartTime, "Z") {
		t.Error("wrote Z; the corpus uses a numeric offset and old fromisoformat rejects Z")
	}
	// Non-UTC input must be normalised, not written with its own offset.
	east := time.FixedZone("UTC+2", 2*60*60)
	turn := sampleTurn()
	turn.Start = time.Date(2026, 9, 10, 9, 18, 20, 896000000, east)
	if got := turn.ToSpan().StartTime; got != want {
		t.Errorf("non-UTC time not normalised: got %q, want %q", got, want)
	}
	// A zero time must be empty, never year 1.
	turn.Start = time.Time{}
	if got := turn.ToSpan().StartTime; got != "" {
		t.Errorf("zero time rendered as %q; want empty", got)
	}
}

// TestCacheSplitIsAlwaysEmitted. This is the mooncake unblock and the main technical reason
// to record through the proxy at all, so it must be present even when zero: "the provider
// reported no cache activity" and "this recorder cannot see cache activity" are different
// facts, and claude-record can only ever express the second.
func TestCacheSplitIsAlwaysEmitted(t *testing.T) {
	turn := sampleTurn()
	turn.FreshInput, turn.CacheRead, turn.CacheWrite = 0, 0, 0
	at := turn.ToSpan().Attributes
	for _, k := range []string{AttrFreshInput, AttrCacheRead, AttrCacheWrite} {
		v, ok := at[k]
		if !ok {
			t.Errorf("%s absent at zero; absence is indistinguishable from 'unknown'", k)
			continue
		}
		if v != 0 {
			t.Errorf("%s = %v, want 0", k, v)
		}
	}
}

// TestInputTokensMatchesTheCollectorsTotal pins the parity claim on the one column most likely
// to be compared across the two recorders. claude-record's total_input_tokens() sums all three
// components, so this must too -- and the components must ALSO survive separately, which is
// the part claude-record cannot do.
func TestInputTokensMatchesTheCollectorsTotal(t *testing.T) {
	turn := sampleTurn() // 1200 fresh + 18000 cache read + 400 cache write
	const want = 1200 + 18000 + 400
	if got := turn.InputTokens(); got != want {
		t.Errorf("InputTokens = %d, want %d (fresh+cache_read+cache_write, as converter.py sums)", got, want)
	}
	at := turn.ToSpan().Attributes
	if at[AttrInputTokens] != want {
		t.Errorf("%s = %v, want %d", AttrInputTokens, at[AttrInputTokens], want)
	}
	// The split is the addition: each component recoverable on its own.
	for k, exp := range map[string]int{AttrFreshInput: 1200, AttrCacheRead: 18000, AttrCacheWrite: 400} {
		if at[k] != exp {
			t.Errorf("%s = %v, want %d; the split is what mooncake needs", k, at[k], exp)
		}
	}
}

// TestIncompletenessIsPresentOnlyWhenTrue. Presence is the signal: an analysis filters on
// the key existing, so writing an empty value on every healthy turn would make the filter
// match everything.
func TestIncompletenessIsPresentOnlyWhenTrue(t *testing.T) {
	if at := sampleTurn().ToSpan().Attributes; at[AttrIncomplete] != nil {
		t.Errorf("%s present on a complete turn: %v", AttrIncomplete, at[AttrIncomplete])
	}
	for _, reason := range []string{IncompleteTeeCap, IncompleteRetainCap, IncompleteTruncated, IncompleteNoBody} {
		turn := sampleTurn()
		turn.Incomplete = reason
		if got := turn.ToSpan().Attributes[AttrIncomplete]; got != reason {
			t.Errorf("%s = %v, want %q", AttrIncomplete, got, reason)
		}
	}
}

// TestAnIncompleteTurnStillCarriesItsUsage is the property that makes the incompleteness
// marker worth having rather than a reason to drop the row. The two unrecoverable CG paths
// lose the response BODY; the token counts still arrive, and those are what an
// inference-performance analysis actually needs.
func TestAnIncompleteTurnStillCarriesItsUsage(t *testing.T) {
	turn := sampleTurn()
	turn.ResponseBody = ""
	turn.Incomplete = IncompleteRetainCap

	at := turn.ToSpan().Attributes
	if _, ok := at[AttrOutputMsgs]; ok {
		t.Error("empty response body emitted as an attribute; absent is honest, empty is a false claim")
	}
	if at[AttrIncomplete] != IncompleteRetainCap {
		t.Errorf("incompleteness not marked: %v", at[AttrIncomplete])
	}
	if at[AttrInputTokens] != 19600 || at[AttrOutputTokens] != 350 {
		t.Errorf("usage lost on an incomplete turn: in=%v out=%v", at[AttrInputTokens], at[AttrOutputTokens])
	}
}

// TestErrorTurnGetsErrorStatus: an upstream failure is a turn worth recording (it is a real
// latency and a real cost), so it must be emitted with a distinguishable status rather than
// dropped.
func TestErrorTurnGetsErrorStatus(t *testing.T) {
	turn := sampleTurn()
	turn.Err = "upstream 502"
	sp := turn.ToSpan()
	if sp.Status.Code != StatusError {
		t.Errorf("status.code = %d, want %d", sp.Status.Code, StatusError)
	}
	if sp.Status.Message != "upstream 502" {
		t.Errorf("status.message = %q", sp.Status.Message)
	}
}

// TestFileEnvelopeMatchesTheCorpus pins the top level, which is where a real OTLP writer
// would put resourceSpans and break every existing script.
func TestFileEnvelopeMatchesTheCorpus(t *testing.T) {
	spans := []Span{sampleTurn().ToSpan()}
	f := NewFile("068e40b8-4ff7-46a3-bb94-621359465a97", spans, time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))
	m := mustJSON(t, f)

	want := []string{"trace_id", "span_count", "collected_at", "spans", "collector_version"}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	if len(m) != len(want) {
		t.Errorf("envelope has %d keys, corpus has %d: %v", len(m), len(want), keysOf(m))
	}
	if _, ok := m["resourceSpans"]; ok {
		t.Error("emitted OTLP resourceSpans nesting; the corpus is flat and the scripts read d['spans']")
	}
	if m["span_count"] != float64(1) {
		t.Errorf("span_count = %v, want 1", m["span_count"])
	}
	// The producer must be identifiable: a mixed corpus has to be attributable per row.
	if m["collector_version"] != CollectorVersion {
		t.Errorf("collector_version = %v, want %q", m["collector_version"], CollectorVersion)
	}
	if !strings.HasPrefix(CollectorVersion, "cg-record/") {
		t.Errorf("CollectorVersion %q is not distinguishable from claude-record's", CollectorVersion)
	}
}

// TestMarshalIsLoadableAndIndented round-trips through the exact bytes that hit disk.
func TestMarshalIsLoadableAndIndented(t *testing.T) {
	f := NewFile("t1", []Span{sampleTurn().ToSpan()}, time.Now())
	b, err := f.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.HasSuffix(string(b), "\n") {
		t.Error("no trailing newline")
	}
	if !strings.Contains(string(b), "\n  \"trace_id\"") {
		t.Error("not indented two spaces like the corpus")
	}
	var back File
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("own output does not round-trip: %v", err)
	}
	if back.SpanCount != 1 || len(back.Spans) != 1 {
		t.Errorf("round-trip lost spans: count=%d len=%d", back.SpanCount, len(back.Spans))
	}
	if back.Spans[0].Name != "chat claude-sonnet-5" {
		t.Errorf("round-trip changed name: %q", back.Spans[0].Name)
	}
}

// TestSpanNameHandlesAMissingModel: the model is parsed out of a request body, so it can be
// absent on a malformed one. The span must stay selectable rather than becoming `chat `.
func TestSpanNameHandlesAMissingModel(t *testing.T) {
	if got := SpanName(""); got != "chat unknown" {
		t.Errorf("SpanName(\"\") = %q, want \"chat unknown\"", got)
	}
	if !strings.HasPrefix(SpanName(""), "chat ") {
		t.Error("a modelless span fell out of the 'chat ' selector")
	}
}

// TestEmptyOptionalsAreOmittedNotBlank. A blank string in the corpus reads as "the model was
// asked for no schema", which is a different claim from "this recorder had none".
func TestEmptyOptionalsAreOmittedNotBlank(t *testing.T) {
	turn := Turn{Model: "m", SessionID: "s", SpanID: "sp"}
	at := turn.ToSpan().Attributes
	for _, k := range []string{
		AttrSystem, AttrMaxTokens, AttrEffort, AttrOutputType,
		AttrInputMsgs, AttrOutputMsgs, AttrOutputSchema, AttrStopReason,
		AttrUpstreamMS, AttrFirstByteMS, AttrRounds, AttrResponseSize, AttrIncomplete,
	} {
		if v, ok := at[k]; ok {
			t.Errorf("%s emitted as %v on an empty turn; omit it instead", k, v)
		}
	}
	// But the always-on ones must still be there.
	for _, k := range []string{AttrOperation, AttrModel, AttrInputTokens, AttrOutputTokens, AttrFreshInput} {
		if _, ok := at[k]; !ok {
			t.Errorf("%s missing; it is not optional", k)
		}
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
