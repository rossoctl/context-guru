package offload

import (
	"math"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// The gate must suppress when the request is cache-aware and the output is small — the
// exact case that made extract_llm ~8x underwater on Terminal-Bench. A 400-token output
// on a caching backend is worth ~400 x $0.30/MTok x (1+reuses); against a ~$0.012 call
// that cannot pay, and the reason must say so.
func TestGateSuppressesSmallOutputWhenCacheAware(t *testing.T) {
	val := savedTokenValue(&components.Ctx{CacheAware: true})
	if val.cached != true {
		t.Fatal("cache-aware ctx must price saved tokens at the cache-read rate")
	}
	d := evaluateGate(400, defaultCompressionRatio, val, callCost(cheapmodel.HaikuPricing(), 400), false, 5, false, true)
	if d.allow {
		t.Fatalf("small cached output must be suppressed: saving=$%.5f cost=$%.5f", d.expSaving, d.expCost)
	}
	if d.reason == "" {
		t.Fatal("every suppression must carry a reason (the operator's first question)")
	}
	if d.expSaving >= d.expCost {
		t.Fatalf("suppression must be justified by the numbers: saving=%v cost=%v", d.expSaving, d.expCost)
	}
}

// On a NON-caching backend the same reduction is billed at the full fresh-input rate — 10x
// more valuable — so the same output that loses under caching can win here. This is the
// asymmetry the gate exists to exploit, so assert it on ONE fixture, not two.
func TestGatePermitsOnNonCachingBackend(t *testing.T) {
	size := 60000 // above both break-evens (~1.8k non-caching, ~42.6k cached)
	cached := evaluateGate(size, defaultCompressionRatio,
		savedTokenValue(&components.Ctx{CacheAware: true}), callCost(cheapmodel.HaikuPricing(), size), false, 5, false, true)
	fresh := evaluateGate(size, defaultCompressionRatio,
		savedTokenValue(&components.Ctx{CacheAware: false}), callCost(cheapmodel.HaikuPricing(), size), false, 5, false, true)

	if !fresh.allow {
		t.Fatalf("non-caching backend must permit a %d-token output: saving=$%.5f cost=$%.5f",
			size, fresh.expSaving, fresh.expCost)
	}
	// The rate difference must show up in the valuation, not just the verdict — but it is
	// ~3x, not 10x, and the difference is the point of the first-turn split.
	//
	// 10x is the ratio between a cache-READ and fresh input, so it is the right factor for a
	// REPLAY turn. It is wrong for the turn the cut is applied on: that content is new, so it
	// is billed as a cache-write ($3.75/MTok) which is actually DEARER than fresh input
	// ($3.00). Blended over reuses=4 the totals are 15.00 vs 4.95 per MTok => 3.03x. An
	// assertion of 10x here was pinning the old conflation of the two rates.
	if ratio := fresh.expSaving / cached.expSaving; math.Abs(ratio-3.0303) > 0.01 {
		t.Fatalf("fresh/cached total saving must be ~3.03x (applied turn is a cache-write, "+
			"only replays take the 10x haircut), got %.4fx", ratio)
	}
}

// High-reuse (recurring) content is permitted even under caching, because the saving is
// collected on every turn the frozen compaction is replayed. Recurrence was measured at
// 82/103 across sessions, so this is the common case, not an edge case.
func TestGatePermitsHighReuseContent(t *testing.T) {
	val := savedTokenValue(&components.Ctx{CacheAware: true})
	// 12000 tokens: above the cached-RECURRING break-even (~11,550) and below the
	// cached-once one (~12,900). This size is the gate's whole thesis in one fixture —
	// recurrence is what tips an otherwise-losing call into profit, so the SAME size goes
	// both ways.
	//
	// It used to be 34000, against break-evens of ~30.5k and ~42.6k. Correcting the applied
	// turn from the cache-read rate to the cache-write rate moved both down ~2.6-3.3x AND
	// narrowed the gap between them: recurrence now multiplies the saving by 1.12x rather
	// than 1.40x, because the applied turn dominates the sum. So the window this fixture has
	// to sit in is ~11,550-12,900 — genuinely tight, and the reason the bound is asserted
	// here rather than left implicit.
	size := 12000
	once := evaluateGate(size, defaultCompressionRatio, val, callCost(cheapmodel.HaikuPricing(), size), false, 5, false, true)
	recur := evaluateGate(size, defaultCompressionRatio, val, callCost(cheapmodel.HaikuPricing(), size), true, 5, false, true)

	if recur.expSaving <= once.expSaving {
		t.Fatalf("recurring content must be valued higher: recur=%v once=%v", recur.expSaving, once.expSaving)
	}
	if !recur.allow {
		t.Fatalf("recurring %d-token content should be permitted: saving=$%.5f cost=$%.5f",
			size, recur.expSaving, recur.expCost)
	}
	if once.allow {
		t.Fatalf("at %d tokens, NON-recurring cached content should still be suppressed "+
			"(saving=$%.5f cost=$%.5f) — recurrence is what must tip the decision",
			size, once.expSaving, once.expCost)
	}
}

// The break-even output size is the headline economic fact of issue #28, so pin it: under
// caching an extraction call must remove ~10x more tokens than on a non-caching backend.
// If these numbers drift, the component's viability has changed and the docs' verdict
// needs revisiting — which is exactly what this test exists to catch.
func TestBreakEvenSizesMatchTheDocumentedVerdict(t *testing.T) {
	breakEven := func(cacheAware, recurring bool) int {
		val := savedTokenValue(&components.Ctx{CacheAware: cacheAware})
		for size := 200; size <= 400_000; size += 100 {
			if evaluateGate(size, defaultCompressionRatio, val, callCost(cheapmodel.HaikuPricing(), size), recurring, 5, false, true).allow {
				return size
			}
		}
		return -1
	}
	cachedRecur := breakEven(true, true)
	freshRecur := breakEven(false, true)
	if cachedRecur < 10_000 || cachedRecur > 13_500 {
		t.Errorf("cached+recurring break-even = %d tokens, expected ~11,600 "+
			"(docs/components/extract_llm.md states this figure)", cachedRecur)
	}
	if freshRecur < 1_400 || freshRecur > 2_400 {
		t.Errorf("fresh+recurring break-even = %d tokens, expected ~1,800", freshRecur)
	}
	// The gap used to be ~20x — WIDER than the bare 10x rate haircut, because cost stops
	// growing once the prompt hits the shown-content cap while value keeps scaling. Pricing
	// the APPLIED turn as a cache-write rather than a cache-read cut it to ~6.4x: the applied
	// turn is billed at nearly the fresh rate in both regimes, so only the replay turns still
	// take the haircut.
	if ratio := float64(cachedRecur) / float64(freshRecur); ratio < 5 || ratio > 8 {
		t.Errorf("cached/fresh break-even ratio = %.1fx, expected ~6.4x", ratio)
	}
}

// The cost model must be real arithmetic over model pricing, not a hard-coded constant —
// "~$0.012/call" was one workload's average, and the gate has to track the actual model.
func TestCostModelMatchesKnownTokensTimesKnownPrice(t *testing.T) {
	p := cheapmodel.Pricing{InputPerMTok: 1.00, OutputPerMTok: 5.00,
		CacheWritePerMTok: 1.25, CacheReadPerMTok: 0.10}
	// 1M input @ $1 + 1M output @ $5 + 1M write @ $1.25 + 1M read @ $0.10 = $7.35
	if got := p.Cost(1_000_000, 1_000_000, 1_000_000, 1_000_000); math.Abs(got-7.35) > 1e-9 {
		t.Fatalf("Cost = %v, want 7.35", got)
	}
	// A realistic single extraction call: 3000 fresh in, 200 out on haiku rates.
	// 3000/1e6*1 + 200/1e6*5 = 0.003 + 0.001 = 0.004
	if got := p.Cost(3000, 200, 0, 0); math.Abs(got-0.004) > 1e-9 {
		t.Fatalf("Cost = %v, want 0.004", got)
	}
	// Env override must be honored so an operator prices their own deployment.
	t.Setenv("CHEAP_MODEL_PRICE_IN", "2.50")
	if got := cheapmodel.PricingFromEnv().InputPerMTok; got != 2.50 {
		t.Fatalf("PricingFromEnv InputPerMTok = %v, want 2.50", got)
	}
}

// callCost must fall back to the prior only until a real observation exists; it must never
// return zero, which would make the gate permit everything.
func TestCallCostNeverZero(t *testing.T) {
	if c := callCost(cheapmodel.HaikuPricing(), 3000); c <= 0 {
		t.Fatalf("callCost must be positive, got %v", c)
	}
}

// The trigger must NOT fire on every step of a merely-growing context — firing every step
// is what produced 271 calls. Pressure gates it, and a near-static context is declined.
func TestTriggerDoesNotFireEveryStepOnGrowingContext(t *testing.T) {
	window := 1_000_000
	fired := 0
	prev := 0
	// Simulate 40 turns growing by 5k tokens each: reaches only ~20% of a 1M window.
	for turn := 1; turn <= 40; turn++ {
		cur := turn * 5000
		p := contextPressure(cur, window)
		g := growthRate(cur, prev)
		if ok, _ := shouldFire(p, g, false); ok {
			fired++
		}
		prev = cur
	}
	if fired == 40 {
		t.Fatal("trigger fired on EVERY step of a growing context — the #28 waste case")
	}
	if fired > 12 {
		t.Fatalf("trigger fired on %d/40 low-pressure steps; expected few", fired)
	}
	// It must still fire when pressure is genuinely high — a gate that never fires is
	// just as broken as one that always does.
	if ok, reason := shouldFire(0.75, 0.05, false); !ok {
		t.Fatalf("high pressure must fire, got reason %q", reason)
	}
}

// An explicitly-configured min_tokens keeps governing: existing configs must not change
// behavior silently under them.
func TestExplicitMinTokensStillGoverns(t *testing.T) {
	ok, reason := shouldFire(0.01, 0, true) // pressure so low the derived trigger declines
	if !ok {
		t.Fatal("an explicit min_tokens/trigger must still fire (backward compatibility)")
	}
	if reason == "" {
		t.Fatal("reason must be recorded even on the explicit path")
	}
}

// The derived per-output floor must fall as the window fills — no per-workload tuning.
func TestPressureFloorFallsAsContextFills(t *testing.T) {
	window := 1_000_000
	low := pressureFloor(window, 0.10)
	mid := pressureFloor(window, 0.40)
	high := pressureFloor(window, 0.70)
	full := pressureFloor(window, 0.90)
	if !(low > mid && mid > high && high > full) {
		t.Fatalf("floor must decrease monotonically with pressure: %d %d %d %d", low, mid, high, full)
	}
	if full <= 0 {
		t.Fatal("a nearly-full window must still have a positive floor")
	}
	// An unknown window must yield 0 so the caller keeps its absolute default (fail open).
	if pressureFloor(0, 0.9) != 0 {
		t.Fatal("unknown window must return 0 (fall back to absolute default)")
	}
}

// The observed compression ratio must displace the default only once there is enough
// evidence, and a run of misses must drive it toward zero (shutting the gate).
func TestRatioTrackerLearnsFromObservations(t *testing.T) {
	var r ratioTracker
	if r.ratio() != defaultCompressionRatio {
		t.Fatal("with no observations the conservative default must apply")
	}
	r.observe(100, 1000) // below the sample threshold
	if r.ratio() != defaultCompressionRatio {
		t.Fatal("a tiny sample must not displace the default")
	}
	r.observe(0, 20000) // plenty of evidence that this workload does not compress
	if got := r.ratio(); got >= 0.10 {
		t.Fatalf("repeated misses must drive the ratio down, got %v", got)
	}
}

// REGRESSION (found on a live capture): a FLAT per-call cost estimate deadlocks the gate.
// The gate priced every call at the ~$0.012 Terminal-Bench average — roughly 5x the true
// cost on a workload with small outputs — so it suppressed everything; and because it
// suppressed everything, no call was ever observed and the estimate could never correct
// itself. Measured: observed $0.0024/call vs the $0.012 prior, and the gate declined calls
// that were in fact ~2.2x profitable.
//
// The fix is that cost must be ANALYTIC and SIZE-AWARE, so the estimate is right on the
// very first call with no observations at all.
func TestCallCostIsSizeAwareNotFlat(t *testing.T) {
	p := cheapmodel.HaikuPricing()
	small := callCost(p, 400)
	mid := callCost(p, 2000)
	big := callCost(p, 5000)

	if !(small < mid && mid < big) {
		t.Fatalf("cost must scale with candidate size, got %v %v %v", small, mid, big)
	}
	// A small candidate must cost far less than the old flat prior, or the gate
	// re-deadlocks on exactly the workload that exposed the bug.
	if small >= priorCallCost {
		t.Fatalf("a 400-token candidate must cost well under the flat prior $%.4f, got $%.5f",
			priorCallCost, small)
	}
	// The preamble dominates a small call, so it must be included — a cost model that
	// forgot it would under-price and let the gate permit everything.
	if small <= p.Cost(preambleTokens, 0, 0, 0)*0.5 {
		t.Fatalf("cost must include the ~%d-token preamble, got $%.5f", preambleTokens, small)
	}
	// Beyond the shown-content cap the prompt is truncated, so cost must stop growing —
	// otherwise a huge output is priced as if the whole thing were sent.
	if callCost(p, 50_000) != callCost(p, 500_000) {
		t.Fatal("cost must plateau past the shown-content cap (the prompt is truncated)")
	}
}

// The gate must be decidable on the FIRST call, with no observations — that is what the
// size-aware cost model buys. A 2,000-token output (the largest actually present in the
// measured capture) does not pay even on a non-caching backend at the measured 0.12 ratio,
// and the gate must say so from cold rather than needing a warm-up; a clearly larger output
// must be permitted from cold too. Both directions, no observations, first call.
func TestGateIsDecidableFromColdOnFirstCall(t *testing.T) {
	val := savedTokenValue(&components.Ctx{CacheAware: false})

	small := 2000
	d := evaluateGate(small, defaultCompressionRatio, val,
		callCost(cheapmodel.HaikuPricing(), small), false, 5, false, true)
	if d.allow {
		t.Errorf("a %d-token output should not pay at the measured 0.12 ratio: "+
			"saving=$%.5f cost=$%.5f", small, d.expSaving, d.expCost)
	}

	big := 20000 // comfortably above the ~1.8k-3.4k non-caching break-even
	d = evaluateGate(big, defaultCompressionRatio, val,
		callCost(cheapmodel.HaikuPricing(), big), false, 5, false, true)
	if !d.allow {
		t.Errorf("a %d-token output on a non-caching backend must pay on the first call: "+
			"saving=$%.5f cost=$%.5f", big, d.expSaving, d.expCost)
	}
	// And it must be permitted for the right reason, not by accident.
	if d.reason == "" {
		t.Error("an allowed call must carry a reason")
	}
}

// REGRESSION (found on a live capture): a pessimistic ratio prior can justify itself
// forever. The ratio starts at 0.12; on a workload whose outputs sit below the resulting
// break-even the gate suppresses every call, so the tracker never observes anything and the
// prior becomes permanent. Measured: the gate forwent a genuine +$0.0094 net this way.
//
// A BOUNDED exploration budget breaks the loop. This is the same failure shape as the
// flat-cost deadlock (see TestCallCostIsSizeAwareNotFlat) — a gate that cannot revise its
// own prior is an off switch, not a gate.
func TestGateExploresThenSettles(t *testing.T) {
	var r ratioTracker
	explored := 0
	for i := 0; i < 20; i++ {
		if r.exploring("sessA") {
			explored++
		}
	}
	if explored != maxExploreCalls {
		t.Fatalf("exploration must be bounded to %d calls per session, got %d", maxExploreCalls, explored)
	}
	// PER SESSION, not per process: a second session gets its own budget. A process-wide
	// counter spent everything on the first session, leaving every later one with an
	// unrevisable prior — the off-switch failure exploration exists to prevent, at process
	// scope.
	if !r.exploring("sessB") {
		t.Fatal("a different session must get its own exploration budget")
	}

	// An exploration slot must actually flip an otherwise-suppressed decision.
	val := savedTokenValue(&components.Ctx{CacheAware: true})
	size := 400 // far below break-even: normally suppressed
	cost := callCost(cheapmodel.HaikuPricing(), size)
	suppressed := evaluateGate(size, defaultCompressionRatio, val, cost, false, 5, false, true)
	if suppressed.allow {
		t.Fatal("without an exploration slot a tiny cached output must be suppressed")
	}
	exploring := evaluateGate(size, defaultCompressionRatio, val, cost, false, 5, true, true)
	if !exploring.allow {
		t.Fatal("an exploration slot must permit the call so the ratio can be learned")
	}
	if exploring.reason == "" || exploring.reason == suppressed.reason {
		t.Fatalf("exploration must be distinguishable in the reason, got %q", exploring.reason)
	}

	// Once enough evidence exists, exploration stops even if slots remain unused.
	var r2 ratioTracker
	r2.observe(200, minRatioSampleTokens+1)
	if r2.exploring("anySession") {
		t.Fatal("with sufficient evidence the gate must stop exploring")
	}
}

// REGRESSION (H2): the learned ratio must be shrunk toward the prior and capped.
// minRatioSampleTokens is about one medium output, so a raw mean can be n=1 — and an
// unbounded n=1 estimate would drop the cached break-even from ~30,500 tokens to ~7,000
// permanently, with the gate then spending on that basis for the rest of the process.
func TestLearnedRatioIsShrunkAndCapped(t *testing.T) {
	// One highly-compressible observation, just past the sample threshold.
	var r ratioTracker
	r.observe(1800, 2000) // raw ratio 0.90
	got := r.ratio()
	if got >= 0.90 {
		t.Errorf("a single observation must not be taken at face value, got %v", got)
	}
	if got <= defaultCompressionRatio {
		t.Errorf("real evidence must still move the estimate up from %v, got %v",
			defaultCompressionRatio, got)
	}

	// Overwhelming consistent evidence may dominate the prior, but never exceed the cap.
	var r2 ratioTracker
	for i := 0; i < 200; i++ {
		r2.observe(1900, 2000) // raw ratio 0.95, far above any plausible acceptance
	}
	if capped := r2.ratio(); capped > maxLearnedRatio {
		t.Errorf("learned ratio must be capped at %v, got %v", maxLearnedRatio, capped)
	}

	// Consistent misses must still drive it down toward zero (the gate-shutting direction).
	var r3 ratioTracker
	for i := 0; i < 200; i++ {
		r3.observe(0, 2000)
	}
	if low := r3.ratio(); low >= defaultCompressionRatio {
		t.Errorf("repeated misses must lower the estimate below %v, got %v",
			defaultCompressionRatio, low)
	}
}

// SHIPPING DECISION (in code, not prose): the component is disabled by default on caching
// backends, because every caching workload measured came out net-negative even with a
// correctly-working gate. A default guarded only by a doc note is not a default.
func TestSuppressedByDefaultOnCachingBackend(t *testing.T) {
	val := savedTokenValue(&components.Ctx{CacheAware: true})
	// A candidate far ABOVE the cached break-even — economics alone would permit it.
	size := 200_000
	cost := callCost(cheapmodel.HaikuPricing(), size)

	blocked := evaluateGate(size, defaultCompressionRatio, val, cost, true, 5, false, false)
	if blocked.allow {
		t.Fatal("caching backend must be declined by default even when the economics pass")
	}
	if blocked.reason == "" {
		t.Fatal("the default decline must explain itself")
	}

	// Explicitly allowed: the gate's economics then apply as normal and this passes.
	forced := evaluateGate(size, defaultCompressionRatio, val, cost, true, 5, false, true)
	if !forced.allow {
		t.Fatalf("allow_on_caching_backend must hand control back to the economics: "+
			"saving=$%.5f cost=$%.5f", forced.expSaving, forced.expCost)
	}

	// The default must NOT block a non-caching backend — that is where the component wins.
	fresh := savedTokenValue(&components.Ctx{CacheAware: false})
	ok := evaluateGate(20000, defaultCompressionRatio, fresh,
		callCost(cheapmodel.HaikuPricing(), 20000), false, 5, false, false)
	if !ok.allow {
		t.Fatalf("non-caching traffic must still be permitted: saving=$%.5f cost=$%.5f",
			ok.expSaving, ok.expCost)
	}
}

// The latency brake (PR #37): exploration spends wall clock as well as money, and an agent
// on a task deadline feels the former more. Once calls are observed slow, stop speculating.
func TestTooSlowToExplore(t *testing.T) {
	if tooSlowToExplore(0, 0) {
		t.Error("no observations must not read as slow")
	}
	if tooSlowToExplore(500, 3) {
		t.Error("fast calls must allow exploration")
	}
	if !tooSlowToExplore(slowCallMs, 1) {
		t.Error("at the threshold exploration must stop")
	}
	// The #37 shape: 17.8s across 2 calls.
	if !tooSlowToExplore(17800.0/2, 2) {
		t.Error("PR #37's measured latency must stop exploration")
	}
}

// --- First-turn vs replay pricing (the tail is not a cache-read) ---------------

// The turn a compaction is APPLIED on is not a cache-read turn. When cache-aware,
// extract_llm can only act on the TAIL, and tail content has never been cached — on that
// turn it is billed as a cache-write (or fresh input), 10-12.5x the cache-read rate. The
// old pricing charged every turn at cache-read and so undervalued the component ~3.3x.
//
// This pins the arithmetic rather than the conclusion: the ratio between the two formulas
// is what the shipping decision (disabled on caching backends, justified by a ~30,500
// tok/output break-even) rests on.
func TestCachedSavingPricesTheAppliedTurnAsAWrite(t *testing.T) {
	val := savedTokenValue(&components.Ctx{CacheAware: true})
	if val.firstToken <= val.perToken {
		t.Fatalf("applied turn must be worth MORE than a replay turn on a caching backend: "+
			"first=%v replay=%v", val.firstToken, val.perToken)
	}
	if got, want := val.firstToken*1e6, agentCacheWritePerMTok; got != want {
		t.Errorf("applied turn priced at $%.2f/MTok, want the cache-write rate $%.2f", got, want)
	}
	if got, want := val.perToken*1e6, agentCacheReadPerMTok; got != want {
		t.Errorf("replay turn priced at $%.2f/MTok, want the cache-read rate $%.2f", got, want)
	}

	// The correction's size, at the default first-sight reuse prior (4).
	const reuses = 4.0
	old := (1 + reuses) * agentCacheReadPerMTok
	corrected := agentCacheWritePerMTok + reuses*agentCacheReadPerMTok
	if ratio := corrected / old; ratio < 3.0 || ratio > 3.6 {
		t.Errorf("correction is %.2fx; expected ~3.3x — if the rates changed, re-derive the "+
			"break-even quoted in evaluateGate and savedTokenValue before editing this bound", ratio)
	}
}

// The non-caching path must be BYTE-identical to the old single-rate formula. The fix is
// meant to change the cached case only, and this is the guard that keeps a future edit from
// silently repricing every non-caching workload the published numbers came from.
func TestNonCachingPricingIsUnchangedByTheFirstTurnSplit(t *testing.T) {
	val := savedTokenValue(&components.Ctx{CacheAware: false})
	if val.firstToken != val.perToken {
		t.Fatalf("non-caching: both rates must be fresh input; first=%v replay=%v",
			val.firstToken, val.perToken)
	}
	for _, reuses := range []float64{3, 4, 6} {
		oldWay := (1 + reuses) * val.perToken
		newWay := val.firstToken + reuses*val.perToken
		if oldWay != newWay {
			t.Errorf("reuses=%v: non-caching saving changed (%v -> %v)", reuses, oldWay, newWay)
		}
	}
}

// A cached output that the OLD pricing suppressed and the CORRECTED pricing permits: the
// whole point of the fix. Sized between the two break-evens (~9.2k corrected, ~30.5k old).
func TestCorrectedPricingPermitsMidSizedCachedOutput(t *testing.T) {
	const size = 20000 // above the corrected break-even, below the old one
	cost := callCost(cheapmodel.HaikuPricing(), size)
	val := savedTokenValue(&components.Ctx{CacheAware: true})

	removed := float64(size) * defaultCompressionRatio
	oldSaving := removed * (1 + 4) * val.perToken            // the pre-fix formula
	newSaving := removed * (val.firstToken + 4*val.perToken) // what the gate computes now

	if !(oldSaving < cost) {
		t.Skipf("fixture no longer straddles the break-even (old saving $%.5f vs cost $%.5f); "+
			"resize it rather than deleting this test", oldSaving, cost)
	}
	if !(newSaving > cost) {
		t.Fatalf("corrected pricing still cannot pay for a %d-token cached output: "+
			"saving=$%.5f cost=$%.5f", size, newSaving, cost)
	}
}
