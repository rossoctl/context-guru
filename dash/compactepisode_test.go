package dash

import (
	"math"
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/compactionpoint"
	"github.com/rossoctl/context-guru/kvcache"
)

// The episode walk's tests are written against walkCompactEpisodes rather than against the
// database, because every interesting case is a SEQUENCE of turns — a span closing exactly on its
// boundary, a client compaction inside it, a roll-forward, a model whose window is a guess — and a
// test that has to insert rows to express one is a test nobody writes the fifth variant of. One
// test at the bottom does go through SQL, to pin that the query feeds the walk what it expects.

const (
	testWindow = 1_000_000
	// 10% of the window: the span every episode below has to cross to close.
	testSpan = 100_000
)

// exactWindow / guessedWindow are the two window verdicts, as the walk sees them.
func exactWindow(string) (int, bool)   { return testWindow, true }
func guessedWindow(string) (int, bool) { return testWindow, false }

// testPrice is a priced model with round rates, so an expected dollar figure in a test is
// arithmetic a reader can do in their head.
// defaultCeilingForTest is the compaction point an unlisted model gets: the whole window. Stated here
// rather than imported, because a test asserting "the fallback is the window" should say the number.
const defaultCeilingForTest = 1.00

func testPrice(string) kvcache.Pricing {
	return kvcache.Pricing{Model: "m", Known: true,
		Input: 1e-6, CacheRead: 1e-7, Write5m: 1.25e-6, Write1h: 2e-6}
}

func unpricedModel(string) kvcache.Pricing {
	return kvcache.Pricing{Model: "m", Known: false}
}

// row builds one turn. The defaults are a warm turn that did nothing.
func row(ts, billed int64, opts ...func(*compactRow)) compactRow {
	r := compactRow{
		Tenant: "t", Session: "s", Model: "m", TS: ts,
		// TokensBefore is derived from ts and therefore MONOTONE, deliberately. Deriving it from
		// `billed` (the original choice) made it fall whenever a turn sent less — which is what
		// every turn after a summary does — and a falling tokens_before is exactly how the walk
		// detects a CLIENT-side compaction, so every fixture silently voided its own episode. The
		// real relationship is the opposite: our outgoing shrinks while the client's transcript
		// keeps growing.
		// CacheRead and CacheWrite are left at zero, so `billed` here is entirely FRESH input and
		// therefore entirely new content. That keeps every fixture's span arithmetic readable —
		// "billed" in a fixture means "new content" — while the cache-shaped cases that actually
		// matter (a warm turn's written tail, a cold turn's re-creation) set the split explicitly.
		// TestAtProductionScaleTheSpanSurvivesMoreThanOneTurn is the one that uses real shapes.
		Billed: billed, TokensBefore: 100_000 + ts*100, MissReason: CacheHit,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func fresh(r *compactRow) {
	r.Acted = true
	r.Events = `{"` + offload.EventFreshSummary + `":1}`
}

func replay(r *compactRow) {
	r.Acted = true
	r.Events = `{"` + offload.EventReusedCheckpoint + `":1}`
}

// actedWithNoEvents is a row as it was written BEFORE summarize filed an event on its fresh path:
// the acted flag and nothing else. The inferred population.
func actedWithNoEvents(r *compactRow) { r.Acted = true }

// saved is how many tokens summarize removed on this turn. The credit's DOLLARS are computed by
// creditTurn from this quantity and the turn's own cache verdict, so a fixture states the quantity
// and the assertion states the rate — see that function for why the stored saved_usd is not the
// credit.
//
// saved_usd is still set non-zero, because creditTurn reads it as the "summarize priced something
// here" flag. Its VALUE is deliberately absurd (999) so that any assertion which accidentally
// depends on it fails loudly instead of matching a plausible number.
func saved(tokens int64) func(*compactRow) {
	return func(r *compactRow) { r.SavedGross = tokens; r.SavedUSD = 999 }
}

// The two rates testPrice publishes, named so an expectation reads as quantity x rate rather than
// as a magic constant. readRate is what a removed span would have cost to re-READ on a warm turn;
// writeRate is what it would have cost to RE-CREATE on a turn whose entry had lapsed.
const (
	readRate  = 1e-7
	writeRate = 1.25e-6
)

func miss(reason string) func(*compactRow) {
	return func(r *compactRow) { r.MissReason = reason }
}

func wrote(tokens int64) func(*compactRow) {
	return func(r *compactRow) { r.CacheWrite = tokens }
}

func tokensBefore(n int64) func(*compactRow) {
	return func(r *compactRow) { r.TokensBefore = n }
}

func cgCost(usd float64) func(*compactRow) {
	return func(r *compactRow) { r.CGLLMCostUSD = usd }
}

func walk(rows []compactRow, w windowFn, p priceFn) *CompactionEpisodes {
	return walkCompactEpisodes(rows, w, p, 0.10, 0.90, defaultCeilingForTest)
}

// offUSD reports whether a dollar figure misses its target, with a tolerance.
//
// These figures are sums of float64 rates: 0.05 + 0.07 is 0.12000000000000001, and 200000 * 1.25e-6
// summed twice is not bit-identical to 400000 * 1.25e-6. An exact comparison here asserts IEEE-754
// behaviour rather than the measurement, and fails on arithmetic that is correct to every digit
// anybody will ever read.
func offUSD(got, want float64) bool {
	d := got - want
	return d > 1e-9 || d < -1e-9
}

// only returns the sole episode, failing if there is not exactly one. Most cases below are about
// one episode's shape, and asserting the count first turns "wrong number" into a clear failure
// instead of an index panic.
func only(t *testing.T, out *CompactionEpisodes) CompactionEpisode {
	t.Helper()
	if len(out.Episodes) != 1 {
		t.Fatalf("want exactly 1 episode, got %d: %+v", len(out.Episodes), out.Episodes)
	}
	return out.Episodes[0]
}

// The span is 10% MORE billed input, and the boundary is inclusive. A turn one token short of it
// leaves the episode open; the turn that reaches it closes.
func TestAnEpisodeClosesWhenTheSessionHasBeenBilledTenPercentMoreOfTheWindow(t *testing.T) {

	// Cumulative spend: one turn of (testSpan-1) is one short of the span.
	// t0 is deliberately SMALL: cumulative spend closes the span, so a large t0 would close it on
	// its own turn and leave no span to measure.
	short := walk([]compactRow{
		row(1, 1_000, fresh),
		row(2, testSpan-1, saved(100_000)),
	}, exactWindow, testPrice)
	if e := only(t, short); e.State != EpisodeOpen {
		t.Errorf("one token short of the span must leave the episode open, got %q", e.State)
	}

	closed := walk([]compactRow{
		row(1, 1_000, fresh),
		row(2, testSpan-1, saved(100_000)),
		row(3, 1, saved(100_000)), // one more token of spend reaches the span exactly
	}, exactWindow, testPrice)
	e := only(t, closed)
	if e.State != EpisodeClosed {
		t.Fatalf("reaching the span must close the episode, got %q (gates none; episode %+v)", e.State, e)
	}
	if e.Turns != 3 {
		t.Errorf("closed episode covered %d turns, want 3", e.Turns)
	}
	if e.NewContentBilled != testSpan {
		t.Errorf("new content = %d, want exactly the span %d", e.NewContentBilled, testSpan)
	}
	if e.StartBilled != 1_000 {
		t.Errorf("start_billed = %d, want t0's own size 1000", e.StartBilled)
	}
}

// A guessed window contributes NOTHING rather than a zero, and says so. The substring table of
// last resort answers 200,000 for every Opus against a real 1,000,000, so a 10% span sized against
// it measures 20,000 tokens of work while claiming 100,000 — a fifth of the span, and every
// per-episode figure five times too small.
func TestAGuessedWindowProducesNoEpisodeAndIsCounted(t *testing.T) {
	rows := []compactRow{
		row(1, 1_000, fresh),
		row(2, testSpan, saved(100_000)),
	}
	out := walk(rows, guessedWindow, testPrice)
	if len(out.Episodes) != 0 {
		t.Errorf("a guessed window must yield no episodes, got %+v", out.Episodes)
	}
	if out.Coverage.WindowUnknown != 1 {
		t.Errorf("window_unknown = %d, want 1 — an excluded conversation must be counted, "+
			"or the population is silently narrowed", out.Coverage.WindowUnknown)
	}
	// And the same rows with an exact window DO produce one, so this test cannot pass because
	// the fixture was simply too small.
	if got := walk(rows, exactWindow, testPrice); len(got.Episodes) != 1 {
		t.Fatalf("precondition: the same rows with an exact window must produce 1 episode, got %d",
			len(got.Episodes))
	}
}

// The client compacting its own transcript ends the measurement. It just did the thing the trigger
// exists to get ahead of, so the rest of the span is not comparable to a world where we had not
// compacted — and a voided episode must not contribute money.
func TestAClientCompactionInsideTheSpanVoidsTheEpisode(t *testing.T) {
	out := walk([]compactRow{
		row(1, 1_000, fresh, tokensBefore(300_000)),
		row(2, 10_000, saved(100_000), tokensBefore(310_000)),
		// The drop: our own message-token count falls, which only a client-side compaction does.
		row(3, 10_000, saved(100_000), tokensBefore(40_000)),
		row(4, testSpan, saved(100_000), tokensBefore(50_000)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeVoided {
		t.Fatalf("state = %q, want %q", e.State, EpisodeVoided)
	}
	for _, g := range out.ByProvenance {
		if g.Voided != 1 {
			t.Errorf("provenance %q: voided = %d, want 1", g.Provenance, g.Voided)
		}
		if g.NetUSD != 0 || g.ColdCreditUSD != 0 || g.ReadCreditUSD != 0 {
			t.Errorf("a voided episode must contribute no money, got net %v cold %v read %v",
				g.NetUSD, g.ColdCreditUSD, g.ReadCreditUSD)
		}
	}
}

// An episode whose span never closes is reported as open and kept out of every total: its figures
// would grow purely because the query window moved, which is not a property of the traffic.
func TestAnOpenEpisodeIsReportedButNotTotalled(t *testing.T) {
	out := walk([]compactRow{
		row(1, 1_000, fresh),
		row(2, 10_000, saved(100_000), miss(CacheTTLExpiry)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeOpen {
		t.Fatalf("state = %q, want %q", e.State, EpisodeOpen)
	}
	if len(out.ByProvenance) != 1 {
		t.Fatalf("want one provenance group, got %+v", out.ByProvenance)
	}
	g := out.ByProvenance[0]
	if g.Open != 1 || g.Closed != 0 {
		t.Errorf("group counts: open=%d closed=%d, want 1 and 0", g.Open, g.Closed)
	}
	if g.ColdCreditUSD != 0 || g.NetUSD != 0 {
		t.Errorf("an open episode must not be totalled: cold=%v net=%v", g.ColdCreditUSD, g.NetUSD)
	}
}

// The credit split IS the answer to the question this panel was built for: what the cold rewrites
// would have cost, and separately what the warm reads saved. Each turn's saving lands in the bucket
// its own cache verdict names, and prefix_change lands in NONE of them — that verdict means the
// prompt had changed, which on a summarized session is frequently our own doing, so crediting it
// would pay this component for the misses it caused.
func TestCreditIsSplitByTheCacheVerdictOfTheTurnThatEarnedIt(t *testing.T) {
	out := walk([]compactRow{
		row(1, 1_000, fresh),
		// Equal, small turns: the span must close on the LAST turn so every verdict above is
		// inside it. Uneven sizes closed it at turn 5 and silently dropped turn 6's credit.
		row(2, 10_000, saved(400_000), miss(CacheTTLExpiry)),
		row(3, 10_000, saved(100_000), miss(CacheHit)),
		row(4, 10_000, saved(200_000), miss(CacheColdStart)),
		row(5, 10_000, saved(800_000), miss(CachePrefixChange)),
		row(6, testSpan, saved(300_000), miss(CacheHit)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeClosed {
		t.Fatalf("precondition: the episode must close, got %q", e.State)
	}
	// EACH BUCKET ALSO PINS ITS RATE, which is the half that was wrong. A cold turn's removal is
	// priced as a re-CREATION and a warm turn's as a re-READ, so the same 400k tokens are worth
	// 12.5x more on the turn whose entry had lapsed. Asserting only the totals let a figure priced
	// at the write rate sit in the read bucket.
	if want := 400_000 * writeRate; offUSD(e.ColdCreditUSD, want) {
		t.Errorf("cold credit = %v, want %v (the ttl_expiry turn's 400k removal at the "+
			"cache-CREATION rate, which is what a lapsed entry would have paid to rebuild)",
			e.ColdCreditUSD, want)
	}
	if want := 400_000 * readRate; offUSD(e.ReadCreditUSD, want) {
		t.Errorf("read credit = %v, want %v (the two hit turns, 100k + 300k, at the cache-READ "+
			"rate — a live entry would have SERVED the removed span, not rewritten it)",
			e.ReadCreditUSD, want)
	}
	if want := 200_000 * readRate; offUSD(e.OtherCreditUSD, want) {
		t.Errorf("other credit = %v, want %v (the cold_start turn, at the read rate)",
			e.OtherCreditUSD, want)
	}
	// And the read bucket must NOT be at the write rate — the defect a live review measured, which
	// inverted the panel's sign. Stated as its own assertion so it cannot be lost in a total.
	if bad := 400_000 * writeRate; !offUSD(e.ReadCreditUSD, bad) {
		t.Errorf("read credit = %v, which is the cache-WRITE rate on turns whose cache HIT. "+
			"Nothing summarize removes inside a span is new content, so the write rate belongs "+
			"only to the lapsed-entry bucket", e.ReadCreditUSD)
	}
	// The prefix_change turn's 800k must be nowhere.
	want := 400_000*writeRate + 400_000*readRate + 200_000*readRate
	if total := e.ColdCreditUSD + e.ReadCreditUSD + e.OtherCreditUSD; offUSD(total, want) {
		t.Errorf("credited %v in total, want %v — a prefix_change turn's saving must not be "+
			"credited to this component, which caused the change", total, want)
	}
}

// t0's own cache write is charged even when the row is labelled `hit`, and it has to be: a PARTIAL
// hit reads as `hit` (repeatRate's first guard says why — which side of the boundary the removed
// content sat on is not knowable from the usage block), so a debit derived from the label alone
// would silently be zero on exactly the turns where we rewrote a live prefix.
func TestTheInvalidationDebitComesFromTheWriteNotTheLabel(t *testing.T) {
	out := walk([]compactRow{
		// A partial hit: labelled hit, and 40k tokens of cache creation all the same.
		row(1, 1_000, fresh, miss(CacheHit), wrote(40_000), cgCost(0.05)),
		row(2, testSpan, saved(600_000), miss(CacheHit)),
	}, exactWindow, testPrice)

	e := only(t, out)
	want := 40_000 * writeRate
	if offUSD(e.InvalidationDebitUSD, want) {
		t.Errorf("invalidation debit = %v, want %v (40k written at the 5m rate) — a debit taken "+
			"from cache_miss_reason would be 0 here", e.InvalidationDebitUSD, want)
	}
	if offUSD(e.SummarizerCostUSD, 0.05) {
		t.Errorf("summarizer cost = %v, want 0.05", e.SummarizerCostUSD)
	}
	if got, exp := e.NetUSD, 600_000*readRate-want-0.05; offUSD(got, exp) {
		t.Errorf("net = %v, want %v (credits minus both debits)", got, exp)
	}
}

// An unpriced model is not a free one. Every dollar is left absent and the episode is counted
// apart, so no total picks these up as real figures — the lie kvcache.Pricing.Known exists to stop.
func TestAnUnpricedModelLeavesTheMoneyAbsentRatherThanZero(t *testing.T) {
	out := walk([]compactRow{
		// cgCost is REQUIRED for this fixture to exercise anything: summarizer_cost_usd is the one
		// dollar field that does not depend on the model's rates, so without a cg cost here the
		// "no dollars published" assertion below passes vacuously.
		row(1, 1_000, fresh, wrote(40_000), cgCost(0.05)),
		row(2, testSpan, saved(600_000), miss(CacheTTLExpiry)),
	}, exactWindow, unpricedModel)

	e := only(t, out)
	if e.Priced {
		t.Fatal("episode on a model with no rates must not be marked priced")
	}
	if e.NetUSD != 0 {
		t.Errorf("net = %v, want 0 — an unpriced episode reports no dollars", e.NetUSD)
	}
	// EVERY dollar field, not just the net: summarizer_cost_usd is a stored per-request figure that
	// does not depend on this model's rates, so it was the one field that survived into an unpriced
	// episode's JSON. The rendered panel masked it; a JSON consumer of this public route did not.
	if e.ColdCreditUSD != 0 || e.ReadCreditUSD != 0 || e.OtherCreditUSD != 0 ||
		e.InvalidationDebitUSD != 0 || e.SummarizerCostUSD != 0 {
		t.Errorf("an unpriced episode published dollars: cold=%v read=%v other=%v debit=%v cg=%v",
			e.ColdCreditUSD, e.ReadCreditUSD, e.OtherCreditUSD,
			e.InvalidationDebitUSD, e.SummarizerCostUSD)
	}
	g := out.ByProvenance[0]
	if g.UnpricedEpisodes != 1 {
		t.Errorf("unpriced_episodes = %d, want 1", g.UnpricedEpisodes)
	}
	if g.ColdCreditUSD != 0 || g.NetUSD != 0 {
		t.Errorf("unpriced episodes must not enter the totals: cold=%v net=%v",
			g.ColdCreditUSD, g.NetUSD)
	}
}

// The conversation key is (tenant, session, model) — exactly kvcache.Conversation, because a cache
// entry does not transfer between models. Two models in one session are two conversations, and one
// cannot close the other's span.
func TestTwoModelsInOneSessionAreTwoConversations(t *testing.T) {
	a := row(1, 1_000, fresh)
	b := row(2, testSpan, saved(900_000))
	b.Model = "other"
	out := walk([]compactRow{a, b}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeOpen {
		t.Errorf("state = %q, want %q — a request on a DIFFERENT model must not close this "+
			"episode's span, since it could never have read this model's cache entry",
			e.State, EpisodeOpen)
	}
	if e.ReadCreditUSD != 0 {
		t.Errorf("read credit = %v, want 0 — the other model's saving is not this episode's",
			e.ReadCreditUSD)
	}
}

// Recorded and inferred are two different claims and are never added together. Recorded is a fact
// filed by the component; inferred is deduced from the ABSENCE of a replay name on an older row,
// which is the only way to see anything at all from before the event existed — and therefore the
// only available comparison against the old size-only trigger.
func TestRecordedAndInferredEpisodesAreReportedApart(t *testing.T) {
	rec := []compactRow{row(1, 1_000, fresh), row(2, testSpan, saved(400_000), miss(CacheTTLExpiry))}
	inf := []compactRow{row(3, 1_000, actedWithNoEvents), row(4, testSpan, saved(600_000), miss(CacheTTLExpiry))}
	inf[0].Session, inf[1].Session = "s2", "s2"

	out := walk(append(rec, inf...), exactWindow, testPrice)
	if len(out.ByProvenance) != 2 {
		t.Fatalf("want two provenance groups, got %+v", out.ByProvenance)
	}
	got := map[string]float64{}
	for _, g := range out.ByProvenance {
		got[g.Provenance] = g.ColdCreditUSD
	}
	wantRec, wantInf := 400_000*writeRate, 600_000*writeRate
	if offUSD(got[EpisodeRecorded], wantRec) || offUSD(got[EpisodeInferred], wantInf) {
		t.Errorf("cold credit by provenance = %v, want recorded %v and inferred %v kept apart",
			got, wantRec, wantInf)
	}
	// And the ordering is stable — recorded first — so a reader is not comparing two rows that
	// swapped places between refreshes.
	if out.ByProvenance[0].Provenance != EpisodeRecorded {
		t.Errorf("first group is %q, want %q", out.ByProvenance[0].Provenance, EpisodeRecorded)
	}
}

// A replay turn is NOT a t0. It is the amortization of a summary already paid for, and treating it
// as a new episode would restart the span on almost every turn of a healthy session.
func TestAReplayTurnDoesNotStartAnEpisode(t *testing.T) {
	out := walk([]compactRow{
		row(1, 1_000, replay, saved(300_000)),
		row(2, testSpan, replay, saved(300_000)),
	}, exactWindow, testPrice)
	if len(out.Episodes) != 0 {
		t.Errorf("replays must not open an episode, got %+v", out.Episodes)
	}
}

// The coverage half, without which the panel is survivorship: a conversation that got big enough
// to be this trigger's business and produced NOTHING is the population that decides whether the
// default earns its place, and it can never appear as an episode.
func TestAQualifyingConversationWithNoSummaryLandsInCoverageNotInEpisodes(t *testing.T) {
	// Billed past 0.9 of the window, several cold rewrites, and no summary anywhere.
	out := walk([]compactRow{
		row(1, 500_000),
		// Coverage asks whether the conversation ever GOT big, so these carry real sizes: it is a
		// question about reaching the fill threshold, not about cumulative spend.
		row(2, 910_000, miss(CacheTTLExpiry), wrote(200_000)),
		row(3, 950_000, miss(CacheTTLExpiry), wrote(300_000)),
	}, exactWindow, testPrice)

	if len(out.Episodes) != 0 {
		t.Fatalf("no summary was produced, so there can be no episode: %+v", out.Episodes)
	}
	c := out.Coverage
	if c.Conversations != 1 || c.NoEpisode != 1 || c.WithEpisode != 0 {
		t.Errorf("coverage = %+v, want 1 qualifying conversation with no episode", c)
	}
	want := 500_000 * writeRate // the two ttl_expiry turns' cache creation
	if offUSD(c.NoEpisodeColdUSD, want) {
		t.Errorf("no_episode_cold_usd = %v, want %v (what those cold rewrites actually paid)",
			c.NoEpisodeColdUSD, want)
	}
	if c.FillFrac != 0.90 {
		t.Errorf("fill_frac = %v, want the 0.90 the walk was given — it is a parameter and must "+
			"be reported as one", c.FillFrac)
	}
}

// A conversation that never reached the fill threshold is not this trigger's business and must not
// appear in the coverage denominator, or the "no episode" count fills up with small sessions and
// the number stops meaning anything.
func TestAConversationBelowTheFillThresholdIsNotCounted(t *testing.T) {
	out := walk([]compactRow{
		row(1, 100_000, miss(CacheTTLExpiry), wrote(50_000)),
		row(2, 200_000, miss(CacheTTLExpiry), wrote(50_000)),
	}, exactWindow, testPrice)
	if out.Coverage.Conversations != 0 {
		t.Errorf("coverage counted %d conversations; one that never reached 0.9 of the window "+
			"is not in this population", out.Coverage.Conversations)
	}
	if out.Coverage.NoEpisodeColdUSD != 0 {
		t.Errorf("no_episode_cold_usd = %v, want 0", out.Coverage.NoEpisodeColdUSD)
	}
}

// A roll-forward inside an open span belongs to that span: it is the same episode still being
// maintained, so its call cost and the cache write it caused are charged there rather than opening
// a second episode whose credit would double-count the first one's turns.
func TestARollForwardInsideTheSpanChargesTheSameEpisode(t *testing.T) {
	out := walk([]compactRow{
		row(1, 1_000, fresh, miss(CacheHit), cgCost(0.05), wrote(10_000)),
		row(2, 50_000, fresh, miss(CacheHit), cgCost(0.07), wrote(20_000), saved(200_000)),
		row(3, testSpan, saved(400_000), miss(CacheTTLExpiry)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if offUSD(e.SummarizerCostUSD, 0.12) {
		t.Errorf("summarizer cost = %v, want 0.12 (both calls charged to the span they happened in)",
			e.SummarizerCostUSD)
	}
	want := 30_000 * writeRate
	if offUSD(e.InvalidationDebitUSD, want) {
		t.Errorf("invalidation debit = %v, want %v (both rewrites)", e.InvalidationDebitUSD, want)
	}
}

// The turn that closes one span and opens the next is the only row that can belong to two
// episodes, and its SAVING must be counted once. The costs of the new summary belong to the new
// span; the saving was earned in the old one.
func TestTheTurnThatClosesOneSpanAndOpensAnotherIsCreditedOnce(t *testing.T) {
	out := walkCompactEpisodes([]compactRow{
		row(1, 1_000, fresh),
		// billed = span + the write, because on a MISS the written tokens are re-creation rather
		// than new content, so only the fresh remainder counts toward the span.
		// A HIT, deliberately: with miss(CacheTTLExpiry) causedWriteUSD returns 0, so the
		// double-charge this test exists to catch was invisible — the review that found the bug
		// found this fixture hiding it. A hit makes the write OURS and the debit non-zero.
		row(2, testSpan+8_000, fresh, saved(500_000), miss(CacheHit), wrote(8_000), cgCost(0.03)),
	}, exactWindow, testPrice, 0.10, 0.90, defaultCeilingForTest)

	if len(out.Episodes) != 2 {
		t.Fatalf("want 2 episodes (one closed, one opened by the same turn), got %d: %+v",
			len(out.Episodes), out.Episodes)
	}
	first, second := out.Episodes[0], out.Episodes[1]
	if first.State != EpisodeClosed {
		t.Errorf("first episode state = %q, want closed", first.State)
	}
	if want := 500_000 * readRate; offUSD(first.ReadCreditUSD, want) {
		t.Errorf("the closing turn's saving belongs to the span it closed: read credit = %v, want %v",
			first.ReadCreditUSD, want)
	}
	if second.ColdCreditUSD != 0 || second.ReadCreditUSD != 0 || second.OtherCreditUSD != 0 {
		t.Errorf("the same saving must not also be credited to the new span: %+v", second)
	}
	// BOTH SIDES OF THE COST, asserted on BOTH episodes. The bug was that each was charged twice —
	// once to the span that closed and again to the span that opened — so asserting only the new
	// episode's figure passed while the sum of episodes exceeded the money that existed.
	if offUSD(second.SummarizerCostUSD, 0.03) {
		t.Errorf("the new summary's own call belongs to the new span: got %v, want 0.03",
			second.SummarizerCostUSD)
	}
	if wantDebit := 8_000 * writeRate; offUSD(second.InvalidationDebitUSD, wantDebit) {
		t.Errorf("the write the new summary caused belongs to the new span: got %v, want %v",
			second.InvalidationDebitUSD, wantDebit)
	}
	if first.SummarizerCostUSD != 0 {
		t.Errorf("summarizer cost = %v on the CLOSING span; the call was an investment in the span "+
			"this turn OPENED, and charging both makes the sum of episodes exceed what was spent",
			first.SummarizerCostUSD)
	}
	if first.InvalidationDebitUSD != 0 {
		t.Errorf("invalidation debit = %v on the CLOSING span; the rewrite belongs to the span this "+
			"turn OPENED", first.InvalidationDebitUSD)
	}
}

// The assumptions the server states must actually describe what it did. The KV-cache page's rule:
// the arithmetic is served, not restated in a template nothing tests.
func TestTheServerStatesTheFractionsItActuallyUsed(t *testing.T) {
	out := walkCompactEpisodes(nil, exactWindow, testPrice, 0.25, 0.5, defaultCeilingForTest)
	if out.Assumptions.SpanFrac != 0.25 || out.Assumptions.FillFrac != 0.5 {
		t.Errorf("assumptions = %+v, want the 0.25/0.5 actually used", out.Assumptions)
	}
	if out.Coverage.FillFrac != 0.5 {
		t.Errorf("coverage fill_frac = %v, want 0.5", out.Coverage.FillFrac)
	}
	// A ZERO SPAN MEANS "DERIVED PER MODEL", not "fall back to a shipped constant". The span's far end
	// is the client's compaction ceiling and that is a per-model fact, so the top-level SpanFrac is the
	// explicit `?span=` OVERRIDE and the spans actually used are one per model in ClientCeilings. A
	// single top-level number would be an average of things that are not comparable.
	def := walkCompactEpisodes(nil, exactWindow, testPrice, 0, 0, 0)
	if def.Assumptions.SpanFrac != 0 {
		t.Errorf("span_frac = %v with no override; it must report 0 to mean DERIVED, or a reader takes "+
			"it for the span every model was measured against", def.Assumptions.SpanFrac)
	}
	if def.Assumptions.FillFrac != defaultFillFrac {
		t.Errorf("fill_frac = %v, want the shipped %v", def.Assumptions.FillFrac, defaultFillFrac)
	}
	// The shipped fill and the shipped ceiling must leave an attributable span, or the component
	// could never be credited for anything on a default deployment.
	if wantSpan, ok := spanFor(defaultFillFrac, defaultCeilingForTest); !ok {
		t.Error("the shipped fill and default ceiling leave no attributable span")
	} else if math.Abs(wantSpan-0.10) > 1e-9 {
		t.Errorf("the shipped pair derives a span of %v, want 0.10", wantSpan)
	}
	if def.Assumptions.SpanRule == "" {
		t.Error("the assumptions must state WHY the span is the width it is; a derived number the " +
			"page cannot explain is one the reader has to take on trust")
	}
	if def.Assumptions.KnownOmission == "" {
		t.Error("the assumptions must name what this measurement leaves out; an omission the " +
			"page does not print is one the reader cannot allow for")
	}
}

// isFreshSummary's three-way answer, pinned by name. The dashboard keys on strings the component
// owns, so a rename there must break a test here rather than quietly report zero episodes.
func TestFreshSummaryDetectionByEventName(t *testing.T) {
	cases := []struct {
		name string
		r    compactRow
		want string
	}{
		{"the event summarize files on its fresh path", row(1, 1, fresh), EpisodeRecorded},
		{"a reused checkpoint is not a fresh summary", row(1, 1, replay), ""},
		{"a gated replay is not a fresh summary",
			row(1, 1, func(r *compactRow) {
				r.Acted = true
				r.Events = `{"` + offload.EventGatedReplayedCheckpoint + `":1}`
			}), ""},
		{"a reserve-exhausted replay is not a fresh summary",
			row(1, 1, func(r *compactRow) {
				r.Acted = true
				r.Events = `{"` + offload.EventReserveExhaustedReplayedCheckpoint + `":1}`
			}), ""},
		{"acted with no events at all predates the event and is inferred",
			row(1, 1, actedWithNoEvents), EpisodeInferred},
		{"a turn that did nothing is not a summary", row(1, 1), ""},
	}
	for _, c := range cases {
		if got := isFreshSummary(c.r); got != c.want {
			t.Errorf("%s: isFreshSummary = %q, want %q", c.name, got, c.want)
		}
	}
}

// One pass through SQL, so the query and the walk cannot drift: the column list, the LEFT JOIN on
// summarize's component row, and the ordering all have to hold for any of the above to mean
// anything about real traffic.
func TestTheQueryFeedsTheWalkAClosedEpisode(t *testing.T) {
	db := openTestDB(t)

	base := 900_000
	t0 := mkEvent(DayMs+1, "sess", "m", 300_000, 100_000)
	t0.FreshInput, t0.CacheRead, t0.CacheWrite = int64(base), 0, 0
	t0.CacheMissReason = CacheTTLExpiry
	t0.CGLLMCostUSD = 0.05
	t0.Components = []CompRow{{
		Component: "summarize", Kind: "offload", Acted: true, Mutated: true,
		SavedGross: 200_000, SavedUnique: 200_000, SavedUSD: 2.5,
		Events: map[string]int{offload.EventFreshSummary: 1},
	}}

	t1 := mkEvent(DayMs+2, "sess", "m", 310_000, 110_000)
	t1.FreshInput, t1.CacheRead, t1.CacheWrite = int64(base+testSpan), 0, 0
	t1.CacheMissReason = CacheTTLExpiry
	t1.Components = []CompRow{{
		Component: "summarize", Kind: "offload", Acted: true, Mutated: true,
		SavedGross: 200_000, SavedUnique: 0, SavedUSD: 1.5,
		Events: map[string]int{offload.EventReusedCheckpoint: 1},
	}}

	if err := db.insertBatch([]*Event{t0, t1}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.compactEpisodeDataset(Filter{TenantAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("dataset returned %d rows, want 2 — the LEFT JOIN must keep every turn in the "+
			"span, not only the ones summarize touched: %+v", len(rows), rows)
	}
	if rows[0].Billed != int64(base) {
		t.Errorf("billed = %d, want %d (fresh + read + write)", rows[0].Billed, base)
	}
	if offUSD(rows[0].SavedUSD, 2.5) || offUSD(rows[1].SavedUSD, 1.5) {
		t.Errorf("saved_usd did not come through: %v and %v", rows[0].SavedUSD, rows[1].SavedUSD)
	}
	// saved_gross is what the credit is computed FROM, so it has to survive the round trip too —
	// the column was added to this query when the credit stopped inheriting saved_usd.
	if rows[0].SavedGross != 200_000 || rows[1].SavedGross != 200_000 {
		t.Errorf("saved_gross did not come through: %d and %d — the credit is priced from this "+
			"column, so a zero here silently empties every bucket",
			rows[0].SavedGross, rows[1].SavedGross)
	}

	out := walk(rows, exactWindow, testPrice)
	e := only(t, out)
	if e.State != EpisodeClosed {
		t.Fatalf("state = %q, want closed", e.State)
	}
	if e.Provenance != EpisodeRecorded {
		t.Errorf("provenance = %q, want %q — the stored event must survive the round trip",
			e.Provenance, EpisodeRecorded)
	}
	// THE REPLAY TURN ONLY. t0's own removal is deliberately not credited: at the moment of
	// compaction nothing has been saved — we have only spent a model call and a cache write. The
	// saving is what the LATER turns realize, which here is the single replay turn.
	//
	// And it is priced from saved_gross at the rate the replay turn's own verdict earns, NOT from
	// the stored saved_usd. The two fixtures make that visible: both turns removed the same 200,000
	// tokens, but their stored saved_usd differ ($2.50 vs $1.50) because saved_unique differs —
	// which is exactly the attribution the episode credit must not inherit.
	if want := 200_000 * writeRate; offUSD(e.ColdCreditUSD, want) {
		t.Errorf("cold credit = %v, want %v — the replay turn's 200k removal at the cache-creation "+
			"rate its lapsed entry would have paid. Reading the stored saved_usd instead would give "+
			"1.5, an attribution this panel does not use", e.ColdCreditUSD, want)
	}
	if offUSD(e.SummarizerCostUSD, 0.05) {
		t.Errorf("summarizer cost = %v, want 0.05", e.SummarizerCostUSD)
	}
}

// THE COMPACTION TURN IS DEBIT-ONLY, and this is the rule that keeps the panel from paying itself
// in advance.
//
// At t0 nothing has been saved. We have SPENT: a model call, and a cache write to put the new
// smaller prefix in place. The saving is realized on later turns — cheaper reads against a shorter
// transcript, and cold rewrites that cost 1.25xC instead of 1.25xF. An earlier version credited
// t0's own saved_usd, which booked a saving at the cache-CREATION rate for work that had not yet
// paid off: the one direction a savings figure must never lean.
func TestTheCompactionTurnIsChargedAndNotCredited(t *testing.T) {
	out := walk([]compactRow{
		// t0 carries a large saved_usd AND a large write. Only the write may survive.
		row(1, 1_000, fresh, saved(900_000), miss(CacheHit), wrote(40_000), cgCost(0.05)),
		row(2, testSpan, saved(200_000), miss(CacheHit)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeClosed {
		t.Fatalf("precondition: the episode must close, got %q", e.State)
	}
	if offUSD(e.ColdCreditUSD, 0) {
		t.Errorf("cold credit = %v, want 0 — t0's own removal must not be credited", e.ColdCreditUSD)
	}
	wantCredit := 200_000 * readRate
	if offUSD(e.ReadCreditUSD, wantCredit) {
		t.Errorf("read credit = %v, want %v (the later turn only)", e.ReadCreditUSD, wantCredit)
	}
	wantDebit := 40_000 * writeRate
	if offUSD(e.InvalidationDebitUSD, wantDebit) {
		t.Errorf("invalidation debit = %v, want %v — t0's write IS charged", e.InvalidationDebitUSD, wantDebit)
	}
	if offUSD(e.NetUSD, wantCredit-wantDebit-0.05) {
		t.Errorf("net = %v, want %v", e.NetUSD, wantCredit-wantDebit-0.05)
	}
}

// A cache write on a prefix_change turn INSIDE a span we opened is charged to us.
//
// prefix_change means the prompt no longer matched what was cached, and inside a summarized span
// the thing that changes the prompt is us. It is already excluded from the credit for that reason;
// excluding it from the debit too would be having it both ways — counting neither our damage nor
// its cost.
//
// This is not hypothetical. Observed on a live run: a turn whose summarizer call hung held the
// request past its own cache lifetime, the entry expired in our hands, the full 197k prefix was
// re-written, and the row was recorded as prefix_change because idle is measured at ARRIVAL while
// the expiry happened inside the pipeline.
func TestASelfCausedPrefixChangeWriteInsideTheSpanIsCharged(t *testing.T) {
	out := walk([]compactRow{
		row(1, 1_000, fresh, miss(CacheHit), wrote(5_000)),
		row(2, 50_000, saved(300_000), miss(CachePrefixChange), wrote(197_000)),
		row(3, testSpan, saved(100_000), miss(CacheHit)),
	}, exactWindow, testPrice)

	e := only(t, out)
	want := (5_000 + 197_000) * writeRate
	if offUSD(e.InvalidationDebitUSD, want) {
		t.Errorf("invalidation debit = %v, want %v (t0's write plus the self-caused rewrite)",
			e.InvalidationDebitUSD, want)
	}
	// And that turn's removal is still credited nowhere.
	if offUSD(e.ColdCreditUSD+e.OtherCreditUSD, 0) || offUSD(e.ReadCreditUSD, 100_000*readRate) {
		t.Errorf("a prefix_change turn must be credited nowhere: cold=%v read=%v other=%v",
			e.ColdCreditUSD, e.ReadCreditUSD, e.OtherCreditUSD)
	}
}

// An unfinished span reports its net rather than vanishing.
//
// Excluding it was survivorship one level below the coverage line: an unfinished span is exactly
// the case where we paid for a summary and have not yet recouped it, so dropping those made the
// panel systematically optimistic. It stays OUT of the settled total — a partial figure grows as
// the time range widens — but it is reported, and it is allowed to be negative.
func TestAnUnfinishedSpanReportsItsNetApartFromTheSettledTotal(t *testing.T) {
	out := walk([]compactRow{
		// Paid for a summary and a write; only one small read back so far.
		row(1, 1_000, fresh, miss(CacheHit), wrote(40_000), cgCost(0.05)),
		row(2, 10_000, saved(100_000), miss(CacheHit)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeOpen {
		t.Fatalf("precondition: the span must still be open, got %q", e.State)
	}
	want := 100_000*readRate - 40_000*writeRate - 0.05
	if offUSD(e.NetUSD, want) {
		t.Errorf("open episode net = %v, want %v — an unfinished span must carry a net, and it "+
			"is allowed to be negative", e.NetUSD, want)
	}
	g := out.ByProvenance[0]
	if offUSD(g.NetUSD, 0) {
		t.Errorf("settled net = %v, want 0 — an open span must not enter the settled total", g.NetUSD)
	}
	if offUSD(g.OpenNetUSD, want) {
		t.Errorf("open net = %v, want %v — the exposure must be visible", g.OpenNetUSD, want)
	}
	if g.OpenTurns != 2 {
		t.Errorf("open turns = %d, want 2", g.OpenTurns)
	}
	if out.Assumptions.OpenRule == "" {
		t.Error("the server must state what the open figure means; an unexplained second total " +
			"is worse than no second total")
	}
}

// A voided span also reports what it spent. The client compacted mid-span so the payoff is not
// comparable, but the money we spent was still spent.
func TestAVoidedSpanStillReportsWhatItSpent(t *testing.T) {
	out := walk([]compactRow{
		row(1, 1_000, fresh, miss(CacheHit), wrote(40_000), cgCost(0.05), tokensBefore(300_000)),
		row(2, 10_000, saved(200_000), miss(CacheHit), tokensBefore(310_000)),
		row(3, 20_000, saved(200_000), miss(CacheHit), tokensBefore(40_000)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeVoided {
		t.Fatalf("precondition: the span must be voided, got %q", e.State)
	}
	g := out.ByProvenance[0]
	if offUSD(g.NetUSD, 0) {
		t.Errorf("settled net = %v, want 0 — a voided span is not comparable", g.NetUSD)
	}
	if g.VoidedNetUSD >= 0 {
		t.Errorf("voided net = %v, want negative — we paid for a summary the client then "+
			"superseded, and that spend is reported rather than dropped", g.VoidedNetUSD)
	}
}

// A COMMISSIONED summary is a t0, and this test exists because a live run proved it was not.
//
// Summaries are produced off the hot path, so the goroutine that finishes one has no request row
// and no Report to file `fresh_summary` against. With the walk keying on that event alone, a real
// Claude Code session that fired the trigger, paid for a summary and spliced it on the next turn
// produced ZERO episodes — while the coverage half correctly reported one qualifying conversation
// with no episode. The measurement was blind to exactly the thing it was built to measure.
//
// Keying on the commission is also the better definition of t0: it carries the debits and no
// credit, which is true of the turn that spent the money whether or not the summary ever landed.
func TestACommissionedSummaryOpensAnEpisode(t *testing.T) {
	commissioned := func(r *compactRow) {
		r.Acted = false // the commissioning turn splices nothing, so it did not "act"
		r.Events = `{"` + offload.EventSummaryStarted + `":1}`
	}
	awaited := func(r *compactRow) {
		r.Acted = true
		r.Events = `{"` + offload.EventAwaitedCheckpoint + `":1}`
	}

	out := walk([]compactRow{
		row(1, 1_000, commissioned, wrote(40_000), cgCost(0.05), miss(CacheHit)),
		row(2, 50_000, awaited, saved(300_000), miss(CacheHit)),
		row(3, testSpan, saved(200_000), miss(CacheTTLExpiry)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.Provenance != EpisodeRecorded {
		t.Errorf("provenance = %q, want %q — summary_started is a recorded fact about that turn",
			e.Provenance, EpisodeRecorded)
	}
	if e.State != EpisodeClosed {
		t.Fatalf("state = %q, want closed", e.State)
	}
	if e.StartTS != 1 {
		t.Errorf("t0 = %d, want the commissioning turn (1)", e.StartTS)
	}
	// t0's costs are charged; t0 earns no credit.
	if offUSD(e.SummarizerCostUSD, 0.05) {
		t.Errorf("summarizer cost = %v, want 0.05 — the commissioning turn paid for the call",
			e.SummarizerCostUSD)
	}
	if offUSD(e.InvalidationDebitUSD, 40_000*writeRate) {
		t.Errorf("invalidation debit = %v, want %v", e.InvalidationDebitUSD, 40_000*writeRate)
	}
	// The awaited turn is a SPLICE, so it must not have opened a second episode — and its saving
	// belongs to this one.
	if want := 300_000 * readRate; offUSD(e.ReadCreditUSD, want) {
		t.Errorf("read credit = %v, want %v (the awaited turn's removal, at the read rate)",
			e.ReadCreditUSD, want)
	}
	if want := 200_000 * writeRate; offUSD(e.ColdCreditUSD, want) {
		t.Errorf("cold credit = %v, want %v (the closing turn, whose entry had lapsed)",
			e.ColdCreditUSD, want)
	}
}

// THE SPAN AXIS MUST BE MONOTONE, and this is the case that proves the obvious axis is not.
//
// Compaction REDUCES what a turn sends. On the live validation run the billed input fell from
// 154,584 to 31,682 the moment the summary landed. A span defined as "this turn's size exceeds
// t0's size plus 10% of the window" therefore waits for the transcript to regrow past a size the
// compaction had just removed — so it never closes, the episode stays `open` forever, and it
// contributes to no total. That is the same blindness as having no episode at all, arrived at from
// the opposite direction.
//
// Cumulative spend is monotone by construction and is what "10% more of the context has been
// spent" means.
func TestTheSpanClosesOnCumulativeSpendEvenWhenEachTurnGetsSmaller(t *testing.T) {
	out := walk([]compactRow{
		row(1, 1_000, fresh, miss(CacheTTLExpiry), tokensBefore(122_043)),
		// Every later turn SENDS far less than t0 — exactly as it does once a summary lands — while
		// the client's own transcript keeps GROWING. Both halves are from the live run.
		row(2, 31_682, saved(105_250), miss(CacheHit), tokensBefore(122_190)),
		row(3, 31_700, saved(105_367), miss(CacheHit), tokensBefore(122_324)),
		row(4, 31_700, saved(105_469), miss(CacheHit), tokensBefore(122_562)),
		row(5, 31_700, saved(105_536), miss(CacheHit), tokensBefore(122_800)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeClosed {
		t.Fatalf("state = %q, want closed: 31,682 x 4 is past 10%% of a 1M window, and a "+
			"span that waits for a per-turn SIZE to exceed t0's would never close here",
			e.State)
	}
	if e.NewContentBilled < testSpan {
		t.Errorf("new content = %d, want at least the span %d", e.NewContentBilled, testSpan)
	}
}

// A COLD t0 COSTS US NOTHING IN CACHE WRITES, and getting this wrong turned a gain into a reported
// loss on the live run.
//
// t0 there was a genuine ttl_expiry: the entry had already lapsed, so that turn was going to write
// its whole prefix whatever we did. Charging its 154,581-token write to us produced a $0.193 debit
// against $0.136 of credit — a reported net LOSS on what is in fact the cheapest possible moment to
// compact, and the exact moment the trigger's default was widened to catch.
func TestAColdT0IsNotChargedForAWriteThatWasDueAnyway(t *testing.T) {
	cold := walk([]compactRow{
		row(1, 1_000, fresh, miss(CacheTTLExpiry), wrote(154_581), tokensBefore(122_043)),
		row(2, 31_682, saved(105_250), miss(CacheHit), tokensBefore(122_190)),
		row(3, 31_700, saved(105_367), miss(CacheHit), tokensBefore(122_324)),
		row(4, 31_700, saved(105_469), miss(CacheHit), tokensBefore(122_562)),
		row(5, 31_700, saved(105_536), miss(CacheHit), tokensBefore(122_800)),
	}, exactWindow, testPrice)
	e := only(t, cold)
	if offUSD(e.InvalidationDebitUSD, 0) {
		t.Errorf("invalidation debit = %v, want 0 — the entry had already expired, so that write "+
			"was due whatever we did and none of it is our cost", e.InvalidationDebitUSD)
	}
	if e.NetUSD <= 0 {
		t.Errorf("net = %v, want positive: crediting the reads while charging a write we did not "+
			"cause reports a loss on the cheapest moment to compact", e.NetUSD)
	}

	// The mirror: a t0 whose cache was LIVE did cause its write, and is charged.
	warm := walk([]compactRow{
		row(1, 1_000, fresh, miss(CacheHit), wrote(100_000), tokensBefore(122_043)),
		row(2, 31_682, saved(105_250), miss(CacheHit), tokensBefore(122_190)),
		row(3, 31_700, saved(105_367), miss(CacheHit), tokensBefore(122_324)),
		row(4, 31_700, saved(105_469), miss(CacheHit), tokensBefore(122_562)),
		row(5, 31_700, saved(105_536), miss(CacheHit), tokensBefore(122_800)),
	}, exactWindow, testPrice)
	e2 := only(t, warm)
	if offUSD(e2.InvalidationDebitUSD, 100_000*1.25e-6) {
		t.Errorf("invalidation debit = %v, want %v — a live entry that we rewrote IS our cost",
			e2.InvalidationDebitUSD, 100_000*1.25e-6)
	}
}

// AT PRODUCTION SCALE, with the live run's own numbers. Every other span test here keeps t0 tiny
// and the window at 1,000,000, and that is what hid a real defect: with the axis on cumulative
// SPEND, the live run's 200,000 window gave a 20,000 span against a next-turn 31,682, so the span
// closed on the turn immediately after t0 — seconds of wall clock, in which no cold miss can occur,
// leaving the headline ColdCreditUSD bucket structurally empty.
//
// The fixtures could not show it because a 1M window makes the span 100,000, which the small
// fixture turns never reach. So this test uses the real window and the real per-turn figures, and
// asserts the span is long enough to still be open after several turns.
func TestAtProductionScaleTheSpanSurvivesMoreThanOneTurn(t *testing.T) {
	const haikuWindow = 200_000
	window := func(string) (int, bool) { return haikuWindow, true }

	// The live run, verbatim: t0 is a cold turn that re-created its whole prefix, and the turns
	// after it are compacted and warm. read/write are what the provider reported.
	rows := []compactRow{
		{Tenant: "t", Session: "s", Model: "m", TS: 1, Billed: 167_266, CacheRead: 0,
			CacheWrite: 167_263, MissReason: CacheTTLExpiry, TokensBefore: 122_043,
			Events: `{"` + offload.EventSummaryStarted + `":1}`},
		// saved_gross is the removed span on each replay turn — roughly the whole compacted-away
		// prefix, re-removed every turn, which is what makes the credit an amortization rather
		// than a one-off. saved_usd is left at its live values deliberately: they are what the
		// credit used to be read from, and 0.14 on the first replay turn is the write-rate
		// mispricing this panel no longer inherits.
		{Tenant: "t", Session: "s", Model: "m", TS: 2, Billed: 32_056, CacheRead: 28_730,
			CacheWrite: 3_323, MissReason: CacheHit, TokensBefore: 122_190,
			SavedGross: 105_250, SavedUSD: 0.14},
		{Tenant: "t", Session: "s", Model: "m", TS: 3, Billed: 32_214, CacheRead: 32_053,
			CacheWrite: 158, MissReason: CacheHit, TokensBefore: 122_324,
			SavedGross: 105_367, SavedUSD: 0.01},
		{Tenant: "t", Session: "s", Model: "m", TS: 4, Billed: 32_489, CacheRead: 32_211,
			CacheWrite: 275, MissReason: CacheHit, TokensBefore: 122_562,
			SavedGross: 105_469, SavedUSD: 0.01},
	}
	out := walkCompactEpisodes(rows, window, testPrice, 0.10, 0.50, defaultCeilingForTest)
	e := only(t, out)

	// t0's 167,263-token write is a re-creation of an expired prefix, not new content, so it must
	// not count toward the span. If it did, t0 would close its own span.
	if e.NewContentBilled >= 0.10*haikuWindow {
		t.Errorf("new content = %d after four turns, which already meets the %.0f span: the axis "+
			"is counting re-sent or re-created tokens as new, and the span will close before any "+
			"cold miss can happen", e.NewContentBilled, 0.10*haikuWindow)
	}
	// The real new content on those three warm turns: the written tails (3,323 + 158 + 275) PLUS
	// the few fresh tokens each turn carries, which are the new user message itself and are new by
	// definition. billed - read - write is 3 on each of them.
	const wantNew = 3_323 + 158 + 275 + 3 + 3 + 3
	if e.NewContentBilled != wantNew {
		t.Errorf("new content = %d, want %d (the warm turns' written tails plus their fresh input)",
			e.NewContentBilled, wantNew)
	}
	if e.State != EpisodeOpen {
		t.Errorf("state = %q, want %q: four turns of a real session add ~3.7k of new content "+
			"against a 20,000 span, so the episode must still be accruing", e.State, EpisodeOpen)
	}
	// And it is still reported, with its net, rather than dropped for being unfinished.
	g := out.ByProvenance[0]
	if g.OpenTurns == 0 || g.OpenNetUSD == 0 {
		t.Errorf("an open episode must report its exposure: turns=%d net=%v",
			g.OpenTurns, g.OpenNetUSD)
	}
	// THE CREDIT ON THESE THREE WARM TURNS IS PRICED AT THE READ RATE, on the real shapes — the
	// case where inheriting saved_usd went wrong on live traffic. The first replay turn's stored
	// saved_usd is $0.14 for a removal this panel prices at ~$0.0105, because saved_usd counted the
	// whole span as first-time content at the cache-CREATION rate. Asserting the real figures here,
	// rather than only on round fixtures, is what keeps that from coming back.
	wantRead := float64(105_250+105_367+105_469) * readRate
	if offUSD(e.ReadCreditUSD, wantRead) {
		t.Errorf("read credit = %v, want %v (three warm turns' removals at the cache-read rate)",
			e.ReadCreditUSD, wantRead)
	}
	if inherited := 0.14 + 0.01 + 0.01; !offUSD(e.ReadCreditUSD, inherited) {
		t.Errorf("read credit = %v, which is the sum of the stored saved_usd values. Those price "+
			"the removal at the cache-creation rate on turns whose cache HIT, which is the defect "+
			"that inverted this panel's sign on a live run", e.ReadCreditUSD)
	}
	// t0 was itself a ttl_expiry, so we caused none of its 167,263-token write and the only debit
	// is the summarizer's call — which this fixture does not carry. So the open net is the credit.
	if offUSD(g.OpenNetUSD, wantRead) {
		t.Errorf("open net = %v, want %v: t0's write was due whatever we did (its entry had already "+
			"lapsed), so nothing offsets the credit here", g.OpenNetUSD, wantRead)
	}
}

// THE SPAN IS AN ATTRIBUTION BOUNDARY, and its far end belongs to the CLIENT rather than to the model.
//
// We fire at the fill threshold. In the world where we did not compact, the conversation keeps growing
// until the CLIENT compacts at its own ceiling; past that point both worlds are running on a
// summarized transcript and nothing further is attributable to us. So the span is
// `ceiling - fill`.
//
// AN EARLIER VERSION COMPUTED `1 - fill`, which silently asserted the client runs the transcript all
// the way to the model's limit. That is an assumption about the client, not arithmetic about the
// model. It is now a parameter, and the case below where the ceiling sits BELOW the trigger is the one
// that assumption made unrepresentable.
func TestTheSpanRunsFromOurTriggerToTheClientsCeiling(t *testing.T) {
	for _, tc := range []struct {
		name          string
		fill, ceiling float64
		want          float64
		wantOK        bool
	}{
		{"the shipped pair", 0.90, 1.00, 0.10, true},
		{"a lower trigger needs a wider span, or it stops crediting while the client keeps going",
			0.50, 1.00, 0.50, true},
		{"a client that compacts early narrows the span", 0.80, 0.90, 0.10, true},
		// THE CASE THE OLD FORMULA COULD NOT EXPRESS. Claude Code's own indicator reads ~167,000 of a
		// 200,000 haiku window, so a deployment whose client really does act there while our trigger
		// sits at 0.9 would have the client compacting FIRST, every time. There is then no window in
		// which anything is attributable to us, and this component cannot help on that deployment.
		{"the client compacts before we would ever fire", 0.90, 0.835, 0, false},
		{"the ceiling exactly at the trigger leaves nothing", 0.90, 0.90, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := spanFor(tc.fill, tc.ceiling)
			if ok != tc.wantOK {
				t.Fatalf("spanFor(%v, %v) ok = %v, want %v", tc.fill, tc.ceiling, ok, tc.wantOK)
			}
			if ok && math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("spanFor(%v, %v) = %v, want %v", tc.fill, tc.ceiling, got, tc.want)
			}
		})
	}
}

// A trigger at or above the client's ceiling is REPORTED as its own condition, not as an empty result.
//
// "No episodes" and "no episodes are POSSIBLE here" are different facts, and only the second one names
// a configuration to change. The previous version substituted the shipped 0.10 span in this case,
// which invented a window the configuration says does not exist and would have published credits for
// turns the client had already compacted away.
func TestATriggerAboveTheClientCeilingIsReportedNotSilentlyRescaled(t *testing.T) {
	rows := []compactRow{
		row(1, 1_000, fresh),
		row(2, testSpan, saved(400_000), miss(CacheTTLExpiry)),
	}
	// Precondition: these rows DO produce an episode when a span exists, so a zero below is about the
	// ceiling and not about the fixture.
	if got := walkCompactEpisodes(rows, exactWindow, testPrice, 0, 0.90, 1.00); len(got.Episodes) != 1 {
		t.Fatalf("precondition: want 1 episode with a valid span, got %d", len(got.Episodes))
	}

	out := walkCompactEpisodes(rows, exactWindow, testPrice, 0, 0.90, 0.835)
	if len(out.Episodes) != 0 {
		t.Errorf("a trigger above the client's ceiling must produce no episodes, got %d",
			len(out.Episodes))
	}
	if out.Coverage.NoAttributableSpan != 1 {
		t.Errorf("no_attributable_span = %d, want 1: the panel must report that this configuration "+
			"cannot help at all, not an empty dataset", out.Coverage.NoAttributableSpan)
	}
	// And the excluded conversation contributes no ceiling entry, because it was never measured
	// against one — an entry there would claim the panel had used a span it refused to derive.
	if len(out.Assumptions.ClientCeilings) != 0 {
		t.Errorf("client_ceilings = %+v, want none: nothing was measured", out.Assumptions.ClientCeilings)
	}
}

// And the derivation reaches the served assumptions end to end, or the page prints a number the
// measurement did not use.
func TestTheDerivedSpanReachesTheServedAssumptions(t *testing.T) {
	out := walkCompactEpisodes([]compactRow{row(1, 1_000, fresh)}, exactWindow, testPrice, 0, 0.50, 1.00)
	if len(out.Assumptions.ClientCeilings) != 1 {
		t.Fatalf("want one ceiling entry, got %+v", out.Assumptions.ClientCeilings)
	}
	c := out.Assumptions.ClientCeilings[0]
	if math.Abs(c.SpanFrac-0.50) > 1e-9 {
		t.Errorf("span_frac = %v for a 0.50 fill against a 1.00 ceiling, want 0.50", c.SpanFrac)
	}
	if math.Abs(c.Frac-1.00) > 1e-9 {
		t.Errorf("ceiling frac = %v, want the 1.00 it was given", c.Frac)
	}
}

// THE CEILING IS RESOLVED PER MODEL, and a query spanning two models must not average them.
//
// It is a fact about the CLIENT's behaviour on a given model: Claude Code compacts a haiku session at
// a measured 0.996 of a 200,000 window, and an unlisted model has no entry at all. One number for the
// whole query would be an average of things that are not comparable, and it would hide which
// published figures rest on a measurement.
func TestTheCeilingIsResolvedPerModelAndCarriesItsProvenance(t *testing.T) {
	haiku := row(1, 1_000, fresh)
	haiku.Model = "claude-haiku-4-5"
	other := row(2, 1_000, fresh)
	other.Model, other.Session = "some-unlisted-model", "s2"

	out := walkCompactEpisodes([]compactRow{haiku, other},
		func(string) (int, bool) { return 200_000, true }, testPrice, 0, 0.90, 0)
	if len(out.Assumptions.ClientCeilings) != 2 {
		t.Fatalf("want one entry per model, got %+v", out.Assumptions.ClientCeilings)
	}
	byModel := map[string]ClientCeilingUsed{}
	for _, c := range out.Assumptions.ClientCeilings {
		byModel[c.Model] = c
	}
	h := byModel["claude-haiku-4-5"]
	if h.Provenance != string(compactionpoint.Measured) {
		t.Errorf("haiku ceiling provenance = %q, want %q — it was observed on a real client run",
			h.Provenance, compactionpoint.Measured)
	}
	if math.Abs(h.Frac-0.996) > 1e-9 {
		t.Errorf("haiku ceiling = %v, want the measured 0.996 in BILLED tokens (not the ~0.835 the "+
			"client's own indicator shows for the same turn)", h.Frac)
	}
	u := byModel["some-unlisted-model"]
	if u.Provenance != string(compactionpoint.WindowFallback) {
		t.Errorf("unlisted model provenance = %q, want %q", u.Provenance, compactionpoint.WindowFallback)
	}
	if u.Frac != defaultCeilingForTest {
		t.Errorf("unlisted model ceiling = %v, want the fallback %v", u.Frac, defaultCeilingForTest)
	}
	// Every entry says WHY, or the provenance is a label with nothing behind it.
	for _, c := range out.Assumptions.ClientCeilings {
		if c.Note == "" {
			t.Errorf("%s carries no note; a provenance label with no reasoning cannot be audited", c.Model)
		}
	}
}

// The per-model table itself, including the routed forms a gateway produces.
func TestClientCeilingTableMatchesRoutedModelIDs(t *testing.T) {
	for _, tc := range []struct {
		id   string
		frac float64
		prov compactionpoint.Source
	}{
		{"claude-haiku-4-5", 0.996, compactionpoint.Measured},
		{"aws/claude-haiku-4-5", 0.996, compactionpoint.Measured},
		{"claude-opus-5", 1.00, compactionpoint.Assumed},
		{"aws/claude-opus-5[1m]", 1.00, compactionpoint.Assumed},
		{"claude-sonnet-5", 1.00, compactionpoint.Assumed},
		{"gpt-5", defaultCeilingForTest, compactionpoint.WindowFallback},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got := compactionpoint.For(tc.id)
			if math.Abs(got.Frac-tc.frac) > 1e-9 || got.Source != tc.prov {
				t.Errorf("compactionpoint.For(%q) = %v/%s, want %v/%s",
					tc.id, got.Frac, got.Source, tc.frac, tc.prov)
			}
		})
	}
}
