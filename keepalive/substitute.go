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
// Deliberately NOT Ctx.CachePhase itself: that function also handles CacheTTLMinimum and an
// unknown idle time, neither of which arises here — a live kaEntry always knows its own idle
// time exactly (now - prevStartedAt, never "unknown"), and the keeper holds the TTL tier the provider
// billed for this thread (kaEntry.ttl1h, or the host's own pol.HeadTTL1h ask). A
// narrower function is simpler and cannot drift from a case this call site never reaches.
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
// is 300 s - 270 s = 30 s, inside the 60 s default, so the window stays 60 s. On OpenAI it is
// 30 min - 28 min = 120 s, so the window becomes 124 s.
func substituteWindow(seconds int, ttl, idle time.Duration) time.Duration {
	w := preExpiryFor(seconds)
	if seconds > 0 {
		return w
	}
	if slack := ttl - idle + 2*tick; slack > w {
		return slack
	}
	return w
}

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
	// (a manager strategy's K>1 schedule, or a component configured with a narrower
	// pre_expiry_seconds than the keeper's own Idle). `any` has no such condition. Computed by
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
	cost := k.recordSubstitutePing(j, res.Model, u, ms, startedAt)
	k.summarySubstituted.Add(1)
	return cost
}

// recordSubstitutePing is the booking both substitutes share: a call that read the cached prefix in
// place of a ping is booked as the ping it replaced.
func (k *Keeper) recordSubstitutePing(j pingJob, model string, u Usage, ms float64, startedAt time.Time) float64 {
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
			Strategy:   appliedStrategy,
			FreshInput: u.FreshInput,
			// 0, not pingOutputBudget(route): this call asked for a full answer, not the 1-token read
			// an ordinary ping would have, so reporting the ping's own budget here would misdescribe
			// what was actually requested.
			MaxTokens: 0,
		})
	}
	return cost
}

// sweepOutputBudgetGuess is the assumed output size of the sweep's question, used ONLY to project its
// cost against the keep-alive $ cap before dispatching it, never a cap on the real call. A measured
// 12-candidate answer was 7,191 tokens including the model's reasoning; this is deliberately below
// that, because it only has to keep the question from being sent where even a small answer exceeds
// the per-ping ceiling.
const sweepOutputBudgetGuess = 4000

// projectedSweepUSD mirrors projectedSummaryUSD for the sweep's question.
func (k *Keeper) projectedSweepUSD(model, endpoint string, prefix int64) float64 {
	if model == "" || prefix <= 0 {
		return 0
	}
	price, ok := k.price(endpoint, model, prefix)
	if !ok || price.Zero() {
		return 0
	}
	return float64(prefix)*price.CacheRead + float64(sweepOutputBudgetGuess)*price.Output
}

// sweepWaitCause is the thread's wait cause (#453) for the question to store: what the response that
// established this entry was about to wait on, from its own tool_use names and stop reason and the
// thread's open-task counts. Recorded with the held answer; nothing decides on it.
func (k *Keeper) sweepWaitCause(j pingJob) offload.SweepPingWait {
	e := j.e
	k.mu.Lock()
	names, stop, usageOK := e.toolUseNames, e.stopReason, e.usageOK
	k.mu.Unlock()
	bg, tm := 0, 0
	if k.o.OpenTasks != nil {
		bg, tm = k.o.OpenTasks(j.tenant, j.session, j.thread)
	}
	waitingFor, tool := waitcause.ClassifyWaitingFor(names, stop, false, usageOK, bg, tm)
	return offload.SweepPingWait{WaitingFor: waitingFor, WaitTool: tool}
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
	if cost := k.projectedSweepUSD(model, endpoint, prefix); cost > ceiling {
		k.sweepOverBudget.Add(1)
		slog.Debug("context-guru: sweep keep-alive question would exceed the per-ping cost cap; "+
			"pinging normally instead",
			"tenant", tenantLabel(j.tenant), "session", j.session, "estimate_usd", cost, "ceiling_usd", ceiling)
		return false
	}
	start := k.now()
	res := dispatch(offload.CacheAwareSummarizerCallTimeout(), k.sweepWaitCause(j))
	ms := float64(k.now().Sub(start).Microseconds()) / 1000.0
	if !res.Substituted {
		k.sweepAskFailed.Add(1)
		slog.Debug("context-guru: sweep keep-alive question did not use the cache; pinging normally instead",
			"tenant", tenantLabel(j.tenant), "session", j.session, "thread", j.thread, "reason", string(res.Reason))
		return false
	}
	u := Usage{FreshInput: int64(res.FreshInput), CacheRead: int64(res.CacheRead),
		CacheWrite: int64(res.CacheWrite), Output: int64(res.Output), CacheWrite1h: int64(res.CacheWrite1h)}
	spent := k.recordSubstitutePing(j, res.Model, u, ms, start)
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
