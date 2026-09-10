package dash

import (
	"testing"

	"github.com/rossoctl/context-guru/components/offload"
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
		Billed: billed, TokensBefore: billed / 3, MissReason: CacheHit,
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

func saved(usd float64) func(*compactRow) {
	return func(r *compactRow) { r.SavedUSD = usd }
}

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
	return walkCompactEpisodes(rows, w, p, 0.10, 0.90)
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
	base := int64(900_000)

	short := walk([]compactRow{
		row(1, base, fresh),
		row(2, base+testSpan-1, saved(1)),
	}, exactWindow, testPrice)
	if e := only(t, short); e.State != EpisodeOpen {
		t.Errorf("one token short of the span must leave the episode open, got %q", e.State)
	}

	closed := walk([]compactRow{
		row(1, base, fresh),
		row(2, base+testSpan-1, saved(1)),
		row(3, base+testSpan, saved(2)),
	}, exactWindow, testPrice)
	e := only(t, closed)
	if e.State != EpisodeClosed {
		t.Fatalf("reaching the span must close the episode, got %q (gates none; episode %+v)", e.State, e)
	}
	if e.Turns != 3 {
		t.Errorf("closed episode covered %d turns, want 3", e.Turns)
	}
	if e.StartBilled != base || e.EndBilled != base+testSpan {
		t.Errorf("span recorded as %d..%d, want %d..%d", e.StartBilled, e.EndBilled, base, base+testSpan)
	}
}

// A guessed window contributes NOTHING rather than a zero, and says so. The substring table of
// last resort answers 200,000 for every Opus against a real 1,000,000, so a 10% span sized against
// it measures 20,000 tokens of work while claiming 100,000 — a fifth of the span, and every
// per-episode figure five times too small.
func TestAGuessedWindowProducesNoEpisodeAndIsCounted(t *testing.T) {
	rows := []compactRow{
		row(1, 900_000, fresh),
		row(2, 900_000+testSpan, saved(5)),
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
		row(1, 900_000, fresh, tokensBefore(300_000)),
		row(2, 940_000, saved(3), tokensBefore(310_000)),
		// The drop: our own message-token count falls, which only a client-side compaction does.
		row(3, 950_000, saved(3), tokensBefore(40_000)),
		row(4, 900_000+testSpan, saved(3), tokensBefore(50_000)),
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
		row(1, 900_000, fresh),
		row(2, 910_000, saved(7), miss(CacheTTLExpiry)),
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
	base := int64(900_000)
	out := walk([]compactRow{
		row(1, base, fresh),
		row(2, base+10_000, saved(4), miss(CacheTTLExpiry)),
		row(3, base+20_000, saved(1), miss(CacheHit)),
		row(4, base+30_000, saved(2), miss(CacheColdStart)),
		row(5, base+40_000, saved(8), miss(CachePrefixChange)),
		row(6, base+testSpan, saved(3), miss(CacheHit)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if e.State != EpisodeClosed {
		t.Fatalf("precondition: the episode must close, got %q", e.State)
	}
	if offUSD(e.ColdCreditUSD, 4) {
		t.Errorf("cold credit = %v, want 4 (only the ttl_expiry turn)", e.ColdCreditUSD)
	}
	if offUSD(e.ReadCreditUSD, 4) {
		t.Errorf("read credit = %v, want 4 (the two hit turns, 1 + 3)", e.ReadCreditUSD)
	}
	if offUSD(e.OtherCreditUSD, 2) {
		t.Errorf("other credit = %v, want 2 (the cold_start turn)", e.OtherCreditUSD)
	}
	// The prefix_change turn's $8 must be nowhere.
	if total := e.ColdCreditUSD + e.ReadCreditUSD + e.OtherCreditUSD; offUSD(total, 10) {
		t.Errorf("credited %v in total, want 10 — a prefix_change turn's saving must not be "+
			"credited to this component, which caused the change", total)
	}
}

// t0's own cache write is charged even when the row is labelled `hit`, and it has to be: a PARTIAL
// hit reads as `hit` (repeatRate's first guard says why — which side of the boundary the removed
// content sat on is not knowable from the usage block), so a debit derived from the label alone
// would silently be zero on exactly the turns where we rewrote a live prefix.
func TestTheInvalidationDebitComesFromTheWriteNotTheLabel(t *testing.T) {
	base := int64(900_000)
	out := walk([]compactRow{
		// A partial hit: labelled hit, and 40k tokens of cache creation all the same.
		row(1, base, fresh, miss(CacheHit), wrote(40_000), cgCost(0.05)),
		row(2, base+testSpan, saved(6), miss(CacheHit)),
	}, exactWindow, testPrice)

	e := only(t, out)
	want := 40_000 * 1.25e-6
	if offUSD(e.InvalidationDebitUSD, want) {
		t.Errorf("invalidation debit = %v, want %v (40k written at the 5m rate) — a debit taken "+
			"from cache_miss_reason would be 0 here", e.InvalidationDebitUSD, want)
	}
	if offUSD(e.SummarizerCostUSD, 0.05) {
		t.Errorf("summarizer cost = %v, want 0.05", e.SummarizerCostUSD)
	}
	if got, exp := e.NetUSD, 6-want-0.05; offUSD(got, exp) {
		t.Errorf("net = %v, want %v (credits minus both debits)", got, exp)
	}
}

// An unpriced model is not a free one. Every dollar is left absent and the episode is counted
// apart, so no total picks these up as real figures — the lie kvcache.Pricing.Known exists to stop.
func TestAnUnpricedModelLeavesTheMoneyAbsentRatherThanZero(t *testing.T) {
	base := int64(900_000)
	out := walk([]compactRow{
		row(1, base, fresh, wrote(40_000)),
		row(2, base+testSpan, saved(6), miss(CacheTTLExpiry)),
	}, exactWindow, unpricedModel)

	e := only(t, out)
	if e.Priced {
		t.Fatal("episode on a model with no rates must not be marked priced")
	}
	if e.NetUSD != 0 {
		t.Errorf("net = %v, want 0 — an unpriced episode reports no dollars", e.NetUSD)
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
	base := int64(900_000)
	a := row(1, base, fresh)
	b := row(2, base+testSpan, saved(9))
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
	base := int64(900_000)
	rec := []compactRow{row(1, base, fresh), row(2, base+testSpan, saved(4), miss(CacheTTLExpiry))}
	inf := []compactRow{row(3, base, actedWithNoEvents), row(4, base+testSpan, saved(6), miss(CacheTTLExpiry))}
	inf[0].Session, inf[1].Session = "s2", "s2"

	out := walk(append(rec, inf...), exactWindow, testPrice)
	if len(out.ByProvenance) != 2 {
		t.Fatalf("want two provenance groups, got %+v", out.ByProvenance)
	}
	got := map[string]float64{}
	for _, g := range out.ByProvenance {
		got[g.Provenance] = g.ColdCreditUSD
	}
	if offUSD(got[EpisodeRecorded], 4) || offUSD(got[EpisodeInferred], 6) {
		t.Errorf("cold credit by provenance = %v, want recorded 4 and inferred 6 kept apart", got)
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
		row(1, 900_000, replay, saved(3)),
		row(2, 900_000+testSpan, replay, saved(3)),
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
	want := 500_000 * 1.25e-6 // the two ttl_expiry turns' cache creation
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
	base := int64(900_000)
	out := walk([]compactRow{
		row(1, base, fresh, cgCost(0.05), wrote(10_000)),
		row(2, base+50_000, fresh, cgCost(0.07), wrote(20_000), saved(2)),
		row(3, base+testSpan, saved(4), miss(CacheTTLExpiry)),
	}, exactWindow, testPrice)

	e := only(t, out)
	if offUSD(e.SummarizerCostUSD, 0.12) {
		t.Errorf("summarizer cost = %v, want 0.12 (both calls charged to the span they happened in)",
			e.SummarizerCostUSD)
	}
	want := 30_000 * 1.25e-6
	if offUSD(e.InvalidationDebitUSD, want) {
		t.Errorf("invalidation debit = %v, want %v (both rewrites)", e.InvalidationDebitUSD, want)
	}
}

// The turn that closes one span and opens the next is the only row that can belong to two
// episodes, and its SAVING must be counted once. The costs of the new summary belong to the new
// span; the saving was earned in the old one.
func TestTheTurnThatClosesOneSpanAndOpensAnotherIsCreditedOnce(t *testing.T) {
	base := int64(900_000)
	out := walkCompactEpisodes([]compactRow{
		row(1, base, fresh),
		row(2, base+testSpan, fresh, saved(5), miss(CacheTTLExpiry), wrote(8_000), cgCost(0.03)),
	}, exactWindow, testPrice, 0.10, 0.90)

	if len(out.Episodes) != 2 {
		t.Fatalf("want 2 episodes (one closed, one opened by the same turn), got %d: %+v",
			len(out.Episodes), out.Episodes)
	}
	first, second := out.Episodes[0], out.Episodes[1]
	if first.State != EpisodeClosed {
		t.Errorf("first episode state = %q, want closed", first.State)
	}
	if offUSD(first.ColdCreditUSD, 5) {
		t.Errorf("the closing turn's saving belongs to the span it closed: cold credit = %v, want 5",
			first.ColdCreditUSD)
	}
	if second.ColdCreditUSD != 0 || second.ReadCreditUSD != 0 || second.OtherCreditUSD != 0 {
		t.Errorf("the same saving must not also be credited to the new span: %+v", second)
	}
	if offUSD(second.SummarizerCostUSD, 0.03) {
		t.Errorf("the new summary's own call belongs to the new span: got %v, want 0.03",
			second.SummarizerCostUSD)
	}
}

// The assumptions the server states must actually describe what it did. The KV-cache page's rule:
// the arithmetic is served, not restated in a template nothing tests.
func TestTheServerStatesTheFractionsItActuallyUsed(t *testing.T) {
	out := walkCompactEpisodes(nil, exactWindow, testPrice, 0.25, 0.5)
	if out.Assumptions.SpanFrac != 0.25 || out.Assumptions.FillFrac != 0.5 {
		t.Errorf("assumptions = %+v, want the 0.25/0.5 actually used", out.Assumptions)
	}
	if out.Coverage.FillFrac != 0.5 {
		t.Errorf("coverage fill_frac = %v, want 0.5", out.Coverage.FillFrac)
	}
	// A zero falls back to the shipped default rather than producing a degenerate span.
	def := walkCompactEpisodes(nil, exactWindow, testPrice, 0, 0)
	if def.Assumptions.SpanFrac != defaultSpanFrac || def.Assumptions.FillFrac != defaultFillFrac {
		t.Errorf("assumptions with zero fractions = %+v, want the shipped defaults", def.Assumptions)
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

	out := walk(rows, exactWindow, testPrice)
	e := only(t, out)
	if e.State != EpisodeClosed {
		t.Fatalf("state = %q, want closed", e.State)
	}
	if e.Provenance != EpisodeRecorded {
		t.Errorf("provenance = %q, want %q — the stored event must survive the round trip",
			e.Provenance, EpisodeRecorded)
	}
	if offUSD(e.ColdCreditUSD, 4.0) {
		t.Errorf("cold credit = %v, want 4.0 (2.5 + 1.5, both turns ttl_expiry)", e.ColdCreditUSD)
	}
	if offUSD(e.SummarizerCostUSD, 0.05) {
		t.Errorf("summarizer cost = %v, want 0.05", e.SummarizerCostUSD)
	}
}
