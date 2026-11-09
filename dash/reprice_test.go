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
