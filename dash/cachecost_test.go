package dash

import (
	"database/sql"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// ibmSonnet is the rate ete-litellm actually bills for aws/claude-sonnet-5 — half the
// public API's, which is why the operator price list exists at all.
var ibmSonnet = modelinfo.Price{Input: 1.52e-6, Output: 7.6e-6, CacheRead: 1.52e-7, CacheWrite: 1.9e-6}

// The re-sent part of a removal is worth the cache-READ rate on a turn whose cache hit,
// and the cache-WRITE rate on a turn whose cache missed — because on a miss the provider
// re-billed the whole prompt, so the removed content would have been re-billed too.
//
// This is the correction with the largest effect on the real service: TTL-expired turns
// were 4% of production requests and 31% of spend, every token of it cache_creation, and
// their savings were being valued at a rate 12.5x too low.
func TestRepeatSavingsAreValuedByTheCacheOutcome(t *testing.T) {
	const removed, unique = 10_000, 0 // all of it re-sent history, none of it new

	mk := func(read, write int64) *Event {
		e := &Event{
			TS: 1000, SessionID: "s", Model: "aws/claude-sonnet-5",
			TokensBefore: 90_000, TokensAfter: 90_000 - removed,
			SavedUnique: unique,
			FreshInput:  10, CacheRead: read, CacheWrite: write, OutputTokens: 50,
		}
		e.Price(ibmSonnet, true)
		return e
	}
	hit, miss := mk(80_000, 0), mk(0, 80_000)

	wantHit := float64(removed) * ibmSonnet.CacheRead
	wantMiss := float64(removed) * ibmSonnet.CacheWrite
	if got := hit.BaselineCostUSD - hit.CostUSD; math.Abs(got-wantHit) > 1e-12 {
		t.Errorf("cache HIT: removal valued at %.10f, want the read rate %.10f", got, wantHit)
	}
	if got := miss.BaselineCostUSD - miss.CostUSD; math.Abs(got-wantMiss) > 1e-12 {
		t.Errorf("cache MISS: removal valued at %.10f, want the write rate %.10f", got, wantMiss)
	}
	// The point of the change, stated as a ratio so a partial revert also fails.
	ratio := (miss.BaselineCostUSD - miss.CostUSD) / (hit.BaselineCostUSD - hit.CostUSD)
	if ratio < 10 {
		t.Errorf("a cold turn's removal is only %.1fx a warm turn's; the write/read spread is 12.5x", ratio)
	}
	// A turn that neither read nor wrote cache (a provider with no cache at all) values
	// the removal at the fresh rate, never at zero.
	none := mk(0, 0)
	if got := none.BaselineCostUSD - none.CostUSD; math.Abs(got-float64(removed)*ibmSonnet.Input) > 1e-12 {
		t.Errorf("no cache at all: removal valued at %.10f, want the fresh rate", got)
	}
}

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

// The three conditions, one test each, because each one rules out a different way of
// over-claiming — and the figure this replaces failed all three.
func TestCachesplitSavingNeedsTheSplit_TheHit_AndAMovedTail(t *testing.T) {
	const stable = 8_478 // what the split measured as the stable half on a real session
	split := []CompRow{{Component: "cachesplit", Kind: "reformat", Acted: true, Mutated: true}}
	mk := func(comps []CompRow, read int64, tailMoved bool, stableTok int) *Event {
		e := &Event{TS: 1, SessionID: "s", Model: "aws/claude-sonnet-5",
			TokensBefore: 50_000, TokensAfter: 50_000,
			FreshInput: 10, CacheRead: read, OutputTokens: 20,
			Components: comps, TailChanged: tailMoved, SplitStableTokens: stableTok}
		e.Price(ibmSonnet, true)
		return e
	}
	// The counterfactual is a MISS, which bills those tokens as cache creation, not as
	// fresh input: they carry cache_control. So the spread is write minus read. And the
	// amount is the STABLE HALF, not the request's whole cache_read — the control arm still
	// hit on 45,805 tokens with cachesplit off, so the rest was never ours to claim.
	want := float64(stable) * (ibmSonnet.CacheWrite - ibmSonnet.CacheRead)
	if got := mk(split, 54_304, true, stable).CachesplitSavedUSD; math.Abs(got-want) > 1e-12 {
		t.Errorf("all three conditions met: %.10f, want %.10f", got, want)
	}
	for name, e := range map[string]*Event{
		"nothing split":             mk(nil, 54_304, true, 0),
		"component ran but skipped": mk([]CompRow{{Component: "cachesplit", Skipped: true}}, 54_304, true, 0),
		"the split produced no hit": mk(split, 0, true, stable),
		// A turn whose snapshot did NOT move: the stable half would have been served from
		// cache either way, split or not, so there is nothing of ours in it.
		"the tail did not change": mk(split, 54_304, false, stable),
		// A read too small to have contained our half. Refused rather than clamped: a hit on
		// some other, smaller prefix says nothing about ours.
		"read smaller than the half we split": mk(split, 3_000, true, stable),
	} {
		if e.CachesplitSavedUSD != 0 {
			t.Errorf("%s: claimed %.10f", name, e.CachesplitSavedUSD)
		}
	}
	// The subtle over-claim, and the reason a hit alone is not enough: the session's first
	// request AFTER the stable half itself changed (someone edited CLAUDE.md, the tool list
	// moved). An earlier agent breakpoint still hits, so cache_read is large — but our half is
	// being billed as CREATION on this very request, so there is nothing to credit.
	changed := &Event{TS: 1, SessionID: "s", Model: "aws/claude-sonnet-5",
		TokensBefore: 50_000, TokensAfter: 50_000, FreshInput: 10,
		CacheRead: 20_000, CacheWrite: 12_000, OutputTokens: 20,
		Components: split, TailChanged: true, SplitStableTokens: stable}
	changed.Price(ibmSonnet, true)
	if changed.CachesplitSavedUSD != 0 {
		t.Errorf("credited a request that re-created the stable half: %.10f", changed.CachesplitSavedUSD)
	}
	// The provider's own cache saving is still measured on every one of them — it is a
	// diagnostic, and losing it would hide a pipeline destroying a prefix.
	if mk(nil, 54_304, false, 0).CacheSavedUSD <= 0 {
		t.Error("the provider-cache diagnostic stopped being measured")
	}
	// And ours is a small fraction of it, which is the whole correction: 5,654 tokens at
	// 1.15x versus 54,304 at 0.9x. Pinned as a bound so a revert to "credit the whole
	// cache_read" fails here rather than in production.
	e := mk(split, 54_304, true, stable)
	if e.CachesplitSavedUSD > e.CacheSavedUSD/3 {
		t.Errorf("ours %.10f is too large a share of the provider figure %.10f",
			e.CachesplitSavedUSD, e.CacheSavedUSD)
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
	added := []string{"cache_saved_usd", "cachesplit_saved_usd", "split_stable_tokens"}
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
		var sum float64
		if err := db2.sql.QueryRow(`SELECT COALESCE(SUM(` + col + `),0) FROM requests`).Scan(&sum); err != nil {
			t.Fatalf("%s was not added back: %v", col, err)
		}
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
				Gates: map[string]int{"no_filter_match": 15, "marker_or_kept_verbatim": 4}},
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
	if got := byName["cmdfilter"].Gates["marker_or_kept_verbatim"]; got != 4 {
		t.Errorf("aggregated marker_or_kept_verbatim = %d, want 4", got)
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
	e := &Event{TS: now, TenantID: "t1", SessionID: "t1:live", Model: "m", TokensBefore: 10}
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
func TestTailChangeIsPerSessionAndSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	rec, err := NewRecorder(Options{DBPath: path, BatchSize: 1, FlushInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	const now = int64(1_700_000_000_000)
	const tailA, tailB = uint64(0xAAAA), uint64(0xBBBB)

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

	e := &Event{TS: now + 5, TenantID: "t", SessionID: "t:s", Model: "m", TokensBefore: 10,
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
