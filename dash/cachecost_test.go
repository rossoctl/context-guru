package dash

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// ibmSonnet is the rate litellm-gateway actually bills for aws/claude-sonnet-5 — half the
// public API's, which is why the operator price list exists at all.
var ibmSonnet = modelinfo.Price{Input: 1.52e-6, Output: 7.6e-6, CacheRead: 1.52e-7, CacheWrite: 1.9e-6}

// The prompt cache is a saving of its own, and the dashboard reported none of it.
func TestCacheSavedIsCacheReadsAgainstTheFreshRate(t *testing.T) {
	e := &Event{TS: 1, SessionID: "s", Model: "m", TokensBefore: 100, TokensAfter: 100,
		FreshInput: 1_000, CacheRead: 200_000, CacheWrite: 0, OutputTokens: 100}
	e.Price(ibmSonnet, true)
	want := 200_000 * (ibmSonnet.Input - ibmSonnet.CacheRead)
	if math.Abs(e.CacheSavedUSD-want) > 1e-12 {
		t.Fatalf("cache_saved_usd = %.10f, want %.10f", e.CacheSavedUSD, want)
	}
	// It must never be part of the compaction counterfactual: this request removed
	// nothing, so its baseline equals its cost.
	if math.Abs(e.BaselineCostUSD-e.CostUSD) > 1e-15 {
		t.Fatalf("cache savings leaked into the baseline: %.10f vs %.10f", e.BaselineCostUSD, e.CostUSD)
	}
	// A request with no rates at all reports nothing rather than zero dollars saved.
	u := &Event{TS: 2, SessionID: "s", Model: "m", CacheRead: 200_000}
	u.Price(modelinfo.Price{}, true)
	if u.CacheSavedUSD != 0 || u.TokenAccounting == AccountingComplete {
		t.Fatalf("an unpriced request reported cache savings: %v / %s", u.CacheSavedUSD, u.TokenAccounting)
	}
}

// The aggregate: our two savings add, and the provider's does not join them.
func TestOverviewClaimsOnlyTheTurnsWhoseTailMoved(t *testing.T) {
	const stable = 8_478
	db := openTestDB(t)
	var evs []*Event
	for i := range 4 {
		e := &Event{TS: int64(1000 + i), SessionID: "s", Model: "aws/claude-sonnet-5",
			TokensBefore: 50_000, TokensAfter: 49_000, SavedUnique: 1_000,
			FreshInput: 10, CacheRead: 54_304, CacheWrite: 0, OutputTokens: 20,
			// cachesplit acts on EVERY turn of a session — it is the same system prompt
			// every time. So the component test alone cannot distinguish the turn that did
			// the work from the three that rode along.
			Components:        []CompRow{{Component: "cachesplit", Kind: "reformat", Acted: true, Mutated: true}},
			SplitStableTokens: stable,
			// One turn in four moved the snapshot — the agent committed. The other three
			// would have been cache reads whatever we did.
			TailChanged: i == 0,
		}
		e.Price(ibmSonnet, true)
		// The split's credit is the accountant library's (a split effect, on the turn whose tail
		// moved): stored as written. The Overview sums it.
		if e.TailChanged {
			e.CachesplitSavedUSD = float64(stable) * (ibmSonnet.CacheWrite - ibmSonnet.CacheRead)
		}
		evs = append(evs, e)
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	want := float64(stable) * (ibmSonnet.CacheWrite - ibmSonnet.CacheRead)
	if math.Abs(o.CachesplitSavedUSD-want) > 1e-12 {
		t.Errorf("cachesplit_saved_usd = %.10f, want ONE request's worth %.10f", o.CachesplitSavedUSD, want)
	}
	// The provider's figure counts all four requests' whole reads, and is an order of
	// magnitude larger. Both are reported; only one is added to the total.
	if o.CacheSavedUSD <= o.CachesplitSavedUSD*10 {
		t.Errorf("the provider figure (%.10f) should dwarf ours (%.10f)", o.CacheSavedUSD, o.CachesplitSavedUSD)
	}
	if math.Abs(o.TotalSavedUSD-(o.NetSavedUSD+o.CachesplitSavedUSD)) > 1e-15 {
		t.Errorf("total_saved_usd = %.10f, want net %.10f + ours %.10f",
			o.TotalSavedUSD, o.NetSavedUSD, o.CachesplitSavedUSD)
	}
	steps := map[string]float64{}
	for _, s := range o.Waterfall {
		steps[s.Key] = s.DeltaUSD
	}
	if _, ok := steps["cache_saved"]; ok {
		t.Error("the provider's cache saving is still a step in OUR savings waterfall")
	}
	if math.Abs(steps["cachesplit_saved"]+o.CachesplitSavedUSD) > 1e-15 ||
		math.Abs(steps["total_saved"]-o.TotalSavedUSD) > 1e-15 {
		t.Errorf("waterfall does not carry the prefix saving: %v", steps)
	}
	// Per-model too, since that is where a wrong rate shows up first.
	groups, err := db.Breakdown(Filter{}, "model")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || math.Abs(groups[0].CachesplitSavedUSD-want) > 1e-12 {
		t.Errorf("per-model prefix saving is wrong: %+v", groups)
	}
}

// A new column must not cost the history. The version-bump path renames the file aside
// and discards every request row; on the live service that is a hundred thousand rows to
// gain one column, so these are added by ALTER TABLE on open instead. This opens a database
// that predates every one of them and checks both halves: the columns appear, and the rows
// are still there.
//
// It is the migration the deployed service actually runs on restart, so every entry in
// additiveColumns for `requests` belongs in the list below.
func TestAdditiveColumnKeepsExistingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{{TS: 1, SessionID: "s", Model: "m", TokensBefore: 10}}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Drop the columns back off to simulate the on-disk shape before these changes.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	added := []string{"cache_saved_usd", "cachesplit_saved_usd", "split_stable_tokens", "project", "price_estimated"}
	// An old database has no project index, and SQLite refuses to drop an indexed column.
	if _, err := raw.Exec(`DROP INDEX IF EXISTS idx_requests_project`); err != nil {
		t.Fatal(err)
	}
	for _, col := range added {
		if _, err := raw.Exec(`ALTER TABLE requests DROP COLUMN ` + col); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening a database without the column failed: %v", err)
	}
	defer db2.Close()
	var n int64
	if err := db2.sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows survived the migration, want 1", n)
	}
	for _, col := range added {
		// COUNT rather than SUM: `project` is TEXT, and a numeric aggregate over it would
		// succeed by coercing rather than by the column existing.
		var n2 int64
		if err := db2.sql.QueryRow(`SELECT COUNT(` + col + `) FROM requests`).Scan(&n2); err != nil {
			t.Fatalf("%s was not added back: %v", col, err)
		}
	}
	// A pre-migration row reads as an exact figure: that is the claim every such row always made.
	var est int
	if err := db2.sql.QueryRow(`SELECT price_estimated FROM requests LIMIT 1`).Scan(&est); err != nil || est != 0 {
		t.Errorf("a row written before price_estimated reads %d (err %v), want 0", est, err)
	}
	// A pre-migration row reads as the default, which for `project` means "unattributed" rather
	// than any real folder — the distinction the whole dimension depends on.
	var proj string
	if err := db2.sql.QueryRow(`SELECT project FROM requests LIMIT 1`).Scan(&proj); err != nil {
		t.Fatalf("reading project off a pre-migration row: %v", err)
	}
	if proj != "" {
		t.Errorf("a row written before the column reads project=%q; want \"\", which is how an "+
			"unattributed request is distinguished from one in a folder actually named that", proj)
	}
	// And the whole read path works over rows that predate the columns — a query naming a
	// column the file has just gained is where a migration that "succeeded" still fails.
	if _, err := db2.Overview(Filter{}); err != nil {
		t.Fatalf("Overview over migrated rows: %v", err)
	}
	if _, err := db2.Requests(Filter{}, 10, 0); err != nil {
		t.Fatalf("Requests over migrated rows: %v", err)
	}
}

// Gates are the answer to "act rate 0%, why?" and they never reached the dashboard: they
// were in /stats service-wide and in the log line per request, and the components table
// showed a row of zeros. This pins the round trip, per request and aggregated.
func TestGatesReachTheDashboard(t *testing.T) {
	db := openTestDB(t)
	e := &Event{TS: 1000, SessionID: "s", Model: "m", TokensBefore: 9000, TokensAfter: 9000,
		Components: []CompRow{
			{Component: "cmdfilter", Kind: "offload", Skipped: true,
				Gates: map[string]int{"no_filter_match": 15, "already_marked": 4}},
			{Component: "extract", Kind: "offload", Skipped: true,
				Gates: map[string]int{"no_obvious_noise": 14}},
		}}
	e2 := &Event{TS: 1001, SessionID: "s", Model: "m", TokensBefore: 100, TokensAfter: 100,
		Components: []CompRow{{Component: "cmdfilter", Kind: "offload", Skipped: true,
			Gates: map[string]int{"no_filter_match": 5}}}}
	if err := db.insertBatch([]*Event{e, e2}); err != nil {
		t.Fatal(err)
	}
	comps, err := db.Components(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*ComponentRow{}
	for _, c := range comps {
		byName[c.Component] = c
	}
	if got := byName["cmdfilter"].Gates["no_filter_match"]; got != 20 {
		t.Errorf("aggregated no_filter_match = %d, want 20 (15 + 5)", got)
	}
	if got := byName["cmdfilter"].Gates["already_marked"]; got != 4 {
		t.Errorf("aggregated already_marked = %d, want 4", got)
	}
	if got := byName["extract"].Gates["no_filter_match"]; got != 0 {
		t.Errorf("a gate leaked across components: %d", got)
	}
	// And per request, which is what the drawer shows.
	got, err := db.Request(1, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Components[0].Gates["no_filter_match"] != 15 {
		t.Errorf("per-request gates = %v", got.Components[0].Gates)
	}
	if len(got.Components) != 2 {
		t.Fatalf("%d component rows", len(got.Components))
	}

	// "Gated nothing" and "written before this column existed" are DIFFERENT facts, and the
	// UI renders them differently ("—" against "unknown"). A component that declined nothing
	// must therefore arrive as an empty map, not as an absent one — with omitempty on the
	// field and "" in the column, every healthy component read "unknown", which is the
	// confusion this column was added to remove.
	e3 := &Event{TS: 1002, SessionID: "s", Model: "m", TokensBefore: 10, TokensAfter: 10,
		Components: []CompRow{{Component: "format", Kind: "reformat", Acted: true, Mutated: true}}}
	if err := db.insertBatch([]*Event{e3}); err != nil {
		t.Fatal(err)
	}
	got3, err := db.Request(3, false)
	if err != nil {
		t.Fatal(err)
	}
	if got3.Components[0].Gates == nil {
		t.Error("a component that gated nothing came back as nil, i.e. as \"unknown\"")
	}
	if len(got3.Components[0].Gates) != 0 {
		t.Errorf("gates = %v, want empty", got3.Components[0].Gates)
	}
	b, err := json.Marshal(got3.Components[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"gates":{}`) {
		t.Errorf("the empty map is omitted from the JSON the UI reads: %s", b)
	}
}

// "First request of the session" used to mean "first this PROCESS has seen", which is not the
// same thing and not the safe direction. Two ways it over-claimed:
//
//   - a restart mid-conversation: the recency map is in memory and started empty, so every
//     live session's next turn looked like a session start and earned a credit for a cache
//     hit that would have happened anyway;
//   - the 20,000-entry bound, which emptied the whole map at once and did it to every live
//     session simultaneously.
//
// Both are fixed at the source rather than papered over in the price: the map is seeded from
// the database at startup, and it is pruned by AGE, where forgetting is genuinely correct
// because a session silent that long has lost its provider cache anyway.
func TestSessionRecencySurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000_000)
	e := &Event{TS: now, TenantID: "t1", SessionID: "t1:live", Model: "m", TokensBefore: 10, Status: 200}
	rec.Record(e)
	// Drain, then stop: this is the restart.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		_ = rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.Close()

	rec2, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close()
	if seen, _, _ := rec2.Observe("t1", "t1:live", "m", now+1000); seen {
		t.Fatal("a fresh recorder should not know the session before it is seeded")
	}
	rec3, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec3.Close()
	if n, err := rec3.SeedSessions(now + 1000); err != nil || n == 0 {
		t.Fatalf("SeedSessions recovered %d sessions: %v", n, err)
	}
	seen, _, since := rec3.Observe("t1", "t1:live", "m", now+1000)
	if !seen {
		t.Error("the session was not recovered across the restart, so its next turn would be " +
			"credited as a first-request cache hit")
	}
	if since != 1000 {
		t.Errorf("recovered gap = %d ms, want 1000 from the stored row", since)
	}
	// A session silent past every cache TTL is forgettable, and forgetting it is correct.
	if seen, _, _ := rec3.Observe("t1", "t1:ancient", "m", now); seen {
		t.Error("an unknown session read as seen")
	}
	rec3.mu.Lock()
	rec3.lastSeen["t1:ancient"] = now - staleSession - 1
	rec3.pruneSessionsLocked(now)
	_, still := rec3.lastSeen["t1:ancient"]
	_, kept := rec3.lastSeen["t1:live"]
	rec3.mu.Unlock()
	if still {
		t.Error("pruning kept a session idle past every cache TTL")
	}
	if !kept {
		t.Error("pruning dropped an active session; that is the wholesale reset this replaced")
	}
}

// The tail comparison is per session and must survive a restart, or the first turn after one
// reads as a snapshot change and earns a credit it did not.
// A keep-alive PING must not re-date its session across a restart.
//
// SeedSessions takes each session's LATEST row, and for a pinged session that row is usually a
// ping — 7,782 of 9,234 in the adjudicated replay were session-final. Seeding recency from one
// would make the next real request's gap read as the time since the PING rather than since the
// last real turn: a twenty-minute gap reading as four, no ttl_expiry recorded, and the cache
// miss this whole mechanism is judged on made invisible. proxy/keepalive.go promises in as many
// words that a ping never touches the session-recency map; this is the promise being kept on the
// restart path, which is the one place the ping code cannot reach.
func TestARestartDoesNotRecoverRecencyFromAPing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000_000)
	// A real turn, then a ping 19 minutes later — the shape a keep-alive leaves behind.
	rec.Record(&Event{TS: now, TenantID: "t1", SessionID: "t1:live", Model: "m", TokensBefore: 10, Status: 200})
	rec.Record(&Event{TS: now + 1_140_000, TenantID: "t1", SessionID: "t1:live", Model: "m",
		KeepAlive: true, CacheRead: 50_000, CostUSD: 0.01})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		_ = rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n)
		if n == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.Close()

	rec2, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close()
	if _, err := rec2.SeedSessions(now + 1_200_000); err != nil {
		t.Fatal(err)
	}
	// The next real request arrives 20 minutes after the last REAL turn.
	seen, _, since := rec2.Observe("t1", "t1:live", "m", now+1_200_000)
	if !seen {
		t.Fatal("the session was not recovered at all")
	}
	if since != 1_200_000 {
		t.Errorf("recovered gap = %d ms, want 1200000 — the gap since the last real turn. %d ms "+
			"is the gap since the PING, which would hide the very cache expiry the keep-alive "+
			"exists to demonstrate", since, since)
	}
}

func TestTailChangeIsPerSessionAndSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000_000)
	// Top bit SET on purpose. SQLite's INTEGER is signed, so a hash above 2^63 round-trips
	// as a negative number, and reading it back into a uint64 failed the whole seeding query
	// — in production, on every restart, while this test passed against 0xBBBB. FNV-1a puts
	// half of all real hashes in this range.
	const tailA, tailB = uint64(0xAAAA_AAAA_AAAA_AAAA), uint64(0xEC5B_3A1F_9D42_7E05)

	// First request of a session: nothing to match, so it counts as changed.
	if _, _, _, changed := rec.ObserveSplit("t", "t:s", "m", now, tailA); !changed {
		t.Error("a session's first request should count as a tail change")
	}
	// Same tail again: not a change, so not ours.
	if _, _, _, changed := rec.ObserveSplit("t", "t:s", "m", now+1, tailA); changed {
		t.Error("an unchanged tail was reported as changed")
	}
	// The agent commits: the snapshot moves.
	if _, _, _, changed := rec.ObserveSplit("t", "t:s", "m", now+2, tailB); !changed {
		t.Error("a moved tail was not reported")
	}
	// Another session's tail is independent.
	if _, _, _, changed := rec.ObserveSplit("t", "t:other", "m", now+3, tailB); !changed {
		t.Error("a different session's first request should count as changed")
	}
	// No split, no credit and no state.
	if _, _, _, changed := rec.ObserveSplit("t", "t:s", "m", now+4, 0); changed {
		t.Error("a request with no split reported a tail change")
	}

	e := &Event{TS: now + 5, TenantID: "t", SessionID: "t:s", Model: "m", TokensBefore: 10, Status: 200,
		SplitStableTokens: 5654, SplitTailHash: tailB}
	rec.Record(e)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int64
		_ = rec.DB().sql.QueryRow(`SELECT COUNT(*) FROM requests WHERE split_tail_hash <> 0`).Scan(&n)
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rec.Close()

	rec2, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.Close()
	if _, err := rec2.SeedSessions(now + 6); err != nil {
		t.Fatal(err)
	}
	// The SAME tail after the restart is not a change; without the stored hash it would have
	// looked like one and paid out.
	if _, _, _, changed := rec2.ObserveSplit("t", "t:s", "m", now+6, tailB); changed {
		t.Error("the tail was forgotten across the restart, so the next turn earned a free credit")
	}
	if _, _, _, changed := rec2.ObserveSplit("t", "t:s", "m", now+7, tailA); !changed {
		t.Error("a real change after a restart was missed")
	}
}

// The pre-instrumentation window: requests written before the split's size and tail could be
// recorded price at $0.00, which on the page is indistinguishable from "the component did
// nothing". This values them on READ — nothing stored, nothing rewritten — and the test pins
// both halves: what it credits, and the four things it refuses to credit.
func TestHistoricalSplitValuationIsReadOnlyAndConservative(t *testing.T) {
	db := openTestDB(t)
	const stable = 5697
	mk := func(id int64, sess string, ts int64, model string, read, write int64, stableTok int) *Event {
		e := &Event{TS: ts, SessionID: sess, Model: model, TokensBefore: 100, TokensAfter: 100,
			FreshInput: 10, CacheRead: read, CacheWrite: write, OutputTokens: 20,
			SplitStableTokens: stableTok}
		e.Price(ibmSonnet, true)
		return e
	}
	evs := []*Event{
		// The measured row that teaches us this model's stable half.
		mk(1, "s-new", 5000, "aws/claude-sonnet-5", 54_304, 1_000, stable),
		// Qualifies: pre-instrumentation, session-first, read covers the half, write does not.
		mk(2, "s-a", 1000, "aws/claude-sonnet-5", 54_304, 1_000, 0),
		// Same session, second request: mid-session, so it needs a tail hash it does not have.
		mk(3, "s-a", 1100, "aws/claude-sonnet-5", 55_000, 900, 0),
		// Session-first but the write is big enough to have re-created the stable half.
		mk(4, "s-b", 1200, "aws/claude-sonnet-5", 54_304, stable+1, 0),
		// Session-first but no cache read at all — the overwhelming real case.
		mk(5, "s-c", 1300, "aws/claude-sonnet-5", 0, 55_000, 0),
		// A model whose stable half was never measured: not valued, and counted as uncovered.
		mk(6, "s-d", 1400, "some/unmeasured-model", 54_304, 1_000, 0),
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	h, err := db.CachesplitHistoricalUSD(Filter{TenantAll: true}, staticPricer{ibmSonnet})
	if err != nil {
		t.Fatal(err)
	}
	want := float64(stable) * (ibmSonnet.CacheWrite - ibmSonnet.CacheRead)
	if h.Requests != 1 {
		t.Errorf("credited %d requests, want exactly the one that qualifies", h.Requests)
	}
	if got := h.USD; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("valued at %.10f, want one session start's worth %.10f", got, want)
	}
	if h.Uncovered != 1 {
		t.Errorf("uncovered = %d, want the one model with no measured stable half", h.Uncovered)
	}
	// Read-only: not one stored row may have changed. This is the property that makes the
	// figure safe to compute over other people's history.
	var stored float64
	if err := db.sql.QueryRow(`SELECT COALESCE(SUM(cachesplit_saved_usd),0) FROM requests
		WHERE split_stable_tokens = 0`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("the valuation wrote %v into history; it must only ever read", stored)
	}
	// And it is absent, not zero, when there are no rates: an unpriced number must never read
	// as "saved nothing".
	if none, err := db.CachesplitHistoricalUSD(Filter{TenantAll: true}, nil); err != nil || none.USD != 0 || none.Requests != 0 {
		t.Errorf("with no pricer: %+v (%v)", none, err)
	}
}

// TestHistoricalSplitValuationByTenantMatchesPerTenant is the equivalence check that makes
// the grouped query safe: it must return, for every tenant, exactly what calling
// CachesplitHistoricalUSD once per tenant would — the whole point of computing the
// session-first ranking once is that grouping it afterward changes nothing about the
// answer. Two tenants, each with a multi-request session, so a bug that let a mid-session
// row through (or leaked one tenant's rows into another's total) would show up here.
func TestHistoricalSplitValuationByTenantMatchesPerTenant(t *testing.T) {
	db := openTestDB(t)
	const stable = 5697
	mk := func(tenant, sess string, ts int64, read, write int64, stableTok int) *Event {
		e := &Event{TS: ts, TenantID: tenant, SessionID: sess, Model: "aws/claude-sonnet-5",
			TokensBefore: 100, TokensAfter: 100, FreshInput: 10, CacheRead: read, CacheWrite: write,
			OutputTokens: 20, SplitStableTokens: stableTok}
		e.Price(ibmSonnet, true)
		return e
	}
	evs := []*Event{
		mk("t-x", "s-new", 5000, 54_304, 1_000, stable), // teaches the stable half
		// #501: the stable half is learned per tenant, so t-y must have measured its own.
		mk("t-y", "s-new-y", 5100, 54_304, 1_000, stable),
		// t-x: session-first qualifies, second request in the same session must not.
		mk("t-x", "s-a", 1000, 54_304, 1_000, 0),
		mk("t-x", "s-a", 1100, 55_000, 900, 0),
		// t-y: a single session-first request of its own — must not be counted against t-x.
		mk("t-y", "s-b", 1200, 54_304, 1_000, 0),
	}
	if err := db.insertBatch(evs); err != nil {
		t.Fatal(err)
	}
	pricer := staticPricer{ibmSonnet}
	grouped, err := db.CachesplitHistoricalUSDByTenant(0, pricer)
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range []string{"t-x", "t-y"} {
		want, err := db.CachesplitHistoricalUSD(Filter{Tenant: tenant}, pricer)
		if err != nil {
			t.Fatal(err)
		}
		got := grouped[tenant]
		if got != want {
			t.Errorf("tenant %s: grouped = %+v, per-tenant call = %+v", tenant, got, want)
		}
	}
	if got := grouped["t-x"].Requests; got != 1 {
		t.Errorf("t-x requests = %d, want 1 (only the session-first row)", got)
	}
	if got := grouped["t-y"].Requests; got != 1 {
		t.Errorf("t-y requests = %d, want 1", got)
	}
}

// staticPricer prices every model the same, which is what a test wants and what production must
// never do.
type staticPricer struct{ p modelinfo.Price }

func (s staticPricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	if model == "some/unmeasured-model" {
		return modelinfo.Price{}, false
	}
	return s.p, true
}
