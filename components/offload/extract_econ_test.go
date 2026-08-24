package offload

import (
	"context"
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/store"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	d := evaluateGate(400, defaultCompressionRatio, val, callCost(cheapmodel.HaikuPricing(), 400, 0), false, 5, false, true)
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
		savedTokenValue(&components.Ctx{CacheAware: true}), callCost(cheapmodel.HaikuPricing(), size, 0), false, 5, false, true)
	fresh := evaluateGate(size, defaultCompressionRatio,
		savedTokenValue(&components.Ctx{CacheAware: false}), callCost(cheapmodel.HaikuPricing(), size, 0), false, 5, false, true)

	if !fresh.allow {
		t.Fatalf("non-caching backend must permit a %d-token output: saving=$%.5f cost=$%.5f",
			size, fresh.expSaving, fresh.expCost)
	}
	// The 10x rate difference must show up in the valuation, not just the verdict.
	if ratio := fresh.expSaving / cached.expSaving; math.Abs(ratio-10) > 0.01 {
		t.Fatalf("fresh tokens must be worth 10x cached ones, got %.3fx", ratio)
	}
}

// High-reuse (recurring) content is permitted even under caching, because the saving is
// collected on every turn the frozen compaction is replayed. Recurrence was measured at
// 82/103 across sessions, so this is the common case, not an edge case.
func TestGatePermitsHighReuseContent(t *testing.T) {
	val := savedTokenValue(&components.Ctx{CacheAware: true})
	// 44,000 tokens: above the ~40.3k cached-RECURRING break-even, below the cached-once
	// one. This size is the gate's whole thesis in one fixture — recurrence is what tips an
	// otherwise-losing call into profit, so the SAME size goes both ways.
	//
	// It was 34,000 against a ~30.5k break-even until the prompt was measured: the analytic
	// cost was built on preambleTokens = 1463 and no tokenizer markup, i.e. 3,663 o200k
	// tokens priced as if they were the provider's, against a real 5,600. Correcting both
	// (1,893 o200k x 1.29) moved the break-even up 32%. Nothing about the component got
	// worse — the number was simply 35% optimistic, in the numerator of every decision.
	size := 44000
	once := evaluateGate(size, defaultCompressionRatio, val, callCost(cheapmodel.HaikuPricing(), size, 0), false, 5, false, true)
	recur := evaluateGate(size, defaultCompressionRatio, val, callCost(cheapmodel.HaikuPricing(), size, 0), true, 5, false, true)

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
			if evaluateGate(size, defaultCompressionRatio, val, callCost(cheapmodel.HaikuPricing(), size, 0), recurring, 5, false, true).allow {
				return size
			}
		}
		return -1
	}
	cachedRecur := breakEven(true, true)
	freshRecur := breakEven(false, true)
	// ~40,300 and ~3,100 on the MEASURED prompt (1,893 o200k preamble x 1.29 real-token
	// markup). The previous ~30,500 / ~1,800 were the same arithmetic over a preamble
	// constant that was 29% low and no markup at all, so they under-stated call cost by ~35%
	// and let the gate allow calls that could not pay. Re-derive from the profile's prompt
	// budget, not from these literals, if the prompt text changes.
	if cachedRecur < 35_000 || cachedRecur > 46_000 {
		t.Errorf("cached+recurring break-even = %d tokens, expected ~40,300 "+
			"(docs/components/extract_llm.md states this figure)", cachedRecur)
	}
	if freshRecur < 2_500 || freshRecur > 3_800 {
		t.Errorf("fresh+recurring break-even = %d tokens, expected ~3,100", freshRecur)
	}
	// These figures survived a round trip worth recording, because the arithmetic looked
	// convincing in both directions. They were briefly moved to ~112,800 / ~11,300 on a reuse
	// prior of 1.5 derived from saved_gross/saved_unique over only the requests that MADE CALLS —
	// which is not an amortization figure at all (saved_unique is identical over those rows and
	// over all rows, so restricting the numerator subtracts every replay by construction). The
	// per-session measurement is 4.0-215.0, median 12.0, so 6/3/4 is conservative and inside the
	// range. See expectedReuses.
	//
	// On a WARM turn the removal and its replays are both cache reads, so pricing them separately
	// (tokenValue.repeatPerToken, added in the same change) leaves these two numbers untouched —
	// which is why they came back to exactly where they were. The rate split only moves the COLD
	// break-even, where a write is 12.5x a read.
	//
	// The gap is WIDER than the bare 10x rate haircut, because cost stops growing once the
	// prompt hits the shown-content cap while value keeps scaling with output size.
	if ratio := float64(cachedRecur) / float64(freshRecur); ratio < 12 || ratio > 30 {
		t.Errorf("cached/fresh break-even ratio = %.1fx, expected ~20x", ratio)
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
	if c := callCost(cheapmodel.HaikuPricing(), 3000, 0); c <= 0 {
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
		if ok, _ := shouldFire(p, g, false, false); ok {
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
	if ok, reason := shouldFire(0.75, 0.05, false, false); !ok {
		t.Fatalf("high pressure must fire, got reason %q", reason)
	}
}

// An explicitly-configured min_tokens keeps governing: existing configs must not change
// behavior silently under them.
func TestExplicitMinTokensStillGoverns(t *testing.T) {
	ok, reason := shouldFire(0.01, 0, true, false) // pressure so low the derived trigger declines
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
	small := callCost(p, 400, 0)
	mid := callCost(p, 2000, 0)
	big := callCost(p, 5000, 0)

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
	if callCost(p, 50_000, 0) != callCost(p, 500_000, 0) {
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
		callCost(cheapmodel.HaikuPricing(), small, 0), false, 5, false, true)
	if d.allow {
		t.Errorf("a %d-token output should not pay at the measured 0.12 ratio: "+
			"saving=$%.5f cost=$%.5f", small, d.expSaving, d.expCost)
	}

	big := 20000 // comfortably above the ~1.8k-3.4k non-caching break-even
	d = evaluateGate(big, defaultCompressionRatio, val,
		callCost(cheapmodel.HaikuPricing(), big, 0), false, 5, false, true)
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
	cost := callCost(cheapmodel.HaikuPricing(), size, 0)
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
	cost := callCost(cheapmodel.HaikuPricing(), size, 0)

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
		callCost(cheapmodel.HaikuPricing(), 20000, 0), false, 5, false, false)
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

// A named compaction model is priced at ITS rates, not the agent's.
//
// The rate card is overridden with the agent's own rates when model.source is `incoming`,
// which was right when that meant "the agent's model does the compacting". `model.model` now
// re-points an incoming-source client at a cheap model on the same endpoint, and the override
// then prices haiku calls as opus: on the deployment where this was found, 4.75x, which is
// what made the Components tab disagree with the request row and turned a paying
// configuration into a losing one on screen.
func TestANamedCompactionModelIsNotPricedAsTheAgent(t *testing.T) {
	// The agent is expensive; the component's own default card is haiku-class.
	agentRates := components.TokenRates{Input: 3.8 / 1e6, Output: 19.0 / 1e6,
		CacheRead: 0.38 / 1e6, CacheWrite: 4.75 / 1e6}

	for _, tc := range []struct {
		name      string
		modelName string
		wantAgent bool
	}{
		{"no model named: the agent compacts, so the agent's rates", "", true},
		{"a model named: its own rates", "claude-haiku-4-5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ExtractLLM{modelSource: "incoming", modelName: tc.modelName,
				pricing: cheapmodel.HaikuPricing()}
			got, _ := e.pricingFor(components.Ctx{SelfRates: agentRates})
			isAgent := got.InputPerMTok == agentRates.Input*1e6
			if isAgent != tc.wantAgent {
				t.Errorf("input rate $%.2f/MTok: agent-priced=%v, want %v (agent is $%.2f, "+
					"the component's own card is $%.2f)", got.InputPerMTok, isAgent,
					tc.wantAgent, agentRates.Input*1e6, cheapmodel.HaikuPricing().InputPerMTok)
			}
		})
	}
}

// TestTheGateSeesTheWholePromptNotJustTheCandidate pins the defect that let the cold sweep
// spend money: callCost used a flat 200-token overhead, but under `context: full` the
// rendered transcript is the prompt. Measured on production, five haiku calls on one request
// each sent ~138,000 prompt tokens while the gate priced them at <=6,663 — 21x to 31x low.
func TestTheGateSeesTheWholePromptNotJustTheCandidate(t *testing.T) {
	p := cheapmodel.HaikuPricing()
	const candidate = 3433 // a real candidate size from the production rows
	blind := callCost(p, candidate, 0)
	// The real overhead on those calls: ~138k prompt tokens, almost all of it transcript.
	seeing := callCost(p, candidate, 138_000)
	if !(seeing > blind*10) {
		t.Fatalf("a 138k-token prompt must cost far more than a blind estimate: blind=%.6f seeing=%.6f", blind, seeing)
	}
	// And the gate must actually refuse on that cost. A 3433-token candidate at the
	// cache-read rate cannot repay a six-figure prompt.
	val := savedTokenValue(&components.Ctx{CacheAware: true})
	if d := evaluateGate(candidate, defaultCompressionRatio, val, seeing, false, 5, false, true); d.allow {
		t.Fatalf("gate allowed a call whose prompt costs $%.4f: %s", seeing, d.reason)
	}
	// Zero means "unknown", and must fall back to the old constant rather than to no cost.
	if callCost(p, candidate, 0) != callCost(p, candidate, promptOverheadTokens) {
		t.Fatal("overhead 0 must fall back to promptOverheadTokens")
	}
}

// TestSavedTokenValueAtPricesTheTailAtTheWriteRate pins the correction this change is: the
// same request, the same rates, two candidates, and position is the only difference.
func TestSavedTokenValueAtPricesTheTailAtTheWriteRate(t *testing.T) {
	// A real rate card, so the assertion is about position and not about the fallback table.
	c := &components.Ctx{CacheAware: true, MaxCachedIdx: 3, SelfRates: components.TokenRates{
		Input: 3.00 / 1e6, CacheRead: 0.30 / 1e6, CacheWrite: 3.75 / 1e6,
	}}
	depth := savedTokenValueAt(c, 2) // 2 <= MaxCachedIdx: inside the live cached prefix
	tail := savedTokenValueAt(c, 4)  // 4 > MaxCachedIdx: being written into the cache now

	if !depth.cached || depth.perToken != 0.30/1e6 {
		t.Fatalf("a candidate inside the cached prefix must be priced at the READ rate and "+
			"reported cached: got cached=%v perToken=%g", depth.cached, depth.perToken)
	}
	if tail.cached || tail.perToken != 3.75/1e6 {
		t.Fatalf("a candidate in the uncached TAIL must be priced at the cache-WRITE rate and "+
			"NOT reported cached — it is entering the cache on this turn, not being read from "+
			"it: got cached=%v perToken=%g", tail.cached, tail.perToken)
	}
	// The repeat rate is the read rate on both sides: whatever we remove, the turns that
	// replay that removal are warm ones.
	if tail.repeatPerToken != 0.30/1e6 || depth.repeatPerToken != 0.30/1e6 {
		t.Fatalf("repeat rate must be the read rate on both sides: tail=%g depth=%g",
			tail.repeatPerToken, depth.repeatPerToken)
	}
	if got := tail.perToken / depth.perToken; math.Abs(got-12.5) > 0.01 {
		t.Fatalf("the tail/depth ratio is the size of the old mis-pricing and should be "+
			"12.5x (1.25 / 0.1); got %.2fx", got)
	}
	// A cold sweep prices at the write rate wherever the candidate sits, because there is no
	// live prefix for it to be inside.
	cold := &components.Ctx{CacheAware: true, ColdCache: true, MaxCachedIdx: 3}
	if a, b := savedTokenValueAt(cold, 1), savedTokenValueAt(cold, 9); a != b || a.cached {
		t.Fatalf("on a cold sweep position must not change the price: %+v vs %+v", a, b)
	}
}

// TestTheTailIsStillGatedOnItsOwnEconomics is the other half of
// TestDefaultConfigsSpendOnlyOnTheUncachedTail, and the reason that one is a re-pricing
// rather than an opening of the floodgates. Correcting the tail's VALUE does not remove the
// gate: a tail candidate too small to repay a call must still be refused.
//
// explore is false throughout — the exploration allowance deliberately spends a bounded call
// when no compression ratio has been observed yet, and it would mask the arithmetic here.
func TestTheTailIsStillGatedOnItsOwnEconomics(t *testing.T) {
	rates := components.TokenRates{Input: 3.00 / 1e6, CacheRead: 0.30 / 1e6, CacheWrite: 3.75 / 1e6}
	c := &components.Ctx{CacheAware: true, MaxCachedIdx: 0, SelfRates: rates}
	tail := savedTokenValueAt(c, 1)
	depth := savedTokenValueAt(c, 0)
	const ratio = 0.30 // a plausible observed compression ratio
	cost := 0.012      // ~one cheap-model call

	small := evaluateGate(1_200, ratio, tail, cost, false, 4, false, false)
	if small.allow {
		t.Fatalf("a 1,200-token tail candidate cannot repay a $%.3f call even at the write "+
			"rate (expected saving $%.5f) — the gate must still refuse it: %q",
			cost, small.expSaving, small.reason)
	}
	big := evaluateGate(200_000, ratio, tail, cost, false, 4, false, false)
	if !big.allow {
		t.Fatalf("a 200,000-token tail candidate must clear a $%.3f call (expected saving "+
			"$%.5f): %q", cost, big.expSaving, big.reason)
	}
	// Same candidate, same size, priced as cached: the 12.5x haircut is what used to make
	// every tail call look unaffordable, so this is the before-and-after in one assertion.
	if d := evaluateGate(200_000, ratio, depth, cost, false, 4, false, false); d.allow {
		t.Fatalf("priced at the READ rate the same 200,000-token candidate should not clear "+
			"the gate; if it does, this test no longer demonstrates the mis-pricing: %q", d.reason)
	}
}

// TestConcurrentIdenticalExtractionsCollapseToOneCall pins the single-flight fix.
//
// The persistent cross-session cache only helps a request that arrives after the first one
// finished. Measured on two live sessions started together, the same 4,577-token candidate was
// extracted twice 1.6s apart — $0.0224 for a result the system was already deriving, 54% of
// that run's entire extraction spend. Ten colleagues on one repo through one proxy is exactly
// that shape, so this is not a theoretical race.
//
// The assertion is on the MODEL CALL COUNT, not on the output: both requests must still get a
// reduced result, and only one of them may pay for it.
func TestConcurrentIdenticalExtractionsCollapseToOneCall(t *testing.T) {
	// A model that blocks until released, so both goroutines are provably in flight together
	// — a sleep would make this a timing coincidence rather than a test.
	release := make(chan struct{})
	var calls atomic.Int64
	blocking := blockingModel{calls: &calls, release: release}

	cfg := "strategy: code\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n" +
		"trigger:\n  min_request_tokens: 1\n"
	comp, err := newExtractLLM([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	e := comp.(*ExtractLLM)
	e.mode = markerFull

	pad := strings.Repeat("padding ", 200)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`
	// One shared Store, as a real proxy has, but two DIFFERENT sessions: the point is that
	// cross-session dedup happens while the first extraction is still running.
	st := store.NewMemory(store.Options{})

	var wg sync.WaitGroup
	for _, session := range []string{"session-A", "session-B"} {
		wg.Add(1)
		go func(session string) {
			defer wg.Done()
			req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
				userMsg("find the keep records"), toolResultMsg(body),
			}}
			c := &components.Ctx{Ctx: context.Background(), Session: session, Store: st,
				Model: components.ModelSpec{Static: blocking}, CtxWindow: 1_000_000}
			var rep components.Report
			if _, err := e.Offload(req, &rep, c); err != nil {
				t.Error(err)
			}
		}(session)
	}
	// Both goroutines are now either waiting on the model or waiting on the leader. Release.
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("two concurrent requests for byte-identical content made %d model calls; "+
			"single-flight must collapse them to 1 (this race cost 54%% of a measured run's "+
			"extraction spend)", got)
	}
}

type blockingModel struct {
	calls   *atomic.Int64
	release chan struct{}
}

func (m blockingModel) Complete(context.Context, string) (string, error) {
	m.calls.Add(1)
	<-m.release
	return "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n", nil
}
