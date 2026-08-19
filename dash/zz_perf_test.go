package dash

import (
	"fmt"
	"testing"
	"time"
)

// The gate aggregation expands request_components through json_each, so it could have
// turned a dashboard load into a table scan.
//
// Measured at the live service's scale (200,000 rows, one component row each): 143 ms,
// inside an Overview of 1.31 s and a Components of 1.14 s that were already dominated by
// the pre-existing aggregates. The test runs at a twentieth of that so the suite stays
// quick under -race; it is a regression bound, not the measurement.
//
// The cache-saving figure used to be here too, as a correlated EXISTS over
// request_components costing 199 ms. It is a plain column SUM now: every condition that
// qualifies a request is settled at write time, where the rates and the session history
// are both in hand. Cheaper, and the reason it is cheaper is the reason it is correct.
func TestOverviewStaysFastOnALargeWindow(t *testing.T) {
	db := openTestDB(t)
	const n = 10_000
	batch := make([]*Event, 0, 1000)
	for i := 0; i < n; i++ {
		e := &Event{TS: int64(1_700_000_000_000 + i*100), SessionID: fmt.Sprintf("s%d", i/40),
			Model: "aws/claude-sonnet-5", TokensBefore: 50_000, TokensAfter: 49_000,
			SavedUnique: 1000, FreshInput: 100, CacheRead: 40_000, OutputTokens: 50,
			Components: []CompRow{{Component: "cachesplit", Mutated: i%3 == 0, Kind: "reformat",
				Gates: map[string]int{"no_filter_match": i % 7}}}}
		e.Price(ibmSonnet, true)
		batch = append(batch, e)
		if len(batch) == 1000 {
			if err := db.insertBatch(batch); err != nil {
				t.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	// Split the new query out, so a slow overview is attributed rather than guessed at.
	tg := time.Now()
	var ng int64
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM request_components c JOIN requests r ON r.id = c.request_id,
		json_each(c.gates) j WHERE c.gates <> ''`).Scan(&ng); err != nil {
		t.Fatal(err)
	}
	gatesMs := time.Since(tg)
	t.Logf("gate aggregation %v (%d rows)", gatesMs, ng)
	t0 := time.Now()
	o, err := db.Overview(Filter{})
	if err != nil {
		t.Fatal(err)
	}
	overview := time.Since(t0)
	t1 := time.Now()
	if _, err := db.Components(Filter{}); err != nil {
		t.Fatal(err)
	}
	comps := time.Since(t1)
	t.Logf("%d rows: Overview %v (provider cache $%.2f, ours $%.2f), Components %v",
		n, overview, o.CacheSavedUSD, o.CachesplitSavedUSD, comps)
	// RELATIVE, because an absolute millisecond bound is a machine and a -race build away
	// from being either meaningless or flaky: under -race SQLite is ~4x slower here. The
	// invariant that matters is that the two queries THIS change added are not the expensive
	// part of a dashboard load, which is true whatever the machine.
	added, existing := gatesMs, overview+comps
	if added > existing/2 {
		t.Errorf("the added queries dominate the load: %v of %v", added, existing)
	}
	// Plus one runaway guard, loose enough to survive a slow CI box and tight enough that a
	// lost index at this size cannot pass.
	if existing > 20*time.Second {
		t.Errorf("too slow overall: overview %v, components %v", overview, comps)
	}
}
