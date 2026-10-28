package keepalive

import (
	"log/slog"
	"math"
	"time"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/internal/waitcause"
)

// cachePhaseAt classifies the cache phase of an entry last touched at prevStartedAt, for a
// `cache_state: pre_expiry` keep-alive-substitute candidate to check against (see
// fireSummarySubstitute) — the keeper's own "due for a ping" timing is a DIFFERENT question from
// "is this candidate's configured pre-expiry window live right now", and the two can diverge (a
// manager strategy's K>1 schedule, or a component configured with a narrower pre_expiry_seconds
// than the keeper's own Idle).
//
// prevStartedAt, NOT e.startedAt: by the time fire() runs, sweep has already overwritten
// e.startedAt with this ping's own (not yet sent) start — reading it here would answer "how long
// until THIS ping's refresh expires" (always nearly the full TTL) instead of "how close was the
// ENTRY to expiring before this ping". See pingJob.prevStartedAt.
//
// Deliberately NOT Ctx.CachePhase itself: that function also handles an unknown idle time, which
// does not arise here — a live kaEntry always knows its own idle time exactly (now - prevStartedAt,
// never "unknown"). It also handles CacheTTLMinimum, which DOES reach this function: OpenAI's
// 30 minutes is a minimum. Ctx.CachePhase never reports PreExpiry for a minimum; this function does,
// because the keeper knows when it pings and only asks whether that ping is close to the end.
//
// ttl is the entry's cache lifetime, from cacheLifetime: 30 minutes on an OpenAI thread, which an
// earlier version of this function treated as 5, so at 28 minutes it read Cold and a pre_expiry
// substitute never ran there (#715).
func cachePhaseAt(prevStartedAt, now time.Time, ttl, preExpiry time.Duration) components.CachePhase {
	// Past the lifetime this reads Cold. On OpenAI the 30 minutes is a minimum, so that is not proof
	// of expiry; the only caller compares against PreExpiry, so Cold and Unknown act the same there.
	remaining := ttl - now.Sub(prevStartedAt)
	if remaining <= 0 {
		return components.CachePhaseCold
	}
	if remaining <= preExpiry {
		return components.CachePhasePreExpiry
	}
	return components.CachePhaseWarm
}

// preExpiryFor resolves a candidate's configured pre_expiry_seconds (0 = the component's own
// default), mirroring components.Trigger.PreExpiry without needing a Trigger value here.
func preExpiryFor(seconds int) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return components.DefaultPreExpiry
}

// substituteWindow is the pre-expiry window the substitute's phase check uses for this entry. An
// operator's own pre_expiry_seconds is kept as set. The DEFAULT is widened to the keeper's own
// slack (the cache lifetime minus its idle period) plus two sweep ticks, because the keeper pings
// exactly that far before expiry: a window narrower than that reads every ping as Warm, and a
// pre_expiry substitute never takes one (#715, #717 review). On Anthropic at the defaults the slack
// is 300 s - 240 s = 60 s, so the window is 60 s + 2 ticks = 64 s. On OpenAI it is
// 30 min - 28 min = 120 s, so the window becomes 124 s.
//
// CAPPED at maxSubstituteWindow. The slack of a SHORT schedule is large — a 60 s idle on a
// 5-minute cache leaves 240 s, a 1-hour ask that was not granted leaves about 55 minutes — and an
// uncapped window would then cover the whole idle span, so the substitute would take every ping
// from the first, minutes before expiry, which is what pre_expiry exists to refuse (#717 review).
func substituteWindow(seconds int, ttl, idle time.Duration) time.Duration {
	w := preExpiryFor(seconds)
	if seconds > 0 {
		return w
	}
	if slack := min(ttl-idle+2*tick, maxSubstituteWindow); slack > w {
		return slack
	}
	return w
}

// maxSubstituteWindow bounds the widened default window: twice the component's 60 s default plus
// two 2 s sweep ticks, 124 s. That still covers every schedule this repo ships, the widest being
// OpenAI's 120 s slack (modelinfo.OpenAIMinimumCacheTTL - OpenAIDefaultKeepAliveIdle) plus the
// same two ticks.
const maxSubstituteWindow = 2*components.DefaultPreExpiry + 2*tick

// fireSummarySubstitute tries to replace this due ping with cache_aware_summarizer's own
// cache-reading call. Returns true when it did — the caller must then not also send an ordinary
// ping. False covers every reason not to, and the caller's existing ping flow is the fallback for
// all of them without needing to know which: no material is registered for this session (the
// component is not in this tenant's pipeline, or no turn has reached the registration point —
// see offload.KeepAliveSubstitute), the registry's own "a checkpoint already exists" refusal, a
// projected cost over the keep-alive $ cap, or the call itself declining, timing out, erroring,
// or coming back empty.
//
// Fail open throughout, by construction: nothing here can leave this ping unset for the idle
// span it was due for, because every "no" falls through to the code fire() already runs.
func (k *Keeper) fireSummarySubstitute(j pingJob) bool {
	// THIS thread's candidate (#423): the registry is keyed by session + thread, because a
	// candidate's span is one thread's conversation and the ping refreshes one thread's prefix.
	dispatch, info, reason, ok := offload.KeepAliveSubstitute(thread.Key(j.session, j.thread))
	if !ok {
		// WAS silent — "no candidate" (the ordinary case for most sessions) and "a candidate
		// existed but a checkpoint already covers it" (routine, but a DIFFERENT fact) used to be
		// one indistinguishable false. Counted separately so a session that SHOULD have a
		// candidate and does not (cache_aware_summarizer is in the pipeline, turns are declining
		// with a cache_state gate that implies registration — see Offload's own comment on
		// registering unconditionally) is diagnosable instead of looking identical to a session
		// the component never touched at all.
		switch reason {
		case offload.KeepAliveReasonCheckpointExists:
			k.summarySubstituteCheckpointExists.Add(1)
		default:
			k.summarySubstituteNoCandidate.Add(1)
		}
		slog.Debug("context-guru: cache_aware_summarizer keep-alive substitute not offered",
			"tenant", tenantLabel(j.tenant), "session", j.session, "reason", string(reason))
		return false
	}
	e := j.e
	k.mu.Lock()
	model, prefix, ceiling, ttl1h := e.model, e.prefix, e.pol.Ceiling(), e.ttl1h || e.pol.HeadTTL1h
	endpoint, ttl, idle := e.endpoint, cacheLifetime(e.provider, ttl1h), e.pol.Idle
	k.mu.Unlock()
	phase := cachePhaseAt(j.prevStartedAt, k.now(), ttl, substituteWindow(info.PreExpirySeconds, ttl, idle))
	// A `pre_expiry` candidate asked to spend only when ITS OWN notion of the pre-expiry window
	// is live, not merely whenever the keeper happens to be sending a ping — the two can diverge
	// (a manager strategy's K>1 schedule, a short idle override, or a component configured with a
	// narrower pre_expiry_seconds than the keeper's own Idle). With pre_expiry_seconds unset the
	// window is widened to the keeper's slack, but only up to maxSubstituteWindow, so a short
	// schedule still reads its early pings as Warm (substituteWindow). `any` has no such condition. Computed by
	// cachePhaseAt from this entry's own clock and cache lifetime, not approximated from timing
	// alone. It differs from Ctx.CachePhase on OpenAI on purpose: that one treats the 30 minutes as
	// a minimum and never reports PreExpiry there, while the keeper knows exactly when it pings.
	if info.CacheState == components.CacheStatePreExpiry && phase != components.CachePhasePreExpiry {
		k.summarySubstitutePhaseMismatch.Add(1)
		return false
	}
	// Respect the keep-alive $ cap on a PROJECTION, before spending anything.
	cost := k.projectedSummaryUSD(model, endpoint, prefix)
	if cost > ceiling {
		k.summarySubstituteOverBudget.Add(1)
		slog.Debug("context-guru: cache_aware_summarizer keep-alive substitute would exceed the "+
			"per-ping cost cap; pinging normally instead",
			"tenant", tenantLabel(j.tenant), "session", j.session,
			"estimate_usd", cost, "ceiling_usd", ceiling)
		return false
	}
	// REFRESHING an existing reserve (as opposed to creating a session's first one) only earns
	// back its own cost when the material it folds in is worth more than the call itself — see
	// issue #415. Creating the first reserve has no such test: it is compared against "no reserve
	// at all" (the full-rewrite cold return this whole mechanism exists to avoid), not against a
	// slightly smaller one, so info.IsRefresh gates this block to the refresh case alone.
	if info.IsRefresh {
		saving := k.expectedRefreshSavingUSD(model, endpoint, prefix, info.RefreshTailTokens)
		if saving < cost {
			k.summarySubstituteRefreshNotWorthIt.Add(1)
			slog.Debug("context-guru: cache_aware_summarizer refresh would cost more than it saves; "+
				"pinging normally instead",
				"tenant", tenantLabel(j.tenant), "session", j.session,
				"tail_tokens", info.RefreshTailTokens, "expected_saving_usd", saving, "projected_cost_usd", cost)
			return false
		}
		k.summarySubstituteRefreshPaid.Add(1)
		slog.Debug("context-guru: cache_aware_summarizer refresh pays for itself; refreshing the reserve",
			"tenant", tenantLabel(j.tenant), "session", j.session,
			"tail_tokens", info.RefreshTailTokens, "expected_saving_usd", saving, "projected_cost_usd", cost)
	}
	start := k.now()
	res := dispatch(offload.CacheAwareSummarizerCallTimeout())
	ms := float64(k.now().Sub(start).Microseconds()) / 1000.0
	if !res.Committed {
		k.summarySubstituteFailed.Add(1)
		switch res.Reason {
		case offload.KeepAliveReasonInFlight:
			k.summarySubstituteInFlight.Add(1)
		case offload.KeepAliveReasonConcurrencyFull:
			k.summarySubstituteConcurrencyFull.Add(1)
		}
		slog.Debug("context-guru: cache_aware_summarizer keep-alive substitute produced no "+
			"summary; pinging normally instead",
			"tenant", tenantLabel(j.tenant), "session", j.session, "reason", string(res.Reason))
		return false
	}
	spent := k.recordSummarySubstitute(j, res, ms, start)
	slog.Debug("context-guru: cache keep-alive ping (via cache_aware_summarizer)",
		"tenant", tenantLabel(j.tenant), "session", j.session, "ping", j.ping,
		"cache_read", res.CacheRead, "cache_write", res.CacheWrite, "output", res.Output,
		"cost_usd", spent, "ms", ms)
	return true
}

// recordSummarySubstitute books a substituted ping exactly like record1 books an ordinary one —
// the session's running spend, the process counter, the cache-liveness clock, and the host's
// KeepAliveReport — plus the one difference that justifies a separate function: `pings` is
// incremented here as PINGS.Add(1) the same as record1, so a substituted ping is counted once
// toward the process-wide ping figure (never twice: the ordinary ping path that would have
// counted it never runs, because fireSummarySubstitute returned true). This row is the ONE record
// of a committed substitute call's spend: commissionSync defers only a detail row for it (no
// ledger tokens), so requests.cg_llm_cost_usd does not count it a second time (#455).
func (k *Keeper) recordSummarySubstitute(j pingJob, res offload.KeepAliveSummaryResult, ms float64, startedAt time.Time) float64 {
	u := Usage{FreshInput: int64(res.FreshInput), CacheRead: int64(res.CacheRead),
		CacheWrite: int64(res.CacheWrite), Output: int64(res.Output),
		CacheWrite1h: int64(res.CacheWrite1h)}
	cost := k.recordSubstitutePing(j, res.Model, u, ms, startedAt, 0)
	k.summarySubstituted.Add(1)
	return cost
}

// recordSubstitutePing is the booking both substitutes share: a call that read the cached prefix in
// place of a ping is booked as the ping it replaced.
//
// answerUSD is what the answer cost beyond the ping's own read (0 for the summary): it is carried on
// the ping row in its own field.
func (k *Keeper) recordSubstitutePing(j pingJob, model string, u Usage, ms float64, startedAt time.Time, answerUSD float64) float64 {
	e := j.e
	k.mu.Lock()
	provider, route, preset, agent := e.provider, e.route, e.preset, e.agent
	appliedStrategy, endpoint, prefix := e.tag, e.endpoint, e.prefix
	k.mu.Unlock()
	if model == "" {
		model = e.model
	}
	var price Price
	priced := false
	if model != "" {
		price, priced = k.price(endpoint, model, prefix)
	}
	// Same clock, same gate, same reason as record1: a substitute call reads the SAME cached
	// prefix a ping would (it is built from the byte-identical forwarded body — see
	// offload.registerKeepAliveCandidate), so it refreshes the SAME liveness clock
	// cache_state: pre_expiry reads, and only when it actually read something.
	if u.CacheRead > 0 {
		apply.RecordCacheTouch(j.st, j.tenant, j.raw, provider, startedAt.UnixMilli())
	}
	var cost float64
	if priced && !price.Zero() && (u.CacheRead > 0 || u.CacheWrite > 0 || u.Output > 0) {
		// u.CacheWrite1h, not 0: see record1's own call to CostWithCacheWrite1h for why the
		// 1h-tier subset has to be passed through rather than folded into the 5-minute rate.
		cost = price.CostWithCacheWrite1h(u.FreshInput, u.CacheRead, u.CacheWrite, u.Output, u.CacheWrite1h)
	}
	k.mu.Lock()
	e.spent += cost
	e.refreshed = u.CacheRead
	if u.CacheRead > 0 {
		e.refreshedAt = startedAt
	}
	k.mu.Unlock()
	for {
		old := k.spentUSD.Load()
		if k.spentUSD.CompareAndSwap(old, math.Float64bits(math.Float64frombits(old)+cost)) {
			break
		}
	}
	k.pings.Add(1)
	if ka, ok := j.emitter.(components.KeepAliveEmitter); ok {
		ka.KeepAlivePing(components.KeepAliveReport{
			Tenant: j.tenant, Session: j.session, Thread: j.thread, Model: model, Provider: string(provider),
			Route: route, Pings: j.ping, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite,
			Output: u.Output, CostUSD: cost, Status: 200, DurationMs: ms,
			TS: k.now().UnixMilli(), Agent: agent, Preset: preset,
			Strategy:       appliedStrategy,
			FreshInput:     u.FreshInput,
			SweepAnswerUSD: answerUSD,
			// 0, not pingOutputBudget(route): this call asked for a full answer, not the 1-token read
			// an ordinary ping would have, so reporting the ping's own budget here would misdescribe
			// what was actually requested.
			MaxTokens: 0,
			Window:    j.prevStartedAt,
		})
	}
	return cost
}

// sweepAnswerUSD prices the ANSWER of a question that replaced a ping (#404 point 9): everything the
// question cost beyond the read the plain ping would have made of the same prefix. That is the output
// (thinking tokens are billed as output), any fresh input (the question's own text) and any cache
// write, each at the model's rate. The read is the ping's and is not the answer's cost. 0 when the
// model has no price, which reads as unknown and never as free: the ping row's own cost is unchanged.
func (k *Keeper) sweepAnswerUSD(j pingJob, model string, u Usage) float64 {
	e := j.e
	k.mu.Lock()
	endpoint, prefix, em := e.endpoint, e.prefix, e.model
	k.mu.Unlock()
	if model == "" {
		model = em
	}
	if model == "" {
		return 0
	}
	price, ok := k.price(endpoint, model, prefix)
	if !ok || price.Zero() {
		return 0
	}
	return price.CostWithCacheWrite1h(u.FreshInput, 0, u.CacheWrite, u.Output, u.CacheWrite1h)
}

// sweepOutputBudgetGuess is the assumed output size of the sweep's question, used ONLY to project its
// cost against the keep-alive $ cap before dispatching it, never a cap on the real call. A measured
// 12-candidate answer was 7,191 tokens including the model's reasoning; this is deliberately below
// that, because it only has to keep the question from being sent where even a small answer exceeds
// the per-ping ceiling.
const sweepOutputBudgetGuess = 4000

// projectedSweepUSD mirrors projectedSummaryUSD for the sweep's question: ONE question, which reads the
// cached prefix once and pays one answer.
func (k *Keeper) projectedSweepUSD(model, endpoint string, prefix int64) float64 {
	return k.projectedSweepBatchesUSD(model, endpoint, prefix, 1)
}

// projectedSweepBatchesUSD is the projected cost of a sweep of n questions (#132). A long list is asked in
// batches (extract_sweep_batch.go), and EACH batch reads the whole cached prefix again and pays its own
// answer, so n questions cost n x (prefix read + answer guess). Pricing one answer for a sweep of four
// would let it spend four times the per-ping ceiling the operator approved.
func (k *Keeper) projectedSweepBatchesUSD(model, endpoint string, prefix int64, n int) float64 {
	if model == "" || prefix <= 0 || n < 1 {
		return 0
	}
	price, ok := k.price(endpoint, model, prefix)
	if !ok || price.Zero() {
		return 0
	}
	return float64(n) * (float64(prefix)*price.CacheRead + float64(sweepOutputBudgetGuess)*price.Output)
}

// affordableSweepBatches is how many of the planned batches the per-ping ceiling admits: the largest
// n <= planned with n x (one question's projected cost) <= ceiling. 0 means not even one fits, and the
// caller sends the ordinary ping. A ceiling that admits fewer than planned LOWERS the batch count, so a
// ping still sweeps what it can afford (the smallest candidates are left unasked).
func (k *Keeper) affordableSweepBatches(model, endpoint string, prefix int64, planned int, ceiling float64) int {
	if planned < 1 {
		planned = 1
	}
	one := k.projectedSweepBatchesUSD(model, endpoint, prefix, 1)
	if one <= 0 { // unpriced: the old behaviour, nothing to compare, nothing to lower
		return planned
	}
	// NO CAP admits every planned batch. Policy.Ceiling() returns +Inf for a negative MaxUSDPerPing ("no
	// ceiling", which somebody has to type on purpose). Dividing +Inf by a cost and converting it to int is
	// implementation-defined (MinInt64 on gc); the clamp below would turn that into "zero batches", so the
	// one policy that asks for no cap would get no sweep at all.
	//
	// A ceiling of zero or less (or NaN) keeps the meaning the old check had: every positive cost exceeds
	// it, so nothing is admitted. Policy.Ceiling() never returns one (it resolves zero to the default), so
	// this is only a guard for a direct caller. It is EXPLICIT because int(NaN) and int(-Inf) are
	// implementation-defined: on gc they happen to be negative, which the clamp below also turns into zero,
	// so a test on this platform cannot tell this guard from that accident. The guard keeps the answer
	// right on a platform where the conversion differs.
	if math.IsInf(ceiling, 1) {
		return planned
	}
	if !(ceiling > 0) {
		return 0
	}
	n := int(ceiling / one)
	if n > planned {
		n = planned
	}
	if n < 0 {
		n = 0
	}
	return n
}

// sweepWaitCause is the thread's wait cause (#453) for the question to store: what the response that
// established this entry was about to wait on, from its own tool_use names and stop reason and the
// thread's open-task counts. Recorded with the held answer; nothing decides on it.
func (k *Keeper) sweepWaitCause(j pingJob) offload.SweepPingWait {
	e := j.e
	k.mu.Lock()
	names, stop, usageOK, turnRead := e.toolUseNames, e.stopReason, e.usageOK, e.turnRead
	k.mu.Unlock()
	bg, tm := 0, 0
	if k.o.OpenTasks != nil {
		bg, tm = k.o.OpenTasks(j.tenant, j.session, j.thread)
	}
	waitingFor, tool := waitcause.ClassifyWaitingFor(names, stop, false, usageOK, bg, tm)
	return offload.SweepPingWait{WaitingFor: waitingFor, WaitTool: tool, LastTurnRead: int(turnRead)}
}

// fireSweepSubstitute sends extract_llm_sweep's question instead of this due ping (#404). Returns true
// when the question stood in for the ping; the caller then sends no ordinary ping. Every other
// outcome returns false and the ordinary ping goes out, so a ping is never lost to this.
//
// The question is the same read of the cached prefix a ping is, plus an answer. It is sent only when
// the sweep registered candidates for THIS thread (offload.SweepPingSubstitute), the model is one the
// question does not hurt (#422, counted as sweep_model_declined), and its projected cost fits the
// per-ping ceiling.
func (k *Keeper) fireSweepSubstitute(j pingJob) (substituted bool) {
	// Last line of defence: a panic anywhere in the question's bookkeeping leaves the ordinary ping.
	defer func() {
		if r := recover(); r != nil {
			k.sweepAskFailed.Add(1)
			slog.Error("context-guru: sweep keep-alive question panicked; pinging normally instead",
				"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "panic", r)
			substituted = false
		}
	}()
	dispatch, info, reason, ok := offload.SweepPingSubstitute(thread.Key(j.session, j.thread))
	if !ok {
		slog.Debug("context-guru: sweep keep-alive question not offered",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "reason", string(reason))
		return false
	}
	e := j.e
	k.mu.Lock()
	model, prefix, ceiling, endpoint := e.model, e.prefix, e.pol.Ceiling(), e.endpoint
	k.mu.Unlock()
	// THE CEILING PRICES EVERY QUESTION THE SWEEP WILL ASK, not one (#132). The sweep plans info.Batches
	// questions, each reading the prefix again and paying its own answer. If the ceiling admits fewer than
	// planned, ask fewer (the smallest candidates stay unasked, counted as sweep_over_ask_cap); if it
	// admits none, send the ordinary ping.
	planned := info.Batches
	if planned < 1 {
		planned = 1
	}
	allowed := k.affordableSweepBatches(model, endpoint, prefix, planned, ceiling)
	if allowed < 1 {
		cost := k.projectedSweepUSD(model, endpoint, prefix)
		k.sweepOverBudget.Add(1)
		slog.Debug("context-guru: sweep keep-alive question would exceed the per-ping cost cap; "+
			"pinging normally instead",
			"tenant", tenantLabel(j.tenant), "session", j.session, "estimate_usd", cost, "ceiling_usd", ceiling)
		return false
	}
	wait := k.sweepWaitCause(j)
	if allowed < planned {
		k.sweepBatchesLowered.Add(1)
		wait.MaxBatches = allowed
		slog.Debug("context-guru: sweep keep-alive: the per-ping cost cap admits fewer questions than planned; "+
			"asking fewer",
			"tenant", tenantLabel(j.tenant), "session", j.session, "planned", planned, "allowed", allowed,
			"ceiling_usd", ceiling)
	}
	start := k.now()
	res := dispatch(offload.CacheAwareSummarizerCallTimeout(), wait)
	ms := float64(k.now().Sub(start).Microseconds()) / 1000.0
	if !res.Substituted {
		// A full slot is counted by the offload side as sweep_concurrency_full. It is not a failed ask.
		if res.Reason != offload.SweepReasonConcurrencyFull {
			k.sweepAskFailed.Add(1)
		}
		slog.Debug("context-guru: sweep keep-alive question did not use the cache; pinging normally instead",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "reason", string(res.Reason))
		return false
	}
	u := Usage{FreshInput: int64(res.FreshInput), CacheRead: int64(res.CacheRead),
		CacheWrite: int64(res.CacheWrite), Output: int64(res.Output), CacheWrite1h: int64(res.CacheWrite1h)}
	spent := k.recordSubstitutePing(j, res.Model, u, ms, start, k.sweepAnswerUSD(j, res.Model, u))
	k.sweepSubstituted.Add(1)
	// THE guard, as in fire(): a question that wrote more than it read did not match the prefix. The
	// cache is refreshed by the write, but a repeat would pay the same mismatch, so stop.
	if u.CacheWrite > u.CacheRead {
		k.wrote.Add(1)
		k.markStopped(j.e)
		slog.Error("context-guru: sweep keep-alive question CREATED a cache entry instead of reading one; "+
			"not pinging this session again",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread,
			"cache_write", u.CacheWrite, "cache_read", u.CacheRead)
	}
	slog.Debug("context-guru: cache keep-alive ping (via extract_llm_sweep)",
		"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "ping", j.ping,
		"offered", info.Offered, "proposed", res.Proposed, "held", res.Held,
		"cache_read", res.CacheRead, "cache_write", res.CacheWrite, "output", res.Output,
		"cost_usd", spent, "ms", ms)
	return true
}
