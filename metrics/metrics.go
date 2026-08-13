// Package metrics turns component/run reports into telemetry. It provides
// concrete components.Emitter implementations; the pipeline depends only on the
// Emitter interface, so this package imports components and not vice versa.
//
// v1 ships: Nop (in components), Slog (structured logs in OTel gen_ai.*
// vocabulary), and Aggregator (in-process rollups behind /stats). An OTel-SDK
// emitter and honest-metrics extras (bounce-adjusted savings, waste signals)
// land in P5. Each host surfaces these natively (proxy -> bifrost Prometheus/
// OTel + /metrics; AuthBridge -> StatsSource).
package metrics

import (
	"log/slog"
	"sort"
	"sync"

	"github.com/rossoctl/context-guru/components"
)

// Tee fans a report out to several emitters.
type Tee []components.Emitter

func (t Tee) Component(r components.Report) {
	for _, e := range t {
		e.Component(r)
	}
}
func (t Tee) Run(r components.RunReport) {
	for _, e := range t {
		e.Run(r)
	}
}

// FilterAct / FilterMiss forward cmdfilter's ledger to whichever tee'd emitters
// record it (so a Tee still satisfies components.FilterStatsSink).
func (t Tee) FilterAct(family, filter, contentKey string, saved int) {
	for _, e := range t {
		if s, ok := e.(components.FilterStatsSink); ok {
			s.FilterAct(family, filter, contentKey, saved)
		}
	}
}

func (t Tee) FilterMiss(selector string) {
	for _, e := range t {
		if s, ok := e.(components.FilterStatsSink); ok {
			s.FilterMiss(selector)
		}
	}
}

// Slog logs each component and run in the GenAI semantic-convention vocabulary.
//
// At DEBUG, not INFO. These are per-component records — several per request — so at
// INFO they buried the one line per request that describes the request, which is that
// level's whole job. The proxy no longer wires this emitter at all: apply logs the same
// decisions with the tenant, the session and the gate histogram attached (see
// apply.logDecisions). It stays exported for a library host that wants this vocabulary.
type Slog struct{ L *slog.Logger }

func (s Slog) logger() *slog.Logger {
	if s.L != nil {
		return s.L
	}
	return slog.Default()
}

func (s Slog) Component(r components.Report) {
	s.logger().Debug("context_engineering.component",
		"context_engineering.component", r.Component,
		"context_engineering.kind", r.Kind,
		"context_engineering.tokens.before", r.TokensBefore,
		"context_engineering.tokens.after", r.TokensAfter,
		"context_engineering.tokens.saved", r.Saved(),
		"context_engineering.reverted", r.Reverted,
		"context_engineering.discarded_changes", r.Discarded,
		"context_engineering.duration_ms", r.DurationMs,
	)
}

func (s Slog) Run(r components.RunReport) {
	s.logger().Debug("context_engineering.run",
		"context_engineering.session", r.Session,
		"context_engineering.tokens.before", r.TokensBefore,
		"context_engineering.tokens.after", r.TokensAfter,
		"context_engineering.tokens.saved", r.Saved(),
		"context_engineering.duration_ms", r.DurationMs,
	)
}

// Aggregator keeps process-wide and per-component rollups for /stats. Savings
// are token-weighted (SUM saved / SUM before), the honest aggregate — not a
// mean of per-request percentages (rtk's lesson).
type Aggregator struct {
	mu       sync.Mutex
	requests int64
	before   int64
	after    int64
	wasted   int64 // tokens re-served via expand (offloaded then needed back)
	bounces  int64
	perComp  map[string]*compStat
	// End-to-end latency accounting (W7). addedMs is the wall time context-guru itself
	// adds per request (normalize + pipeline + writeback); upstreamMs is the provider
	// round-trip (incl. the expand loop). Split by bypass so a run can compare the
	// with-CG path against a transparent (x-context-guru-bypass) baseline.
	addedMs       float64
	addedSamples  int64
	upstreamMs    float64
	upstreamMsByp float64 // upstream latency on bypassed (baseline) requests
	upstreamN     int64
	upstreamNByp  int64
	// SSE time-to-first-byte accounting: how long after the upstream call started the
	// client got its first response byte, split by whether we had to buffer the whole
	// stream to inspect it for an expand call. Buffering is the only thing that stops a
	// stream being a stream, so counting it makes that cost visible instead of inferred
	// (it used to be unconditional and unmeasured — issue #26).
	sseTTFBMs    float64
	sseTTFBMsBuf float64
	sseStreamed  int64
	sseBuffered  int64
	// Mode dimension (#31). Enforced requests are counted per mode; observe-mode results
	// are kept in PHYSICALLY separate fields with their own serialized names, so no query
	// over the enforced rollups can accidentally include a hypothetical.
	mode            components.Mode // the configured mode, for the /stats banner
	syncRequests    int64
	potentialRuns   int64
	potentialBefore int64
	potentialAfter  int64
	potentialMs     float64
	potentialComp   map[string]*compStat
	// cmdfilter's per-family / per-filter ledger, plus the selector-miss ledger that
	// makes the next filter to write data instead of guesswork.
	filterFam  map[string]*filterStat
	filterName map[string]*filterStat
	filterMiss map[string]int64
	// Provider-billed usage, summed from response bodies (W8). These are the tiers
	// that actually cost money — on a prompt-caching backend a cache write bills
	// ~11.5x a read, so content-token savings alone cannot express the economics.
	freshInput int64
	cacheRead  int64
	cacheWrite int64
	outputTok  int64
	// attempted is the tokens compaction was ALLOWED to touch (the uncached tail
	// when cache-aware); frozen is what cache safety made us leave alone. Together
	// they give /stats an honest denominator and the cost of its own safety.
	attempted int64
	frozen    int64
}

// filterStat is one cmdfilter family's or filter's ledger. SavedUnique dedups by
// content key exactly as compStat does — the agent re-sends history verbatim every
// turn, so the cumulative figure double-counts the same compaction.
type filterStat struct {
	Acts        int64 `json:"acts"`
	Saved       int64 `json:"saved_tokens"`
	SavedUnique int64 `json:"saved_tokens_unique"`

	seenKeys map[string]struct{} // content keys already counted (not serialized)
}

// maxMissKeys bounds the selector-miss ledger; output shapes are unbounded in
// principle. Once full we only keep counting selectors already tracked.
// ponytail: fixed cap, no eviction — first-seen wins. Swap for a count-min sketch if
// the ledger ever gets dominated by whatever arrived first.
const maxMissKeys = 200

// FilterAct implements components.FilterStatsSink: one applied cmdfilter filter.
func (a *Aggregator) FilterAct(family, filter, contentKey string, saved int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.filterFam == nil {
		a.filterFam, a.filterName = map[string]*filterStat{}, map[string]*filterStat{}
	}
	bump(a.filterFam, family, contentKey, saved)
	bump(a.filterName, filter, contentKey, saved)
}

func bump(m map[string]*filterStat, key, contentKey string, saved int) {
	fs := m[key]
	if fs == nil {
		fs = &filterStat{seenKeys: map[string]struct{}{}}
		m[key] = fs
	}
	fs.Acts++
	fs.Saved += int64(saved)
	if _, seen := fs.seenKeys[contentKey]; !seen {
		fs.seenKeys[contentKey] = struct{}{}
		fs.SavedUnique += int64(saved)
	}
}

// FilterMiss implements components.FilterStatsSink: a selector that matched nothing.
func (a *Aggregator) FilterMiss(selector string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.filterMiss == nil {
		a.filterMiss = map[string]int64{}
	}
	if _, known := a.filterMiss[selector]; !known && len(a.filterMiss) >= maxMissKeys {
		return
	}
	a.filterMiss[selector]++
}

// SetMode records the configured operating mode so /stats can label itself — the
// observe-mode banner has to be unmistakable, and a consumer needs to know from the
// payload alone whether the numbers were enforced.
func (a *Aggregator) SetMode(m components.Mode) {
	a.mu.Lock()
	a.mode = m
	a.mu.Unlock()
}

type compStat struct {
	Runs        int64 `json:"runs"`
	Acted       int64 `json:"acted"`   // runs that actually saved tokens
	Mutated     int64 `json:"mutated"` // runs that changed the request at all (may save 0 content tokens, e.g. cacheinject)
	Reverted    int64 `json:"reverted"`
	Saved       int64 `json:"saved_tokens"`        // CUMULATIVE: summed every turn the compaction re-appears
	SavedUnique int64 `json:"saved_tokens_unique"` // UNIQUE: each distinct compaction counted once (deduped by content key)
	// OvercountRatio = Saved / SavedUnique — how many times, on average, each distinct
	// compaction was re-counted (the agent re-sends history verbatim every turn). ~1.0
	// is honest; large values mean the cumulative figure is inflated by re-sends.
	OvercountRatio float64 `json:"overcount_ratio"`
	DurationMs     float64 `json:"duration_ms"` // cumulative wall time this component spent (its own latency cost on the hot path)
	// Discarded counts changes this component made that the WRITEBACK layer then threw
	// away (bifrost could not round-trip the message, so splicing would have dropped
	// provider fields). Nonzero means the component ran, mutated, and had no effect on
	// the wire — which for two whole benchmark studies looked exactly like a working
	// Reformat (issue #32).
	Discarded int64 `json:"discarded_changes"`
	// Gates is the rejection histogram: gate name -> candidates declined by it, summed
	// over every run. It is what turns "acted: 0" into a diagnosis — whether the
	// component saw no candidates, or saw them and a specific guard refused.
	Gates    map[string]int64    `json:"gates,omitempty"`
	seenKeys map[string]struct{} // content keys already counted toward SavedUnique (not serialized)
	// pending* hold the saving credited by this component's most recent fresh report,
	// so a discard follow-up can REVERSE it (see reverseDiscarded). Not serialized.
	pendingSaved   int64
	pendingUnique  int64
	pendingChanged int64 // messages that report changed, for a proportional reversal
}

// forSnapshot returns a copy of this rollup that is safe to hand outside the
// aggregator's lock: the derived ratio filled in, the working set dropped, and the gate
// histogram DEEP-copied.
//
// It exists as one method rather than inline in each snapshot loop because it previously
// was inline, in two loops, and only one of them copied Gates — so `/stats` handed out
// the live observe histogram and raced the observe worker pool writing into it. The
// enforced rollup was correct and the hypothetical one was not, which is the failure mode
// duplicated copy logic always produces: the second copy drifts from the first, silently.
func (cs compStat) forSnapshot() compStat {
	if cs.SavedUnique > 0 {
		cs.OvercountRatio = float64(cs.Saved) / float64(cs.SavedUnique)
	}
	cs.seenKeys = nil // never serialize the working set
	if len(cs.Gates) > 0 {
		g := make(map[string]int64, len(cs.Gates))
		for k, v := range cs.Gates {
			g[k] = v
		}
		cs.Gates = g
	}
	return cs
}

// addGates merges one report's gate histogram into the rollup.
func (cs *compStat) addGates(g map[string]int) {
	if len(g) == 0 {
		return
	}
	if cs.Gates == nil {
		cs.Gates = make(map[string]int64, len(g))
	}
	for k, v := range g {
		cs.Gates[k] += int64(v)
	}
}

// NewAggregator returns an empty aggregator.
func NewAggregator() *Aggregator { return &Aggregator{perComp: map[string]*compStat{}} }

func (a *Aggregator) Component(r components.Report) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Observe mode is a HYPOTHETICAL: nothing was applied to any request. Its numbers live
	// in a physically separate map with its own vocabulary, so no aggregate over the
	// enforced rollups can reach them. Mixing them would silently inflate the product's
	// headline savings claim, which is why this is a correctness boundary rather than a
	// presentation choice (#31).
	if r.Mode == components.ModeObserve {
		a.observeComp(r)
		return
	}
	cs := a.perComp[r.Component]
	if cs == nil {
		cs = &compStat{}
		a.perComp[r.Component] = cs
	}
	// A Discarded report is a follow-up from the writeback layer attributing thrown-away
	// changes to the component that made them — not a fresh run. Count it and stop, or
	// Runs would double per request.
	if r.Discarded > 0 {
		cs.Discarded += int64(r.Discarded)
		a.reverseDiscarded(cs, int64(r.Discarded))
		return
	}
	cs.Runs++
	cs.addGates(r.Gates)
	cs.Saved += int64(r.Saved())
	cs.pendingSaved, cs.pendingChanged = int64(r.Saved()), int64(len(r.ChangedIdx))
	uniqueBefore := cs.SavedUnique
	cs.DurationMs += r.DurationMs // per-component latency cost on the hot path
	// Unique savings: dedup by the content-derived CacheKeys so the same compaction,
	// re-sent verbatim on later turns, is not re-counted. Attribute this run's saved
	// tokens proportionally to how many of its keys are NEW. Components that stash no
	// key (rare) fall back to counting the run as unique.
	if saved := int64(r.Saved()); saved > 0 && !r.Reverted && !r.Skipped {
		if len(r.CacheKeys) == 0 {
			cs.SavedUnique += saved
		} else {
			if cs.seenKeys == nil {
				cs.seenKeys = map[string]struct{}{}
			}
			newKeys := 0
			for _, k := range r.CacheKeys {
				if _, seen := cs.seenKeys[k]; !seen {
					cs.seenKeys[k] = struct{}{}
					newKeys++
				}
			}
			if newKeys > 0 {
				cs.SavedUnique += saved * int64(newKeys) / int64(len(r.CacheKeys))
			}
		}
	}
	cs.pendingUnique = cs.SavedUnique - uniqueBefore
	if r.Reverted {
		cs.Reverted++
	}
	if !r.Reverted && !r.Skipped {
		cs.Mutated++ // did something, even if it saved no content tokens
	}
	if r.Saved() > 0 && !r.Reverted && !r.Skipped {
		cs.Acted++
	}
}

// observeComp accumulates one observe-mode component report into the hypothetical
// namespace. Caller holds the lock.
// reverseDiscarded un-credits the saving of a change the WRITEBACK layer threw away.
// The discard is only known after the component has already reported, so the fresh
// report's saving is on the books by now; without this, a component whose rewrite never
// reached the wire still published its tokens as saved (issue #32 — the Discarded
// counter was wired for visibility only, so the number it existed to question stayed
// uncorrected). The wire is byte-identical to the input in that case, so the honest
// figure is zero.
//
// n is the number of DISCARDED messages charged to this component; pendingChanged is how
// many it changed. Reverse proportionally when only some were discarded. The pending
// amounts are then zeroed, so several messages discarded in one request (RecordDiscards
// sums them into ONE report per component, but a second report cannot be ruled out)
// never subtract twice.
//
// ponytail: the reversal uses the component's LAST fresh report, because the discard
// report carries only a count. Under concurrent requests that can be a different
// request's report of the same size — the rollup is a sum, so the total stays honest.
// Carrying the discarded token count on the Report (components/) would make it exact.
func (a *Aggregator) reverseDiscarded(cs *compStat, n int64) {
	share := func(v int64) int64 {
		if cs.pendingChanged > n && n > 0 {
			return v * n / cs.pendingChanged
		}
		return v
	}
	saved, unique := share(cs.pendingSaved), share(cs.pendingUnique)
	cs.Saved -= saved
	cs.SavedUnique -= unique
	// The headline saving is before−after over run reports, and it counted this rewrite
	// too. Add the discarded tokens back to `after`: they are still on the wire.
	a.after += saved
	cs.pendingSaved, cs.pendingUnique, cs.pendingChanged = 0, 0, 0
}

func (a *Aggregator) observeComp(r components.Report) {
	if a.potentialComp == nil {
		a.potentialComp = map[string]*compStat{}
	}
	cs := a.potentialComp[r.Component]
	if cs == nil {
		cs = &compStat{}
		a.potentialComp[r.Component] = cs
	}
	cs.Runs++
	cs.addGates(r.Gates)
	cs.Saved += int64(r.Saved())
	cs.DurationMs += r.DurationMs
	if saved := int64(r.Saved()); saved > 0 && !r.Reverted && !r.Skipped {
		if cs.seenKeys == nil {
			cs.seenKeys = map[string]struct{}{}
		}
		if len(r.CacheKeys) == 0 {
			cs.SavedUnique += saved
		} else {
			newKeys := 0
			for _, k := range r.CacheKeys {
				if _, seen := cs.seenKeys[k]; !seen {
					cs.seenKeys[k] = struct{}{}
					newKeys++
				}
			}
			if newKeys > 0 {
				cs.SavedUnique += saved * int64(newKeys) / int64(len(r.CacheKeys))
			}
		}
	}
	if r.Reverted {
		cs.Reverted++
	}
	if !r.Reverted && !r.Skipped {
		cs.Mutated++
	}
	if r.Saved() > 0 && !r.Reverted && !r.Skipped {
		cs.Acted++
	}
}

// RecordExpand notes that `tokens` of previously-offloaded content had to be
// re-served (the model called expand). This is the bounce signal: it means an
// offload was premature, so the honest savings figure subtracts it (lean-ctx's
// adjusted savings).
func (a *Aggregator) RecordExpand(tokens int) {
	a.mu.Lock()
	a.wasted += int64(tokens)
	a.bounces++
	a.mu.Unlock()
}

// RecordUsage adds one response's provider-billed token tiers. Called with what
// the provider reported; a response that reports nothing contributes nothing
// (never a zero-filled row that would read as "free").
func (a *Aggregator) RecordUsage(fresh, cacheRead, cacheWrite, output int64) {
	a.mu.Lock()
	a.freshInput += fresh
	a.cacheRead += cacheRead
	a.cacheWrite += cacheWrite
	a.outputTok += output
	a.mu.Unlock()
}

// RecordEligibility notes how many tokens this request's offloaders were allowed
// to touch, and how many cache-awareness froze — the numerator's honest
// denominator, and the cost of our own safety mechanism.
func (a *Aggregator) RecordEligibility(attempted, frozen int) {
	a.mu.Lock()
	a.attempted += int64(attempted)
	a.frozen += int64(frozen)
	a.mu.Unlock()
}

// RecordAddedLatency notes the wall time (ms) context-guru added to one request
// (normalize + pipeline + writeback). Only meaningful on the active path.
func (a *Aggregator) RecordAddedLatency(ms float64) {
	a.mu.Lock()
	a.addedMs += ms
	a.addedSamples++
	a.mu.Unlock()
}

// RecordUpstreamLatency notes one provider round-trip (ms), split by whether the
// request bypassed context-guru — so a run can compare with-CG vs baseline latency.
func (a *Aggregator) RecordUpstreamLatency(ms float64, bypassed bool) {
	a.mu.Lock()
	if bypassed {
		a.upstreamMsByp += ms
		a.upstreamNByp++
	} else {
		a.upstreamMs += ms
		a.upstreamN++
	}
	a.mu.Unlock()
}

// RecordSSE notes one streaming response: ms from issuing the upstream request to
// the first byte handed to the client, and whether the stream had to be fully
// buffered first (which turns TTFB into total-response time).
func (a *Aggregator) RecordSSE(ttfbMs float64, buffered bool) {
	a.mu.Lock()
	if buffered {
		a.sseTTFBMsBuf += ttfbMs
		a.sseBuffered++
	} else {
		a.sseTTFBMs += ttfbMs
		a.sseStreamed++
	}
	a.mu.Unlock()
}

func (a *Aggregator) Run(r components.RunReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Observe: hypothetical. Separate counters, separate JSON keys (potential_* /
	// projected_*), never added to requests/before/after.
	if r.Mode == components.ModeObserve {
		a.potentialRuns++
		a.potentialBefore += int64(r.TokensBefore)
		a.potentialAfter += int64(r.TokensAfter)
		a.potentialMs += r.DurationMs
		return
	}
	a.requests++
	a.before += int64(r.TokensBefore)
	a.after += int64(r.TokensAfter)
	a.syncRequests++
}

// Snapshot is the JSON served at /stats. It reports both gross savings and the
// honest, bounce-adjusted figure, plus quality signals naming components that
// never earned their place (rtk's top_passthrough idea).
type Snapshot struct {
	Requests      int64               `json:"requests"`
	TokensBefore  int64               `json:"tokens_before"`
	TokensAfter   int64               `json:"tokens_after"`
	SavedTokens   int64               `json:"saved_tokens"`
	SavingsPct    float64             `json:"savings_pct"`
	WastedTokens  int64               `json:"wasted_tokens"`  // re-served via expand
	Bounces       int64               `json:"bounces"`        // expand events
	AdjustedSaved int64               `json:"adjusted_saved"` // saved - wasted (may be negative)
	Components    map[string]compStat `json:"components"`
	// TopPassthrough names components that ran but never saved a token — dead
	// weight in the pipeline, candidates to drop from the config.
	TopPassthrough []string `json:"top_passthrough"`
	// TopDiscarded names components whose changes the writeback layer threw away at
	// least once — they mutated the request but (for those changes) never reached the
	// wire. Any entry here needs investigating; see the per-component
	// `discarded_changes` for the count.
	TopDiscarded []string `json:"top_discarded"`
	// LLM* report the cheap (config-source) model usage the CONTEXT-GURU components
	// themselves incurred (e.g. extract:code's Starlark-writer calls) — the CG
	// components' OWN cost, separate from the agent. Priced externally.
	LLMCalls        int64 `json:"llm_calls"`
	LLMInputTokens  int64 `json:"llm_input_tokens"`
	LLMOutputTokens int64 `json:"llm_output_tokens"`
	// LLMTimeouts/LLMErrors make the FAIL-OPEN PATH VISIBLE. A component that
	// abandons its model call leaves the output verbatim (correct — compaction must
	// never break the agent's request) but reports nothing, so an arm that quietly
	// stops compacting under load looks like an arm that got faster. MEASURED on a
	// KV-pressured on-prem server: llm_calls fell 2,093 -> 255 at equal request
	// volume while per-request overhead ROSE 55%, and the resulting "42-point
	// improvement" was the treatment partially switching itself off.
	//
	// Read them together with LLMCallTimeoutMs: a non-zero timeout count means the
	// budget is too small for the server's current load, and this arm's savings are
	// an UNDERCOUNT rather than a measurement. Filled by the host at serve time
	// (offload owns the deadline and lives below metrics), same as the Frozen* fields.
	LLMTimeouts      int64 `json:"llm_timeouts"`
	LLMErrors        int64 `json:"llm_errors"`
	LLMCallTimeoutMs int64 `json:"llm_call_timeout_ms"`
	// Summarize* are the same three figures for `summarize`, which owns a SEPARATE
	// budget (its call is one big span, not one tool output, so the two cannot share a
	// ceiling). Reported separately rather than folded into LLM* above because the
	// components run in different arms: a summarize-only pipeline would otherwise
	// report llm_timeouts 0 with its own deadline expiring on every request.
	//
	// summarize's failure is already visible as a per-component `reverted`, but that
	// cannot distinguish "budget too small for this load" (savings are an undercount)
	// from "the model call is failing" (the arm is not measuring summarization at all).
	SummarizeTimeouts      int64 `json:"summarize_timeouts"`
	SummarizeErrors        int64 `json:"summarize_errors"`
	SummarizeCallTimeoutMs int64 `json:"summarize_call_timeout_ms"`
	// Extract is extract_llm's own economics (#28 part F), including NET savings after
	// its LLM cost — the honest headline for the one component that spends to save.
	// Purely ADDITIVE: no field above was renamed or removed, so deploy/harbor/*.py
	// keeps parsing /stats unchanged.
	Extract *ExtractStats `json:"extract,omitempty"`
	// End-to-end latency (W7): mean ms context-guru added per request, and mean
	// provider round-trip on the active vs bypassed (baseline) path — a with/without
	// context-guru session-latency comparison.
	AddedLatencyMsAvg     float64 `json:"cg_added_ms_avg"`
	UpstreamMsAvg         float64 `json:"upstream_ms_avg"`
	UpstreamMsAvgBypassed float64 `json:"upstream_ms_avg_bypassed"`
	// SSE streaming health (#26). SSEBuffered counts streams context-guru had to read
	// in full before the client saw a byte (to look for an expand tool call); those
	// requests lose streaming entirely, so their TTFB is reported separately. A high
	// buffered share on traffic that never expands is the regression to watch. All four
	// count once per CLIENT REQUEST, not per upstream round: a request that drove
	// several expand rounds waited for all of them.
	SSEStreamed  int64   `json:"sse_streamed"`
	SSEBuffered  int64   `json:"sse_buffered"`
	SSETTFBMsAvg float64 `json:"sse_ttfb_ms_avg"` // streamed-through requests: a real TTFB
	// SSETTFBMsAvgBuf is time-to-LAST-byte by construction, not a comparable TTFB: a
	// buffered response is read in full before the client is written to, so its first
	// byte cannot precede the buffer completing. Read it as "what buffering cost these
	// requests", not as a latency to compare against sse_ttfb_ms_avg.
	SSETTFBMsAvgBuf float64 `json:"sse_ttfb_ms_avg_buffered"`
	SSEBufferedPct  float64 `json:"sse_buffered_pct"`
	// Freeze-replay health — the cache-WRITE cost line. A frozen decision replayed
	// (frozen_hits) keeps an already-cached message byte-identical. A decision the store
	// DROPS (frozen_dropped: TTL expiry / pin cap) would flip that message's
	// representation and force the provider to re-write the whole suffix at 11.5x the
	// read price — unless it is re-derived (frozen_repaired). frozen_flips =
	// dropped − repaired is the count that actually cost cache-writes; it should be 0.
	// frozen_misses counts every replay lookup that found nothing, which is dominated by
	// the normal "never frozen yet" case — read it beside frozen_dropped, not instead.
	// Filled by the host at serve time (offload + store live below metrics).
	FrozenHits     int64 `json:"frozen_hits"`
	FrozenMisses   int64 `json:"frozen_misses"`
	FrozenDropped  int64 `json:"frozen_dropped"`
	FrozenRepaired int64 `json:"frozen_repaired"`
	FrozenFlips    int64 `json:"frozen_flips"`
	// CompactionResets counts turns whose cached-prefix boundary restarted because the
	// AGENT compacted its own transcript (it shrank under a stable session id). The
	// session id deliberately survives that compaction so one conversation is one
	// session in the dashboard — which means the boundary is the only thing left that can
	// notice, and if it does not, every message of every later turn is treated as already
	// cached and no component can act for the rest of the session. So this is the counter
	// that says "these sessions restarted their prefix N times"; a long run with real
	// auto-compaction should be non-zero, and a run where it stays 0 while savings fall
	// off a cliff mid-session is the regression it exists to expose. Filled by the host at
	// serve time (the counter lives in `modes`, which metrics cannot import).
	CompactionResets int64 `json:"compaction_resets"`
	// cmdfilter attribution: which command FAMILIES pay off (builds/tests/iac/pkg/net),
	// which individual filters fire, and which output shapes matched no filter (the
	// backlog of filters worth writing). Additive fields — nothing above is renamed.
	CmdfilterFamilies map[string]filterStat `json:"cmdfilter_families,omitempty"`
	CmdfilterFilters  map[string]filterStat `json:"cmdfilter_filters,omitempty"`
	CmdfilterMisses   []SelectorMiss        `json:"cmdfilter_selector_misses,omitempty"`

	// --- Operating mode (#31). Everything below is ADDITIVE: no existing key was renamed
	// or removed, because deploy/harbor/*.py parses this payload. ---

	// Mode is the configured operating mode ("sync" | "observe").
	Mode string `json:"mode"`
	// SyncEnforced counts requests whose forwarded body context-guru actually shaped. In
	// observe mode it is 0 BY CONSTRUCTION — that is the machine-readable form of
	// "context-guru did not modify requests".
	SyncEnforced int64 `json:"sync_enforced"`

	// Observe mode: HYPOTHETICALS. Distinct keys (potential_* / projected_*) that never
	// share a name with an enforced metric, so a consumer cannot sum a hypothetical into a
	// real saving even by accident. All zero outside observe mode.
	ObserveNotice            string              `json:"observe_notice,omitempty"`
	ObserveRequests          int64               `json:"observe_hypothetical_requests"`
	ActualBaselineTokens     int64               `json:"actual_baseline_tokens"`     // what the agent really sent
	ProjectedOptimizedTokens int64               `json:"projected_optimized_tokens"` // what it would have sent
	PotentialSavedTokens     int64               `json:"potential_saved_tokens"`
	PotentialSavingsPct      float64             `json:"potential_savings_pct"`
	PotentialComponents      map[string]compStat `json:"potential_components,omitempty"`
	// PotentialOverheadMsAvg is the mean wall time a compaction WOULD have added to each
	// request had this mode been enforcing — measured off-path, so it is what sync would
	// cost, not what observe costs.
	PotentialOverheadMsAvg float64 `json:"potential_overhead_ms_avg"`
	// ObserveLLMNotice warns that context-guru's own model spend (llm_calls /
	// llm_input_tokens / llm_output_tokens, which feed cg_llm_cost in the harnesses) is
	// OFF-PATH measurement cost in observe mode, not the cost of an enforced compaction.
	// The tokens were really spent — the number is not hypothetical and must not be moved
	// into the potential_* namespace — but attributing it to enforcement would be wrong.
	// cg_added_ms_avg is likewise a real measurement of the enforced path, and in observe
	// mode it correctly reads ~0 because that path does no pipeline work. Zeroing either
	// would hide a true number rather than protect anyone.
	ObserveLLMNotice string `json:"observe_llm_notice,omitempty"`

	// ObserveQueue is the off-path worker pool's counter tuple, filled by the host at
	// serve time (the pool lives in `modes`, which sits above this package). All five
	// counters are exposed rather than just `queued` because `dropped` is the one that
	// changes a reader's conclusion: a drop is an observation silently given up, so a
	// rising `dropped` means the potential_*/projected_* figures UNDERSTATE what
	// compaction would have saved. Reporting the queue depth while hiding the drops is
	// exactly the gap noted in headroom's dashboard. Omitted when no pool is running, so
	// a sync-only deployment shows no phantom queue.
	ObserveQueue *QueueStats `json:"observe_queue,omitempty"`

	// Provider-billed token tiers (W8), summed from response usage. ADDITIVE: the
	// benchmark harnesses parse this payload, so fields are only ever added here,
	// never renamed or removed (see the golden shape test).
	FreshInputTokens int64 `json:"fresh_input_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	// AttemptedTokens is what compaction was ALLOWED to touch; FrozenTokens is what
	// cache-awareness deliberately left alone. SavingsPctAttempted divides savings
	// by the former — the honest ratio, since SavingsPct's whole-request denominator
	// recounts the transcript every turn and trends to ~0% on a long session.
	AttemptedTokens int64 `json:"attempted_tokens"`
	FrozenTokens    int64 `json:"frozen_tokens"`
	// SavingsPctAttempted = saved / attempted. 0 when nothing was attempted.
	SavingsPctAttempted float64 `json:"savings_pct_attempted"`
	// SavingsPctNewInput = saved / (fresh + cache_write + saved): savings as a
	// fraction of what would have newly entered the provider. 0 (not 100) when the
	// provider reported no usage — savings must never be divided by themselves.
	SavingsPctNewInput float64 `json:"savings_pct_new_input"`
}

// QueueStats mirrors modes.Stats. Declared here as a plain struct rather than importing
// modes because the dependency runs the other way (modes uses metrics, not vice versa).
type QueueStats struct {
	Queued    int64 `json:"queued"`
	Pending   int64 `json:"pending"`
	Processed int64 `json:"processed"`
	Dropped   int64 `json:"dropped"`
	Errors    int64 `json:"errors"`
}

// SelectorMiss is one output shape that matched no filter, with how often it appeared.
type SelectorMiss struct {
	Selector string `json:"selector"`
	Count    int64  `json:"count"`
}

// observeNotice is the machine- and human-readable banner: in observe mode nothing was
// applied, and every number prefixed potential_/projected_ is a hypothetical.
const observeNotice = "OBSERVE MODE: context-guru did not modify any request. " +
	"Every potential_*/projected_* field is a hypothetical, not a realized saving."

// observeLLMNotice covers the one place observe legitimately writes an enforced-namespace
// key: its own model spend is real money, so it stays where cost tooling already reads it,
// labelled for what it is.
const observeLLMNotice = "In observe mode llm_calls/llm_input_tokens/llm_output_tokens " +
	"are the cost of MEASURING off-path, not of enforcing a compaction. The spend is real " +
	"(not hypothetical); it simply bought a projection rather than a saving."

// Snapshot returns a point-in-time copy of the rollups.
func (a *Aggregator) Snapshot() Snapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	saved := a.before - a.after
	var pct float64
	if a.before > 0 {
		pct = float64(saved) / float64(a.before) * 100
	}
	comps := make(map[string]compStat, len(a.perComp))
	var passthrough, discarded []string
	for k, v := range a.perComp {
		if v.Discarded > 0 {
			discarded = append(discarded, k)
		}
		// forSnapshot, not an inline copy: the snapshot outlives this lock and is
		// marshalled by the caller, so any live map handed out races the next Component().
		comps[k] = v.forSnapshot()
		// Dead weight = ran but never changed the request at all (always skipped
		// or reverted). A component that mutated but saved no content tokens (e.g.
		// cacheinject adds provider cache_control) is NOT passthrough.
		if v.Runs > 0 && v.Mutated == 0 {
			passthrough = append(passthrough, k)
		}
	}
	sort.Strings(passthrough)
	sort.Strings(discarded)
	addedAvg, upAvg, upAvgByp := 0.0, 0.0, 0.0
	if a.addedSamples > 0 {
		addedAvg = a.addedMs / float64(a.addedSamples)
	}
	if a.upstreamN > 0 {
		upAvg = a.upstreamMs / float64(a.upstreamN)
	}
	if a.upstreamNByp > 0 {
		upAvgByp = a.upstreamMsByp / float64(a.upstreamNByp)
	}
	ttfb, ttfbBuf, bufPct := 0.0, 0.0, 0.0
	if a.sseStreamed > 0 {
		ttfb = a.sseTTFBMs / float64(a.sseStreamed)
	}
	if a.sseBuffered > 0 {
		ttfbBuf = a.sseTTFBMsBuf / float64(a.sseBuffered)
	}
	if n := a.sseStreamed + a.sseBuffered; n > 0 {
		bufPct = float64(a.sseBuffered) / float64(n) * 100
	}
	mode := a.mode
	if mode == "" {
		mode = components.ModeSync
	}
	attemptedPct := 0.0
	if a.attempted > 0 {
		attemptedPct = float64(saved) / float64(a.attempted) * 100
	}
	// Guard on the BILLED figure, not the sum: with no usage data the denominator
	// would be `saved` alone and the ratio would read ~100%. No data => report 0.
	newInputPct := 0.0
	if a.freshInput+a.cacheWrite > 0 {
		newInputPct = float64(saved) / float64(a.freshInput+a.cacheWrite+saved) * 100
	}
	snap := Snapshot{
		Requests: a.requests, TokensBefore: a.before, TokensAfter: a.after,
		SavedTokens: saved, SavingsPct: pct,
		WastedTokens: a.wasted, Bounces: a.bounces, AdjustedSaved: saved - a.wasted,
		Components: comps, TopPassthrough: passthrough, TopDiscarded: discarded,
		AddedLatencyMsAvg: addedAvg, UpstreamMsAvg: upAvg, UpstreamMsAvgBypassed: upAvgByp,
		SSEStreamed: a.sseStreamed, SSEBuffered: a.sseBuffered,
		SSETTFBMsAvg: ttfb, SSETTFBMsAvgBuf: ttfbBuf, SSEBufferedPct: bufPct,
		CmdfilterFamilies: copyFilterStats(a.filterFam),
		CmdfilterFilters:  copyFilterStats(a.filterName),
		CmdfilterMisses:   topMisses(a.filterMiss, 20),
		Mode:              string(mode),
		SyncEnforced:      a.syncRequests,
		FreshInputTokens:  a.freshInput, CacheReadTokens: a.cacheRead,
		CacheWriteTokens: a.cacheWrite, OutputTokens: a.outputTok,
		AttemptedTokens: a.attempted, FrozenTokens: a.frozen,
		SavingsPctAttempted: attemptedPct, SavingsPctNewInput: newInputPct,
	}
	if a.potentialRuns > 0 || mode == components.ModeObserve {
		snap.ObserveNotice = observeNotice
		snap.ObserveLLMNotice = observeLLMNotice
		snap.ObserveRequests = a.potentialRuns
		snap.ActualBaselineTokens = a.potentialBefore
		snap.ProjectedOptimizedTokens = a.potentialAfter
		snap.PotentialSavedTokens = a.potentialBefore - a.potentialAfter
		if a.potentialBefore > 0 {
			snap.PotentialSavingsPct = float64(a.potentialBefore-a.potentialAfter) / float64(a.potentialBefore) * 100
		}
		if a.potentialRuns > 0 {
			snap.PotentialOverheadMsAvg = a.potentialMs / float64(a.potentialRuns)
		}
		if len(a.potentialComp) > 0 {
			pc := make(map[string]compStat, len(a.potentialComp))
			for k, v := range a.potentialComp {
				pc[k] = v.forSnapshot()
			}
			snap.PotentialComponents = pc
		}
	}
	return snap
}

func copyFilterStats(src map[string]*filterStat) map[string]filterStat {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]filterStat, len(src))
	for k, v := range src {
		fs := *v
		fs.seenKeys = nil // don't serialize the working set
		out[k] = fs
	}
	return out
}

// topMisses returns the n most frequent unmatched selectors, descending (ties by
// selector, so the output is deterministic).
func topMisses(src map[string]int64, n int) []SelectorMiss {
	if len(src) == 0 {
		return nil
	}
	out := make([]SelectorMiss, 0, len(src))
	for s, c := range src {
		out = append(out, SelectorMiss{Selector: s, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Selector < out[j].Selector
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
