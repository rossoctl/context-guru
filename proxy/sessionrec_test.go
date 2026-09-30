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

func traceFiles(t *testing.T, dir string) []string {
	t.Helper()
	f, err := filepath.Glob(filepath.Join(dir, "*.otel.json"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(f)
	return f
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

	spans := readOneTrace(t, dir)
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
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
	// Note the two namespaces are not a slip: semconv has no field for the cache split, so
	// the totals are gen_ai.usage.* and the split is cg.usage.* -- which is the whole mooncake
	// unblock, since mooncake needs the split.
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
	f, spans := readTrace(t, filepath.Join(dir, testSessionUUID+".otel.json"))
	if len(spans) != 2 {
		t.Fatalf("want 2 spans for the 2-turn session, got %d", len(spans))
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

			spans := readOneTrace(t, dir)
			if len(spans) != 1 {
				t.Fatalf("want 1 span, got %d", len(spans))
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
