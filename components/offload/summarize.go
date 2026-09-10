package offload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"gopkg.in/yaml.v3"
)

func init() { components.Register("summarize", newSummarize) }

// defaultSummarizeCallTimeout bounds the single summarizer call. It is higher than
// extract's ceiling because summarize makes ONE call over a large span, and a
// big trajectory legitimately takes the model longer to read and compress.
//
// 150s was sized against an IDLE server. MEASURED on a 50-task SWE-bench arm there,
// this component spent 26,890,609 ms over 1,372 calls — a ~19.6s mean, so 150s was
// ~7.6x the mean and never binding. Two things make that headroom evaporate under
// load, and they ADD:
//
//   - queue wait, which is what a loaded server actually charges: p50 17.2s / p95
//     78.8s measured under KV pressure, before the model starts work at all;
//   - this component's own prefill, which is large by construction — the same arm
//     sent 78,155,276 input tokens across those 1,372 calls, i.e. ~57k prompt tokens
//     per call (it summarizes the whole middle of the transcript).
//
// So the budget must cover queue + a 57k-token prefill + generation, and only the
// last of those three is what the idle-server mean measured. 300s is a CEILING, not
// a target: on an idle server nothing changes, because the call still returns in
// ~20s.
//
// ⚠️ Unlike extract_llm this is bounded ONCE for the whole component, not per call —
// s.summarize retries up to 3x against THIS SAME ctx (see the loop), so the total is
// this value, not 3x it. Raising it therefore raises the worst-case stall on ONE
// agent turn by exactly this much, and that stall is billed against the benchmark's
// own [agent] timeout_sec.
//
//	CONTEXT_GURU_SUMMARIZE_TIMEOUT=300s   (Go duration; bare integers are seconds)
const defaultSummarizeCallTimeout = 300 * time.Second

// summarizeCallTimeout is resolved once at process start from the environment.
var summarizeCallTimeout = resolveSummarizeCallTimeout()

func resolveSummarizeCallTimeout() time.Duration {
	return resolveTimeoutEnv("CONTEXT_GURU_SUMMARIZE_TIMEOUT", defaultSummarizeCallTimeout)
}

// Timeout/error counters, the summarize counterpart of extract_llm's llmTimeouts /
// llmErrors and served at /stats beside them.
//
// summarize's fail path is LOUDER than extract_llm's — it returns the error, the
// pipeline reverts the component, and that shows up as a per-component `reverted`
// count. But `reverted` cannot say WHY, and the two causes call for opposite
// responses: a blown deadline means the budget is too small for this load (the arm's
// savings are an undercount), while a model/transport error means the route is wrong
// (the arm is not measuring summarization at all). Separating them is the difference
// between "raise the ceiling" and "fix the -nothink route".
var (
	summarizeTimeouts int64
	summarizeErrors   int64
)

// SummarizeTimeouts returns the number of summarize calls abandoned on the per-request
// deadline. Non-zero means CONTEXT_GURU_SUMMARIZE_TIMEOUT is too small for the current
// server load, and this arm compacted less than the method would on an idle server.
func SummarizeTimeouts() int64 { return atomic.LoadInt64(&summarizeTimeouts) }

// SummarizeErrors returns non-timeout failures of summarize model calls (transport,
// HTTP status, empty/unparseable body, or a cancelled parent request).
func SummarizeErrors() int64 { return atomic.LoadInt64(&summarizeErrors) }

// SummarizeCallTimeout exposes the resolved budget so /stats can report the
// configuration next to the counters (a timeout count is meaningless without it).
func SummarizeCallTimeout() time.Duration { return summarizeCallTimeout }

// maxTrajectoryChars caps the trajectory text sent to the summarizer so a very
// large span still fits the model's context window (≈70k tokens, well under a
// 200k window with room for the summary) instead of erroring and failing open.
// The most recent turns are kept; the full original span is always stashed for
// expand, so the older prefix is never lost — only left out of THIS summary.
const maxTrajectoryChars = 280_000

// Summarize compresses the middle of a long trajectory into one LLM-written
// summary (ported from CE-Manager's ReSum-style summarizer). It restructures the
// message list to [msg0, <summary system message>, last-K messages], replacing
// everything in between. It is an Offload: the replaced span is stashed under a
// <<cg:HASH>> marker (carried in the summary message) so the expand tool can
// restore it. NeedsModel — it no-ops when no model is available.
//
// This is the one component that changes the message count; apply.Body rebuilds
// the body preserving the retained messages' original bytes.
type Summarize struct {
	level             string
	keepLast          int
	minTokens         int
	resummarizeTokens int
	includeToolCalls  bool
	modelSource       string
	modelClient       components.Model // config-pinned client (model: block), or nil
	trigger           components.Trigger
	mode              markerMode
}

type summarizeConfig struct {
	SummaryLevel string `yaml:"summary_level"`      // concise | regular | highly_detailed
	KeepLast     int    `yaml:"keep_last"`          // messages kept verbatim at the tail
	StartFrom    int    `yaml:"start_from_message"` // legacy: folds into trigger.min_messages
	MinTokens    int    `yaml:"min_tokens"`         // min content tokens in the span to bother
	// ResummarizeTokens: once a summary exists, reuse it (no LLM call) until the
	// un-summarized tail since the last checkpoint grows past this many tokens,
	// then roll the checkpoint forward with a fresh summary. 0 = re-summarize
	// every eligible turn (old behavior).
	ResummarizeTokens int                `yaml:"resummarize_tokens"`
	IncludeToolCalls  bool               `yaml:"include_tool_calls"`
	Model             modelConfig        `yaml:"model"`
	Trigger           components.Trigger `yaml:"trigger"`
	MarkerMode        string             `yaml:"marker_mode"` // full (default) | summary | off
}

func newSummarize(raw []byte) (components.Component, error) {
	cfg := summarizeConfig{SummaryLevel: "regular", KeepLast: 3, StartFrom: 6, MinTokens: 500, ResummarizeTokens: 6000}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	// Legacy start_from_message is a message-count gate; the canonical knob is
	// trigger.min_messages. Fold one into the other so both work.
	if cfg.Trigger.MinMessages == 0 {
		cfg.Trigger.MinMessages = cfg.StartFrom
	}
	if err := applySummarizeTriggerDefaults(raw, &cfg.Trigger); err != nil {
		return nil, err
	}
	return &Summarize{
		level: cfg.SummaryLevel, keepLast: cfg.KeepLast,
		minTokens: cfg.MinTokens, resummarizeTokens: cfg.ResummarizeTokens,
		includeToolCalls: cfg.IncludeToolCalls, modelSource: cfg.Model.Source, modelClient: cfg.Model.Client(), trigger: cfg.Trigger,
		mode: parseMarkerMode(cfg.MarkerMode),
	}, nil
}

// summarizeDefaultRequestFrac and summarizeDefaultCacheState are summarize's OWN trigger defaults,
// and they are the answer to "when is compacting worth an LLM call plus the accuracy it costs".
//
// Measured on production traffic (snapshot 2026-09-04): every session that reached 90% of a 1M
// window and kept running went cold in that band repeatedly — minimum 2 full-prefix rewrites,
// median 7, maximum 46 — paying $571 that would have been $40 on a compacted prefix. On opus at
// 0.9 of 1M one cold rewrite is ~$5.88 against ~$0.50 compacted, and 110 of 126 rewrites landed
// on the way UP through the 90-99% band rather than at the ceiling, so a gate at 0.9 catches most
// of them before they are paid. Sessions that crossed 90% and then simply ended cost −$0.84 in
// total across the whole corpus, which is why the fill threshold can be this low.
//
// 0.9 IS NOT AN OPTIMUM, and should not be defended as one. Swept from 0.6 to 0.95 the trade is
// almost perfectly linear — every 5-point step down recovers a further $320-500 at $0.22-0.32 per
// extra turn spent on a compacted context — so there is no knee to sit in. 0.9 is the
// conservative end of a defensible range.
//
// WHAT STOPS A SESSION THAT NEVER ENTERS THE WINDOW: nothing here, deliberately. A session whose
// turns are seconds apart stays warm indefinitely and this trigger declines on every turn while
// the transcript grows. The ceiling is the CLIENT's: Claude Code runs its own compaction as it
// approaches its budget. A deployment whose client does not — anything driving a raw API — must
// set `cache_state: any` to get the old size-only behaviour, and both shipped example configs do
// exactly that. This is a documented requirement rather than a backstop in the code because a
// backstop firing at 0.99 would be compacting on a live prefix, which is the cost the whole
// design avoids.
const (
	summarizeDefaultRequestFrac = 0.9
	summarizeDefaultCacheState  = components.CacheStatePreExpiry
)

// The four names summarize files on Report.Events. Promoted to constants because a SECOND
// package now keys on them: the dashboard's compaction-episode walk (dash/compactepisode.go)
// identifies a fresh summary as an act carrying EventFreshSummary and a replay as one carrying
// any of the other three. A renamed string here would leave that walk matching nothing and
// reporting zero episodes — a silent zero, which is the failure mode this repo has already paid
// for twice (see Report.Replays on #176).
const (
	// EventFreshSummary marks a turn that PAID for a summary: a model call was made, a
	// checkpoint was written, and the transcript was restructured around new bytes. It is the
	// t0 of a compaction episode.
	EventFreshSummary = "fresh_summary"
	// The three replay names — a turn that re-emitted an existing checkpoint for free. Each
	// says WHY the replay happened, which is the operator-facing distinction: the trigger
	// permitted this turn and the tail had not grown (reused), the trigger declined it
	// (gated), or the store's rewind reserve refused a new stash (reserve exhausted).
	EventReusedCheckpoint                   = "reused_checkpoint"
	EventGatedReplayedCheckpoint            = "gated_replayed_checkpoint"
	EventReserveExhaustedReplayedCheckpoint = "reserve_exhausted_replayed_checkpoint"
)

// applySummarizeTriggerDefaults installs summarize's trigger defaults for keys the operator did
// not write, and validates cache_state.
//
// IT PROBES THE RAW YAML RATHER THAN TESTING FOR THE ZERO VALUE, and it has to. MinRequestFrac is
// a float64 whose zero means "no constraint", so `min_request_frac: 0` — the way an operator says
// "compact in the pre-expiry window at ANY size" — is indistinguishable from an absent key once
// decoded. Defaulting off the zero value would make that configuration unwritable. The probe is
// the same shape newExtractSweep uses to catch its banned keys before Decode's KnownFields check.
//
// The probe is deliberately loose: an unparseable document has already been rejected by
// components.Decode in the caller, so a failure here means only that the presence question cannot
// be answered, and the default is then the safer answer.
func applySummarizeTriggerDefaults(raw []byte, t *components.Trigger) error {
	present := map[string]bool{}
	if len(raw) > 0 {
		var probe struct {
			Trigger map[string]yaml.Node `yaml:"trigger"`
		}
		if err := yaml.Unmarshal(raw, &probe); err == nil {
			for k := range probe.Trigger {
				present[k] = true
			}
		}
	}
	if !present["min_request_frac"] {
		t.MinRequestFrac = summarizeDefaultRequestFrac
	}
	if t.CacheState == "" {
		t.CacheState = summarizeDefaultCacheState
	}
	// Refused rather than silently read as "any": a typo in the one key that decides WHEN this
	// component fires would otherwise turn the cache gate off and look like it was on.
	if !slices.Contains(components.CacheStates, t.CacheState) {
		return fmt.Errorf("summarize: trigger.cache_state %q is not one of %v", t.CacheState, components.CacheStates)
	}
	return nil
}

func (Summarize) Name() string                 { return "summarize" }
func (Summarize) Enabled(*components.Ctx) bool { return true }
func (*Summarize) NeedsModel() bool            { return true }

func (s *Summarize) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	msgs := req.Input
	// Keep msg0 (system/first) + the last keepLast; summarize the span between — with both
	// boundaries aligned so neither cuts inside a tool exchange. See summarizeSpan.
	headCount, start, end := summarizeSpan(msgs, s.keepLast)

	// THE GATES DECIDE WHETHER TO PAY FOR A *NEW* SUMMARY. THEY DO NOT DECIDE WHETHER THE SUMMARY
	// THIS SESSION ALREADY MADE STAYS IN THE BODY WE FORWARD. Those were one decision until the
	// trigger gained a cache-state condition, and conflating them is a cache-DESTRUCTIVE bug
	// rather than a lost saving — the same shape extract_llm_sweep's "NOT a return" comment
	// guards against.
	//
	// Why it only became a bug now: every earlier trigger was a size threshold, which is roughly
	// monotone — once a transcript is big enough to fire it stays big enough, so the gate kept
	// firing and the replay below kept happening. `cache_state: pre_expiry` is true on a small
	// fraction of turns by construction. With the old `return nil, nil` here, turn N emitted
	// [head, summary, tail] and turn N+1 emitted the FULL transcript, which diverges from the
	// bytes the provider just cached at the first summarized message and re-writes the whole
	// suffix at 1.25x fresh. Every quiet turn would pay the rewrite this component exists to
	// avoid, so the feature would have been a net loss rather than a smaller win.
	//
	// So: gate, record why, and keep going to the replay.
	sized := s.trigger.Fires(req, c)
	if !sized {
		rep.Gate("below_request_trigger")
	}
	// A fraction resolved against a GUESSED window fires at the wrong size — 0.9 of the
	// substring table's 200,000 is 180k on a model whose real window is 1M. Declining is the
	// only honest answer; see components.Trigger.FracResolvable.
	resolvable := s.trigger.FracResolvable(c)
	if !resolvable {
		rep.Gate("window_not_exact")
	}
	phase := c.CachePhase(s.trigger.PreExpiry())
	phased := s.trigger.CacheAllows(phase)
	if !phased {
		// NOT extract_llm_sweep's `not_in_pre_expiry_window`, deliberately. That component has
		// exactly one permitted state, so a name asserting which one is always true of it. This
		// trigger's state is CONFIGURED, and a deployment running `cache_state: cold` reading
		// "not in the pre-expiry window" off /stats would be told something false about its own
		// configuration. The phase it actually saw is appended, because "the cache state declined"
		// without saying which state is the sort of counter that can tell you THAT something
		// happened and never why.
		rep.Gate("cache_state_declined_" + phase.String())
	}
	fires := sized && resolvable && phased

	// STRUCTURAL, and safe to return on even though the replay below must otherwise always run:
	// end <= start means the transcript is at most keepLast+1 messages long, and both replay
	// paths require cp.CoveredCount >= 1, which puts their boundary past `end`. So no checkpoint
	// could ever have been reused here — this return is equivalent to falling through, not a
	// loss. A checkpointed session can only reach it by SHRINKING, in which case the covered
	// hash cannot match either and the provider's prefix is new anyway.
	if end <= start {
		rep.Skipped = true
		return nil, nil
	}

	// Reuse a prior summary if the covered prefix is unchanged and the tail since
	// that checkpoint is still small — no LLM call, and the summary message stays
	// byte-identical (KV-cache stable). Roll the checkpoint forward only once the
	// tail grows past resummarize_tokens.
	//
	// ABOVE the gates' effect and ABOVE model resolution, both deliberately. A replay makes no
	// model call, so a deployment with no summarizer configured must still get one — leaving the
	// model check above this sent the full transcript on every turn after the first, which is the
	// same byte-flip described above arriving by a different route.
	reusedMsgs, reusedKeys, reused, stale := s.tryReuse(c, rep, msgs, headCount, start, end)
	if reused {
		if len(reusedKeys) == 0 {
			rep.Irreversible = true // reused a non-full checkpoint (nothing stashed)
		}
		rep.Replay(EventReusedCheckpoint)
		req.Input = reusedMsgs
		return reusedKeys, nil
	}

	model := s.modelClient // config-pinned client wins
	if model == nil {
		model = c.Model.For(s.modelSource)
	}
	if model == nil {
		rep.Gate("no_model") // NeedsModel but none available → degrade gracefully
	}

	// No new summary this turn — gated, or nothing to summarize with. Fall back to the most
	// cache-stable shape available, which is the standing checkpoint when there is one. Both
	// reasons take the same path because they have the same remedy; the gates above already
	// recorded WHICH it was.
	if !fires || model == nil {
		if stale {
			return s.replayStale(c, rep, req, msgs, headCount, start, EventGatedReplayedCheckpoint)
		}
		// Nothing was ever emitted in a summarized shape for this session, so the full
		// transcript is not a flip of anything — it is what the provider already has.
		rep.Skipped = true
		return nil, nil
	}

	// Never summarize away content the agent EXPANDED. summarize replaces the span wholesale
	// rather than editing in place, so it is the one offloader skipReduce cannot protect — see
	// trimSpanForKeptVerbatim for why that is both a bounce loop and a broken pointer.
	//
	// ON THE FRESH PATH ONLY, and below tryReuse rather than above it. The cost argument for
	// keeping it below the gate is unchanged (a turn that pays no model call must not pay a
	// contentKey hash plus a store Get per span message, nor file kept_verbatim_after_expand for
	// a decision nothing made). What is new is that the trim LOWERS `end`, and tryReuse reads
	// `end` twice — the `boundary > end` bail and the tail-token measure — so running it first
	// could turn a reusable checkpoint into no reuse, and on a gated turn that means the full
	// transcript. The guard against expanded content must not be the thing that flips the cache.
	//
	// Accepted consequence: a turn where the trim would previously have forced a fresh summary
	// excluding expanded content now replays the standing checkpoint instead, which already
	// covers that content. That is a small weakening of the anti-bounce guard bought with byte
	// stability, and it was already true of the pre-existing reuse path.
	if trimmed := trimSpanForKeptVerbatim(msgs, start, end,
		func(text string) bool { return isKeptVerbatim(c, contentKey(text)) }); trimmed != end {
		rep.Gate(GateKeptVerbatim)
		end = trimmed
		if end <= start {
			// Nothing summarizable is left once expanded content is protected. Declining is the
			// outcome summarize already has for an empty span, and the safe direction.
			rep.Skipped = true
			return nil, nil
		}
	}

	span := msgs[start:end]
	if schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: span}) < s.minTokens {
		rep.Skipped = true
		return nil, nil
	}

	// THE RESERVE IS CONSULTED BEFORE THE MODEL CALL, not after it.
	//
	// The span is all the marker key depends on (key = hashKey(spanJSON)), and the payload is
	// the span itself — so nothing about the stash needs the summary to exist. The check used
	// to sit after s.summarize, which meant a saturated reserve paid the call (measured at ~57k
	// prompt tokens) and threw the result away, then did it again on the NEXT turn, and every
	// turn after, because a refusal saves no checkpoint and so changes nothing about the next
	// turn's inputs. Asking first turns an unbounded stream of wasted calls into a skip.
	//
	// A probe, not a claim: StashRoom reserves nothing, so the real PutStash below can still
	// refuse if another session took the slot in between. That is a rare race rather than the
	// steady state, and it lands on the same refusal path. Claiming the slot up here instead
	// would leak one payload's worth of reserve every time the model call then failed — the
	// resource this whole change exists to protect.
	mode := effectiveMode(c, s.mode)
	var spanJSON []byte
	var key string
	if mode == markerFull {
		var err error
		if spanJSON, err = json.Marshal(span); err != nil {
			return nil, err
		}
		key = hashKey(string(spanJSON))
		if !store.StashRoom(c.Store, len(spanJSON)) {
			return s.refuse(c, rep, req, msgs, headCount, start, stale)
		}
	}

	ctx, cancel := context.WithTimeout(c.Ctx, summarizeCallTimeout)
	defer cancel()
	summary, err := s.summarize(ctx, model, span, conversationGoal(req))
	if err != nil {
		// Classify before returning. Our own ctx is the reliable signal: the parent
		// request may still be healthy while THIS component's budget expired, and the
		// http client wraps the cause, so errors.Is walks to it.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			atomic.AddInt64(&summarizeTimeouts, 1)
		} else {
			atomic.AddInt64(&summarizeErrors, 1)
		}
		return nil, err // fail-open: the pipeline reverts this component
	}
	if strings.TrimSpace(summary) == "" {
		rep.Skipped = true
		return nil, nil
	}

	// Stash the replaced span so expand can restore it — full mode only. In
	// summary/off there is no restoration; flag the deliberate lossy drop so the
	// pipeline's dropped-without-stash guard permits it.
	// full is reversible only if the store persists the stash; otherwise degrade
	// to an irreversible off-style drop (no unresolvable marker).
	if mode == markerFull {
		// A summary REPLACES the span it covers, so the marker in the summary text is the
		// only route back to it. If the store's rewind reserve cannot hold the span, this
		// component must not summarize at all: unlike the per-message offloaders it cannot
		// leave "this message" verbatim, so refusing means skipping the whole checkpoint.
		//
		// Reachable despite the StashRoom probe above (the probe claims nothing and another
		// session can take the slot in between), which is why the refusal path still exists
		// here — the probe removes the steady-state waste, not the race.
		if !store.PutStash(c.Store, key, spanJSON) {
			return s.refuse(c, rep, req, msgs, headCount, start, stale)
		}
	} else {
		rep.Irreversible = true
	}

	summaryText := summaryWrapper(summary, key, mode)
	// USER, not system. The summary is injected context, and Anthropic will not accept a
	// system-role message in the middle of `messages`: system content belongs in the
	// top-level `system` field, and a system role inside the array must precede an
	// assistant message or end it. This component emits [msgs[0], summary, tail...], so
	// when msgs[0] is itself the system prompt — the normal case — a system-role summary
	// lands at index 1 and the provider rejects the whole request:
	//
	//	400 messages.1: role 'system' must precede an 'assistant' message or end the array
	//
	// Measured on live LOCA-bench traffic: every task that triggered a summarization failed
	// this way, including in an arm with NO other component enabled, so it is this
	// component's own output and not a pipeline interaction. It went unnoticed because
	// every prior measurement replayed through /compact, which never forwards upstream and
	// therefore never has the body validated by a provider.
	//
	// A user-role message carrying the summary is both valid and conventional — it is what
	// Claude Code's own compaction does.
	summaryMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&summaryMsg, summaryText)

	// Checkpoint: this summary subsumes the leading span (len(span) messages from
	// index 1). A later turn appends messages, so this same prefix stays stable.
	saveCheckpoint(c, sumCheckpoint{
		SummaryMsg: summaryText, CoveredCount: end - start,
		CoveredHash: spanHash(span), Key: key,
	})

	// The fresh path's own event, filed HERE rather than at the top of the path: everything
	// above this line can still decline (a blank summary, a refused stash, an empty span after
	// the expand trim), and an event filed before those would name a summary that was never
	// emitted. This is the first point at which a new summary is committed.
	//
	// An Event, not a Replay: this turn spent money. Report.Replay is for the free paths, and
	// keeping the two apart is what lets the episode walk below tell a paid t0 from the
	// amortization that follows it.
	rep.Event(EventFreshSummary)

	// [msg0, summary, last-K] — reassign; apply.Body rebuilds losslessly.
	out := make([]bschemas.ChatMessage, 0, 2+s.keepLast)
	out = append(out, msgs[:headCount]...)
	out = append(out, summaryMsg)
	out = append(out, msgs[end:]...)
	// Removing a span can orphan the tail's leading tool_result blocks; a provider rejects
	// the whole request if it does. See dropOrphanedToolResults.
	if repaired, n := dropOrphanedToolResults(out); n > 0 {
		out = repaired
	}
	req.Input = out
	if key != "" {
		return []string{key}, nil
	}
	return nil, nil
}

// refuse is what summarize does when the rewind reserve will not hold the span: no new
// checkpoint is made, and the transcript is left in the most cache-stable shape available.
//
// That last part is the whole reason this is a function rather than `return nil, nil`. Plain
// `nil, nil` sends the transcript FULL, and when this session had already emitted a checkpoint
// on earlier turns — [msg0, summary, tail] — that is a flip of already-cached content in the
// exact direction #188 exists to avoid, triggered by #188's own new refusal path. The provider
// re-writes the whole suffix at ~11.5x the read price, for a request that saves nothing.
//
// So when a checkpoint is STALE-BUT-VALID (tryReuse confirmed the covered prefix is
// byte-unchanged and declined only because the tail grew past resummarize_tokens), re-emit it.
// Rolling it forward was an improvement that is now unavailable; the old checkpoint's bytes are
// still the ones the provider has cached. A checkpoint that is absent or genuinely diverged has
// nothing to fall back to, and there the full transcript is correct: nothing was cached in the
// summarized shape.
func (s *Summarize) refuse(c *components.Ctx, rep *components.Report, req *bschemas.BifrostChatRequest,
	msgs []bschemas.ChatMessage, headCount, start int, stale bool) ([]string, error) {
	stashRefusals.Add(1)
	rep.Gate("stash_reserve_exhausted")
	if !stale {
		rep.Skipped = true
		return nil, nil
	}
	return s.replayStale(c, rep, req, msgs, headCount, start, EventReserveExhaustedReplayedCheckpoint)
}

// replayStale re-emits the standing checkpoint on a turn that will not produce a new one.
//
// Extracted from refuse because there are now TWO reasons to be in this position and exactly one
// correct response to both: the rewind reserve would not hold a fresh span (refuse), or the
// trigger declined this turn / no summarizer is available (Offload's gated path). Sharing the
// body is not tidiness — it is the same argument emitCheckpoint's own comment makes about the
// fresh and replayed paths. Two callers emitting the summary through two code paths is two
// chances for them to emit different bytes for the same decision, and different bytes here is
// precisely the cache-write the checkpoint exists to prevent.
//
// The caller must have been told `stale` by tryReuse, which means the covered prefix's hash was
// verified there: this function's job is to re-emit, not to re-decide. It still re-checks what it
// must, because it re-loads the checkpoint rather than being handed one.
func (s *Summarize) replayStale(c *components.Ctx, rep *components.Report, req *bschemas.BifrostChatRequest,
	msgs []bschemas.ChatMessage, headCount, start int, event string) ([]string, error) {
	cp, ok := loadCheckpoint(c)
	if !ok || cp.CoveredCount <= 0 {
		rep.Skipped = true
		return nil, nil // nothing to fall back to
	}
	boundary := start + cp.CoveredCount
	if boundary > len(msgs) {
		rep.Skipped = true
		return nil, nil
	}
	// The stash refresh, and the reason it is a refresh rather than a new claim: this payload
	// was accepted on the turn the checkpoint was made, so a key already present is retained
	// whatever the reserve says (see store.Stasher) and this cannot be the thing that refuses.
	// A false answer means the payload has since expired and the replayed marker is dangling —
	// counted as stash_missing, and the replay proceeds because the summary text must stay
	// byte-identical to the turn that created it.
	if cp.Key != "" {
		if b, err := json.Marshal(msgs[start:boundary]); err == nil {
			commitRefresh(c, rep, markerFull, cp.Key, string(b))
		}
	}
	out, keys := s.emitCheckpoint(cp, msgs, headCount, boundary)
	if len(keys) == 0 {
		rep.Irreversible = true
	}
	// The component DID act — it rewrote the transcript — so it is not Skipped. Saying
	// otherwise would report a turn that emitted a summary as a turn that did nothing.
	replaced := len(msgs) - len(out)
	if replaced <= 0 {
		rep.Skipped = true
		return nil, nil
	}
	// A MESSAGE-count guard is not a TOKEN guard, and only the second one is what the pipeline
	// judges. emitCheckpoint can return fewer messages but MORE tokens — a short covered span
	// replaced by a verbose summary — and the pipeline's never-worse rule then reverts this
	// component and forwards the full transcript, which is the byte-flip the replay existed to
	// avoid, arriving silently. Declining here reaches the same wire bytes with a name attached,
	// which is the difference between a diagnosable decline and an invisible one.
	if schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: out}) >=
		schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: msgs}) {
		rep.Gate("replay_would_not_shrink")
		rep.Skipped = true
		return nil, nil
	}
	// Counted as a REPLAY, not as an act: no model call, no new spend, the same bytes an earlier
	// turn already emitted. `acted` is Saved() > 0 and a replay saves tokens, so without this a
	// component amortizing old work would be indistinguishable from one paying for new work —
	// two readings with opposite consequences (see Report.Replays, #176). Under a cache-state
	// trigger the replay is the DOMINANT activation, so this is the difference between /stats
	// describing this component correctly and describing it backwards.
	rep.Replay(event)
	req.Input = out
	return keys, nil
}

// emitCheckpoint builds [head, priorSummary, msgs[boundary:]] from a checkpoint, with the
// boundary walked past any leading tool messages and orphaned tool_results dropped. Shared by
// tryReuse and the refusal fallback so a replayed checkpoint is byte-identical whichever path
// emits it — a difference between them would itself be a cache flip.
func (s *Summarize) emitCheckpoint(cp sumCheckpoint, msgs []bschemas.ChatMessage,
	headCount, boundary int) ([]bschemas.ChatMessage, []string) {
	// USER for the same reason as the fresh-summary path: a system role at index 1 is rejected
	// by the provider. The replayed checkpoint must match that shape exactly, or a replayed
	// turn would emit different bytes from the turn that created it.
	summaryMsg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&summaryMsg, cp.SummaryMsg)
	// The replayed boundary must respect exchange atomicity exactly as the fresh path does,
	// or a replayed turn emits different bytes from the turn that created it.
	for boundary < len(msgs) && msgs[boundary].Role == bschemas.ChatMessageRoleTool {
		boundary++
	}
	out := make([]bschemas.ChatMessage, 0, 2+(len(msgs)-boundary))
	out = append(out, msgs[:headCount]...)
	out = append(out, summaryMsg)
	out = append(out, msgs[boundary:]...)
	// Removing a span can orphan the tail's leading tool_result blocks; a provider rejects
	// the whole request if it does. See dropOrphanedToolResults.
	if repaired, n := dropOrphanedToolResults(out); n > 0 {
		out = repaired
	}
	if cp.Key != "" {
		return out, []string{cp.Key}
	}
	return out, nil
}

// tryReuse re-emits the previous summary if (1) a checkpoint exists, (2) the
// covered prefix (msgs[1:1+CoveredCount]) is byte-unchanged, and (3) the tail
// since that boundary is below resummarize_tokens. It returns the rebuilt
// [msg0, priorSummary, msgs[boundary:]] and the (refreshed) stash key. No LLM
// call. ok=false means "re-summarize fresh".
//
// stale reports WHICH kind of ok=false this was: true means the checkpoint is still VALID and
// only the size test declined it (the tail grew past resummarize_tokens), so re-emitting it is
// still byte-correct. The fresh path needs that distinction, because if it cannot produce a new
// checkpoint its only cache-safe fallback is the old one — see the refusal path in Offload.
func (s *Summarize) tryReuse(c *components.Ctx, rep *components.Report, msgs []bschemas.ChatMessage,
	headCount, start, end int) (out []bschemas.ChatMessage, keys []string, ok, stale bool) {
	cp, ok := loadCheckpoint(c)
	if !ok || cp.CoveredCount <= 0 {
		return nil, nil, false, false
	}
	boundary := start + cp.CoveredCount
	if boundary > end { // covered prefix would overlap the kept tail — can't reuse
		return nil, nil, false, false
	}
	covered := msgs[start:boundary]
	if spanHash(covered) != cp.CoveredHash {
		return nil, nil, false, false // prefix diverged (different session / edited) → fresh
	}
	// The un-summarized middle since the checkpoint (excludes the kept last-K).
	//
	// `resummarizeTokens <= 0` means "roll forward on every eligible turn", and it is checked HERE
	// rather than at the top of this function so that it produces STALE rather than nothing. The
	// distinction did not matter while the caller always had a model call available: it fell
	// through to the fresh path either way. It matters now, because a caller that will not pay
	// for a roll-forward has to be told a valid checkpoint exists — otherwise a config carrying
	// `resummarize_tokens: 0` replays nothing and oscillates between the full and summarized
	// shapes on alternating turns, which is the worst of both.
	if s.resummarizeTokens <= 0 ||
		schema.MessagesTokens(&bschemas.BifrostChatRequest{Input: msgs[boundary:end]}) >= s.resummarizeTokens {
		// STALE, not invalid: the prefix hash matched just above, so this checkpoint is still a
		// faithful summary of msgs[start:boundary] and re-emitting it produces the same bytes
		// earlier turns sent. Rolling it forward is merely BETTER, so a fresh attempt that
		// cannot complete may fall back to it instead of sending the transcript full.
		return nil, nil, false, true
	}
	// Refresh the stashed original span so expand keeps resolving it (full-mode
	// checkpoints only — summary/off never stashed, so Key is empty).
	if cp.Key != "" {
		if b, err := json.Marshal(covered); err == nil {
			// A refresh of a key already present always succeeds (see store.Stasher), so a
			// false answer here means the payload had ALREADY left the store — the marker in
			// the replayed summary is dangling and cannot be un-dangled, because the summary
			// text must stay byte-identical to the turn that created it. Counted as
			// stash_missing (a broken promise) rather than stash_refused (a removal declined);
			// the alternative, re-summarizing to avoid the marker, is the cache-write this
			// checkpoint exists to prevent.
			commitRefresh(c, rep, markerFull, cp.Key, string(b))
		}
	}
	emitted, ks := s.emitCheckpoint(cp, msgs, headCount, boundary)
	return emitted, ks, true, false
}

// summarize builds the trajectory string and asks the model once (bounded retry).
// goal is the current task + recent turns, so the summary is grounded in what the
// agent is actually trying to do — not a blind digest of the middle.
func (s *Summarize) summarize(ctx context.Context, model components.Model, span []bschemas.ChatMessage, goal string) (string, error) {
	sys := summarizerSystemPrompt
	if !s.includeToolCalls {
		sys += summarizerMaskedNote
	}
	user := strings.Replace(summarizerUserPrompt, "{trajectory}", trajectoryString(span, s.includeToolCalls), 1)
	if g := strings.TrimSpace(goal); g != "" {
		user = "CURRENT TASK / QUESTION (summarize toward this):\n" + g + "\n\n" + user
	}
	if suffix := summaryLevelSuffix[s.level]; suffix != "" {
		user += "\n" + suffix
	}
	prompt := sys + "\n\n" + user
	var lastErr error
	for i := 0; i < 3; i++ {
		out, err := model.Complete(ctx, prompt)
		if err != nil {
			lastErr = err
			// The three attempts share ONE deadline (the ctx is built by the caller,
			// outside this loop), so once it has expired every retry fails instantly
			// and only obscures the cause. Stop and report the deadline.
			if ctx.Err() != nil {
				break
			}
			continue
		}
		return ensureSummaryTags(out), nil
	}
	return "", lastErr
}

// trajectoryString renders the span as "[role]\n{content}" blocks. When tool
// calls are excluded, tool-role content is replaced by a placeholder.
func trajectoryString(span []bschemas.ChatMessage, includeToolCalls bool) string {
	var b strings.Builder
	for i, m := range span {
		if i > 0 {
			b.WriteString("\n\n")
		}
		content := schema.MessageText(m)
		if !includeToolCalls && m.Role == bschemas.ChatMessageRoleTool {
			content = "<masked_tool_output>"
		}
		b.WriteString("[")
		b.WriteString(string(m.Role))
		b.WriteString("]\n")
		b.WriteString(content)
	}
	s := b.String()
	// Keep the trajectory within the model's context window: if it's huge, keep
	// the most recent portion (the full original span is stashed for expand).
	if len(s) > maxTrajectoryChars {
		s = "…[older trajectory omitted from this summary; full original preserved for expand]\n\n" +
			s[len(s)-maxTrajectoryChars:]
	}
	return s
}

func ensureSummaryTags(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if !strings.Contains(s, "<summary>") {
		s = "<summary>\n" + s
	}
	if !strings.Contains(s, "</summary>") {
		s = s + "\n</summary>"
	}
	return s
}

// summaryWrapper is the synthetic system message that replaces the span. In full
// mode it carries the <<cg:HASH>> marker so the expand tool can recover the full
// original trajectory; in summary mode a non-resolvable ⟪cg⟫ sentinel; in off
// mode nothing (the summary text itself is the only trace).
func summaryWrapper(summary, key string, mode markerMode) string {
	body := "=== History Summary ===\n" +
		"The earlier trajectory is summarized below.\n\n" +
		summary + "\n\n" +
		"Use this summary as the older context, and use the following messages as the most recent context. " +
		"Continue the task accordingly. Do not summarize the conversation again."
	switch mode {
	case markerFull:
		return body + "\n" + expand.Marker(key) + " [full earlier trajectory: call " + expand.ToolName + "]"
	case markerSummary:
		return body + "\n" + expand.SummaryMarker
	default: // off
		return body
	}
}

// Prompts ported verbatim from CE-Manager (src/ce_manager/prompts/summarizer.py).
const summarizerSystemPrompt = "You analyze long agent trajectories with tool calls and produce compact, factual summaries. Do not guess or invent information."

const summarizerMaskedNote = " Notice, all the tool calls content have been removed from the trajectory, you should base your summary only on the remaining content."

const summarizerUserPrompt = `You are an expert at analyzing conversation history and extracting relevant information. Your task is to thoroughly evaluate the conversation history and current question to provide a comprehensive summary that will help answer the question.

Task Guidelines:
1. Information Analysis
• Carefully analyze the conversation history to identify truly useful information.
• Focus on information that directly contributes to answering the question.
• Do NOT make assumptions, guesses, or inferences beyond what is explicitly stated in the conversation.
• If information is missing or unclear, do NOT include it in your summary.

2. Summary Requirements
• Extract only the most relevant information that is explicitly present in the conversation.
• Synthesize information from multiple exchanges when relevant. Only include information that is certain and clearly stated in the conversation.
• Do NOT output or mention any information that is uncertain, insufficient, or cannot be confirmed from the conversation.

3. Output Format Your response should be structured as follows:
<summary>
• Essential Information: [Organize the relevant and certain information from the conversation history that helps address the question.]
</summary>

Strictly avoid fabricating, inferring, or exaggerating any information not present in the conversation. Only output information that is certain and explicitly stated.

Trajectory: {trajectory}`

var summaryLevelSuffix = map[string]string{
	"concise":         "Please generate a concise summary",
	"regular":         "Please generate a comprehensive and useful summary",
	"highly_detailed": "Please generate a highly detailed, fully comprehensive, explicitly grounded summary that includes every relevant and certain piece of information from the conversation.",
}

func init() {
	f := []components.Field{
		{Key: "summary_level", Type: components.FieldEnum, Default: "regular",
			Options: []string{"concise", "regular", "highly_detailed"},
			Hint:    "How much detail the summary is asked for. The keys of summaryLevelSuffix — an unrecognised value silently produces no level instruction at all."},
		{Key: "keep_last", Type: components.FieldInt, Default: 3,
			Hint: "Messages kept verbatim at the tail; only what precedes them is summarized."},
		{Key: "start_from_message", Type: components.FieldInt, Default: 6,
			Hint: "Legacy message-count gate, folded into trigger.min_messages when that is unset. Set trigger.min_messages instead."},
		{Key: "min_tokens", Type: components.FieldInt, Default: 500, Min: 1,
			Hint: "Minimum content tokens in the span before a summary is worth an LLM call."},
		{Key: "resummarize_tokens", Type: components.FieldInt, Default: 6000,
			Hint: "Once a summary exists, reuse it with no model call until the tail since that checkpoint grows past this many tokens. 0 = re-summarize every eligible turn."},
		{Key: "include_tool_calls", Type: components.FieldBool,
			Hint: "Include tool calls and their results in the trajectory handed to the summarizer."},
		markerModeField(),
	}
	f = append(f, modelFields("model")...)
	// The shared trigger descriptors, with the two defaults THIS component overrides in its
	// constructor. Field.Default is documented as "what an ABSENT key means to the component", and
	// what an absent key means here is not what it means to extract_llm — so a descriptor copying
	// the shared default would be advertising a figure summarize does not use. The settings page
	// draws straight from these, and form.go's normalize() writes an enum's Default back into the
	// saved document for an unset value, so a wrong one here does not merely mislabel the page: it
	// persists a cache_state summarize never chose.
	tf := components.TriggerFields("trigger")
	for i := range tf {
		switch tf[i].Key {
		case "trigger.cache_state":
			tf[i].Default = summarizeDefaultCacheState
		case "trigger.min_request_frac":
			tf[i].Default = summarizeDefaultRequestFrac
		}
	}
	components.RegisterFields("summarize", summarizeConfig{}, append(f, tf...))
}
