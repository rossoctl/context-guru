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
	"github.com/rossoctl/context-guru/internal/testutil"
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
	added := []string{"cache_saved_usd", "project", "price_estimated", "cache_bp_root"}
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
	testutil.WaitFor(t, "the rows are stored", func() bool { return rec.Stats().Written == 1 })
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
	testutil.WaitFor(t, "the rows are stored", func() bool { return rec.Stats().Written == 2 })
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

// staticPricer prices every model the same, which is what a test wants and what production must
// never do.
type staticPricer struct{ p modelinfo.Price }

func (s staticPricer) Price(_ context.Context, model string) (modelinfo.Price, bool) {
	if model == "some/unmeasured-model" {
		return modelinfo.Price{}, false
	}
	return s.p, true
}

// A database written before cachesplit was retired (#567) holds the columns nothing reads any
// more. Opening it must work, new rows must insert, and the old rows must stay readable. The
// columns are added by hand here, as an old binary created them.
func TestADatabaseWithTheRetiredCachesplitColumnsStillOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.insertBatch([]*Event{{TS: 1, SessionID: "s", Model: "m", TokensBefore: 10}}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`ALTER TABLE requests ADD COLUMN cachesplit_saved_usd REAL NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN split_stable_tokens INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN split_tail_hash INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE requests ADD COLUMN tok_split_saved_in INTEGER NOT NULL DEFAULT 0`,
		`UPDATE requests SET cachesplit_saved_usd = 1.5, split_stable_tokens = 5000`,
	} {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("an old database with the retired columns failed to open: %v", err)
	}
	defer db2.Close()
	if err := db2.insertBatch([]*Event{{TS: 2, SessionID: "s", Model: "m", TokensBefore: 10}}); err != nil {
		t.Fatalf("inserting into an old database failed: %v", err)
	}
	o, err := db2.Overview(Filter{})
	if err != nil {
		t.Fatalf("reading an old database failed: %v", err)
	}
	if o.Requests != 2 {
		t.Errorf("requests = %d, want 2", o.Requests)
	}
	// The retired figure is gone from the totals, old rows included.
	if b, _ := json.Marshal(o); strings.Contains(string(b), "cachesplit") {
		t.Errorf("the overview still carries a cachesplit field: %s", b)
	}
}
