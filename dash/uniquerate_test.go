package dash

import (
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Event.uniqueRate's whole contract, in one table. Rates chosen so each answer is a distinct
// number: fresh 1e-6, read 1e-7, write 1.25e-6.
func TestUniqueRateIsTheTierAFirstEntryWouldHavePaid(t *testing.T) {
	p := modelinfo.Price{Input: 1e-6, CacheRead: 1e-7, CacheWrite: 1.25e-6}
	for _, tc := range []struct {
		name               string
		read, write, fresh int64
		want               float64
		why                string
	}{
		{"warm read-only turn", 80_000, 0, 10, p.Input,
			"nothing was written, so a first entry would have been billed as input"},
		{"cold turn that re-created the prefix", 0, 80_000, 10, p.CacheWrite,
			"the write covered the prompt, so a first entry enters as cache creation"},
		{"no cache at all", 0, 0, 80_000, p.Input, "fresh"},
		{"write that did NOT cover the prompt", 0, 2_000, 100_000, p.Input,
			"one breakpoint after tools bills 2k creation and 100k input; the removed " +
				"transcript would have been in the input, not the creation"},
		{"warm turn with an uncovering write", 80_000, 2_000, 100_000, p.Input,
			"the case the read-wins tier bucket cannot answer, which is why the read path " +
				"needs its own GROUP BY key for it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &Event{CacheRead: tc.read, CacheWrite: tc.write, FreshInput: tc.fresh}
			if got := e.uniqueRate(p); got != tc.want {
				t.Errorf("uniqueRate = %.3g, want %.3g — %s", got, tc.want, tc.why)
			}
			// The invariant that holds in EVERY row of this table: content removed on this turn
			// was never in the cache to be read from, so the read rate is never the answer.
			// Returning it would understate a first removal 12.5x on the Anthropic family.
			if e.uniqueRate(p) == p.CacheRead {
				t.Error("uniqueRate returned the CACHE-READ rate; this content was never in " +
					"the cache, which is what makes it the unique term rather than the replay term")
			}
		})
	}
	// A model can carry cache rates and no fresh rate (fixture at capture_test.go:414). Falling
	// through to a zero would price the first removal of real content at $0.00 — a claim, not an
	// absence — so the guard sends it to the write rate instead.
	noInput := modelinfo.Price{CacheRead: 2e-7, CacheWrite: 2.5e-6}
	e := &Event{CacheRead: 80_000, CacheWrite: 0, FreshInput: 0}
	if got := e.uniqueRate(noInput); got != noInput.CacheWrite {
		t.Errorf("uniqueRate on a model with no fresh rate = %.3g, want the write rate %.3g; "+
			"a zero here is a claim that removing real content was worth nothing",
			got, noInput.CacheWrite)
	}
}

// The read path values a component exactly as the write path did, on a turn whose write did NOT
// cover the prompt while the cache DID read. That row lands in the read-wins 'read' tier bucket,
// so the tier key alone cannot tell the read path which unique rate to use — this is the test
// that fails if the fifth GROUP BY key is dropped.
func TestReadPathPricesUniqueOnTheWriteCoverageNotTheTier(t *testing.T) {
	db := openTestDB(t)
	// read > 0 AND write > 0 AND write < fresh: 'read' by tier, uncovered by uniqueRate.
	e := &Event{
		TS: 1000, SessionID: "s1", Model: "m1", TenantID: "t1",
		TokensBefore: 10000, TokensAfter: 8000,
		FreshInput: 100_000, CacheRead: 80_000, CacheWrite: 2_000,
		TokenAccounting: AccountingComplete,
		Components: []CompRow{{Component: "extract", Kind: "offload", Acted: true, Mutated: true,
			SavedGross: 2000, SavedUnique: 500}},
	}
	e.Price(handPrice["m1"], true) // the WRITE path's own figure, to diff the read path against
	insertReq(t, db, e)
	rows, err := db.Components(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecomposeComponentSavedUSD(Filter{TenantAll: true}, handPrice, rows); err != nil {
		t.Fatal(err)
	}
	m1 := handPrice["m1"]
	wantFirst, wantReplay := 500*m1.Input, 1500*m1.CacheRead
	if math.Abs(rows[0].SavedUSDFirstRemoval-wantFirst) > 1e-12 {
		t.Errorf("first removal = %.12f, want %.12f (500 tokens at the FRESH rate: this turn's "+
			"2k write did not cover its 100k of fresh input, and the read-wins tier bucket "+
			"cannot express that)", rows[0].SavedUSDFirstRemoval, wantFirst)
	}
	if math.Abs(rows[0].SavedUSDReplay-wantReplay) > 1e-12 {
		t.Errorf("replay = %.12f, want %.12f (still the read rate — repeatRate is unchanged)",
			rows[0].SavedUSDReplay, wantReplay)
	}
	// And it agrees with what the write path stored for the same row, which is the property the
	// duplication exists to preserve.
	if got := compByName(t, e, "extract").SavedUSD; math.Abs(got-(wantFirst+wantReplay)) > 1e-12 {
		t.Errorf("write path stored %.12f, read path decomposed %.12f — the four copies of this "+
			"formula have drifted", got, wantFirst+wantReplay)
	}
}
