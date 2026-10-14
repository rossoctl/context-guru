package offload

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
)

// THE QUESTION AT THE KEEP-ALIVE PING (#404, points 1, 2, 11).
//
// proxy/keepalive.go's keeper spends the caller's credential on a ping whose only job is to read the
// cached prefix and reset the provider's TTL. A prefix ask does the same read. So when this component
// has candidates for a thread, the keeper sends the sweep's question INSTEAD of the ping: same
// refresh, and the only extra cost is the answer. There is no timer of its own. No keep-alive means
// no question, and a prompt under the keep-alive floor (80k tokens, #444) gets none, because the
// keeper never pings it.
//
// THE SAME PATTERN AS cache_aware_summarizer's substitute (cache_aware_keepalive.go), deliberately:
// the request pipeline REGISTERS material per thread after it runs, and the keeper LOOKS IT UP when a
// ping falls due. The keeper cannot ask itself, because everything the question needs (the candidate
// rules, the evidence, the verdict checks) lives here.
//
// WHAT IS REGISTERED is a snapshot of the conversation, not a list of candidates. Collecting
// candidates hashes every tool output and runs the co-reference index; doing that on every request
// for a ping that fires once per idle span would put a full pass on the hot path. The snapshot is a
// slice copy, and the pass runs only when a ping is due.
//
// FAIL OPEN. Every "no" leaves the ordinary ping to the keeper: no candidate, a model whose cache the
// question would break, nothing to ask about, an ask already made for this conversation, a full
// concurrency bound, a failed call. The ping is always sent in one form or the other.

// SweepPingReason says why SweepPingSubstitute declined, so the keeper's counters can tell "this
// thread never registered" from "it did and the model is one the question would hurt".
type SweepPingReason string

const (
	SweepReasonNone SweepPingReason = ""
	// SweepReasonNoCandidate: nothing registered. The component is not in this tenant's pipeline, or
	// the thread's last request did not reach the registration point. The ordinary case.
	SweepReasonNoCandidate SweepPingReason = "no_candidate"
	// SweepReasonModelDeclined: sweep_model_declined (#422). On this model the question would strip
	// the thinking blocks from the cached prefix and rewrite the cache at the write rate.
	SweepReasonModelDeclined SweepPingReason = "model_declined"
	// SweepReasonNoInventory: the conversation has no output the sweep may offer.
	SweepReasonNoInventory SweepPingReason = "no_inventory"
	// SweepReasonBelowMin: fewer candidates than min_inventory (1 unless an operator raised it).
	SweepReasonBelowMin SweepPingReason = "below_min_inventory"
	// SweepReasonAlreadyAsked: this conversation was already asked about at an earlier ping.
	SweepReasonAlreadyAsked SweepPingReason = "already_asked"
	// SweepReasonConcurrencyFull: the bound on simultaneous questions is full.
	SweepReasonConcurrencyFull SweepPingReason = "concurrency_full"
	// SweepReasonNoAsker: the route has no prefix asker.
	SweepReasonNoAsker SweepPingReason = "no_asker"
	// SweepReasonNeedsShadow: sweep_ask_needs_shadow. Until the cold apply exists (PR C of #404) a held
	// answer is used by nothing, so asking would only cost money. The ping stays plain unless shadow
	// mode is on, where the answer is at least measured.
	SweepReasonNeedsShadow SweepPingReason = "ask_needs_shadow"
	// SweepReasonPanic: the question panicked and was recovered. The ping stays plain, and the
	// conversation is marked asked so the next ping is plain too.
	SweepReasonPanic SweepPingReason = "panic"
)

// SweepPingWait is the thread's wait cause (#453) at the moment of the question, handed in by the
// keeper, which is where the tool names and the open-task counts are. Stored with the held answer.
// Never read to decide anything here.
type SweepPingWait struct {
	WaitingFor, WaitTool string
}

// SweepPingInfo is what a registered candidate looks like before the question is sent, for the
// keeper's cost projection and logs.
type SweepPingInfo struct {
	Model   string
	Offered int
}

// SweepPingResult is what one question cost and produced, shaped like KeepAliveSummaryResult so the
// keeper books it exactly as it books a ping.
type SweepPingResult struct {
	Model                                     string
	FreshInput, Output, CacheWrite, CacheRead int
	CacheWrite1h                              int
	// Substituted is true when the question reached the provider and touched the prefix (read or
	// wrote it). The keeper then counts it as the ping for this idle span. False means the keeper
	// sends the ordinary ping: the call never left, or failed before using the cache.
	Substituted bool
	// Held is true when the model's reply parsed and the answer is now held for this thread.
	Held bool
	// Offered, Proposed and ProposedTokens describe the answer: candidates asked, drops voted for,
	// and what those drops would remove.
	Offered, Proposed, ProposedTokens int
	Reason                            SweepPingReason
	// Gates and Events are the adjudication's own gate and event names for this question
	// (sweep_kept, sweep_unparseable, sweep_dropped, ...), the lists a request's Report would have
	// carried. The process-wide totals are in SweepPingCounters().Gates.
	Gates, Events map[string]int
}

// Process-wide counters, read by the keeper for /stats. Named for what they count, not for who
// reads them.
var (
	sweepPingAsked         atomic.Int64
	sweepPingHeld          atomic.Int64
	sweepPingFailed        atomic.Int64
	sweepPingModelDeclined atomic.Int64
	sweepPingNoInventory   atomic.Int64
	sweepPingAlreadyAsked  atomic.Int64
	sweepPingNoCandidate   atomic.Int64
	sweepPingHoldNoToolID  atomic.Int64
	sweepPingNeedsShadow   atomic.Int64
	sweepPingPanics        atomic.Int64

	sweepPingGateMu sync.Mutex
	sweepPingGates  = map[string]int64{}
)

// SweepPingStats is the ledger of the ping-time question.
type SweepPingStats struct {
	// Asked: questions that reached the provider. Held: of those, answers that parsed and are held.
	Asked, Held, Failed int64
	// ModelDeclined is sweep_model_declined: pings sent plain because the question would break the
	// cache on this model.
	ModelDeclined int64
	// NoInventory, AlreadyAsked and NoCandidate are the ordinary reasons a ping stayed a ping.
	NoInventory, AlreadyAsked, NoCandidate int64
	// HoldNoToolID counts judged outputs that could not be held because the agent's request has no
	// tool id for them.
	HoldNoToolID int64
	// NeedsShadow is sweep_ask_needs_shadow: pings sent plain because shadow mode is off and no cold
	// apply exists yet to use the answer.
	NeedsShadow int64
	// RetiredKeys counts retired settings found (and ignored) in configs, by key.
	RetiredKeys map[string]int64
	// Panics counts panics recovered in the ping-time question (collection, the ask, or the hold). Each
	// leaves the ping a plain ping.
	Panics int64
	// Gates counts the adjudication's own gate and event names (sweep_kept, sweep_unparseable, ...)
	// across ping-time questions, the counters a request's Report would have carried.
	Gates map[string]int64
}

// SweepPingCounters snapshots the ledger.
func SweepPingCounters() SweepPingStats {
	sweepPingGateMu.Lock()
	g := make(map[string]int64, len(sweepPingGates))
	for k, v := range sweepPingGates {
		g[k] = v
	}
	sweepPingGateMu.Unlock()
	sweepRetiredMu.Lock()
	rk := make(map[string]int64, len(sweepRetiredIgnored))
	for k, v := range sweepRetiredIgnored {
		rk[k] = v
	}
	sweepRetiredMu.Unlock()
	return SweepPingStats{RetiredKeys: rk, Asked: sweepPingAsked.Load(), Held: sweepPingHeld.Load(),
		Failed: sweepPingFailed.Load(), ModelDeclined: sweepPingModelDeclined.Load(),
		NoInventory: sweepPingNoInventory.Load(), AlreadyAsked: sweepPingAlreadyAsked.Load(),
		NoCandidate: sweepPingNoCandidate.Load(), HoldNoToolID: sweepPingHoldNoToolID.Load(), NeedsShadow: sweepPingNeedsShadow.Load(), Panics: sweepPingPanics.Load(), Gates: g}
}

func bumpSweepPingGates(counts map[string]int) {
	if len(counts) == 0 {
		return
	}
	sweepPingGateMu.Lock()
	for n, c := range counts {
		sweepPingGates[n] += int64(c)
	}
	sweepPingGateMu.Unlock()
}

// countNames turns the adjudication's name lists into counts.
func countNames(into map[string]int, names []string) {
	for _, n := range names {
		into[n]++
	}
}

// sweepSlots bounds simultaneous questions. A question is a long call (a verdict per candidate, up to
// the reply budget), unlike a one-token ping, and the keeper dispatches every due thread at once.
var sweepSlots = make(chan struct{}, 4)

// Registry bounds. Smaller than the summarizer's: a registration holds a whole conversation.
const (
	maxSweepPingCandidates = 256
	// maxSweepPingAge is past the longest idle span the default keep-alive spends on one conversation
	// (K+1 pings of X, about 14 minutes). The keeper clears a registration when it stops tracking the
	// thread; this is the backstop for hosts that run without it.
	maxSweepPingAge = 30 * time.Minute
)

type sweepPingCandidate struct {
	e *ExtractSweep
	// ctx is the minimal request context the question needs, detached from the registering request
	// (see registerPing).
	ctx *components.Ctx
	// input is the pipeline's conversation at this component's position; raw is the agent's own,
	// the one the hash and the stale-prefix guard are measured on. Both are shallow copies.
	input, raw   []bschemas.ChatMessage
	registeredAt time.Time
	// asked is set once a question has been sent for this conversation, so a second ping in the same
	// idle span sends a plain ping instead of paying for the same answer twice.
	asked atomic.Bool
}

var (
	sweepPingMu   sync.Mutex
	sweepPingCand = map[string]*sweepPingCandidate{}
)

// registerPing snapshots this request's conversation for the keeper. Deferred from Offload so it sees
// the conversation as the component leaves it, replays included.
func (e *ExtractSweep) registerPing(c *components.Ctx, req *bschemas.BifrostChatRequest) {
	if c == nil || c.Session == "" || c.PrefixAsk == nil || c.Store == nil || c.Bypass ||
		c.Mode == components.ModeObserve || req == nil || len(req.Input) == 0 {
		return
	}
	baseCtx := c.Ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	cc := &components.Ctx{
		// context.WithoutCancel: the registering request is long finished by the time a ping asks, and
		// net/http cancels its context when the handler returns. Same detachment as the summarizer's.
		Ctx: context.WithoutCancel(baseCtx), Session: c.Session, Thread: c.Thread, Store: c.Store,
		PrefixAsk: c.PrefixAsk, ModelName: c.ModelName, SelfRates: c.SelfRates, RatesFor: c.RatesFor,
		CtxWindow: c.CtxWindow, CacheAware: c.CacheAware,
		// At the ping the stash holds THIS request's forwarded body, so every message in the snapshot
		// is inside the cached prefix. The stale-prefix guard then checks the deepest candidate.
		MaxCachedIdx: len(req.Input) - 1,
	}
	if len(c.Restored) > 0 {
		cc.Restored = make(map[string]string, len(c.Restored))
		for k, v := range c.Restored {
			cc.Restored[k] = v
		}
	}
	input := append([]bschemas.ChatMessage(nil), req.Input...)
	raw := input
	if c.RawInput != nil {
		raw = append([]bschemas.ChatMessage(nil), c.RawInput...)
		cc.RawInput = raw
	}
	cand := &sweepPingCandidate{e: e, ctx: cc, input: input, raw: raw, registeredAt: time.Now()}
	key := thread.Key(c.Session, c.Thread)
	sweepPingMu.Lock()
	defer sweepPingMu.Unlock()
	if _, exists := sweepPingCand[key]; !exists && len(sweepPingCand) >= maxSweepPingCandidates {
		pruneSweepPingLocked(cand.registeredAt)
		if len(sweepPingCand) >= maxSweepPingCandidates {
			return // fail toward not remembering a new thread; its pings stay pings
		}
	}
	sweepPingCand[key] = cand
}

func pruneSweepPingLocked(now time.Time) {
	for k, cand := range sweepPingCand {
		if now.Sub(cand.registeredAt) > maxSweepPingAge {
			delete(sweepPingCand, k)
		}
	}
}

// clearSweepPing drops a thread's registration. Called from ClearKeepAliveCandidate, which the keeper
// already calls on every path that stops tracking a thread.
func clearSweepPing(threadKey string) {
	sweepPingMu.Lock()
	delete(sweepPingCand, threadKey)
	sweepPingMu.Unlock()
}

// clearStaleSweepPing drops a registration made before requestStartedAt: one left by an EARLIER
// request. See ClearStaleKeepAliveCandidate for why this is a timestamp test.
func clearStaleSweepPing(threadKey string, requestStartedAt time.Time) {
	sweepPingMu.Lock()
	if cand, ok := sweepPingCand[threadKey]; ok && cand.registeredAt.Before(requestStartedAt) {
		delete(sweepPingCand, threadKey)
	}
	sweepPingMu.Unlock()
}

// sweepModelProfiles is the embedded model registry, the same one cache_aware_summarizer classifies
// models with. #422 asks for "the same model classification that #402 adds".
var sweepModelProfiles = sync.OnceValue(func() *summarizerProfiles {
	p, err := loadSummarizerProfiles(summarizerProfilesYAML)
	if err != nil {
		return nil
	}
	return p
})

// sweepModelDeclines is point 11 (#422): on a model that keeps only the LAST turn's thinking, the
// question (a user message that is not a tool result) strips every earlier thinking block from the
// cached prefix, and the provider rewrites the cache from the first assistant turn at the write rate.
// Haiku 4.5 and the older Sonnet and Opus models are in the registry's list; the question is skipped
// only when the conversation actually carries a thinking block before its last message, because that
// is the case that was measured to break the cache. A model the registry does not name is "keeps all"
// and is never skipped on a guess.
//
// TODO(#404, PR B): use the shared thinking-would-be-stripped helper in components/offload/ask_role.go
// once refactor/ask-instruction-role lands, instead of the summarizer's current check below.
func sweepModelDeclines(modelID string, msgs []bschemas.ChatMessage) bool {
	p := sweepModelProfiles()
	return p != nil && p.thinkingLastTurnOnly(modelID) && thinkingWouldBeStripped(msgs)
}

// SweepPingSubstitute reports whether a thread has a sweep question the keep-alive may send instead
// of a bare ping, and a dispatch to send it. threadKey is thread.Key(session, thread).
//
// ok=false means send the ordinary ping; reason says why. dispatch BLOCKS for the whole call and the
// keeper runs it off its own goroutine, as it does for any ping.
func SweepPingSubstitute(threadKey string) (dispatch func(timeout time.Duration, wait SweepPingWait) SweepPingResult,
	info SweepPingInfo, reason SweepPingReason, ok bool) {

	sweepPingMu.Lock()
	cand := sweepPingCand[threadKey]
	sweepPingMu.Unlock()
	// FAIL OPEN. This runs on the keeper's own goroutine, which no pipeline recover covers, and it now
	// carries candidate collection, the co-reference index and the hold. A panic must leave the ping a
	// plain ping and the process up. The conversation is marked asked, so the next ping is plain too.
	defer func() {
		if r := recover(); r != nil {
			notePingPanic("collect", threadKey, r)
			if cand != nil {
				cand.asked.Store(true)
			}
			dispatch, info, reason, ok = nil, SweepPingInfo{}, SweepReasonPanic, false
		}
	}()
	if cand == nil {
		sweepPingNoCandidate.Add(1)
		return nil, SweepPingInfo{}, SweepReasonNoCandidate, false
	}
	// TODO(#404, PR C): remove this check. It exists only because, until the cold apply lands, a held
	// answer changes no request, so every answer would be paid for and unused. PR C removes it.
	if !cand.e.shadow {
		sweepPingNeedsShadow.Add(1)
		return nil, SweepPingInfo{}, SweepReasonNeedsShadow, false
	}
	if cand.asked.Load() {
		sweepPingAlreadyAsked.Add(1)
		return nil, SweepPingInfo{}, SweepReasonAlreadyAsked, false
	}
	// The concurrency bound BEFORE the work: collection hashes every output and runs the co-reference
	// index, and with many threads due at once it would run unbounded. The slot itself is taken in ask.
	if len(sweepSlots) >= cap(sweepSlots) {
		return nil, SweepPingInfo{}, SweepReasonConcurrencyFull, false
	}
	c := cand.ctx
	if sweepModelDeclines(c.ModelName, cand.input) {
		sweepPingModelDeclined.Add(1)
		logging.From(c.Ctx).Debug("cg.sweep.model_declined", "session", c.Session, "thread", c.Thread,
			"model", c.ModelName, "gate", "sweep_model_declined")
		return nil, SweepPingInfo{}, SweepReasonModelDeclined, false
	}
	rep := &components.Report{Component: "extract_llm_sweep", Kind: "offload"}
	cands := cand.collect(rep)
	if len(cands) == 0 {
		// The collection's gates say WHY nothing was offered (sweep_keep_still_held, below_output_floor,
		// ...), and no question will carry them out.
		bumpSweepPingGates(rep.Gates)
		sweepPingNoInventory.Add(1)
		return nil, SweepPingInfo{}, SweepReasonNoInventory, false
	}
	if len(cands) < cand.e.minInventory {
		sweepPingNoInventory.Add(1)
		return nil, SweepPingInfo{}, SweepReasonBelowMin, false
	}
	info = SweepPingInfo{Model: c.ModelName, Offered: len(cands)}
	return func(timeout time.Duration, wait SweepPingWait) (res SweepPingResult) {
		// The same recover for the ask itself: the asker, the verdict checks, buildHeld and the Store
		// all run here. cand.asked was set before the call and stays set.
		defer func() {
			if r := recover(); r != nil {
				notePingPanic("ask", threadKey, r)
				res = SweepPingResult{Reason: SweepReasonPanic}
			}
		}()
		return cand.ask(cands, rep, timeout, wait)
	}, info, SweepReasonNone, true
}

// notePingPanic counts and logs a recovered panic.
func notePingPanic(where, threadKey string, r any) {
	sweepPingPanics.Add(1)
	slog.Error("cg.sweep.ping_panic: recovered; the ping stays a plain ping", "where", where,
		"thread", threadKey, "panic", fmt.Sprint(r), "gate", "sweep_ping_panic")
}

// collect finds the candidates for the question: the same rules as the in-request loop, on the
// snapshot. The 12 largest, by the same cap.
func (cand *sweepPingCandidate) collect(rep *components.Report) []sweepCand {
	e, c := cand.e, cand.ctx
	req := &bschemas.BifrostChatRequest{Input: cand.input}
	var cands []sweepCand
	for _, i := range toolIndices(req) {
		msg := &req.Input[i]
		if !schema.Rewritable(*msg) {
			rep.Gate("non_text_blocks")
			continue
		}
		content := schema.MessageText(*msg)
		if content == "" || expand.HasPlaceholder(content) {
			rep.Gate("empty_or_marker_present")
			continue
		}
		id := extract.ContentKey(content)
		if isKeptVerbatim(c, id) {
			rep.Gate(GateKeptVerbatim)
			continue
		}
		// A decision already stored for these bytes is replayed by the request path, and the output
		// is then a marker in this snapshot. A hit here means a decision made after the snapshot.
		if _, hit := c.Store.Get(resultKey(c.Session, id)); hit {
			continue
		}
		cd, ok := e.admit(c, rep, req, i, content, id)
		if !ok {
			continue
		}
		cands = append(cands, cd)
	}
	if len(cands) > maxAskItems {
		sort.SliceStable(cands, func(a, b int) bool {
			return schema.TextTokens(cands[a].content) > schema.TextTokens(cands[b].content)
		})
		rep.GateN("sweep_over_ask_cap", len(cands)-maxAskItems)
		cands = cands[:maxAskItems]
	}
	rep.EventN("sweep_offered", len(cands))
	return cands
}

// ask sends the question and holds the answer.
func (cand *sweepPingCandidate) ask(cands []sweepCand, collected *components.Report, timeout time.Duration, wait SweepPingWait) SweepPingResult {
	select {
	case sweepSlots <- struct{}{}:
		defer func() { <-sweepSlots }()
	default:
		return SweepPingResult{Reason: SweepReasonConcurrencyFull}
	}
	if !cand.asked.CompareAndSwap(false, true) {
		return SweepPingResult{Reason: SweepReasonAlreadyAsked}
	}
	e := cand.e
	c := *cand.ctx
	key := thread.Key(c.Session, c.Thread)
	if timeout <= 0 {
		timeout = llmCallTimeout
	}
	ctx, cancel := context.WithTimeout(c.Ctx, timeout)
	defer cancel()
	// A DETACHED sink: this call belongs to no request. A chained one would double-charge the request
	// whose context this inherited. Same as the summarizer's keep-alive substitute.
	ctx, sink := cheapmodel.WithDetachedSink(ctx)
	c.Ctx = ctx
	req := &bschemas.BifrostChatRequest{Input: cand.input}
	rep := &components.Report{Component: "extract_llm_sweep", Kind: "offload"}
	start := time.Now()
	drop, r := e.adjudicateWith(req, &c, rep, cands, askOpts{
		trigger:    "ping",
		gateReason: "asked at the keep-alive ping, in place of the ping",
	})
	ms := float64(time.Since(start).Microseconds()) / 1000.0
	gates, events := map[string]int{}, map[string]int{}
	for k, v := range collected.Gates {
		gates[k] += v
	}
	for k, v := range collected.Events {
		events[k] += v
	}
	countNames(gates, r.gates)
	countNames(events, r.events)
	bumpSweepPingGates(gates)
	bumpSweepPingGates(events)

	in, out := r.rec.PromptTokens, r.rec.CompletionTokens
	res := SweepPingResult{Model: sink.Model(), FreshInput: int(in), Output: int(out),
		CacheWrite: int(r.rec.CacheWrite), CacheRead: int(r.rec.CacheRead),
		CacheWrite1h: int(sink.CacheWrite1h()), Offered: len(cands), Gates: gates, Events: events}
	if res.Model == "" {
		res.Model = c.ModelName
	}
	// The provider touched the prefix if it read or wrote any of it. That refreshed the TTL, which is
	// all a ping is for, so the keeper counts this as the ping.
	res.Substituted = res.CacheRead > 0 || res.CacheWrite > 0
	if res.Substituted || in > 0 || out > 0 {
		sweepPingAsked.Add(1)
	} else {
		// Nothing left the process (no stashed prefix, a stale one): not an ask. Allow the next ping
		// to try again.
		cand.asked.Store(false)
	}
	if !r.answered {
		sweepPingFailed.Add(1)
		cand.deferBilling(sink, ms, res.Substituted)
		logging.From(c.Ctx).Debug("cg.sweep.ping_no_answer", "session", c.Session, "thread", c.Thread,
			"gates", r.gates)
		return res
	}
	h, unholdable := buildHeld(key, c.ModelName, cand.raw, cands, drop, time.Now().UnixMilli(), wait, r.rec.CostUSD)
	if unholdable > 0 {
		sweepPingHoldNoToolID.Add(int64(unholdable))
	}
	if len(h.Outputs) > 0 {
		putHeld(c.Store, h)
		sweepPingHeld.Add(1)
		res.Held = true
	}
	res.Proposed, res.ProposedTokens = h.proposed()
	cand.deferBilling(sink, ms, res.Substituted)
	logging.From(c.Ctx).Info("cg.sweep.held", "session", c.Session, "thread", c.Thread,
		"offered", len(cands), "proposed", res.Proposed, "proposed_tokens", res.ProposedTokens,
		"cache_read", res.CacheRead, "cache_write", res.CacheWrite, "output", res.Output,
		"waiting_for", wait.WaitingFor, "wait_tool", wait.WaitTool, "held", res.Held)
	return res
}

// deferBilling books the call the way the summarizer's substitute does (#455). When the call stood in
// for the ping, the keeper books its cost on the ping row and only a detail row is deferred to the
// next real request, so extraction_calls lists it without cg_llm_cost_usd counting it a second time.
// When it did not (the keeper sends an ordinary ping), the call's own usage is deferred whole, or it
// would be booked nowhere.
func (cand *sweepPingCandidate) deferBilling(sink *cheapmodel.Sink, ms float64, substituted bool) {
	c := cand.ctx
	if substituted {
		deferDetailOnly(c.Store, c.Session, "extract_llm_sweep", "prefix_ask", "keepalive_ping", ms, sink)
		return
	}
	deferUsage(c.Store, c.Session, "extract_llm_sweep", "prefix_ask", "keepalive_ping", ms, sink)
}
