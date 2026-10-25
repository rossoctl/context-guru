package dash

import (
	"context"
	"math"
	"testing"

	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
)

// Tests for the sweep's money (#404 PR C, point 9): M1 and M2.
//
// A $1-per-token-class price makes every dollar figure here equal to a token count times a small
// integer, so a wrong rate shows as a wrong number a person can check by hand.

var sweepPrice = modelinfo.Price{Input: 10e-6, CacheRead: 1e-6, CacheWrite: 12.5e-6, Output: 50e-6}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// sweepRow is one request carrying a sweep component row that removed `removed` tokens, of which
// `unique` are new content keys (the cold request's own drops).
func sweepEvent(ts int64, session string, miss string, cacheRead, cacheWrite, fresh int64, removed, unique int) *Event {
	e := &Event{TS: ts, TenantID: "t", SessionID: session, Model: "claude-sonnet-5", Status: 200,
		TokensBefore: 100_000, TokensAfter: 100_000 - removed, SavedUnique: unique,
		FreshInput: fresh, CacheRead: cacheRead, CacheWrite: cacheWrite, OutputTokens: 500,
		CacheMissReason: miss,
		Components: []CompRow{{Component: SweepComponent, Kind: "offload", Acted: removed > 0, Mutated: true,
			SavedGross: removed, SavedUnique: unique}},
	}
	e.Price(sweepPrice, true)
	return e
}

// ⭐ M1. The credit on the COLD request is removed tokens x the cache-write rate (1.25 x input): the
// removed tokens are new content on that request. On each LATER request it is removed tokens x the rate
// that request paid. Both come out of the one pricing path every component uses, so this asserts the
// path gives the numbers point 9 states.
func TestM1TheCreditIsTheWriteRateOnTheColdRequestAndTheRequestsRateAfter(t *testing.T) {
	const removed = 10_000
	factor, _ := tokens.BilledDeltaFactor("claude-sonnet-5")

	// The cold request: the cache expired, so the whole prompt was written.
	cold := sweepEvent(1, "s", CacheTTLExpiry, 0, 90_000, 50, removed, removed)
	wantCold := float64(removed) * sweepPrice.CacheWrite * factor
	if got := cold.Components[0].SavedUSD; !approx(got, wantCold) {
		t.Errorf("cold-request credit = %v, want removed x write rate x factor = %v", got, wantCold)
	}
	if rate := sweepPrice.CacheWrite / sweepPrice.Input; !approx(rate, 1.25) {
		t.Fatalf("fixture: the write rate is %vx input, want 1.25x", rate)
	}

	// A later WARM request: the same drops replay (gross removed, unique 0), at the read rate.
	warm := sweepEvent(2, "s", CacheHit, 90_000, 200, 50, removed, 0)
	wantWarm := float64(removed) * sweepPrice.CacheRead * factor
	if got := warm.Components[0].SavedUSD; !approx(got, wantWarm) {
		t.Errorf("warm-request credit = %v, want removed x read rate x factor = %v", got, wantWarm)
	}

	// A later COLD request (the cache expired again): the replayed drops are written at 1.25x.
	cold2 := sweepEvent(3, "s", CacheTTLExpiry, 0, 90_000, 50, removed, 0)
	wantCold2 := float64(removed) * sweepPrice.CacheWrite * factor
	if got := cold2.Components[0].SavedUSD; !approx(got, wantCold2) {
		t.Errorf("a later cold request's credit = %v, want %v (written, so the write rate)", got, wantCold2)
	}
}

// ⭐ M1 (cost half). The answer cost is booked ONCE, on the ping row the question replaced, in its own
// column, and it is the OUTPUT (thinking included) plus the question's own fresh input and any write,
// never the cache read the ping would have made anyway.
func TestM1TheAnswerCostIsBookedOnceOnThePingRowAndInItsOwnColumn(t *testing.T) {
	db := openTestDB(t)
	const out, fresh = 7_191, 600 // a measured 12-candidate answer including its reasoning
	ping := &Event{TS: 10, TenantID: "t", SessionID: "s", Model: "claude-sonnet-5", KeepAlive: true, Status: 200,
		FreshInput: fresh, CacheRead: 80_000, OutputTokens: out}
	ping.Price(sweepPrice, true)
	// The keeper books the answer cost with the same arithmetic the proxy uses: output + fresh + write,
	// no read.
	ping.SweepAnswerUSD = sweepPrice.CostWithCacheWrite1h(fresh, 0, 0, out, 0)
	wantAnswer := float64(out)*sweepPrice.Output + float64(fresh)*sweepPrice.Input
	if !approx(ping.SweepAnswerUSD, wantAnswer) {
		t.Fatalf("answer cost = %v, want output + fresh = %v", ping.SweepAnswerUSD, wantAnswer)
	}
	// It is PART of the ping's cost: the read is the rest.
	if rest := ping.CostUSD - ping.SweepAnswerUSD; !approx(rest, 80_000*sweepPrice.CacheRead) {
		t.Errorf("ping cost minus the answer = %v, want the read it would have made anyway = %v",
			rest, 80_000*sweepPrice.CacheRead)
	}
	// An ordinary ping carries none.
	plain := &Event{TS: 11, TenantID: "t", SessionID: "s", Model: "claude-sonnet-5", KeepAlive: true,
		Status: 200, CacheRead: 80_000, OutputTokens: 1}
	plain.Price(sweepPrice, true)
	if plain.SweepAnswerUSD != 0 {
		t.Errorf("an ordinary ping carries an answer cost of %v", plain.SweepAnswerUSD)
	}
	if err := db.insertBatch([]*Event{ping, plain}); err != nil {
		t.Fatal(err)
	}
	l, err := db.SweepLedgerFor(context.Background(), Filter{TenantAll: true, WithKeepAlive: true}, staticPricer{sweepPrice})
	if err != nil {
		t.Fatal(err)
	}
	if !approx(l.AnswerUSD, wantAnswer) || l.Questions != 1 {
		t.Errorf("ledger answer = %v over %d questions, want %v over 1", l.AnswerUSD, l.Questions, wantAnswer)
	}
	// It is NOT counted twice: a second read of the same window gives the same figure.
	l2, _ := db.SweepLedgerFor(context.Background(), Filter{TenantAll: true}, staticPricer{sweepPrice})
	if !approx(l2.AnswerUSD, wantAnswer) {
		t.Errorf("a window read without the keep-alive flag lost or doubled the answer: %v", l2.AnswerUSD)
	}
}

// The expands of SWEEP markers come off the sweep's credit, in dollars, at the write rate. An expand of
// anything else does not.
func TestTheSweepsExpandsComeOffItsCreditAndOnlyTheSweeps(t *testing.T) {
	db := openTestDB(t)
	cold := sweepEvent(1, "s", CacheTTLExpiry, 0, 90_000, 50, 10_000, 10_000)
	back := sweepEvent(2, "s", CacheHit, 80_000, 12_000, 50, 10_000, 0)
	back.Expands, back.ExpandTokens = 3, 4_000           // three expands in all
	back.SweepExpands, back.SweepExpandTokens = 1, 3_000 // one of them a sweep marker
	back.SweepFalseExpands = 1
	if err := db.insertBatch([]*Event{cold, back}); err != nil {
		t.Fatal(err)
	}
	l, err := db.SweepLedgerFor(context.Background(), Filter{TenantAll: true}, staticPricer{sweepPrice})
	if err != nil {
		t.Fatal(err)
	}
	if l.Expands != 1 || l.ExpandTokens != 3_000 || l.FalseExpands != 1 {
		t.Fatalf("ledger = %+v, want exactly the sweep's expand (1, 3000 tokens)", l)
	}
	factor, _ := tokens.BilledDeltaFactor("claude-sonnet-5")
	want := 3_000 * sweepPrice.CacheWrite * factor
	if !approx(l.ExpandChargeUSD, want) {
		t.Errorf("expand charge = %v, want sweep expand tokens x write rate = %v (NOT the other 1,000)", l.ExpandChargeUSD, want)
	}
	row := &ComponentRow{Component: SweepComponent, SavedUSD: 1.0, NetUSD: 0.9, NetUSDWithEstimate: 0.9}
	ApplySweepLedgerToRow(row, l)
	if !approx(row.SavedUSD, 1.0-want) || !approx(row.NetUSD, 0.9-want) {
		t.Errorf("the charge was not taken off the sweep's credit and net: saved %v net %v", row.SavedUSD, row.NetUSD)
	}
	other := &ComponentRow{Component: "extract_llm", SavedUSD: 1.0, NetUSD: 0.9}
	ApplySweepLedgerToRow(other, l)
	if other.SavedUSD != 1.0 || other.NetUSD != 0.9 {
		t.Errorf("another component's credit was changed by the sweep's ledger: %+v", other)
	}
	// And the totals still carry every expand, as before: SavedAdjusted is unchanged by the split.
	if back.ExpandTokens != 4_000 {
		t.Errorf("the total expand tokens were changed to %d", back.ExpandTokens)
	}
}

// ⭐ M2. A replay of the two losing sweeps from #404's Technical details (-$0.57 and -$0.45) shows no
// loss and no false credit.
//
// BUILT FROM THE NUMBERS IN THE ISSUE, not from stored customer data. The price is the one the issue's
// own credited figures imply: 29,978 tokens x $2.50/MTok (the write rate of a $2/MTok input) x 1.69
// (the sonnet tokenizer factor, #240) = $0.1267, and the issue says $0.126; 11,788 gives $0.0498 against
// $0.050. So this is the gateway's Sonnet 5 price, and the test first reproduces the issue's credit
// with the real pricing path before it says anything about the new design.
//
//	                                d7e06cef (req 44)    e8f16627 (req 3472)
//	removed locally                 29,978               11,788
//	sweep turn cache_write          283,064              316,084
//	billed prefix before -> after   392k -> 346k         395k -> 379k
//	question cost (whole)           $0.036               $0.062
//	credited in saved_usd           $0.126               $0.050
//	turns the session had left      9                    119
//
// THE OLD LOSS. The sweep fired while the cache was LIVE, so the request rewrote the prefix at the
// write rate where a read was due: extra = cache_write x (write - read) = $0.651 and $0.727, which is
// the issue's "$0.65-0.73". The credit counted the removed tokens and not that rewrite.
//
// THE NEW DESIGN. The apply happens on a COLD request, whose prefix is written anyway, so there is no
// extra rewrite: the cold request with the drops writes `after` where it would have written `before`.
// The question replaces a ping, so only the answer is the sweep's cost.
func TestM2ReplayOfTheTwoLosingSweepsShowsNoLossAndNoFalseCredit(t *testing.T) {
	p := modelinfo.Price{Input: 2e-6, CacheRead: 0.2e-6, CacheWrite: 2.5e-6, Output: 10e-6}
	factor, _ := tokens.BilledDeltaFactor("claude-sonnet-5")

	type sweep struct {
		name                  string
		removedLocal          int
		turnRead, turnWrite   int64
		before, after         int64
		question, issueCredit float64
		issueNet              float64
		turnsLeft             int
	}
	for _, s := range []sweep{
		{"d7e06cef", 29_978, 63_078, 283_064, 392_000, 346_000, 0.036, 0.126, -0.57, 9},
		{"e8f16627", 11_788, 62_965, 316_084, 395_000, 379_000, 0.062, 0.050, -0.45, 119},
	} {
		t.Run(s.name, func(t *testing.T) {
			removedBilled := s.before - s.after

			// --- 1. Reproduce the issue's credit with the real pricing path. If this is off, the
			// price or the path is not the issue's, and nothing below means anything.
			old := &Event{TS: 1, TenantID: "t", SessionID: s.name, Model: "claude-sonnet-5", Status: 200,
				TokensBefore: 100_000, TokensAfter: 100_000 - s.removedLocal, SavedUnique: s.removedLocal,
				FreshInput: 50, CacheRead: s.turnRead, CacheWrite: s.turnWrite, OutputTokens: 400,
				CacheMissReason: CachePrefixChange,
				Components: []CompRow{{Component: SweepComponent, Kind: "offload", Acted: true, Mutated: true,
					SavedGross: s.removedLocal, SavedUnique: s.removedLocal}}}
			old.Price(p, true)
			if got := old.Components[0].SavedUSD; math.Abs(got-s.issueCredit) > 0.003 {
				t.Fatalf("the pricing path gives a credit of %.4f; the issue's was %.3f. The fixture's price is not the issue's", got, s.issueCredit)
			}

			// --- 2. The old loss, from the issue's arithmetic: the rewrite the credit ignored.
			extra := float64(s.turnWrite) * (p.CacheWrite - p.CacheRead)
			oldNet := s.issueCredit - extra - s.question
			if oldNet >= 0 {
				t.Fatalf("fixture: the old way must LOSE on this sweep, got %+.3f", oldNet)
			}
			t.Logf("OLD   rewrite extra %.3f, net %+.3f (issue says %+.2f)", extra, oldNet, s.issueNet)

			// --- 3. The new design on the COLD request that applies the drops.
			coldWith := &Event{TS: 2, TenantID: "t", SessionID: s.name, Model: "claude-sonnet-5", Status: 200,
				TokensBefore: int(s.before), TokensAfter: int(s.after), SavedUnique: s.removedLocal,
				FreshInput: 50, CacheWrite: s.after, OutputTokens: 400, CacheMissReason: CacheTTLExpiry,
				Components: []CompRow{{Component: SweepComponent, Kind: "offload", Acted: true, Mutated: true,
					SavedGross: s.removedLocal, SavedUnique: s.removedLocal}}}
			coldWith.Price(p, true)
			coldWithout := &Event{TS: 2, TenantID: "t", SessionID: s.name, Model: "claude-sonnet-5", Status: 200,
				FreshInput: 50, CacheWrite: s.before, OutputTokens: 400, CacheMissReason: CacheTTLExpiry}
			coldWithout.Price(p, true)

			// NO LOSS ON THE REQUEST: the cold request with the drops costs less than without them, by
			// exactly the removed tokens at the write rate. There is no extra rewrite term at all.
			cash := coldWithout.CostUSD - coldWith.CostUSD
			if want := float64(removedBilled) * p.CacheWrite; !approx(cash, want) {
				t.Errorf("cash saved on the cold request = %v, want billed-removed x write rate = %v", cash, want)
			}
			if cash <= 0 {
				t.Fatalf("the cold apply made its own request dearer: with %.4f, without %.4f", coldWith.CostUSD, coldWithout.CostUSD)
			}

			// NO FALSE CREDIT, in the sense the issue means: the credit is the removed tokens at the write
			// rate on this request and carries NO term for a rewrite that is not happening, and it is the
			// SAME arithmetic that reproduced the issue's figure in step 1.
			credit := coldWith.Components[0].SavedUSD
			if want := float64(s.removedLocal) * p.CacheWrite * factor; !approx(credit, want) {
				t.Errorf("cold-request credit = %v, want local-removed x write rate x factor = %v", credit, want)
			}
			// The credit is an ESTIMATE in the provider's units: local count x the family factor. On
			// these two sweeps it is above the provider's own count of what stopped being sent, because
			// the issue measured billed/local of 1.53 and 1.36 where the factor is 1.69. That is #240's
			// per-family average meeting two samples below it; it is the same for every component.
			t.Logf("NEW   cold-request credit %.4f vs cash saved %.4f (x%.2f: billed/local %.2f against the factor %.2f)",
				credit, cash, credit/cash, float64(removedBilled)/float64(s.removedLocal), factor)

			// --- 4. The NET as the dashboard keeps it: credit minus the answer. The answer is part of
			// the question's cost, so the whole question is the most it can be. Charged in full:
			worst := credit - s.question
			// Every later request replays the same drops at the tier it paid. The issue says how many
			// turns each session had left. Warm turns, read rate.
			perTurn := float64(s.removedLocal) * p.CacheRead * factor
			total := credit + perTurn*float64(s.turnsLeft)
			t.Logf("NEW   worst-case net on the cold request alone %+.4f; per later warm turn +%.5f; over the %d turns left %+.4f (answer charged at the whole old question, %.3f)",
				worst, perTurn, s.turnsLeft, total-s.question, s.question)
			if total-s.question <= 0 {
				t.Errorf("the sweep still LOSES over the %d turns the session had left: %+.4f", s.turnsLeft, total-s.question)
			}
			// And the break-even is a number the dashboard can show: the answer must stay under it.
			if be := credit; be <= 0 {
				t.Errorf("no break-even: credit %v", be)
			}
		})
	}
}

// The false-drop count is DROPS expanded back, not expand calls: a stale tool error repaired on three
// turns, or an agent that asks for the same output twice, is one false drop. Two expands of one drop
// make Expands 2 and FalseExpands 1, and the rate counts the 1.
func TestTheFalseDropRateCountsDropsNotExpandCalls(t *testing.T) {
	db := openTestDB(t)
	cold := sweepEvent(1, "s", CacheTTLExpiry, 0, 90_000, 50, 10_000, 10_000)
	cold.Components[0].Events = map[string]int{"sweep_applied_cold": 4}
	first := sweepEvent(2, "s", CacheHit, 80_000, 12_000, 50, 10_000, 0)
	first.SweepExpands, first.SweepExpandTokens, first.SweepFalseExpands = 1, 1_000, 1
	again := sweepEvent(3, "s", CacheHit, 80_000, 200, 50, 10_000, 0)
	again.SweepExpands, again.SweepExpandTokens, again.SweepFalseExpands = 1, 1_000, 0 // the same drop again
	ref := sweepEvent(4, "s", CacheHit, 80_000, 200, 50, 10_000, 0)
	ref.Components[0].Events = map[string]int{"sweep_false_ref": 1}
	if err := db.insertBatch([]*Event{cold, first, again, ref}); err != nil {
		t.Fatal(err)
	}
	l, err := db.SweepLedgerFor(context.Background(), Filter{TenantAll: true}, staticPricer{sweepPrice})
	if err != nil {
		t.Fatal(err)
	}
	if l.Applied != 4 || l.Expands != 2 || l.FalseExpands != 1 || l.Referenced != 1 {
		t.Fatalf("ledger = %+v, want 4 applied, 2 expand calls, 1 false expand, 1 referenced", l)
	}
	if l.FalseDrops != 2 || !approx(l.FalseDropRate, 0.5) {
		t.Errorf("false drops = %d rate = %v, want 2 of 4 (the repeated expand is not a second false drop)", l.FalseDrops, l.FalseDropRate)
	}
	if len(l.Threads) != 1 || l.Threads[0].Applied != 4 || l.Threads[0].FalseDrops != 2 {
		t.Errorf("per-thread = %+v, want one thread with 4 applied and 2 false", l.Threads)
	}
	if l.Threads[0].Thread == "s" || len(l.Threads[0].Thread) != 10 {
		t.Errorf("the thread label %q is not a hash", l.Threads[0].Thread)
	}
}

// The answer cost has its own column and is NOT taken off the credit a second time: the component's net
// already carries it as its model spend. A ledger with an answer cost must leave SavedUSD and NetUSD
// changed by the expand charge alone.
func TestTheAnswerCostIsNotSubtractedFromTheCreditAgain(t *testing.T) {
	l := &SweepLedger{Applied: 4, ExpandChargeUSD: 0.01, AnswerUSD: 0.30, Questions: 2}
	row := &ComponentRow{Component: SweepComponent, SavedUSD: 1.0, NetUSD: 0.7, NetUSDWithEstimate: 0.7}
	ApplySweepLedgerToRow(row, l)
	if !approx(row.SavedUSD, 0.99) || !approx(row.NetUSD, 0.69) || !approx(row.NetUSDWithEstimate, 0.69) {
		t.Errorf("saved %v net %v: only the 0.01 expand charge may come off, the 0.30 answer is already in the net", row.SavedUSD, row.NetUSD)
	}
	if !approx(row.SweepAnswerUSD, 0.30) {
		t.Errorf("the answer cost is not carried in its own column: %v", row.SweepAnswerUSD)
	}
}
