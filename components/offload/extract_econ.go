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
	perToken float64
	// repeatPerToken is what the SAME token is worth on a LATER turn, where it would have
	// been served from the provider's prompt cache rather than entering it. Splitting the two
	// is what stopped the gate crediting a replay at the rate of a first removal: on a cold
	// turn perToken is the cache-WRITE rate (1.25x fresh) and every subsequent realization of
	// that same removal is a cache-READ token (0.1x fresh) — 12.5x apart. Pricing the whole
	// amortized stream at the write rate over-credited every cold-turn call by ~12x.
	repeatPerToken float64
	cached         bool // true when priced at the cache-read rate
	// cold is true on a TTL-expired turn, where perToken is the cache-WRITE rate. Both cold
	// and non-caching turns report cached:false — for opposite reasons — so the flag exists to
	// keep the two apart in the reason ledger. Without it every cold sweep was labelled
	// "allow: non-caching backend", which is the string an operator reads to answer "why did
	// this run"; 16 of 93 production calls carried it.
	cold bool
}

// Default agent-model rates (claude-sonnet-5 class, $3/$15 per MTok, cache read 0.1x).
// The gate is a comparison, so what matters is the RATIO between a saved token's value
// and the extraction call's cost — both scale together if an operator's contract differs.
// THESE ARE A FALLBACK, NOT THE PRICE. The real numerator of every gate decision is the
// request's own rate card (Ctx.SelfRates), which the proxy resolves from the provider's live
// price table — see savedTokenValue. Hardcoding it was measurably wrong in both directions:
// the code said $3.75/MTok for a cold-turn token and docs/results/measured-2026-08.md §9 said
// $4.75, a 27% disagreement in the numerator of the ONE regime that pays. NEITHER is right on
// this deployment. Derived 2026-08-19 from the recorded per-request cost_usd and token tiers
// of four captured corpora (solving two independent requests simultaneously):
//
//	aws/claude-sonnet-5   $2.00 in / $10.00 out per MTok  => cache write $2.50, read $0.20
//
// $3.75 is 1.25x sonnet-4-5's $3.00 LIST rate and $4.75 is the opus-5-era figure; both are
// list prices for models this deployment does not bill at. A literal cannot be right for
// every operator, so the literal is now only what we use when the table said nothing.
const (
	agentFreshPerMTok     = 3.00
	agentCacheReadPerMTok = 0.30 // 0.1x fresh, the standard Anthropic cache-read multiplier
	// agentCacheWritePerMTok is 1.25x fresh, the standard cache-WRITE multiplier, and it is
	// what a token is worth on a turn whose cache has expired.
	//
	// This is the most valuable token in the system and the component could not previously
	// see it. MEASURED on this deployment over 1.4 days: turns whose cache had expired were
	// 4% of requests (219 of 5,596) and 31% of spend ($360 of $1,173) — $1.64 each against
	// $0.144 for a warm turn, an 11x difference — because all 56.7M of their tokens were
	// billed as cache_creation_input_tokens. Removing one of those saves 1.25x the fresh
	// rate, i.e. 12.5x what removing a token from a warm cached prefix saves.
	agentCacheWritePerMTok = 3.75
)

// savedTokenValue prices one saved token for THIS request. When the request goes to a
// prompt-caching backend, content the agent re-sends every turn is already in the cached
// prefix, so removing it saves the cache-read rate — the 10x haircut that sinks the
// component's economics.
func savedTokenValue(c *components.Ctx) tokenValue {
	fresh, read, write := agentFreshPerMTok/1e6, agentCacheReadPerMTok/1e6, agentCacheWritePerMTok/1e6
	// The request's own model, at the rates this deployment is actually billed, beats any
	// constant. Ctx.SelfRates comes from the same provider price table the dashboard prices
	// requests with, so the gate's numerator and the operator's bill can no longer disagree.
	// A tier the table left blank falls back to the standard multipliers off fresh rather
	// than to the sonnet-class literal, which would mix two different rate cards.
	if c != nil && !c.SelfRates.Zero() {
		if c.SelfRates.Input > 0 {
			fresh = c.SelfRates.Input
		}
		read, write = c.SelfRates.CacheRead, c.SelfRates.CacheWrite
		if read <= 0 {
			read = fresh * 0.1
		}
		if write <= 0 {
			write = fresh * 1.25
		}
	}
	if c != nil && c.CacheAware {
		// A cache-aware turn whose cache has EXPIRED is the opposite case to a warm one: the
		// whole prefix is about to be re-written at 1.25x fresh, so a token removed here is
		// the most valuable token there is — not the least. Reporting it as `cached` would
		// hand the gate the 10x haircut that (correctly) suppresses warm-turn calls.
		//
		// The REPEAT rate is the read rate either way: once this turn has re-written the
		// prefix, the turns that replay this removal are warm ones.
		if c.ColdCache {
			return tokenValue{perToken: write, repeatPerToken: read, cached: false, cold: true}
		}
		return tokenValue{perToken: read, repeatPerToken: read, cached: true}
	}
	// No caching backend: every turn re-sends at the fresh rate, so a replay is worth as
	// much as the first removal.
	return tokenValue{perToken: fresh, repeatPerToken: fresh, cached: false}
}

// priorCallCost is a last-resort per-call cost estimate (~the Terminal-Bench average).
// It is only used when neither an observation nor a size is available; see callCost for
// why a flat prior must never be the primary estimate.
const priorCallCost = 0.012

// Prompt-size constants for the analytic cost estimate, in tokens.
const (
	// preambleTokens is the invariant contract + examples sent on every call, in o200k
	// tokens. It is billed as fresh input whenever the provider's minimum cacheable prefix
	// is above it — which was the case on claude-haiku-4-5 until the unit fix in
	// cheapmodel.minCacheableO200k.
	//
	// MEASURED 2026-08-19 by tokenizing the assembled prefix internal/extract actually
	// sends (codeSystemBlocks, rewrite: true, aggressiveness: medium — the shipped default):
	//
	//	block 0 (lead-in + codeContract + codeRules) 1,138 o200k
	//	block 1 (aggroMedium, embeds codeExample)      755 o200k
	//	TOTAL                                       1,893 o200k  = 2,956 billed on sonnet
	//
	// The previous 1463 was 29% below that, so every gate decision was priced against a
	// fixed half that does not exist. Re-measure with the profile's TestProfilePromptBudget
	// whenever internal/extract's prompt text changes; a stale figure here is invisible.
	preambleTokens = 1893
	// realTokenMarkup converts an o200k count (internal/tokens, what every estimate here is
	// in) into the count the provider BILLS, which is what Pricing is per. Without it the
	// whole analytic estimate is priced in the wrong unit.
	//
	// MEASURED 2026-08-19, identical bytes both sides: o200k said 6,396; claude-haiku-4-5
	// billed 8,222 (1.29x) and aws/claude-sonnet-5 billed 10,574 (1.65x). The haiku figure is
	// used because haiku-class is the intended compactor; on a sonnet-class compactor this
	// under-states by ~28%, which callCost's observed-cost reconciliation then corrects.
	// (extract_llm.extractContextMargin is the same measurement used for a different purpose
	// — fitting a window rather than pricing — and takes the conservative-high end.)
	realTokenMarkup = 1.29
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
func callCost(pricing cheapmodel.Pricing, sizeTokens, overheadTokens int) float64 {
	shown := sizeTokens
	if shown > maxShownTokens {
		shown = maxShownTokens
	}
	if overheadTokens <= 0 {
		overheadTokens = promptOverheadTokens
	}
	inTok := int64(float64(preambleTokens+shown+overheadTokens) * realTokenMarkup)
	analytic := pricing.Cost(inTok, expectedOutputTokens, 0, 0)

	// Reconcile with reality: if observed calls came in cheaper or dearer than the
	// analytic model predicts (a working preamble cache, a different tokenizer, a
	// gateway contract), scale by that ratio rather than discarding size-sensitivity.
	if avg, ok := cheapmodel.AvgCallCost(pricing); ok && avg > 0 {
		if base := analyticBaseline(pricing, overheadTokens); base > 0 {
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
func analyticBaseline(pricing cheapmodel.Pricing, overheadTokens int) float64 {
	if overheadTokens <= 0 {
		overheadTokens = promptOverheadTokens
	}
	return pricing.Cost(int64(float64(preambleTokens+2000+overheadTokens)*realTokenMarkup),
		expectedOutputTokens, 0, 0)
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
// is collected repeatedly.
//
// MEASURED, per session, over the six production sessions that produced a removal:
// saved_gross/saved_unique = 4.0, 4.4, 8.0, 12.0, 79.9, 215.0 — min 4.0, median 12.0. Removals
// visibly persist: one session carried a single removal across 217 rows. So 6/3/4 is conservative
// against the median and inside the observed range, which is where a prior belongs.
//
// A CORRECTION WORTH KEEPING, because the wrong version was briefly shipped here and the
// arithmetic looked convincing: these were changed to 1.5/0.3/0.6 on a figure of 1.59x derived as
// saved_gross/saved_unique over only the 13 requests that MADE CALLS. That is not an amortization
// figure. saved_unique is 46,380 whether you sum over those 13 rows or all 1,770 — every unique
// removal accrues on a calling turn by definition — so restricting the numerator to calling turns
// while keeping that denominator subtracts every replay by construction. 1.59x measures "what a
// calling turn realizes relative to its own new removals", which is a different question and is
// always near 1. The replays live in the other 1,757 rows (2,408,593 gross against the same
// 46,380 unique).
//
// The claim that accompanied it — that claude-cli drops old turns so a removal is not carried
// forward — is contradicted by the same ledger: 416 rows are pure replays (gross > 0, unique = 0).
//
// The OTHER half of that change was right and stays: a replayed removal is a cache-READ token,
// not a cache-write one. See tokenValue.repeatPerToken. Pricing 6 replays at the read rate adds
// 0.6x the write rate, so the amortization is real but modest — which is why the rate split
// mattered more than the count did.
//
// ponytail: a flat prior per recurrence class, not a fitted model, anchored on the per-session
// distribution rather than an aggregate that a few long sessions dominate (max 215 against a
// median of 12). Upgrade to a per-session decay fit only if a measurement shows the estimate is
// still what misprices calls.
func expectedReuses(seenBefore bool, turnsSoFar int) float64 {
	if seenBefore {
		return 6 // already recurred once, so at or above the observed median
	}
	if turnsSoFar >= 20 {
		return 3 // late in a long session: fewer turns remain to amortize over
	}
	return 4 // the observed minimum
}

// evaluateGate decides whether one candidate output is worth an extraction call.
//
// expected saving = tokens we expect to remove x (1 + expected future reuses) x per-token value
// expected cost   = observed mean cost of one extraction call
//
// Allow only when saving strictly exceeds cost. Every suppression carries a reason.
func evaluateGate(sizeTokens int, ratio float64, val tokenValue, cost float64,
	seenBefore bool, turnsSoFar int, explore, allowCached bool) gateDecision {

	expectedRemoved := float64(sizeTokens) * ratio
	reuses := expectedReuses(seenBefore, turnsSoFar)
	// The compaction is applied on this turn at THIS turn's rate, and replayed on each
	// expected future turn at the rate a re-sent token would have been billed at THERE. On a
	// cold turn those are the cache-write and cache-read rates, 12.5x apart, and collapsing
	// them onto one rate is what made a replay look as valuable as a first removal.
	repeat := val.repeatPerToken
	if repeat <= 0 {
		repeat = val.perToken // an unset repeat rate must not read as free amortization
	}
	saving := expectedRemoved * (val.perToken + reuses*repeat)

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
	case val.cold:
		// NOT "non-caching backend". Both regimes report cached:false — a cold turn because
		// its tokens are worth the cache-WRITE rate, a non-caching backend because they are
		// worth the fresh rate — so selecting on !cached labelled every cold sweep as a
		// backend without a cache. MEASURED: 16 of 93 production calls carried that string, in
		// the ledger an operator reads to answer "why did this run".
		d.reason = "allow: cold cache, whole transcript re-billed at the write rate"
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

// slowCallMs is the MEDIAN per-call latency above which the gate stops exploring. Exploration
// is a bet that costs money AND wall-clock time, and on an agent with a task deadline the
// wall clock is the scarcer resource: PR #37 measured 17.8s across 2 calls that saved 0
// tokens, contributing to a task exhausting its budget. Money-only reasoning cannot see
// that, so latency gets its own brake — once calls are observed to be this slow, a
// speculative call is no longer worth making however cheap it looks.
const slowCallMs = 6000

// tooSlowToExplore reports whether observed extraction latency is high enough that
// speculative calls should stop. Self-tunes to the deployment rather than assuming a
// gateway's speed.
//
// It reads the MEDIAN, not the mean. MEASURED 2026-08-19 on this gateway, n=8 identical
// calls: min 2,091 / p50 3,748 / max 11,663 ms, and an 8-token no-op call ran min 1,490 /
// p50 1,812 / max 15,800. One tail sample drags the mean over the 6,000 ms brake while the
// typical call is well under half of it, so the mean answers "was there a slow call?" when
// the question is "are calls slow?". Per-call latency here is gateway queue time, not prompt
// size (a 1-token call is not faster than an 8k-token one), so the tail is noise about the
// queue rather than evidence about our own work.
func tooSlowToExplore(p50LatencyMs float64, calls int64) bool {
	return calls > 0 && p50LatencyMs >= slowCallMs
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
func shouldFire(pressure, growth float64, minTokensSet, fireOnSize bool) (bool, string) {
	// `fire_on: size` short-circuits everything below: the operator asked for the
	// candidate's own size to be the whole decision, so context pressure is not consulted
	// at all. The per-output floor still gates each candidate, and llm_max_per_request /
	// llm_max_per_session bound how many calls a firing turn may make.
	if fireOnSize {
		return true, "size threshold (fire_on: size)"
	}
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
