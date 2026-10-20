package proxy

// Tests for --record.
//
// The one that matters most is TestRecordDoesNotAlterTheClientResponse: CLAUDE.md's fail-open
// rule is a hard boundary, and rectee_test.go only proves byte-transparency for streamRec in
// ISOLATION. This file proves it for the wired path, which is what the guarantee is about.
//
// The two that would otherwise waste a live dual-recorded session are the session-id test
// (an empty key merges every session into one file) and the SSE-aggregation test (raw event
// bytes handed to the translator yield a corpus where every streamed turn looks unanswered).

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/internal/record"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/store"
)

// quietRecLog keeps expected warnings (dropped turns, restart collisions) out of the test
// output. The counters, not the log lines, are what the assertions read.
func quietRecLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestRecorder(t *testing.T) (*sessionRecorder, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := newSessionRecorder(dir, 1<<20, 8<<20, 64, quietRecLog())
	if err != nil {
		t.Fatalf("newSessionRecorder: %v", err)
	}
	if r == nil {
		t.Fatal("newSessionRecorder returned nil for a non-empty dir")
	}
	return r, dir
}

// testSSE is a realistic Anthropic event stream: what path A actually carries, and the input
// AggregateSSE has to rebuild a message object from.
const testSSE = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-5","content":[],"stop_reason":null}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello there"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// testSessionUUID is shaped like Claude Code's own session id, which is what claude-record
// names its files by -- the join key for the whole dual-recording comparison.
const testSessionUUID = "6d1f2a44-9e3b-4c7a-8f21-0b5d7c9e1a33"

// ccBody builds a request body shaped like Claude Code's: the session UUID inside
// metadata.user_id, a system block, and an advertised tool set (the reconstruction's anchor).
func ccBody(prompt string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      "aws/claude-sonnet-5",
		"max_tokens": 64,
		"metadata": map[string]any{
			"user_id": `{"device_id":"dev1","account_uuid":"acct1","session_id":"` + testSessionUUID + `"}`,
		},
		"system": []any{map[string]any{"type": "text", "text": "You are Claude Code."}},
		"tools": []any{
			map[string]any{"name": "Bash", "description": "run a command",
				"input_schema": map[string]any{"type": "object"}},
		},
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	})
	return b
}

// ccBodyTurns builds a Claude Code body replaying several human turns, the way every real
// call does: the whole conversation so far, alternating user and assistant.
func ccBodyTurns(prompts ...string) []byte {
	msgs := make([]any, 0, len(prompts)*2-1)
	for i, p := range prompts {
		if i > 0 {
			msgs = append(msgs, map[string]any{"role": "assistant", "content": "On it."})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": p})
	}
	var m map[string]any
	_ = json.Unmarshal(ccBody(prompts[0]), &m)
	m["messages"] = msgs
	b, _ := json.Marshal(m)
	return b
}

// ccBodyNoTools is a call advertising no tools: a helper call, with no conversation anchor.
func ccBodyNoTools(prompt string) []byte {
	var m map[string]any
	_ = json.Unmarshal(ccBody(prompt), &m)
	delete(m, "tools")
	b, _ := json.Marshal(m)
	return b
}

func traceFiles(t *testing.T, dir string) []string {
	t.Helper()
	f, err := filepath.Glob(filepath.Join(dir, "*.otel.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(f)
	return f
}

// chatSpans returns only the OBSERVED model-call spans, dropping the derived-turn spans the
// recorder also writes. Tests about one request's translation mean "the chat span", not "every
// span in the file" -- so they filter, rather than counting a total that turn reconstruction is
// entitled to add to.
func chatSpans(t *testing.T, spans []map[string]any) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(spans))
	for _, sp := range spans {
		if n, _ := sp["name"].(string); n != record.SpanDerivedTurn {
			out = append(out, sp)
		}
	}
	return out
}

// derivedTurnSpans returns only the reconstructed-turn spans.
func derivedTurnSpans(t *testing.T, spans []map[string]any) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(spans))
	for _, sp := range spans {
		if n, _ := sp["name"].(string); n == record.SpanDerivedTurn {
			out = append(out, sp)
		}
	}
	return out
}

func readTrace(t *testing.T, path string) (map[string]any, []map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f map[string]any
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	rawSpans, ok := f["spans"].([]any)
	if !ok {
		t.Fatalf("no spans array in %s: %v", path, f)
	}
	spans := make([]map[string]any, 0, len(rawSpans))
	for _, s := range rawSpans {
		m, ok := s.(map[string]any)
		if !ok {
			t.Fatalf("span is not an object: %v", s)
		}
		spans = append(spans, m)
	}
	return f, spans
}

func spanAttrs(t *testing.T, span map[string]any) map[string]any {
	t.Helper()
	a, ok := span["attributes"].(map[string]any)
	if !ok {
		t.Fatalf("span has no attributes: %v", span)
	}
	return a
}

// readOneTrace asserts there is exactly one trace file and returns its spans.
func readOneTrace(t *testing.T, dir string) []map[string]any {
	t.Helper()
	files := traceFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("want exactly 1 trace file in %s, got %v", dir, files)
	}
	_, spans := readTrace(t, files[0])
	return spans
}

func baseEvent(traceID string, resp []byte, isSSE bool) *recEvent {
	now := time.Now()
	return &recEvent{
		traceID:   traceID,
		provider:  bschemas.Anthropic,
		reqBody:   ccBody("fix the failing test"),
		respBody:  resp,
		respIsSSE: isSSE,
		respSize:  len(resp),
		start:     now.Add(-2 * time.Second),
		end:       now,
		// Distinct values, so a test can tell a real measurement from a zero default.
		upstreamMS:  1900,
		firstByteMS: 120,
		rounds:      1,
		usage: Usage{FreshInput: 12, CacheRead: 400, CacheWrite: 7, Output: 3,
			StopReason: "end_turn"},
		usageOK: true,
	}
}

// recordAndClose sends events and closes, so everything is flushed to disk.
func recordAndClose(t *testing.T, r *sessionRecorder, evs ...*recEvent) {
	t.Helper()
	for _, e := range evs {
		r.Record(e)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// --- 1. The guarantee CLAUDE.md makes --------------------------------------------------
//
// A real Handler, a real pipeline, a real Mux, an SSE upstream: driven once with recording
// off and once with it on, the bytes the client receives must be identical. Any divergence
// here means a recording path is mutating a proxied response, which the fail-open rule
// forbids outright.
func TestRecordDoesNotAlterTheClientResponse(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(testSSE))
	}))
	defer up.Close()

	run := func(t *testing.T, rec *sessionRecorder) (int, string, http.Header) {
		t.Helper()
		cfg, err := config.LoadBytes([]byte("preset: codesafe\n"))
		if err != nil {
			t.Fatal(err)
		}
		agg := metrics.NewAggregator()
		pipe, err := cfg.Build(agg)
		if err != nil {
			t.Fatal(err)
		}
		h := New(pipe, store.NewMemory(store.Options{}), agg, Options{
			AnthropicUpstream: up.URL,
			Preset:            cfg.Preset,
			Record:            rec,
		})
		srv := httptest.NewServer(h.Mux())
		defer srv.Close()

		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/anthropic/v1/messages",
			strings.NewReader(string(ccBody("please fix the failing test"))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("User-Agent", "claude-cli/2.0.14 (external, cli)")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body), resp.Header.Clone()
	}

	offStatus, offBody, offHdr := run(t, nil)

	r, dir := newTestRecorder(t)
	onStatus, onBody, onHdr := run(t, r)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if offStatus != onStatus {
		t.Errorf("status changed with recording on: %d -> %d", offStatus, onStatus)
	}
	if offBody != onBody {
		t.Errorf("RECORDING ALTERED THE CLIENT RESPONSE.\n off (%d bytes): %q\n on  (%d bytes): %q",
			len(offBody), offBody, len(onBody), onBody)
	}
	if got, want := onHdr.Get("Content-Type"), offHdr.Get("Content-Type"); got != want {
		t.Errorf("Content-Type changed with recording on: %q -> %q", want, got)
	}

	// And the turn must actually have been recorded -- a test that passes because nothing
	// was captured would prove nothing at all.
	files := traceFiles(t, dir)
	if len(files) == 0 {
		t.Fatal("recording was on but no trace file was written; this test proved nothing")
	}
	_, spans := readTrace(t, files[0])
	if len(spans) == 0 {
		t.Fatal("trace file has no spans")
	}
}

// --- 2. The trace id in observe mode ---------------------------------------------------
//
// THE MOST DANGEROUS BUG THIS DESIGN CAN HAVE. In observe mode tr.Session is empty by
// construction (apply.go: the enforced path never calls BodyOpts, which is what makes
// observe's byte-identity structural). A recorder keyed on it would write ONE file named for
// the empty string holding every session's spans -- wasting exactly the dual-recorded session
// this feature exists for.
func TestRecordSessionIDInObserveMode(t *testing.T) {
	r, _ := newTestRecorder(t)
	defer r.Close()
	h := &Handler{recorder: r}

	// A Tenancy with no session id, as observe mode leaves it.
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	rc := h.newRecCtx(req, bschemas.Anthropic, ccBody("hello"), &Tenancy{})

	if rc.traceID != testSessionUUID {
		t.Fatalf("trace id = %q, want Claude Code's own session UUID %q\n"+
			"(an id not from metadata.user_id cannot be joined to claude-record's file)",
			rc.traceID, testSessionUUID)
	}
	if len(rc.pristine) == 0 {
		t.Error("pristine body not captured")
	}
}

// A body with no metadata.user_id must still get a non-empty, visibly-derived id.
func TestRecordSessionIDFallbackIsNeverEmptyAndIsMarked(t *testing.T) {
	r, _ := newTestRecorder(t)
	defer r.Close()
	h := &Handler{recorder: r}

	body := []byte(`{"model":"aws/claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	rc := h.newRecCtx(req, bschemas.Anthropic, body, &Tenancy{})

	if rc.traceID == "" {
		t.Fatal("trace id is empty: the file would be named .otel.json and merge unrelated sessions")
	}
	if !strings.HasPrefix(rc.traceID, "cg-derived-") && !strings.HasPrefix(rc.traceID, "cg-unkeyed-") {
		t.Errorf("fallback id %q carries no marker; an analyst cannot tell it from a real "+
			"Claude Code UUID", rc.traceID)
	}
}

// Recording off: no recorder, no clone, no work.
func TestRecCtxIsZeroWhenRecordingOff(t *testing.T) {
	h := &Handler{} // recorder nil
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)
	rc := h.newRecCtx(req, bschemas.Anthropic, ccBody("x"), &Tenancy{})
	if rc.traceID != "" || rc.pristine != nil {
		t.Fatalf("recording off must yield the zero recCtx, got %+v", rc)
	}
}

// --- 3. An SSE body must be aggregated BEFORE translation ------------------------------
//
// record.OutputMessages unmarshals ONE Anthropic message object. Hand it raw event-stream
// bytes and it fails, so every streamed turn -- i.e. every turn of a real Claude Code
// session -- would land in the corpus looking unanswered.
func TestRecordSSEResponseIsAggregatedBeforeTranslation(t *testing.T) {
	r, dir := newTestRecorder(t)
	recordAndClose(t, r, baseEvent(testSessionUUID, []byte(testSSE), true))

	spans := chatSpans(t, readOneTrace(t, dir))
	if len(spans) != 1 {
		t.Fatalf("want 1 chat span, got %d", len(spans))
	}
	a := spanAttrs(t, spans[0])
	out, _ := a["gen_ai.output.messages"].(string)
	if out == "" {
		t.Fatal("no output messages recorded: the SSE body was not aggregated")
	}
	if strings.Contains(out, "event:") || strings.Contains(out, "content_block_delta") {
		t.Errorf("raw SSE leaked into the translated output: %s", out)
	}
	if !strings.Contains(out, "Hello there") {
		t.Errorf("aggregated assistant text missing from the output messages: %s", out)
	}
	if inc, _ := a[record.AttrIncomplete].(string); inc != "" {
		t.Errorf("a fully captured turn must carry no incomplete reason, got %q", inc)
	}
	// Usage must come from the proxy's own parse. An aggregated body has NO usage key at all
	// (expand/sse.go never emits one), so a recorder reading it back would report zeros here.
	// The split is what mooncake needs. Read and write are semconv names (shared with
	// claude-record 0.1.5); only the fresh remainder is cg.usage.*, as semconv has no name for it.
	for _, c := range []struct {
		key  string
		want float64
	}{
		{record.AttrCacheRead, 400},
		{record.AttrCacheWrite, 7},
		{record.AttrFreshInput, 12},
		{record.AttrOutputTokens, 3},
		// The semconv total is fresh+read+write. Reporting only fresh_input here is exactly
		// the under-reporting we filed against the collector as issue #17.
		{record.AttrInputTokens, 419},
	} {
		if got := a[c.key]; got != c.want {
			t.Errorf("%s = %v, want %v from the proxy's own Usage struct", c.key, got, c.want)
		}
	}
}

// Hosted mode: --record on the operator side is not consent. A tenant who has not opted in
// is not recorded at all -- no clone, no event -- and one who has is keyed to their own id.
func TestRecordNeedsTheTenantsConsentInHostedMode(t *testing.T) {
	r, _ := newTestRecorder(t)
	defer r.Close()
	h := &Handler{recorder: r, opts: Options{Tenants: &TenantSource{}}}
	req := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", nil)

	if rc := h.newRecCtx(req, bschemas.Anthropic, ccBody("x"), &Tenancy{ID: "t1"}); rc.traceID != "" || rc.pristine != nil {
		t.Fatalf("recorded a tenant who never opted in: %+v", rc)
	}
	if rc := h.newRecCtx(req, bschemas.Anthropic, ccBody("x"), nil); rc.traceID != "" {
		t.Fatalf("recorded an unauthenticated request in hosted mode: %+v", rc)
	}
	rc := h.newRecCtx(req, bschemas.Anthropic, ccBody("x"), &Tenancy{ID: "t1", RecordTraces: true})
	if rc.traceID != testSessionUUID || rc.tenantID != "t1" {
		t.Fatalf("opted-in tenant: got trace %q tenant %q, want %q and t1", rc.traceID, rc.tenantID, testSessionUUID)
	}
}

// Each tenant's traces go in their own directory, and two tenants presenting the same session
// UUID still get two files: one person's session can never be appended to another's.
func TestRecorderSeparatesTenants(t *testing.T) {
	r, dir := newTestRecorder(t)
	a := baseEvent(testSessionUUID, []byte(testSSE), true)
	a.tenantID = "alice"
	b := baseEvent(testSessionUUID, []byte(testSSE), true)
	b.tenantID = "bob"
	recordAndClose(t, r, a, b)

	if root := traceFiles(t, dir); len(root) != 0 {
		t.Errorf("hosted traces written to the shared root: %v", root)
	}
	for _, who := range []string{"alice", "bob"} {
		files := traceFiles(t, filepath.Join(dir, who))
		if len(files) != 1 || filepath.Base(files[0]) != testSessionUUID+".otel.json" {
			t.Errorf("%s: want one file named for the session, got %v", who, files)
		}
	}
}

// --- 4. One file per session, spans threaded ------------------------------------------
func TestRecorderWritesOneFilePerSession(t *testing.T) {
	r, dir := newTestRecorder(t)
	const other = "11112222-3333-4444-5555-666677778888"
	recordAndClose(t, r,
		baseEvent(testSessionUUID, []byte(testSSE), true),
		baseEvent(other, []byte(testSSE), true),
		baseEvent(testSessionUUID, []byte(testSSE), true),
	)

	files := traceFiles(t, dir)
	if len(files) != 2 {
		t.Fatalf("want one file per session (2), got %v", files)
	}
	got := []string{filepath.Base(files[0]), filepath.Base(files[1])}
	want := []string{other + ".otel.json", testSessionUUID + ".otel.json"}
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d = %q, want %q", i, got[i], want[i])
		}
	}

	// The two-turn session must thread its spans: turn 2's parent is turn 1.
	f, allSpans := readTrace(t, filepath.Join(dir, testSessionUUID+".otel.json"))
	spans := chatSpans(t, allSpans)
	if len(spans) != 2 {
		t.Fatalf("want 2 chat spans for the 2-turn session, got %d", len(spans))
	}
	first, _ := spans[0]["span_id"].(string)
	parent, _ := spans[1]["parent_span_id"].(string)
	if first == "" {
		t.Fatal("first span has no id")
	}
	if parent != first {
		t.Errorf("turn 2 parent_span_id = %q, want turn 1 span_id %q; without this a consumer "+
			"cannot order the session", parent, first)
	}
	if tid, _ := f["trace_id"].(string); tid != testSessionUUID {
		t.Errorf("trace_id = %q, want the session UUID (the join key with claude-record)", tid)
	}
}

// --- 5. Every incomplete path must SAY why --------------------------------------------
//
// record/otel.go: a turn that quietly carries an empty response body is a corpus defect
// nobody discovers until analysis. This is that contract, per path.
func TestRecordMarksIncompleteOnEachPath(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*recEvent)
		wantAny []string
	}{
		{
			name:    "no body at all (path C: non-Anthropic SSE)",
			mutate:  func(e *recEvent) { e.respBody, e.respSize = nil, 0 },
			wantAny: []string{record.IncompleteNoBody},
		},
		{
			name: "a non-Anthropic dialect cannot be aggregated",
			mutate: func(e *recEvent) {
				e.provider = bschemas.OpenAI
				e.respIsSSE = true
			},
			wantAny: []string{record.IncompleteNoBody},
		},
		{
			name: "tee cap reached mid-stream",
			mutate: func(e *recEvent) {
				e.incomplete = record.IncompleteTeeCap
				e.respBody = []byte(testSSE[:200])
			},
			wantAny: []string{record.IncompleteTeeCap},
		},
		{
			name: "retain bound blown (path B)",
			mutate: func(e *recEvent) {
				e.incomplete = record.IncompleteRetainCap
				e.respBody, e.respSize = nil, 0
			},
			wantAny: []string{record.IncompleteRetainCap},
		},
		{
			name: "multi-round continuation keeps only the last round",
			mutate: func(e *recEvent) {
				e.rounds = 3
				e.incomplete = joinIncomplete(record.IncompleteNone, record.IncompleteTruncated)
			},
			wantAny: []string{record.IncompleteTruncated},
		},
		{
			name:    "usage was never parsed",
			mutate:  func(e *recEvent) { e.usageOK, e.usage = false, Usage{} },
			wantAny: []string{record.IncompleteNoUsage},
		},
		{
			name: "two reasons compose",
			mutate: func(e *recEvent) {
				e.incomplete = record.IncompleteRetainCap
				e.respBody, e.respSize = nil, 0
				e.usageOK = false
			},
			wantAny: []string{record.IncompleteRetainCap, record.IncompleteNoUsage},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, dir := newTestRecorder(t)
			e := baseEvent(testSessionUUID, []byte(testSSE), true)
			c.mutate(e)
			recordAndClose(t, r, e)

			spans := chatSpans(t, readOneTrace(t, dir))
			if len(spans) != 1 {
				t.Fatalf("want 1 chat span, got %d", len(spans))
			}
			inc, _ := spanAttrs(t, spans[0])[record.AttrIncomplete].(string)
			if inc == "" {
				t.Fatal("no incomplete reason recorded: the turn silently claims to be whole")
			}
			for _, w := range c.wantAny {
				if !strings.Contains(inc, w) {
					t.Errorf("incomplete = %q, want it to contain %q", inc, w)
				}
			}
		})
	}
}

// joinIncomplete must not duplicate a reason it already carries, or a long multi-round
// response would accumulate "tee_cap_reached,tee_cap_reached,...".
func TestJoinIncompleteComposesWithoutDuplicating(t *testing.T) {
	got := joinIncomplete(record.IncompleteNone, record.IncompleteRetainCap)
	if got != record.IncompleteRetainCap {
		t.Errorf("first reason = %q, want %q", got, record.IncompleteRetainCap)
	}
	got = joinIncomplete(got, record.IncompleteTruncated)
	if got != record.IncompleteRetainCap+","+record.IncompleteTruncated {
		t.Errorf("second reason did not append: %q", got)
	}
	if again := joinIncomplete(got, record.IncompleteRetainCap); again != got {
		t.Errorf("a repeated reason must not be appended twice: %q", again)
	}
	if none := joinIncomplete(got, record.IncompleteNone); none != got {
		t.Errorf("IncompleteNone must be a no-op, got %q", none)
	}
}

// --- 6. One bad turn must not kill the writer -----------------------------------------
func TestRecorderSurvivesAPanickingTurn(t *testing.T) {
	r, dir := newTestRecorder(t)

	for _, junk := range [][]byte{
		[]byte("this is not json at all"),
		[]byte(`{"messages":"not an array"}`),
		{},
		[]byte(`{"metadata":{"user_id":12345},"messages":[[[]]]}`),
		[]byte(`{"model":null,"max_tokens":"lots","tools":42}`),
	} {
		e := baseEvent(testSessionUUID, []byte(testSSE), true)
		e.reqBody = junk
		r.Record(e)
	}
	// Then a good turn, which must still land: that is what proves the goroutine lived.
	recordAndClose(t, r, baseEvent(testSessionUUID, []byte(testSSE), true))

	spans := readOneTrace(t, dir)
	if len(spans) == 0 {
		t.Fatal("the writer goroutine died: no spans survived")
	}
	found := false
	for _, s := range spans {
		if out, _ := spanAttrs(t, s)["gen_ai.output.messages"].(string); strings.Contains(out, "Hello there") {
			found = true
		}
	}
	if !found {
		t.Error("the good turn never reached disk after the malformed ones")
	}
}

// --- 7. Windows: a tenant-scoped id must not become an NTFS alternate data stream -----
//
// "tenant:base.otel.json" on NTFS creates a hidden stream on a file called "tenant", not a
// file with that name: no error, no visible file, no clue. apply.SessionIDFor returns exactly
// that shape, so this is the most likely Windows-specific failure in the change.
func TestRecordFilenameIsWindowsSafe(t *testing.T) {
	r, dir := newTestRecorder(t)
	recordAndClose(t, r, baseEvent(`tenant-a:sess/../b\c*d?e`, []byte(testSSE), true))

	files := traceFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("want 1 trace file, got %v -- a colon in the id likely created an NTFS "+
			"alternate data stream instead of a file", files)
	}
	base := filepath.Base(files[0])
	for _, bad := range []string{":", "/", `\`, "*", "?"} {
		if strings.Contains(base, bad) {
			t.Errorf("filename %q still contains %q", base, bad)
		}
	}
	if fi, err := os.Stat(files[0]); err != nil || fi.Size() == 0 {
		t.Errorf("trace file is not a readable non-empty file: err=%v", err)
	}
}

func TestSanitizeTraceIDNeverYieldsAnUnusableName(t *testing.T) {
	for _, in := range []string{"", ".", "..", "   ", "...", "\t"} {
		got := sanitizeTraceID(in)
		if got == "" || strings.Trim(got, ".") == "" {
			t.Errorf("sanitizeTraceID(%q) = %q, which yields a hidden or invalid filename", in, got)
		}
	}
	if got := sanitizeTraceID(testSessionUUID); got != testSessionUUID {
		t.Errorf("a real UUID must pass through unchanged, got %q", got)
	}
}

// A nil recorder must be inert at every entry point: that is the off switch, and every
// non-recording call site depends on it.
func TestNilRecorderIsInert(t *testing.T) {
	var r *sessionRecorder
	r.Record(&recEvent{traceID: "x"})
	if err := r.Close(); err != nil {
		t.Errorf("nil Close: %v", err)
	}
	if r.Dir() != "" {
		t.Error("nil Dir should be empty")
	}
	got, err := newSessionRecorder("", 0, 0, 0, quietRecLog())
	if got != nil || err != nil {
		t.Errorf("an empty dir must yield (nil, nil), got (%v, %v)", got, err)
	}
}

// A file already on disk for this session must not be overwritten. A session spanning a proxy
// restart would otherwise lose everything: the new process starts with an empty span list, and
// its first flush would replace a complete trace with a one-span one.
func TestRecorderDoesNotClobberAnExistingTrace(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, testSessionUUID+".otel.json")
	const marker = "pretend this is a long completed session"
	if err := os.WriteFile(existing, []byte(`{"spans":["`+marker+`"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := newSessionRecorder(dir, 1<<20, 8<<20, 16, quietRecLog())
	if err != nil {
		t.Fatal(err)
	}
	recordAndClose(t, r, baseEvent(testSessionUUID, []byte(testSSE), true))

	raw, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), marker) {
		t.Fatal("the pre-existing trace file was overwritten: a proxy restart would destroy " +
			"the earlier half of a session")
	}
	if files := traceFiles(t, dir); len(files) != 2 {
		t.Errorf("want the original plus a pid-suffixed sibling, got %v", files)
	}
}

// firstByteOrZero must return 0 for an unobserved first byte rather than calling msSince with
// a zero time, which treats zero as "now" and would report the whole request as the TTFB.
func TestFirstByteOrZeroDistinguishesUnobservedFromInstant(t *testing.T) {
	start := time.Now().Add(-3 * time.Second)
	if got := firstByteOrZero(start, time.Time{}); got != 0 {
		t.Errorf("an unobserved first byte must be 0, got %d (the whole request duration)", got)
	}
	if got := firstByteOrZero(start, start.Add(250*time.Millisecond)); got != 250 {
		t.Errorf("observed first byte = %d ms, want 250", got)
	}
	// Sub-millisecond must round to 1, not truncate to 0: 0 means "not observed".
	if got := firstByteOrZero(start, start.Add(600*time.Microsecond)); got == 0 {
		t.Error("a sub-millisecond first byte truncated to 0, which reads as 'not observed'")
	}
}

// --- 8. Derived turn spans -------------------------------------------------------------
//
// The recorder writes one cg.turn.derived span per reconstructed human turn, so a consumer --
// a downstream analyzer, for one -- reads turns straight off the file instead of re-running
// our Go reconstruction. These spans are DERIVED and say so; nothing here is written as
// claude_code.interaction, which would make a derived sequence number indistinguishable from
// a measured one.

func TestRecorderEmitsDerivedTurnSpans(t *testing.T) {
	r, dir := newTestRecorder(t)
	e1 := baseEvent(testSessionUUID, []byte(testSSE), true)
	e2 := baseEvent(testSessionUUID, []byte(testSSE), true)
	// Turn 2 REPLAYS turn 1, which is the property the whole reconstruction rests on: the
	// proxy never saw turn 1 announced, it saw it inside turn 2's message list.
	e2.reqBody = ccBodyTurns("fix the failing test", "now add a regression test")
	recordAndClose(t, r, e1, e2)

	spans := readOneTrace(t, dir)
	chats, turns := chatSpans(t, spans), derivedTurnSpans(t, spans)
	if len(chats) != 2 {
		t.Fatalf("want 2 chat spans, got %d", len(chats))
	}
	// Two calls replaying one shared turn must yield TWO turns, not three: de-duplication
	// within a conversation root is what keeps a 3-turn session from reading as 21.
	if len(turns) != 2 {
		t.Fatalf("want 2 derived turn spans, got %d (%d spans in the file)", len(turns), len(spans))
	}
	want := []string{"fix the failing test", "now add a regression test"}
	for i, sp := range turns {
		a := spanAttrs(t, sp)
		if a[record.AttrTurnDerived] != true {
			t.Errorf("turn %d is not marked derived: %v", i, a[record.AttrTurnDerived])
		}
		if a[record.AttrTurnMethod] != record.TurnMethodBodyRecon {
			t.Errorf("turn %d has no provenance: %v", i, a[record.AttrTurnMethod])
		}
		if got, _ := a[record.AttrTurnPrompt].(string); got != want[i] {
			t.Errorf("turn %d prompt = %q, want %q", i, got, want[i])
		}
		if seq, _ := a[record.AttrTurnSequence].(float64); int(seq) != i+1 {
			t.Errorf("turn %d sequence = %v, want %d", i, a[record.AttrTurnSequence], i+1)
		}
		// A reconstruction knows one instant and must not invent a duration: the gap between
		// two calls mixes the human thinking with the agent working locally, and this side
		// cannot separate them. DerivedTurnSpan enforces this by writing one timestamp to
		// both fields, so this assertion guards that library invariant, not our call into it.
		if sp["start_time"] != sp["end_time"] {
			t.Errorf("turn %d carries a duration (%v..%v); interaction.duration_ms is "+
				"structurally out of reach from a proxy and must not be guessed",
				i, sp["start_time"], sp["end_time"])
		}
		if id, _ := sp["span_id"].(string); id == "" {
			t.Errorf("turn %d has no span id", i)
		}
	}
}

// A periodic flush rewrites the whole file every 10 s, so spansWithDerivedTurns is called
// again and again over a growing span list. Derived spans must be REPLACED, not accumulated,
// and each turn must keep its id -- otherwise a session flushed for an hour carries 360
// copies of turn 1, each under a new id, and a consumer diffing two reads sees churn where
// nothing changed. This is why the derived spans are composed at write time and never stored.
func TestDerivedTurnSpansAreIdempotentAcrossFlushes(t *testing.T) {
	r, _ := newTestRecorder(t)
	defer r.Close()

	// Spare capacity, as a real session's append-grown span list has. Noted because it was
	// checked: aliasing the stored slice (out := s.spans) is a latent hazard -- it scribbles
	// into the backing array past len -- but it does NOT duplicate, because the stored header's
	// length never moves, so the next flush recomposes from the same spans. No behavioural
	// test can catch it, and this one does not claim to.
	s := &recSession{traceID: testSessionUUID, spans: make([]record.Span, 0, 16)}
	for _, body := range [][]byte{
		ccBody("fix the failing test"),
		ccBodyTurns("fix the failing test", "now add a regression test"),
	} {
		e := baseEvent(testSessionUUID, []byte(testSSE), true)
		e.reqBody = body
		turn, err := r.buildTurn(e, "")
		if err != nil {
			t.Fatalf("buildTurn: %v", err)
		}
		s.spans = append(s.spans, turn.ToSpan())
	}

	// Flush 1, then flush 2 with nothing new observed in between. The second must produce
	// exactly the first -- same count, same ids, same order. Anything else means a session
	// flushed every 10 s for an hour accumulates 360 copies of turn 1, or re-ids it so a
	// consumer diffing two reads sees churn where nothing changed.
	first := append([]record.Span(nil), r.spansWithDerivedTurns(s)...)
	second := r.spansWithDerivedTurns(s)

	if len(first) != 4 {
		t.Fatalf("want 2 observed + 2 derived = 4 spans, got %d", len(first))
	}
	if len(first) != len(second) {
		t.Fatalf("the second flush produced a different span count: %d then %d; derived spans "+
			"are being accumulated instead of recomposed", len(first), len(second))
	}
	for i := range first {
		if first[i].SpanID != second[i].SpanID || first[i].Name != second[i].Name {
			t.Errorf("span %d differs between flushes: %q/%q then %q/%q",
				i, first[i].Name, first[i].SpanID, second[i].Name, second[i].SpanID)
		}
	}
	if n := len(derivedSpansOf(first)); n != 2 {
		t.Errorf("want 2 derived spans in a flush, got %d", n)
	}

	// Now the case the periodic flush really is: a THIRD call arrives and the session is
	// flushed again. The derived set must be 3 -- recomposed -- not 2+3=5 accumulated, and
	// the two turns already written must keep the ids they had.
	e := baseEvent(testSessionUUID, []byte(testSSE), true)
	e.reqBody = ccBodyTurns("fix the failing test", "now add a regression test", "and ship it")
	turn, err := r.buildTurn(e, "")
	if err != nil {
		t.Fatalf("buildTurn: %v", err)
	}
	s.spans = append(s.spans, turn.ToSpan())

	third := r.spansWithDerivedTurns(s)
	gotD := derivedSpansOf(third)
	if len(gotD) != 3 {
		t.Fatalf("after a 3rd call the flush has %d derived spans, want 3 (accumulated rather "+
			"than recomposed?)", len(gotD))
	}
	if len(third) != 6 {
		t.Errorf("want 3 observed + 3 derived = 6 spans, got %d", len(third))
	}
	wasD := derivedSpansOf(first)
	for i := range wasD {
		if wasD[i].SpanID != gotD[i].SpanID {
			t.Errorf("turn %d was re-identified when a later turn arrived: %q then %q; a "+
				"consumer diffing two reads would see churn where nothing changed",
				i+1, wasD[i].SpanID, gotD[i].SpanID)
		}
	}
	// Distinct ids, or two turns collapse into one row downstream.
	ids := map[string]bool{}
	for _, sp := range first {
		if sp.SpanID == "" {
			t.Fatal("a span has no id")
		}
		if ids[sp.SpanID] {
			t.Errorf("duplicate span id %q", sp.SpanID)
		}
		ids[sp.SpanID] = true
	}
}

// derivedSpansOf filters record.Span values (the in-memory form), as opposed to
// derivedTurnSpans which filters the decoded JSON a test read back off disk.
func derivedSpansOf(spans []record.Span) []record.Span {
	var out []record.Span
	for _, sp := range spans {
		if sp.Name == record.SpanDerivedTurn {
			out = append(out, sp)
		}
	}
	return out
}

// Fail open: a session whose calls advertise no tools has no conversation anchor, so
// reconstruction yields nothing. The observed spans must still be written unchanged -- a file
// missing its derived turns is degraded, a missing file is a lost session.
func TestDerivedTurnsFailOpenWithoutAnAnchor(t *testing.T) {
	r, dir := newTestRecorder(t)
	e := baseEvent(testSessionUUID, []byte(testSSE), true)
	e.reqBody = ccBodyNoTools("a prompt on a call advertising no tools")
	recordAndClose(t, r, e)

	spans := readOneTrace(t, dir)
	if got := len(chatSpans(t, spans)); got != 1 {
		t.Fatalf("want the observed chat span written regardless, got %d", got)
	}
	if got := len(derivedTurnSpans(t, spans)); got != 0 {
		t.Errorf("want no derived turns without an anchor, got %d; an unanchored call is a "+
			"helper call (a title generator, a WebFetch-fed thread), not a human turn", got)
	}
}
