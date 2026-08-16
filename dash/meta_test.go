package dash

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// fakeKeyShaped builds a string with the SHAPE of a credential without being one. Built
// from parts at run time so no credential-looking literal is committed to the repo, and so
// nothing here can ever be mistaken for a real secret.
func fakeKeyShaped() string { return "sk-" + strings.Repeat("A1b2", 8) }

// A metadata field is a value the CLIENT chose, so it is attacker-influenced input that
// happens to arrive in a small field rather than in a transcript. A request carrying
// `"reasoning_effort": "<key-shaped string>"` must never write that string to the database
// — the invariant is redact BEFORE the insert, so this drives the real capture pipeline
// (Recorder.Record → writer goroutine → INSERT) rather than calling the redactor directly.
func TestCredentialShapedMetadataNeverReachesTheDB(t *testing.T) {
	rec, err := NewRecorder(Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	defer rec.Close()

	key := fakeKeyShaped()
	jwtish := "eyJhbGciOiJIUzI1NiJ9." + strings.Repeat("ab12", 4) + "." + strings.Repeat("cd34", 4)
	rec.Record(&Event{
		TS: time.Now().UnixMilli(), SessionID: "s", Model: "m", TokenAccounting: AccountingComplete,
		Meta: Meta{
			ReasoningEffort: key,
			ThinkingMode:    jwtish,
			ToolChoice:      "AKIA" + strings.Repeat("Z", 12) + "QRST",
			StopReason:      strings.Repeat("x", 400), // over-long: not a credential shape, still refused
		},
	})

	e := waitRow(t, rec)
	for name, got := range map[string]string{
		"reasoning_effort": e.ReasoningEffort,
		"thinking_mode":    e.ThinkingMode,
		"tool_choice":      e.ToolChoice,
		"stop_reason":      e.StopReason,
	} {
		if got != Redacted {
			t.Errorf("%s = %q, want %q", name, got, Redacted)
		}
	}
	// And the raw values are nowhere in those columns, however they were mangled: a
	// PARTIALLY scrubbed value would still be a leak.
	var blob string
	q := `SELECT reasoning_effort || '|' || thinking_mode || '|' || tool_choice || '|' || stop_reason FROM requests`
	if err := rec.DB().sql.QueryRow(q).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{key, jwtish, "AKIA"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("a credential-shaped value reached the database: %q contains %q", blob, secret)
		}
	}
}

// The gate must not eat legitimate values, including ones this build has never heard of.
// A provider that ships a new effort level or stop reason has to show up as itself — a
// dashboard that reports «redacted» for ordinary data is lying about its own coverage.
func TestMetaEnumKeepsLegitimateValues(t *testing.T) {
	for _, v := range []string{
		"low", "medium", "high", "xhigh", "max", // effort, today's ladder
		"adaptive", "enabled", "disabled", // thinking.type
		"end_turn", "max_tokens", "tool_use", "stop_sequence", "pause_turn", "refusal",
		"model_context_window_exceeded",                  // the longest real value: 29 chars
		"stop", "length", "tool_calls", "content_filter", // OpenAI finish_reason
		"auto", "any", "none", "required", "tool", "function", // tool_choice
		"ultra_effort_9000", // unknown to this build, harmless, must survive
	} {
		if got := metaEnum(v); got != v {
			t.Errorf("metaEnum(%q) = %q, want it kept verbatim", v, got)
		}
	}
	if got := metaEnum(""); got != "" {
		t.Errorf("metaEnum(\"\") = %q, want the empty string (absence is not a value)", got)
	}
}

// waitRow drains the writer and returns the single stored row.
func waitRow(t *testing.T, rec *Recorder) *Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p, err := rec.DB().Requests(Filter{TenantAll: true}, 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Requests) == 1 {
			return p.Requests[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer did not persist the row (got %d)", len(p.Requests))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Every metadata field has to survive the round trip through SQLite unchanged — and
// "unset" has to stay distinguishable from zero, which is the whole reason the sampling
// columns are nullable.
func TestMetaRoundTripsThroughTheStore(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	zero, tenth := 0.0, 0.1
	full := Meta{
		ReasoningEffort: "xhigh", ThinkingMode: "enabled", ThinkingBudget: 8000,
		Temperature: &zero, TopP: &tenth, MaxTokens: 4096, Stream: true,
		ToolChoice: "auto", Tools: 7, SystemBlocks: 3,
		CacheBPSystem: 2, CacheBPTools: 1, CacheBPMessages: 1, CacheBPBlocks: 0,
		StopReason: "max_tokens",
	}
	set := mkEvent(1000, "s-set", "m", 100, 90)
	set.Meta = full
	unset := mkEvent(2000, "s-unset", "m", 100, 90)
	if err := db.insertBatch([]*Event{set, unset}); err != nil {
		t.Fatal(err)
	}

	got, err := db.Request(set.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Temperature == nil || *got.Temperature != 0 {
		t.Fatalf("temperature = %v, want a stored 0 — a request asking for determinism must not read as unspecified", got.Temperature)
	}
	// Compare the rest by value, with the pointers normalized away.
	got.Temperature, got.TopP = &zero, &tenth
	if got.Meta != full {
		t.Errorf("meta round trip = %+v, want %+v", got.Meta, full)
	}
	if got.CacheBreakpoints() != 4 {
		t.Errorf("CacheBreakpoints = %d, want 4", got.CacheBreakpoints())
	}

	none, err := db.Request(unset.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if none.Temperature != nil || none.TopP != nil {
		t.Errorf("an unset sampling parameter read back as %v/%v, want nil/nil", none.Temperature, none.TopP)
	}
}

// seedBreakdown writes four priced rows across two efforts and two breakpoint counts, with
// arithmetic simple enough to verify by hand.
func seedBreakdown(t *testing.T, db *DB) {
	t.Helper()
	mk := func(ts int64, session, effort string, bps int, cost, baseline, cgllm float64, complete bool) *Event {
		e := mkEvent(ts, session, "m", 100, 80)
		e.CostUSD, e.BaselineCostUSD, e.CGLLMCostUSD = cost, baseline, cgllm
		e.ReasoningEffort, e.CacheBPSystem = effort, bps
		if !complete {
			e.TokenAccounting = AccountingPartial
		}
		return e
	}
	// day 1 (1970-01-02): high effort, 2 breakpoints
	day1 := DayMs
	// day 2 (1970-01-03): low effort, 1 breakpoint, one of them unpriced
	day2 := 2 * DayMs
	if err := db.insertBatch([]*Event{
		mk(day1+1, "s1", "high", 2, 1.00, 3.00, 0.10, true),
		mk(day1+2, "s2", "high", 2, 0.50, 1.00, 0.00, true),
		mk(day2+1, "s3", "low", 1, 0.25, 0.25, 0.00, true),
		mk(day2+2, "s4", "low", 1, 0, 0, 0, false),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBreakdownSpentVsSaved(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedBreakdown(t, db)

	byEffort, err := db.Breakdown(Filter{TenantAll: true}, "reasoning_effort")
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]*GroupRow{}
	for _, g := range byEffort {
		rows[g.Key] = g
	}
	if len(rows) != 2 {
		t.Fatalf("got %d effort groups, want 2: %+v", len(rows), rows)
	}
	high := rows["high"]
	// spent = billed + our own spend = (1.00+0.50) + 0.10 = 1.60
	// saved = baseline − spent    = (3.00+1.00) − 1.60 = 2.40
	if !near(high.SpentUSD, 1.60) || !near(high.SavedUSD, 2.40) {
		t.Errorf("high: spent/saved = %v/%v, want 1.60/2.40", high.SpentUSD, high.SavedUSD)
	}
	if high.Requests != 2 || high.Sessions != 2 || high.TokensBefore != 200 || high.TokensAfter != 160 {
		t.Errorf("high: %+v, want 2 requests / 2 sessions / 200→160 tokens", high)
	}
	if high.Saved != 40 || high.SavedUnique != 40 {
		t.Errorf("high: saved/unique = %d/%d, want 40/40", high.Saved, high.SavedUnique)
	}
	low := rows["low"]
	// The unpriced row contributes nothing to the money and is COUNTED as unpriced, so
	// the UI can say "unknown" instead of drawing a zero.
	if !near(low.SpentUSD, 0.25) || !near(low.SavedUSD, 0) || low.Incomplete != 1 || low.Requests != 2 {
		t.Errorf("low: spent/saved/incomplete/requests = %v/%v/%d/%d, want 0.25/0/1/2",
			low.SpentUSD, low.SavedUSD, low.Incomplete, low.Requests)
	}

	// Breakpoint COUNT is a dimension in its own right: it is the placement question this
	// project exists to answer, asked in dollars.
	byBPs, err := db.Breakdown(Filter{TenantAll: true}, "cache_breakpoints")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, g := range byBPs {
		got[g.Key] = g.Requests
	}
	if got["2"] != 2 || got["1"] != 2 {
		t.Errorf("by breakpoint count = %v, want two rows at 2 and two at 1", got)
	}

	// The filter still applies, and it is the same filter every other query uses.
	only, err := db.Breakdown(Filter{TenantAll: true, Effort: "low"}, "reasoning_effort")
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Key != "low" {
		t.Errorf("filtered breakdown = %+v, want only the low group", only)
	}
}

// A mistyped dimension is an error, not a chart of some other dimension's numbers.
func TestBreakdownRejectsUnknownDimension(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Breakdown(Filter{TenantAll: true}, "r.model; DROP TABLE requests"); err == nil {
		t.Fatal("an unknown dimension must be refused, not interpolated into the statement")
	}
	// The allowlist is what the API offers, so the two cannot drift.
	for _, d := range BreakdownDims() {
		if _, err := db.Breakdown(Filter{TenantAll: true}, d); err != nil {
			t.Errorf("advertised dimension %q does not work: %v", d, err)
		}
	}
}

// Per-day usage bars are the shared series at a day-wide bucket — no new query, no rollup
// table. This pins the bucketing and the derived savings figure.
func TestDailySeriesBuckets(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedBreakdown(t, db)

	buckets, err := db.Series(Filter{TenantAll: true}, DayMs)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d day buckets, want 2: %+v", len(buckets), buckets)
	}
	if buckets[0].TS != DayMs || buckets[1].TS != 2*DayMs {
		t.Errorf("bucket timestamps = %d/%d, want %d/%d (UTC day boundaries)",
			buckets[0].TS, buckets[1].TS, DayMs, 2*DayMs)
	}
	if buckets[0].Requests != 2 || buckets[1].Requests != 2 {
		t.Errorf("requests per day = %d/%d, want 2/2", buckets[0].Requests, buckets[1].Requests)
	}
	// saved = baseline − cost − our own spend = 4.00 − 1.50 − 0.10 = 2.40
	if !near(buckets[0].SavedUSD, 2.40) {
		t.Errorf("day 1 saved_usd = %v, want 2.40", buckets[0].SavedUSD)
	}
	if buckets[0].TokensBefore != 200 || buckets[0].TokensAfter != 160 || buckets[0].Saved != 40 {
		t.Errorf("day 1 tokens = %d→%d (saved %d), want 200→160 (40)",
			buckets[0].TokensBefore, buckets[0].TokensAfter, buckets[0].Saved)
	}
	// A narrowed range is how the "selectable time range" works: same query, tighter filter.
	one, err := db.Series(Filter{TenantAll: true, Since: 2 * DayMs}, DayMs)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].TS != 2*DayMs {
		t.Errorf("range-limited series = %+v, want only day 2", one)
	}
}

func near(got, want float64) bool {
	d := got - want
	return d < 1e-9 && d > -1e-9
}

// The UI reads every metadata field as a TOP-LEVEL key, which holds only because Meta is
// embedded anonymously. Adding a `json:"meta"` tag to that field would nest all fifteen and
// silently blank them in the request drawer — with no compile error and nothing else
// failing. Hence this.
//
// It pins the null too: an unset sampling parameter has to serialize as `null`, not `0`, or
// the distinction the nullable column exists for dies at the JSON boundary.
func TestEventJSONFlattensMetaAndKeepsNull(t *testing.T) {
	f := 0.7
	b, err := json.Marshal(&Event{Meta: Meta{
		ReasoningEffort: "high", Temperature: &f, CacheBPSystem: 2, StopReason: "end_turn",
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		`"reasoning_effort":"high"`, `"temperature":0.7`, `"cache_bp_system":2`, `"stop_reason":"end_turn"`,
	} {
		if !strings.Contains(string(b), k) {
			t.Errorf("missing %s in %s", k, b)
		}
	}
	if strings.Contains(string(b), `"Meta"`) {
		t.Fatalf("Meta serialized nested rather than flat, which blanks it in the UI: %s", b)
	}
	unset, err := json.Marshal(&Event{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unset), `"temperature":null`) {
		t.Errorf("an unset temperature serialized as something other than null: %s", unset)
	}
}
