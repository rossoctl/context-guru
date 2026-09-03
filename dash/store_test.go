package dash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mkEvent builds a plausible captured request for the query tests.
func mkEvent(ts int64, session, model string, before, after int) *Event {
	return &Event{
		TS: ts, SessionID: session, Model: model, Provider: "anthropic",
		Agent: "claude-code", Preset: "codesmart", Mode: ModeActive,
		TokensBefore: before, TokensAfter: after, AttemptedTokens: before,
		SavedUnique: before - after, FreshInput: 10, CacheRead: 1000, CacheWrite: 100,
		OutputTokens: 50, CostUSD: 0.01, BaselineCostUSD: 0.02,
		CGLatencyMs: 5, UpstreamMs: 500, TokenAccounting: AccountingComplete,
		CacheMissReason: CacheHit,
		Components: []CompRow{
			{Component: "extract", Kind: "offload", Acted: before > after, Mutated: before > after,
				SavedGross: before - after, SavedUnique: before - after, DurationMs: 1.5},
			{Component: "cacheinject", Kind: "reformat", Mutated: true, DurationMs: 0.1},
		},
	}
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestInMemoryDatabasesAreIsolated pins the fix for a latent collision: Open(":memory:")
// used the DSN `file::memory:?cache=shared`, and under cache=shared the NAME identifies
// the database — so every in-memory dashboard in the process was the SAME database.
// Production opens one, so it was not a live bug, but it silently merged the history of
// two proxies both falling back to :memory:, and it leaked rows between :memory: tests,
// which is the flakiest class of failure there is.
func TestInMemoryDatabasesAreIsolated(t *testing.T) {
	a, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.insertBatch([]*Event{mkEvent(1000, "only-in-a", "m", 100, 90)}); err != nil {
		t.Fatal(err)
	}
	page, err := b.Requests(Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Errorf("the second in-memory DB sees %d rows from the first; they are the same database", page.Total)
	}
	// And the first must still hold its own row — a per-connection private database
	// would isolate them by losing the data instead.
	pa, err := a.Requests(Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if pa.Total != 1 {
		t.Errorf("the first in-memory DB holds %d rows; want the 1 just inserted "+
			"(a pooled connection must still see the write)", pa.Total)
	}
}

func TestSchemaMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{mkEvent(1000, "s1", "m", 100, 90)}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Re-opening the same file must find the schema already at version and keep data.
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	page, err := db2.Requests(Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("reopen lost data: total=%d, want 1", page.Total)
	}
}

func TestSchemaVersionMismatchPreservesOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "d.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{mkEvent(1000, "s1", "m", 100, 90)}); err != nil {
		t.Fatal(err)
	}
	// Pretend the file was written by a future version.
	if _, err := db.sql.Exec(`UPDATE meta SET value='9999' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("a version mismatch must start fresh, not fail: %v", err)
	}
	defer db2.Close()
	page, err := db2.Requests(Filter{}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 {
		t.Errorf("mismatch should start a FRESH database; found %d rows", page.Total)
	}
	// The user's old data must be preserved, not deleted.
	entries, _ := os.ReadDir(dir)
	found := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bak") {
			found = true
		}
	}
	if !found {
		t.Errorf("old database was not preserved alongside; files: %v", entries)
	}
}

func TestRetentionPrunesByAgeAndCascades(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()
	old := now.Add(-48 * time.Hour).UnixMilli()
	fresh := now.Add(-1 * time.Hour).UnixMilli()
	ev := mkEvent(old, "old", "m", 100, 90)
	ev.Content = []ContentRow{{Path: "messages.0", Before: "aaa", After: "a"}}
	if err := db.insertBatch([]*Event{ev, mkEvent(fresh, "new", "m", 100, 90)}); err != nil {
		t.Fatal(err)
	}
	oldID := ev.ID

	n, err := db.Prune(now, 24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows; want 1", n)
	}
	page, _ := db.Requests(Filter{}, 0, 10)
	if page.Total != 1 || page.Requests[0].SessionID != "new" {
		t.Fatalf("wrong row survived: %+v", page.Requests)
	}
	// Component and content rows must have gone with the request (ON DELETE CASCADE),
	// or the database grows without bound behind a retention policy that looks fine.
	var comps, content int
	db.sql.QueryRow(`SELECT COUNT(*) FROM request_components WHERE request_id=?`, oldID).Scan(&comps)
	db.sql.QueryRow(`SELECT COUNT(*) FROM request_content WHERE request_id=?`, oldID).Scan(&content)
	if comps != 0 || content != 0 {
		t.Errorf("orphaned rows after prune: %d component, %d content", comps, content)
	}
}

func TestRetentionPrunesBySize(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()
	// Write enough content to exceed a small size cap.
	big := strings.Repeat("some agent transcript text that compresses but not to nothing ", 400)
	var evs []*Event
	for i := 0; i < 60; i++ {
		e := mkEvent(now.Add(-time.Duration(60-i)*time.Minute).UnixMilli(), "s", "m", 1000, 900)
		e.Content = []ContentRow{{Path: "messages.0", Before: big, After: big[:200]}}
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	before, _ := db.sizeBytes()
	if before < 100<<10 {
		t.Skipf("test data only produced %d bytes; cannot exercise the size rule", before)
	}

	// Age rule off, size rule at a fraction of the current size.
	if _, err := db.Prune(now, 0, before/3); err != nil {
		t.Fatal(err)
	}
	after, _ := db.sizeBytes()
	if after >= before {
		t.Errorf("size prune did not shrink the database: %d -> %d", before, after)
	}
	page, _ := db.Requests(Filter{}, 0, 200)
	if page.Total == 0 {
		t.Error("size prune deleted everything; it should drop only the oldest rows")
	}
	// The survivors must be the NEWEST rows.
	if page.Total > 0 {
		var minTS int64
		db.sql.QueryRow(`SELECT MIN(ts) FROM requests`).Scan(&minTS)
		if minTS <= evs[0].TS {
			t.Errorf("oldest row survived a size prune (min ts %d)", minTS)
		}
	}
}

func TestKeysetPaginationCoversEveryRowExactlyOnce(t *testing.T) {
	db := openTestDB(t)
	const n = 37
	var evs []*Event
	for i := 0; i < n; i++ {
		evs = append(evs, mkEvent(int64(1000+i), "s", "m", 100, 90))
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}

	seen := map[int64]int{}
	cursor := int64(0)
	pages := 0
	for {
		page, err := db.Requests(Filter{}, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if page.Total != n {
			t.Errorf("page %d reported total %d; want %d", pages, page.Total, n)
		}
		for _, r := range page.Requests {
			seen[r.ID]++
		}
		// Newest first, strictly descending.
		for i := 1; i < len(page.Requests); i++ {
			if page.Requests[i].ID >= page.Requests[i-1].ID {
				t.Fatalf("page not strictly newest-first: %d then %d",
					page.Requests[i-1].ID, page.Requests[i].ID)
			}
		}
		if page.NextCursor == 0 {
			break
		}
		cursor = page.NextCursor
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != n {
		t.Errorf("paged over %d distinct rows; want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("row %d returned %d times; keyset pagination must not duplicate", id, c)
		}
	}
}

func TestFilterEveryDimension(t *testing.T) {
	db := openTestDB(t)
	a := mkEvent(1000, "sess-a", "model-a", 100, 50)
	a.Provider, a.Agent, a.Preset, a.Mode = "anthropic", "claude-code", "codesmart", ModeActive
	a.TokenAccounting = AccountingComplete
	b := mkEvent(5000, "sess-b", "model-b", 200, 200)
	b.Provider, b.Agent, b.Preset, b.Mode = "openai", "codex", "codesafe", ModeBypass
	b.TokenAccounting = AccountingPartial
	b.UncompressedReason = ReasonBypassed
	b.Components = []CompRow{{Component: "dedup", Kind: "offload"}}
	if err := db.insertBatch([]*Event{a, b}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		f    Filter
		want int64
	}{
		{"session", Filter{Session: "sess-a"}, 1},
		{"model", Filter{Model: "model-b"}, 1},
		{"provider", Filter{Provider: "anthropic"}, 1},
		{"agent", Filter{Agent: "codex"}, 1},
		{"preset", Filter{Preset: "codesmart"}, 1},
		{"mode", Filter{Mode: ModeBypass}, 1},
		{"accounting", Filter{Accounting: AccountingPartial}, 1},
		{"component", Filter{Component: "dedup"}, 1},
		{"component-shared", Filter{Component: "extract"}, 1},
		{"reason-bucket", Filter{Reason: ReasonBypassed}, 1},
		{"reason-compacted", Filter{Reason: "compacted"}, 1},
		{"since", Filter{Since: 2000}, 1},
		{"until", Filter{Until: 2000}, 1},
		{"since+until", Filter{Since: 900, Until: 6000}, 2},
		{"q-session", Filter{Q: "sess-b"}, 1},
		{"q-model", Filter{Q: "model-a"}, 1},
		{"q-agent", Filter{Q: "claude"}, 1},
		{"q-nomatch", Filter{Q: "nothing-here"}, 0},
		{"combined", Filter{Provider: "anthropic", Model: "model-b"}, 0},
		{"unset", Filter{}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := db.Requests(tc.f, 0, 50)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != tc.want {
				t.Errorf("total=%d, want %d", page.Total, tc.want)
			}
			if int64(len(page.Requests)) != tc.want {
				t.Errorf("rows=%d, want %d", len(page.Requests), tc.want)
			}
			// Every aggregate must accept the same filter without erroring.
			if _, err := db.Overview(tc.f); err != nil {
				t.Errorf("Overview: %v", err)
			}
			if _, _, err := db.Sessions(tc.f, 10, 0); err != nil {
				t.Errorf("Sessions: %v", err)
			}
			if _, err := db.Components(tc.f); err != nil {
				t.Errorf("Components: %v", err)
			}
			if _, err := db.Series(tc.f, 60000); err != nil {
				t.Errorf("Series: %v", err)
			}
		})
	}
}

func TestQueryTimeBucketing(t *testing.T) {
	db := openTestDB(t)
	// Three requests inside one minute, two in the next.
	base := int64(1_700_000_000_000)
	base -= base % 60000 // align so the assertion is about bucketing, not phase
	var evs []*Event
	for _, off := range []int64{0, 10_000, 59_999, 60_000, 119_000} {
		evs = append(evs, mkEvent(base+off, "s", "m", 100, 90))
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}

	minute, err := db.Series(Filter{}, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(minute) != 2 {
		t.Fatalf("60s bucketing produced %d buckets; want 2", len(minute))
	}
	if minute[0].Requests != 3 || minute[1].Requests != 2 {
		t.Errorf("bucket counts = %d,%d; want 3,2", minute[0].Requests, minute[1].Requests)
	}
	if minute[0].TS != base {
		t.Errorf("first bucket ts = %d; want the floor %d", minute[0].TS, base)
	}
	if minute[0].Saved != 30 {
		t.Errorf("bucket saved = %d; want 30 (3 requests x 10)", minute[0].Saved)
	}

	// A wider bucket must merge them without any schema change (no rollup tables).
	hour, err := db.Series(Filter{}, 3_600_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(hour) != 1 || hour[0].Requests != 5 {
		t.Errorf("hour bucketing = %d buckets, first has %d requests; want 1 bucket of 5", len(hour), hour[0].Requests)
	}
}

func TestContentRoundTripsCompressed(t *testing.T) {
	db := openTestDB(t)
	before := strings.Repeat("line of tool output\n", 200)
	e := mkEvent(1000, "s", "m", 500, 100)
	e.Content = []ContentRow{{Path: "messages.3", BeforeTokens: 500, AfterTokens: 100,
		Before: before, After: "line of tool output\n<<cg:abc>>"}}
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}

	// Without permission, no content at all — not empty strings that read as "nothing
	// was changed", but no rows.
	got, err := db.Request(e.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 0 {
		t.Errorf("withContent=false returned %d content rows", len(got.Content))
	}
	if len(got.Components) != 2 {
		t.Errorf("component rows = %d; want 2 (components are not gated)", len(got.Components))
	}

	got, err = db.Request(e.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content) != 1 {
		t.Fatalf("content rows = %d; want 1", len(got.Content))
	}
	if got.Content[0].Before != before {
		t.Error("before text did not survive the gzip round trip")
	}
	if got.Content[0].Path != "messages.3" {
		t.Errorf("path = %q", got.Content[0].Path)
	}
}

func TestOverviewDenominatorsAndSafety(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(1000, "s", "m", 1000, 800)
	e.AttemptedTokens, e.FrozenTokens = 400, 600
	e.SavedUnique = 200
	e.FreshInput, e.CacheWrite = 100, 300
	e.ExpandTokens, e.Expands = 50, 1
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if o.SavedGross != 200 || o.SavedUnique != 200 {
		t.Errorf("saved gross/unique = %d/%d", o.SavedGross, o.SavedUnique)
	}
	if o.SavedAdjusted != 150 {
		t.Errorf("adjusted saved = %d; want 150 (200 unique − 50 restored)", o.SavedAdjusted)
	}
	byKey := map[string]Denominator{}
	for _, d := range o.Denominators {
		byKey[d.Key] = d
		if d.Description == "" {
			t.Errorf("denominator %q has no description; every ratio must name its divisor", d.Key)
		}
	}
	if got := byKey["attempted"]; got.Denominator != 400 || got.Percent != 50 {
		t.Errorf("attempted denominator = %d, %.1f%%; want 400, 50%%", got.Denominator, got.Percent)
	}
	// new_input = 200 saved / (100 fresh + 300 cache-write + 200 saved) = 33.33%.
	if got := byKey["new_input"]; got.Denominator != 600 || !got.Available ||
		got.Percent < 33.3 || got.Percent > 33.4 {
		t.Errorf("new_input = %d denominator, %.2f%%, available=%v; want 600, ~33.33%%, true",
			got.Denominator, got.Percent, got.Available)
	}
	if got := byKey["whole_request"]; got.Denominator != 1000 {
		t.Errorf("whole_request denominator = %d; want 1000", got.Denominator)
	}
	if o.SafetyCost.FrozenTokens != 600 || o.SafetyCost.RestoredTokens != 50 {
		t.Errorf("safety cost = %+v", o.SafetyCost)
	}
	if len(o.Waterfall) < 4 {
		t.Errorf("waterfall has %d steps; want the full walk", len(o.Waterfall))
	}
}

// TestOverviewCountsInvalidConfigRequests pins the property the #118 incident showed was
// missing: a request forwarded uncompacted because its account's own configuration failed to
// build (proxy/tenancy.go's build() marks that row's preset "invalid" on purpose, rather than
// taking the account offline) must be visible from Overview, not only from a log line.
func TestOverviewCountsInvalidConfigRequests(t *testing.T) {
	db := openTestDB(t)
	ok1 := mkEvent(1000, "s1", "m", 100, 90)
	ok1.Preset = "codesmart"
	broken := mkEvent(2000, "s2", "m", 100, 100)
	broken.Preset = "invalid"
	ok2 := mkEvent(3000, "s3", "m", 100, 90)
	ok2.Preset = "housellm"
	if err := db.insertBatch([]*Event{ok1, broken, ok2}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if o.Requests != 3 {
		t.Fatalf("requests = %d, want 3", o.Requests)
	}
	if o.InvalidConfigRequests != 1 {
		t.Errorf("invalid_config_requests = %d, want 1", o.InvalidConfigRequests)
	}
}

// A resolved config incident must stop raising the alarm, which is what InvalidConfigRecent is for.
//
// The dashboard's banner is present-tense — "open Settings and fix the configuration" — but it was
// driven by a count over the filter's window, and the default view has NO window. So on this
// deployment 1,752 invalid-config requests from a single afternoon kept the banner up for days
// after the config was fixed, telling every viewer to go and repair something already correct. An
// alarm that cannot switch itself off is one people learn to ignore.
func TestInvalidConfigRecentIgnoresResolvedHistory(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()
	// A burst of breakage well in the past, and nothing wrong since.
	old := mkEvent(now.Add(-48*time.Hour).UnixMilli(), "s-old", "m", 100, 100)
	old.Preset = "invalid"
	fine := mkEvent(now.Add(-time.Minute).UnixMilli(), "s-now", "m", 100, 90)
	fine.Preset = "codesmart"
	if err := db.insertBatch([]*Event{old, fine}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if o.InvalidConfigRequests != 1 {
		t.Fatalf("invalid_config_requests = %d, want 1 (the historical fact must still be reported)",
			o.InvalidConfigRequests)
	}
	if o.InvalidConfigRecent != 0 {
		t.Errorf("invalid_config_recent = %d, want 0: a config fixed two days ago is not a live "+
			"problem, and the banner keyed on this must clear itself", o.InvalidConfigRecent)
	}

	// ...and a breakage happening NOW must still raise it, or the fix has removed the alarm.
	live := mkEvent(now.Add(-2*time.Minute).UnixMilli(), "s-live", "m", 100, 100)
	live.Preset = "invalid"
	if err := db.insertBatch([]*Event{live}); err != nil {
		t.Fatal(err)
	}
	if o, err = db.Overview(Filter{}); err != nil {
		t.Fatal(err)
	}
	if o.InvalidConfigRecent != 1 {
		t.Errorf("invalid_config_recent = %d, want 1: a configuration failing to build right now is "+
			"exactly what this banner exists to surface", o.InvalidConfigRecent)
	}
}

// TestNewInputRatioNeverDividesSavingsByThemselves is the guard the issue calls
// non-negotiable: with no provider usage data the denominator would be `saved`
// alone and the ratio would read ~100%. It must read n/a.
func TestNewInputRatioNeverDividesSavingsByThemselves(t *testing.T) {
	db := openTestDB(t)
	e := mkEvent(1000, "s", "m", 1000, 500)
	e.FreshInput, e.CacheRead, e.CacheWrite, e.OutputTokens = 0, 0, 0, 0
	e.SavedUnique = 500
	e.TokenAccounting = AccountingPartial
	if err := db.insertBatch([]*Event{e}); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range o.Denominators {
		if d.Key != "new_input" {
			continue
		}
		if d.Available {
			t.Fatalf("new_input reported as available with no usage data (%.1f%%)", d.Percent)
		}
		if d.Percent != 0 {
			t.Errorf("new_input percent = %.1f; want 0 with a clear unavailable flag", d.Percent)
		}
		return
	}
	t.Fatal("new_input denominator missing")
}

func TestComponentsAggregateAndOvercount(t *testing.T) {
	db := openTestDB(t)
	// The same compaction re-sent three turns: gross triples, unique stays put.
	var evs []*Event
	for i := 0; i < 3; i++ {
		e := mkEvent(int64(1000+i), "s", "m", 1000, 700)
		e.Components = []CompRow{{Component: "extract", Kind: "offload", Acted: true, Mutated: true,
			SavedGross: 300, SavedUnique: map[bool]int{true: 300, false: 0}[i == 0], DurationMs: 2}}
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Components(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("component rows = %d; want 1", len(rows))
	}
	c := rows[0]
	if c.Runs != 3 || c.Acted != 3 || c.SavedGross != 900 || c.SavedUnique != 300 {
		t.Errorf("aggregation wrong: %+v", c)
	}
	if c.OvercountRatio != 3 {
		t.Errorf("overcount ratio = %v; want 3 (900 gross ÷ 300 unique)", c.OvercountRatio)
	}
	if c.DurationMsTotal != 6 || c.DurationMsAvg != 2 {
		t.Errorf("latency = %v total, %v avg", c.DurationMsTotal, c.DurationMsAvg)
	}
	if c.ActRate != 1 {
		t.Errorf("act rate = %v; want 1", c.ActRate)
	}
}

func TestSessionsAggregate(t *testing.T) {
	db := openTestDB(t)
	if err := db.insertBatch([]*Event{
		mkEvent(1000, "sess-1", "m", 1000, 900),
		mkEvent(2000, "sess-1", "m", 1000, 800),
		mkEvent(3000, "sess-2", "m", 500, 500),
	}); err != nil {
		t.Fatal(err)
	}
	rows, total, err := db.Sessions(Filter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("sessions = %d rows, total %d; want 2/2", len(rows), total)
	}
	// Most recently active first.
	if rows[0].SessionID != "sess-2" {
		t.Errorf("first session = %q; want sess-2 (most recent)", rows[0].SessionID)
	}
	var s1 *SessionRow
	for _, r := range rows {
		if r.SessionID == "sess-1" {
			s1 = r
		}
	}
	if s1 == nil {
		t.Fatal("sess-1 missing")
	}
	if s1.Turns != 2 || s1.Saved != 300 || s1.Start != 1000 || s1.End != 2000 {
		t.Errorf("sess-1 = %+v", s1)
	}
	// Pagination.
	page2, _, err := db.Sessions(Filter{}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].SessionID != "sess-1" {
		t.Errorf("offset paging wrong: %+v", page2)
	}
}

func TestPercentileExact(t *testing.T) {
	db := openTestDB(t)
	var evs []*Event
	for i := 1; i <= 100; i++ {
		e := mkEvent(int64(1000+i), "s", "m", 100, 90)
		e.CGLatencyMs = float64(i)
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// index = floor(99 * 0.95) = 94 -> the 95th smallest value, which is 95.
	if o.CGLatencyMsP95 != 95 {
		t.Errorf("p95 = %v; want 95", o.CGLatencyMsP95)
	}
	if o.CGLatencyMsAvg != 50.5 {
		t.Errorf("avg = %v; want 50.5", o.CGLatencyMsAvg)
	}
}

func TestInMemoryModeWorks(t *testing.T) {
	for _, path := range []string{"", ":memory:"} {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("Open(%q): %v", path, err)
		}
		if err := db.insertBatch([]*Event{mkEvent(1000, "s", "m", 100, 90)}); err != nil {
			t.Errorf("insert into in-memory db: %v", err)
		}
		if db.Path() != "" {
			t.Errorf("in-memory Path() = %q; want empty", db.Path())
		}
		db.Close()
	}
}

func TestUnwritablePathDegradesToMemory(t *testing.T) {
	// A path under a file (not a directory) can never be created.
	f := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecorder(Options{DBPath: filepath.Join(f, "nested", "d.db")})
	if err != nil {
		t.Fatalf("an unwritable dashboard path must NOT stop the proxy: %v", err)
	}
	defer rec.Close()
	if rec.DB().Path() != "" {
		t.Errorf("expected the in-memory fallback; got path %q", rec.DB().Path())
	}
}
