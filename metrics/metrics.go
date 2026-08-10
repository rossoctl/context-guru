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

// Slog logs each component and run in the GenAI semantic-convention vocabulary.
type Slog struct{ L *slog.Logger }

func (s Slog) logger() *slog.Logger {
	if s.L != nil {
		return s.L
	}
	return slog.Default()
}

func (s Slog) Component(r components.Report) {
	s.logger().Info("context_engineering.component",
		"context_engineering.component", r.Component,
		"context_engineering.kind", r.Kind,
		"context_engineering.tokens.before", r.TokensBefore,
		"context_engineering.tokens.after", r.TokensAfter,
		"context_engineering.tokens.saved", r.Saved(),
		"context_engineering.reverted", r.Reverted,
		"context_engineering.duration_ms", r.DurationMs,
	)
}

func (s Slog) Run(r components.RunReport) {
	s.logger().Info("context_engineering.run",
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
	// Mode dimension (#31). Enforced requests are split by mode so a run can tell
	// which path produced the savings above; observe-mode results are kept in
	// PHYSICALLY separate fields with their own serialized names, so no query over the
	// enforced rollups can accidentally include a hypothetical.
	mode            components.Mode // the configured mode, for the /stats banner
	syncRequests    int64
	asyncRequests   int64
	deferredRuns    int64   // off-path async compactions that produced a committed result
	deferredMs      float64 // wall time spent off the request path
	realizedSaved   int64   // tokens saved on-path by replaying a previously deferred compaction
	tailUnprotected int64   // turns where deferral was declined to protect the cache
	potentialRuns   int64
	potentialBefore int64
	potentialAfter  int64
	potentialMs     float64
	potentialComp   map[string]*compStat
	// asyncStats is a snapshot function for the pool's counter tuple, injected by the
	// host so metrics keeps no dependency on the modes package's lifecycle.
	asyncStats func() any
}

// SetMode records the configured operating mode so /stats can label itself — the
// observe-mode banner has to be unmistakable, and a consumer needs to know from the
// payload alone whether the numbers were enforced.
func (a *Aggregator) SetMode(m components.Mode) {
	a.mu.Lock()
	a.mode = m
	a.mu.Unlock()
}

// SetAsyncStats installs a snapshot function for the async queue counters.
func (a *Aggregator) SetAsyncStats(fn func() any) {
	a.mu.Lock()
	a.asyncStats = fn
	a.mu.Unlock()
}

// RecordDeferred notes one completed off-path compaction: whether its result was
// committed, and how long it took (time NOT charged to any request).
func (a *Aggregator) RecordDeferred(ms float64, committed bool) {
	a.mu.Lock()
	a.deferredMs += ms
	if committed {
		a.deferredRuns++
	}
	a.mu.Unlock()
}

// RecordTailUnprotected notes a turn where async declined to defer because the caller
// had cache-written the tail a compaction would have replaced and stripping its
// breakpoint was not permitted. Deferring anyway would cost a 1.25x rewrite of a span
// the provider committed to. A high count means async is doing nothing on this workload
// and async.strip_caller_breakpoints is the knob to consider.
func (a *Aggregator) RecordTailUnprotected() {
	a.mu.Lock()
	a.tailUnprotected++
	a.mu.Unlock()
}

// RecordRealized notes tokens saved on the request path by replaying a compaction an
// EARLIER turn computed off-path. Gated by the caller on the session having had a
// compaction actually land — otherwise it would just re-report the inline saving.
func (a *Aggregator) RecordRealized(tokens int) {
	if tokens <= 0 {
		return
	}
	a.mu.Lock()
	a.realizedSaved += int64(tokens)
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
	OvercountRatio float64             `json:"overcount_ratio"`
	DurationMs     float64             `json:"duration_ms"` // cumulative wall time this component spent (its own latency cost on the hot path)
	seenKeys       map[string]struct{} // content keys already counted toward SavedUnique (not serialized)
}

// NewAggregator returns an empty aggregator.
func NewAggregator() *Aggregator { return &Aggregator{perComp: map[string]*compStat{}} }

func (a *Aggregator) Component(r components.Report) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Observe mode is a HYPOTHETICAL: nothing was applied to any request. Its numbers
	// live in a physically separate map with its own vocabulary, so no aggregate over
	// the enforced rollups can reach them. Mixing them would silently inflate the
	// product's headline savings claim, which is why this is a correctness boundary
	// rather than a presentation choice (#31).
	if r.Mode == components.ModeObserve {
		a.observeComp(r)
		return
	}
	// An off-path async run forwarded nothing. Its savings are counted when a later
	// turn REPLAYS the frozen decision on the request path (RecordRealized), so
	// counting them here too would double-count every deferred compaction — and would
	// credit savings to a request that never carried them. The deferred work itself is
	// visible as async_deferred_runs / async_deferred_ms_total and the queue tuple.
	if r.Deferred {
		return
	}
	cs := a.perComp[r.Component]
	if cs == nil {
		cs = &compStat{}
		a.perComp[r.Component] = cs
	}
	cs.Runs++
	cs.Saved += int64(r.Saved())
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

func (a *Aggregator) Run(r components.RunReport) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Observe: hypothetical. Separate counters, separate JSON keys (potential_* /
	// projected_*), never added to requests/before/after.
	if r.Deferred {
		return // off-path: nothing was forwarded (see Component)
	}
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
	switch r.Mode {
	case components.ModeAsync:
		a.asyncRequests++
	default:
		a.syncRequests++
	}
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
	// LLM* report the cheap (config-source) model usage the CONTEXT-GURU components
	// themselves incurred (e.g. extract:code's Starlark-writer calls) — the CG
	// components' OWN cost, separate from the agent. Priced externally.
	LLMCalls        int64 `json:"llm_calls"`
	LLMInputTokens  int64 `json:"llm_input_tokens"`
	LLMOutputTokens int64 `json:"llm_output_tokens"`
	// End-to-end latency (W7): mean ms context-guru added per request, and mean
	// provider round-trip on the active vs bypassed (baseline) path — a with/without
	// context-guru session-latency comparison.
	AddedLatencyMsAvg     float64 `json:"cg_added_ms_avg"`
	UpstreamMsAvg         float64 `json:"upstream_ms_avg"`
	UpstreamMsAvgBypassed float64 `json:"upstream_ms_avg_bypassed"`

	// --- Operating mode (#31). Everything below is ADDITIVE: no existing key was
	// renamed or removed, because deploy/harbor/*.py parses this payload. ---

	// Mode is the configured operating mode ("sync" | "async" | "observe").
	Mode string `json:"mode"`
	// Enforced counts requests whose forwarded body context-guru actually shaped,
	// split by which mode produced it. In observe mode both are 0 BY CONSTRUCTION —
	// that is the machine-readable form of "context-guru did not modify requests".
	SyncEnforced  int64 `json:"sync_enforced"`
	AsyncEnforced int64 `json:"async_enforced"`

	// ObserveLLMNotice warns that context-guru's own model spend (llm_calls /
	// llm_input_tokens / llm_output_tokens, which feed cg_llm_cost in the harnesses) is
	// OFF-PATH measurement cost in observe mode, not the cost of an enforced compaction.
	// The tokens were really spent — the number is not hypothetical and must not be moved
	// into the potential_* namespace — but attributing it to enforcement would be wrong.
	// cg_added_ms_avg is likewise a real measurement of the enforced path, and in observe
	// mode it correctly reads ~0 because that path does no pipeline work.
	ObserveLLMNotice string `json:"observe_llm_notice,omitempty"`

	// Async: the full queue counter tuple (queued/pending/processed/dropped/errors/
	// stale_discarded) plus the deferred-work accounting. `dropped` and
	// `stale_discarded` are the "we silently gave up savings" counters and are
	// surfaced deliberately — headroom exposes only `queued`, which hides them.
	AsyncQueue          any     `json:"async_queue,omitempty"`
	DeferredRuns        int64   `json:"async_deferred_runs"`
	DeferredMsTotal     float64 `json:"async_deferred_ms_total"`
	RealizedSavedTokens int64   `json:"async_realized_saved_tokens"`
	// TailUnprotectedTurns counts turns async declined to defer because the caller had
	// already cache-written the span a compaction would replace. Non-zero means async is
	// largely inert here; see async.strip_caller_breakpoints.
	TailUnprotectedTurns int64 `json:"async_tail_unprotected_turns"`

	// Observe mode: HYPOTHETICALS. Distinct keys (potential_* / projected_*) that
	// never share a name with an enforced metric, so a consumer cannot sum a
	// hypothetical into a real saving even by accident. All zero outside observe mode.
	ObserveNotice            string              `json:"observe_notice,omitempty"`
	ObserveRequests          int64               `json:"observe_hypothetical_requests"`
	ActualBaselineTokens     int64               `json:"actual_baseline_tokens"`     // what the agent really sent
	ProjectedOptimizedTokens int64               `json:"projected_optimized_tokens"` // what it would have sent
	PotentialSavedTokens     int64               `json:"potential_saved_tokens"`
	PotentialSavingsPct      float64             `json:"potential_savings_pct"`
	PotentialComponents      map[string]compStat `json:"potential_components,omitempty"`
	// PotentialOverheadMsAvg is the mean wall time a compaction WOULD have added to
	// each request had this mode been enforcing — measured off-path, so it is what
	// sync would cost, not what observe costs.
	PotentialOverheadMsAvg float64 `json:"potential_overhead_ms_avg"`
}

// observeNotice is the machine- and human-readable banner: in observe mode nothing
// was applied, and every number prefixed potential_/projected_ is a hypothetical.
const observeNotice = "OBSERVE MODE: context-guru did not modify any request. " +
	"Every potential_*/projected_* field is a hypothetical, not a realized saving."

// observeLLMNotice covers the one place observe legitimately writes an enforced-namespace
// key: its own model spend is real money, so it stays where cost tooling already reads
// it, labelled for what it is.
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
	var passthrough []string
	for k, v := range a.perComp {
		cs := *v
		if cs.SavedUnique > 0 {
			cs.OvercountRatio = float64(cs.Saved) / float64(cs.SavedUnique)
		}
		cs.seenKeys = nil // don't serialize the working set
		comps[k] = cs
		// Dead weight = ran but never changed the request at all (always skipped
		// or reverted). A component that mutated but saved no content tokens (e.g.
		// cacheinject adds provider cache_control) is NOT passthrough.
		if v.Runs > 0 && v.Mutated == 0 {
			passthrough = append(passthrough, k)
		}
	}
	sort.Strings(passthrough)
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
	mode := a.mode
	if mode == "" {
		mode = components.ModeSync
	}
	snap := Snapshot{
		Requests: a.requests, TokensBefore: a.before, TokensAfter: a.after,
		SavedTokens: saved, SavingsPct: pct,
		WastedTokens: a.wasted, Bounces: a.bounces, AdjustedSaved: saved - a.wasted,
		Components: comps, TopPassthrough: passthrough,
		AddedLatencyMsAvg: addedAvg, UpstreamMsAvg: upAvg, UpstreamMsAvgBypassed: upAvgByp,
		Mode:          string(mode),
		SyncEnforced:  a.syncRequests,
		AsyncEnforced: a.asyncRequests,
		DeferredRuns:  a.deferredRuns, DeferredMsTotal: a.deferredMs,
		RealizedSavedTokens:  a.realizedSaved,
		TailUnprotectedTurns: a.tailUnprotected,
	}
	if a.asyncStats != nil {
		snap.AsyncQueue = a.asyncStats()
	}
	if a.potentialRuns > 0 || mode == components.ModeObserve {
		snap.ObserveNotice = observeNotice
		if snap.LLMCalls > 0 || mode == components.ModeObserve {
			snap.ObserveLLMNotice = observeLLMNotice
		}
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
				cs := *v
				if cs.SavedUnique > 0 {
					cs.OvercountRatio = float64(cs.Saved) / float64(cs.SavedUnique)
				}
				cs.seenKeys = nil
				pc[k] = cs
			}
			snap.PotentialComponents = pc
		}
	}
	return snap
}
