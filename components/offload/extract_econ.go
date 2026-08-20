package offload

import (
	"sync"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
)

// The economic gate. extract_llm is the only component that SPENDS money to SAVE money,
// so it is the only one that can be net-negative — and on Terminal-Bench it was, by ~8x:
// 271 calls / $3.26 / ~1,592s of latency against ~197,548 unique tokens saved.
//
// The arithmetic behind that loss is the whole point. A saved token is only worth the
// rate it WOULD have been billed at. On a prompt-caching backend the request is ~99.95%
// cached, so a token removed from a cached region is worth the cache-READ rate
// ($0.20/MTok on the agent model), not the fresh-input rate ($2/MTok) — a 10x haircut.
// An extraction call costing ~$0.012 must therefore remove ~60,000 cache-read tokens to
// break even, versus ~6,000 fresh ones. Most tool outputs are nowhere near that, which is
// why "compress everything" loses on a caching backend and wins on a non-caching one.
//
// The component already contained this insight as a comment on skip_file_reads
// (extract_llm.go, on AUTO mode). This turns that reasoning into an actual gate that
// applies to EVERY candidate, not just line-numbered file reads.

// --- Issue #28 part B: reusing the AGENT's cached prefix — PROTOTYPED AND REJECTED ---
//
// The proposal was to append the extraction instruction as a final user message after the
// agent's existing stable prefix, so extraction reads an already-cached context instead of
// paying fresh input on its own prompt. Prototyped against the live gateway
// (aws/claude-sonnet-5, a ~103k-token cached prefix). It works mechanically — the
// extraction turn read the full prefix from cache, no cache-write, no prefix invalidation:
//
//	agent turn 1 (writes cache)        in=1   write=103,019  read=0
//	agent turn 2 (reads cache)         in=9   write=0        read=103,019
//	extraction reusing agent prefix    in=25  write=0        read=103,019
//
// But cache-read is cheap, not free, and the bill scales with the WHOLE context:
//
//	dedicated haiku call (~3k prompt)      $0.00400
//	reuse @   103,019-token prefix         $0.03398    8.5x
//	reuse @   500,000-token prefix         $0.15307   38.3x
//	reuse @ 1,700,000-token prefix         $0.51307  128.3x
//
// At the ~1.7M contexts this workload actually reaches, one extraction costs ~128x a
// dedicated cheap-model call — and the component issues up to llmConcurrency=4 per turn,
// so ~$2.04/turn against ~$0.016. That is the opposite of the direction this issue exists
// to push. Paying 1.7M cache-read tokens to answer a question about ONE tool output is
// structurally wrong regardless of the rate.
//
// DECISION: NOT IMPLEMENTED. Three independent reasons, any one sufficient:
//  1. Cost: 8.5x-128x a dedicated call, worsening as context grows (measured above).
//  2. Cache-write risk on the agent's own prefix: a write is 11.5x a read, and putting
//     extraction traffic on the agent's cache key risks exactly the mistake this whole
//     workstream is about. The prototype did not trigger one, but it only takes one
//     divergent breakpoint or an eviction between turns.
//  3. Coupling: it ties the compaction model to the agent model, so extraction quality,
//     latency, and spend all move whenever someone changes the agent's model.
//
// The dedicated cheap model stays. Re-open only if a provider prices in-context follow-up
// questions at a flat rate rather than per cache-read token.

// tokenValue is the dollars-per-token a SAVED token is worth, and the reason the gate
// exists. Both rates are per single token (not per million).
type tokenValue struct {
	// perToken is what a saved token is worth on each REPLAY turn — every turn after the
	// one the compaction is first applied on.
	perToken float64
	// firstToken is what it is worth on the turn the compaction IS applied, which is a
	// different rate on a caching backend and was previously conflated with perToken.
	firstToken float64
	cached     bool // true when replays are priced at the cache-read rate
}

// Default agent-model rates (claude-sonnet-5 class, $3/$15 per MTok, cache read 0.1x).
// The gate is a comparison, so what matters is the RATIO between a saved token's value
// and the extraction call's cost — both scale together if an operator's contract differs.
const (
	agentFreshPerMTok     = 3.00
	agentCacheReadPerMTok = 0.30 // 0.1x fresh, the standard Anthropic cache-read multiplier
	// 1.25x fresh, the standard Anthropic cache-WRITE multiplier. Needed because the turn a
	// compaction is applied on is not a cache-read turn — see savedTokenValue.
	agentCacheWritePerMTok = 3.75
)

// savedTokenValue prices one saved token for THIS request, distinguishing the turn the
// compaction is APPLIED on from the turns it is REPLAYED on. Those are different rates on
// a caching backend, and conflating them undervalued the component by ~3.3x.
//
// The old version priced every saved token at the cache-read rate whenever the request was
// cache-aware, reasoning that "content the agent re-sends every turn is already in the
// cached prefix". That is true of a REPLAY turn and false of the turn the cut is made —
// and for extract_llm it is false in the only place the component can act at all. When
// cache-aware it is confined to the TAIL (see the TailOnly check in extract_llm.go), and
// tail content is by definition new this turn: it has never been cached, so on this turn
// it is billed as a cache-WRITE (1.25x fresh) or as plain fresh input if it falls after the
// last cache_control breakpoint — either way 10-12.5x the cache-read rate it was assigned.
//
// Confirmed on live traffic rather than argued: a real SWE-bench trial reported 52,561
// cache_creation tokens against 746,047 cache_read across 18 turns. New tail content is
// cache-CREATED every turn; it is not read.
//
// The correction matters because the ~30,500 tokens/output break-even quoted in
// evaluateGate — the stated justification for disabling this component on caching backends
// by default — was computed with the old pricing. Directly recomputed against a flat
// $0.00766 haiku call cost and the 0.12 default ratio:
//
//	recurring   (reuses=6): 30,397 -> 11,550 tokens/output  (2.63x)
//	first sight (reuses=4): 42,556 -> 12,900 tokens/output  (3.30x)
//
// That does not rescue small-output workloads — SWE-bench's largest measured tool output is
// 2,760 tokens, still an order of magnitude short — so the shipping VERDICT for those is
// unchanged even though the number was wrong. It matters on large-output workloads: on
// LOCA-bench captures the eligible set goes from 7 to 31 of 1,639 outputs.
//
// One consequence worth knowing before tuning: because the applied turn now dominates the
// sum, RECURRENCE is a much weaker lever than the old pricing implied — it multiplies the
// expected saving by 1.12x, not 1.40x.
//
// The non-caching path is unchanged by construction: firstToken == perToken == fresh, so
// the total stays (1 + reuses) x fresh exactly as before.
func savedTokenValue(c *components.Ctx) tokenValue {
	if c != nil && c.CacheAware {
		return tokenValue{
			perToken:   agentCacheReadPerMTok / 1_000_000,
			firstToken: agentCacheWritePerMTok / 1_000_000,
			cached:     true,
		}
	}
	fresh := agentFreshPerMTok / 1_000_000
	return tokenValue{perToken: fresh, firstToken: fresh, cached: false}
}

// priorCallCost is a last-resort per-call cost estimate (~the Terminal-Bench average).
// It is only used when neither an observation nor a size is available; see callCost for
// why a flat prior must never be the primary estimate.
const priorCallCost = 0.012

// Prompt-size constants for the analytic cost estimate, in tokens.
const (
	// preambleTokens is the invariant contract + examples sent on every call (measured
	// 1463 for the code strategy). It is billed as fresh input whenever the provider's
	// minimum cacheable prefix is above it — which is the case on claude-haiku-4-5.
	preambleTokens = 1463
	// promptOverheadTokens covers the goal + keep-list + labels in the variable part.
	promptOverheadTokens = 200
	// expectedOutputTokens is a Starlark filter program's typical length (observed ~77
	// on Terminal-Bench captures; kept a little higher so cost is not under-estimated).
	expectedOutputTokens = 200
	// maxShownTokens bounds the content shown to the model (maxCodeContentChars ≈ 32k
	// chars ≈ 5k tokens), so a huge output does not inflate the estimated prompt without
	// limit — the real prompt is truncated to head+tail.
	maxShownTokens = 5000
)

// callCost returns the expected dollar cost of ONE extraction call for a candidate of
// sizeTokens.
//
// A flat per-call constant is the wrong model and caused a real cold-start deadlock in
// development: the gate priced every call at the ~$0.012 Terminal-Bench average, which is
// ~5x the true cost on a workload with small outputs, so it suppressed everything — and
// because it suppressed everything, no call was ever observed and the estimate never
// corrected itself. Measured on a real capture: observed cost was $0.0024/call against
// the $0.012 prior, and the gate wrongly declined calls that were in fact ~2.2x profitable.
//
// So the estimate is analytic and size-aware first (prompt = preamble + shown content +
// overhead, priced at real rates), blended with the observed mean once real calls exist.
// The observed mean alone is not enough either: it is an average over past candidate
// sizes, and cost genuinely scales with THIS candidate's size.
func callCost(pricing cheapmodel.Pricing, sizeTokens int) float64 {
	shown := sizeTokens
	if shown > maxShownTokens {
		shown = maxShownTokens
	}
	inTok := int64(preambleTokens + shown + promptOverheadTokens)
	analytic := pricing.Cost(inTok, expectedOutputTokens, 0, 0)

	// Reconcile with reality: if observed calls came in cheaper or dearer than the
	// analytic model predicts (a working preamble cache, a different tokenizer, a
	// gateway contract), scale by that ratio rather than discarding size-sensitivity.
	if avg, ok := cheapmodel.AvgCallCost(pricing); ok && avg > 0 {
		if base := analyticBaseline(pricing); base > 0 {
			if ratio := avg / base; ratio > 0.1 && ratio < 10 {
				return analytic * ratio
			}
		}
		return (analytic + avg) / 2 // ratio implausible: hedge between the two
	}
	if analytic <= 0 {
		return priorCallCost
	}
	return analytic
}

// analyticBaseline is the analytic cost of a mid-sized candidate, used as the denominator
// when scaling the analytic estimate to observed reality.
func analyticBaseline(pricing cheapmodel.Pricing) float64 {
	return pricing.Cost(preambleTokens+2000+promptOverheadTokens, expectedOutputTokens, 0, 0)
}

// gateDecision records why the gate allowed or suppressed a call. The reason string is
// the operator's answer to "why did this run?" / "why didn't it?", surfaced in metrics —
// a gate whose decisions you cannot explain is a gate nobody will trust enough to leave on.
type gateDecision struct {
	allow  bool
	reason string
	// expSaving/expCost are the dollar figures the decision compared, so a surprising
	// suppression can be audited rather than guessed at.
	expSaving float64
	expCost   float64
}

// expectedReuses estimates how many future turns this compaction will be re-applied on.
// This is what makes extraction ever worthwhile under caching: the reduction is frozen and
// replayed on every subsequent turn (see state.go's freeze/reapply), so one call's saving
// is collected repeatedly. Recurrence is the strongest available signal — content the
// system has seen before in ANY session is likely to be seen again.
//
// ponytail: a flat prior per recurrence class, not a fitted model. Two observations
// (seen-before, request-position) capture most of the signal; upgrade to a per-session
// decay fit if the benchmark shows the estimate is what's mispricing calls.
func expectedReuses(seenBefore bool, turnsSoFar int) float64 {
	if seenBefore {
		// Recurred at least once already; the measured cross-session recurrence rate was
		// 82/103 (~80%), so expect several more replays.
		return 6
	}
	if turnsSoFar >= 20 {
		return 3 // late in a long session: fewer turns remain to amortize over
	}
	return 4
}

// evaluateGate decides whether one candidate output is worth an extraction call.
//
// expected saving = tokens we expect to remove x (first-turn value + expected future
// reuses x replay value). The two rates differ on a caching backend: the turn the cut is
// made is a cache-write (or fresh input), only later turns are cache-reads.
// expected cost   = observed mean cost of one extraction call
//
// Allow only when saving strictly exceeds cost. Every suppression carries a reason.
func evaluateGate(sizeTokens int, ratio float64, val tokenValue, cost float64,
	seenBefore bool, turnsSoFar int, explore, allowCached bool) gateDecision {

	expectedRemoved := float64(sizeTokens) * ratio
	reuses := expectedReuses(seenBefore, turnsSoFar)
	// The compaction is applied on this turn AND replayed on each expected future turn.
	// Applied once at firstToken, then replayed at perToken. On a non-caching backend the
	// two rates are equal and this is identical to the old (1 + reuses) x perToken.
	saving := expectedRemoved * (val.firstToken + reuses*val.perToken)

	d := gateDecision{expSaving: saving, expCost: cost}
	// Hard decline on a caching backend unless explicitly forced. This is the SHIPPING
	// DECISION, in code rather than prose: the measurements in this change show the
	// component net-negative on every caching workload tested, even with the gate working
	// correctly (break-even ~30,500 tokens/output against a largest-observed 2,053). A
	// default that ships a component our own numbers say loses money — guarded only by a
	// doc note nobody reads — is not a defensible default. `allow_on_caching_backend: true`
	// re-enables it for anyone whose workload genuinely has huge outputs; the gate's
	// economics then apply as normal.
	if val.cached && !allowCached {
		d.reason = "suppressed: disabled by default on caching backends (measured net-negative)"
		return d
	}
	if saving <= cost && explore {
		// No trustworthy ratio yet — spend a bounded call to find out rather than
		// letting a pessimistic prior justify itself forever.
		d.allow = true
		d.reason = "allow: exploring (learning this workload's compression ratio)"
		return d
	}
	if saving <= cost {
		// The honest message: on a caching backend a small output CANNOT pay for a call.
		if val.cached {
			d.reason = "suppressed: cache-aware, saving below call cost"
		} else {
			d.reason = "suppressed: saving below call cost"
		}
		return d
	}
	d.allow = true
	switch {
	case seenBefore:
		d.reason = "allow: recurring content, amortized over reuses"
	case !val.cached:
		d.reason = "allow: non-caching backend, saved tokens at full rate"
	default:
		d.reason = "allow: expected saving exceeds call cost"
	}
	return d
}

// defaultCompressionRatio is the fraction of an output an accepted extraction removes,
// used before this component has observed its own results.
//
// MEASURED, and much lower than intuition suggests. On real captures an accepted
// extraction removed only 31-254 tokens per call on outputs of 400-2000 tokens — an actual
// ratio around 0.10, not the 0.45 originally assumed here. The model mostly declines to cut
// aggressively (correctly: its contract is recall-first), so most of a "reduction" is small.
//
// Note the DIRECTION of conservatism: for a spending gate, conservative means
// UNDER-estimating the saving, i.e. a LOW ratio. An optimistic ratio is precisely how the
// component talked itself into 271 losing calls, so this errs low and lets the observed
// tracker raise it if a workload really does compress well.
const defaultCompressionRatio = 0.12

// ratioTracker learns this workload's ACTUAL compression ratio from accepted results, so
// the gate stops guessing after the first few calls. A call that produced nothing counts
// as ratio 0 — a model that keeps failing to reduce this workload's outputs should drive
// the estimate down and shut the gate, which is precisely the feedback the old
// fixed-threshold design lacked.
type ratioTracker struct {
	mu      sync.Mutex
	removed int64
	total   int64
	// explored counts exploration calls PER SESSION. A process-wide counter spent its
	// whole budget on the first session, after which every later session inherited an
	// unrevisable prior — reintroducing the self-justifying-prior failure at process scope,
	// which is the exact thing exploration exists to prevent. The tracker lives on the
	// Pipeline for the proxy's lifetime, so the map must be keyed by session.
	// ponytail: unbounded map keyed by session; the store's own TTL/LRU bounds sessions in
	// practice, so prune here only if a long-lived proxy shows growth.
	explored map[string]int
}

// observe records one attempted extraction: removedTok of totalTok (0 removed on a miss).
func (r *ratioTracker) observe(removedTok, totalTok int) {
	if totalTok <= 0 {
		return
	}
	r.mu.Lock()
	r.removed += int64(removedTok)
	r.total += int64(totalTok)
	r.mu.Unlock()
}

// ratio returns the estimated compression ratio: the conservative default until enough
// tokens have been seen, then the observed ratio SHRUNK toward that default and capped.
//
// Raw observation is too sharp a tool here. minRatioSampleTokens is about one medium
// output, so the first estimate can be n=1; an unbounded mean would let a single
// compressible early output drop the cached break-even from ~30,500 tokens to ~7,000
// permanently, and the gate would then spend on that basis for the rest of the process.
// Shrinkage (a standard weighted prior) makes early estimates move a little and later ones
// move a lot; the cap stops any amount of evidence claiming an implausible ratio.
func (r *ratioTracker) ratio() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.total < minRatioSampleTokens {
		return defaultCompressionRatio
	}
	observed := float64(r.removed) / float64(r.total)
	// Weight the observation by how much evidence backs it, against a fixed pseudo-count
	// of prior "evidence" worth shrinkPriorTokens.
	w := float64(r.total) / float64(r.total+shrinkPriorTokens)
	est := w*observed + (1-w)*defaultCompressionRatio
	if est > maxLearnedRatio {
		return maxLearnedRatio
	}
	if est < 0 {
		return 0
	}
	return est
}

// shrinkPriorTokens is how much "evidence" the conservative prior is worth. Set to a few
// medium outputs so a single observation cannot swing the estimate far, while a session's
// worth of consistent evidence dominates it.
const shrinkPriorTokens = 8000

// maxLearnedRatio caps the learned ratio. Even a genuinely compressible workload should not
// let the gate assume more than this, because the accepted-result sanity checks bound how
// much an extraction can remove and still be accepted. Measured ratios were ~0.10-0.12.
const maxLearnedRatio = 0.60

// exploring reports whether the tracker still lacks the evidence to estimate a ratio, and
// consumes one exploration slot if so.
//
// This closes the SECOND deadlock of the same shape as the flat-cost one (see callCost).
// The ratio starts at a deliberately pessimistic 0.12; on a workload whose outputs sit
// below the resulting break-even, the gate suppresses every call — so the tracker never
// observes anything and the pessimistic default becomes permanent and self-justifying.
// Measured on a real capture: the gate forwent a genuine +$0.0094 net because of exactly
// this. A gate that can never revise its own prior is not a gate, it is an off switch.
//
// So allow a small, BOUNDED number of calls through before the estimate is trustworthy.
// The budget is PER SESSION: each session's traffic can differ, and a process-wide budget
// would be exhausted by the first session and leave every later one unable to revise its
// prior — the same off-switch failure at a larger scale.
func (r *ratioTracker) exploring(session string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.total >= minRatioSampleTokens {
		return false // enough evidence; no need to spend on learning
	}
	if r.explored == nil {
		r.explored = map[string]int{}
	}
	if r.explored[session] >= maxExploreCalls {
		return false
	}
	r.explored[session]++
	return true
}

// maxExploreCalls bounds the exploration budget PER SESSION. Small on purpose: enough to
// learn the ratio (a couple of outputs clears minRatioSampleTokens), far too few to
// reproduce the 271-call loss even if every one is wasted. Each call is ~$0.003-0.008 and
// ~5-15s of latency, so this is also the knob that bounds exploration's latency cost.
const maxExploreCalls = 2

// slowCallMs is the mean per-call latency above which the gate stops exploring. Exploration
// is a bet that costs money AND wall-clock time, and on an agent with a task deadline the
// wall clock is the scarcer resource: PR #37 measured 17.8s across 2 calls that saved 0
// tokens, contributing to a task exhausting its budget. Money-only reasoning cannot see
// that, so latency gets its own brake — once calls are observed to be this slow, a
// speculative call is no longer worth making however cheap it looks.
const slowCallMs = 6000

// tooSlowToExplore reports whether observed extraction latency is high enough that
// speculative calls should stop. Uses the observed mean, so it self-tunes to the deployment
// rather than assuming a gateway's speed.
func tooSlowToExplore(avgLatencyMs float64, calls int64) bool {
	return calls > 0 && avgLatencyMs >= slowCallMs
}

// minRatioSampleTokens is how much observed content the ratio estimate needs before it
// displaces the default. Kept small so a workload that genuinely compresses well is
// recognized within a few calls rather than after a whole session.
const minRatioSampleTokens = 1500

// --- Triggering (issue #28 part E) -----------------------------------------------
//
// The old trigger was a raw token threshold (min_tokens) that had to be re-picked per
// workload — the component's worst ergonomic problem. The replacement asks the only
// question that generalizes: is this request under enough CONTEXT PRESSURE that removing
// tokens matters, and is there enough evidence that a call will pay?
//
// min_tokens stays honored when set explicitly (backward compatibility); the derived
// trigger is the DEFAULT when it is not.

// contextPressure is the fraction of the model's context window the request occupies.
// 0 when the window is unknown, in which case pressure-based logic is skipped and the
// absolute floors apply — the same fail-open convention Trigger already uses.
func contextPressure(requestTokens, window int) float64 {
	if window <= 0 || requestTokens <= 0 {
		return 0
	}
	return float64(requestTokens) / float64(window)
}

// pressureFloor derives the per-output token floor from context pressure, replacing a
// hand-tuned min_tokens. The shape: when the context is nearly empty compaction buys
// nothing worth an LLM call, so demand a big output; as the window fills, the floor drops
// and smaller outputs become worth reducing. Returns an absolute token count.
//
// The numbers are chosen so a 1M-window model behaves sanely without tuning:
//
//	<25% full  -> 0.6% of window (~6000 tok on 1M): only large outputs
//	 25-60%    -> 0.3% of window (~3000 tok)
//	 60-80%    -> 0.15% of window (~1500 tok)
//	  >80%     -> 0.05% of window (~500 tok): window pressure dominates, compact freely
func pressureFloor(window int, pressure float64) int {
	if window <= 0 {
		return 0 // unknown window: caller falls back to its absolute default
	}
	var frac float64
	switch {
	case pressure > 0.80:
		frac = 0.0005
	case pressure > 0.60:
		frac = 0.0015
	case pressure > 0.25:
		frac = 0.0030
	default:
		frac = 0.0060
	}
	if f := int(frac * float64(window)); f > 0 {
		return f
	}
	return 0
}

// growthRate is tokens added since the previous turn, over the current size — how fast
// this session is accumulating context. A fast-growing request is where compaction has
// the most to work on; a static one has nothing new to reduce and should not re-fire.
func growthRate(currentTokens, prevTokens int) float64 {
	if currentTokens <= 0 || prevTokens <= 0 || currentTokens <= prevTokens {
		return 0
	}
	return float64(currentTokens-prevTokens) / float64(currentTokens)
}

// shouldFire decides whether the LLM path runs on this request at all, and why. It must
// NOT fire on every step of a merely-growing context — that was the old behavior's waste.
//
// minTokensSet reports whether the operator pinned min_tokens explicitly; when they did,
// their threshold governs and this stays out of the way.
func shouldFire(pressure, growth float64, minTokensSet bool) (bool, string) {
	if minTokensSet {
		return true, "explicit min_tokens/trigger configured"
	}
	switch {
	case pressure > 0.60:
		return true, "high context pressure"
	case pressure > 0.25 && growth > 0.10:
		return true, "moderate pressure with fast context growth"
	case pressure > 0.25:
		// Growing slowly at moderate pressure: the per-output floor still gates
		// individual candidates, but do not spend a call on a static context.
		return false, "moderate pressure but context near-static"
	default:
		return false, "low context pressure"
	}
}
