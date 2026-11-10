package dash

import (
	"context"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// lateLoader is a price source that knows nothing until load is called: the proxy's price map
// before its background download ends (issue #920).
type lateLoader struct {
	loaded atomic.Bool
	p      modelinfo.Price
}

func (l *lateLoader) Price(_ context.Context, _ string) (modelinfo.Price, bool) {
	if !l.loaded.Load() {
		return modelinfo.Price{}, false
	}
	return l.p, true
}

func partialEvent(ts int64, session string, cacheWrite int64) *Event {
	e := &Event{TS: ts, SessionID: session, Model: "claude-sonnet-4-5", Provider: "anthropic", Status: 200,
		TokensBefore: 49, FreshInput: 10, CacheWrite: cacheWrite, OutputTokens: 50}
	e.PriceMode(modelinfo.Price{}, true) // the price map has not loaded: partial at $0
	return e
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A request that was booked at $0 because the price map had not loaded is priced once it loads
// (issue #920). Only rows with billed tokens and no price are touched, and rows the proxy wrote
// before this process started are left as they were written.
func TestRecorderRepricesRowsBookedBeforeThePriceMapLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.db")
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	hist := partialEvent(time.Now().Add(-time.Hour).UnixMilli(), "hist", 29650)
	if err := old.insertBatch([]*Event{hist}); err != nil {
		t.Fatal(err)
	}
	old.Close()

	src := &lateLoader{p: modelinfo.Price{Input: 3e-06, Output: 1.5e-05, CacheRead: 3e-07, CacheWrite: 3.75e-06}}
	rec, err := NewRecorder(Options{DBPath: path, FlushInterval: time.Millisecond, BatchSize: 1,
		Prices: src, RepriceInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	first := partialEvent(0, "s1", 29650)
	noTokens := &Event{SessionID: "s2", Model: "claude-sonnet-4-5", Status: 200, TokensBefore: 49}
	noTokens.PriceMode(modelinfo.Price{}, false) // no usage: stays partial, there is nothing to price
	rec.Record(first)
	rec.Record(noTokens)
	waitFor(t, "rows written", func() bool { return rec.Stats().Written == 2 })

	var acct string
	var cost float64
	row := func(session string) (string, float64) {
		if err := rec.db.sql.QueryRow(`SELECT token_accounting, cost_usd FROM requests WHERE session_id = ?`, session).Scan(&acct, &cost); err != nil {
			t.Fatal(err)
		}
		return acct, cost
	}
	time.Sleep(60 * time.Millisecond) // several passes with no price: nothing changes
	if a, c := row("s1"); a != AccountingPartial || c != 0 {
		t.Fatalf("before the load: %q cost %v, want partial and 0", a, c)
	}

	src.loaded.Store(true)
	waitFor(t, "the row to be priced", func() bool { a, _ := row("s1"); return a == AccountingComplete })

	want := 10*3e-06 + 29650*3.75e-06 + 50*1.5e-05
	a, c := row("s1")
	if a != AccountingComplete || math.Abs(c-want) > want*1e-6 {
		t.Errorf("s1: %q cost %v, want complete and %v", a, c, want)
	}
	var base, spend float64
	if err := rec.db.sql.QueryRow(`SELECT baseline_cost_usd FROM requests WHERE session_id = 's1'`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if math.Abs(base-want) > want*1e-6 {
		t.Errorf("baseline_cost_usd = %v, want %v (no saving is booked for a repriced row)", base, want)
	}
	if err := rec.db.sql.QueryRow(`SELECT COALESCE(SUM(usd),0) FROM tenant_spend`).Scan(&spend); err != nil {
		t.Fatal(err)
	}
	if math.Abs(spend-want) > want*1e-6 {
		t.Errorf("tenant_spend = %v, want %v: the monthly rollup must follow the repriced cost", spend, want)
	}
	if a, c := row("s2"); a != AccountingPartial || c != 0 {
		t.Errorf("a row with no billed tokens: %q cost %v, want partial and 0", a, c)
	}
	if a, c := row("hist"); a != AccountingPartial || c != 0 {
		t.Errorf("a row from before this process: %q cost %v, want it left as written", a, c)
	}
}

// A request run in bypass mode before the price map loaded is stored "missing", not "partial": its
// token count is filled in later, off the request path. It is priced all the same, whichever label
// it holds when the price lands (review of #1002, finding 1).
func TestRecorderRepricesABypassedRowBookedBeforeThePriceMapLoaded(t *testing.T) {
	src := &lateLoader{p: modelinfo.Price{Input: 3e-06, Output: 1.5e-05, CacheRead: 3e-07, CacheWrite: 3.75e-06}}
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"), FlushInterval: time.Millisecond,
		BatchSize: 1, Prices: src, RepriceInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	e := &Event{SessionID: "b1", Model: "claude-sonnet-4-5", Provider: "anthropic", Status: 200,
		FreshInput: 10, CacheWrite: 29650, OutputTokens: 50} // bypass: TokensBefore is 0 at write time
	e.PriceMode(modelinfo.Price{}, true)
	if e.TokenAccounting != AccountingMissing {
		t.Fatalf("setup: a bypassed row with no price is %q, want %q", e.TokenAccounting, AccountingMissing)
	}
	rec.Record(e)
	waitFor(t, "row written", func() bool { return rec.Stats().Written == 1 })
	// The count worker flips the label after the write, before the price lands.
	if err := rec.db.setBypassTokens(1, 49, 49, 0); err != nil {
		t.Fatal(err)
	}

	src.loaded.Store(true)
	var acct string
	var cost float64
	waitFor(t, "the bypassed row to be priced", func() bool {
		if err := rec.db.sql.QueryRow(`SELECT token_accounting, cost_usd FROM requests WHERE session_id = 'b1'`).Scan(&acct, &cost); err != nil {
			t.Fatal(err)
		}
		return acct == AccountingComplete
	})
	want := 10*3e-06 + 29650*3.75e-06 + 50*1.5e-05
	if math.Abs(cost-want) > want*1e-6 {
		t.Errorf("bypassed row cost %v, want %v", cost, want)
	}
}

// A row whose compaction call could not be priced is not repriced: its cg_llm_cost_usd would stay
// 0 under a "complete" label, which readers take for "context-guru spent nothing". It stays
// partial at $0, which they take for unknown (review of #1002, finding 2).
func TestRecorderLeavesARowWithAnUnpricedCompactionCallUnknown(t *testing.T) {
	src := &lateLoader{p: modelinfo.Price{Input: 3e-06, Output: 1.5e-05, CacheRead: 3e-07, CacheWrite: 3.75e-06}}
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"), FlushInterval: time.Millisecond,
		BatchSize: 1, Prices: src, RepriceInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	withCG := partialEvent(0, "cg", 29650)
	withCG.CGCostUnpriced = true // the capture site had compaction tokens and no price for them
	plain := partialEvent(0, "plain", 29650)
	rec.Record(withCG)
	rec.Record(plain)
	waitFor(t, "rows written", func() bool { return rec.Stats().Written == 2 })

	src.loaded.Store(true)
	get := func(session string) string {
		var a string
		if err := rec.db.sql.QueryRow(`SELECT token_accounting FROM requests WHERE session_id = ?`, session).Scan(&a); err != nil {
			t.Fatal(err)
		}
		return a
	}
	waitFor(t, "the plain row to be priced", func() bool { return get("plain") == AccountingComplete })
	time.Sleep(60 * time.Millisecond) // a few more passes
	if a := get("cg"); a != AccountingPartial {
		t.Errorf("a row with an unpriced compaction call is %q, want it left partial (unknown)", a)
	}
}

// The queue of rows waiting for a price holds at most maxUnpriced and drops the oldest, whether the
// rows arrive by noteUnpriced or are put back by a pass that found no price (review of #1002).
func TestUnpricedQueueIsBounded(t *testing.T) {
	src := &lateLoader{} // never loads
	rec, err := NewRecorder(Options{DBPath: filepath.Join(t.TempDir(), "d.db"), Prices: src,
		RepriceInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	for i := int64(1); i <= maxUnpriced+10; i++ {
		e := partialEvent(0, "q", 100)
		e.ID = i
		rec.noteUnpriced(e)
	}
	if got := len(rec.unpriced); got != maxUnpriced {
		t.Fatalf("after %d notes the queue holds %d, want %d", maxUnpriced+10, got, maxUnpriced)
	}
	if first := rec.unpriced[0].ID; first != 11 {
		t.Errorf("oldest kept id = %d, want 11: the first 10 should have been dropped", first)
	}

	// A pass that finds no price puts every row back. Rows noted meanwhile must not push it past the cap.
	waiting := rec.unpriced
	rec.unpriced = nil
	for i := int64(1000000); i < 1000000+20; i++ {
		e := partialEvent(0, "q", 100)
		e.ID = i
		rec.noteUnpriced(e)
	}
	rec.unpriced = append(waiting, rec.unpriced...)
	rec.repricePass() // no price: every row is kept and re-queued
	if got := len(rec.unpriced); got > maxUnpriced {
		t.Errorf("after a pass with no price the queue holds %d, want at most %d", got, maxUnpriced)
	}
}
