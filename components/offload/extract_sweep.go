package offload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
	"gopkg.in/yaml.v3"
)

func init() { components.Register("extract_llm_sweep", newExtractSweep) }

// ExtractSweep is the COLD-SWEEP ADJUDICATOR: on a turn whose prompt cache has expired it asks a
// cheap model, one output at a time, whether each candidate is still needed — and either keeps it
// VERBATIM or removes it, leaving a shape descriptor plus a recoverable marker.
//
// WHY IT IS A SEPARATE COMPONENT FROM extract_llm. The two situations want different operations, and
// running one operation in both is what this split ends. On a warm turn extract_llm works the
// uncached tail: the output is recent, the agent may still want most of it, and a smaller version of
// it is more useful than none of it. On a cold sweep the same code rewrote outputs DEEP IN HISTORY,
// which is the wrong operation on either branch of the only question that matters — deep history is
// either still load-bearing, in which case rewriting corrupts content the model has already reasoned
// about, or it is spent, in which case the answer is to remove it rather than to produce a smaller
// version of something nobody will read.
//
// WHY THE COLD TURN IS WORTH ITS OWN COMPONENT AT ALL. On a turn whose cache has expired the provider
// re-bills the ENTIRE transcript as cache creation at 1.25x the fresh rate. MEASURED on this
// deployment over 1.4 days: those turns were 4% of requests and 31% of spend ($360 of $1,173, ~$1.64
// each against $0.144 warm), and the shipped pipeline saved 0.015% of it. Two things are true only
// there — removing a token is worth 12.5x what it is worth warm, and touching deep history is free
// because there is no live cached prefix left to invalidate.
//
// WHAT IT NEVER DOES. It selects no compaction strategy, produces no rewritten text, and there is no
// reply field a model could return content through. `strategy`, `rewrite`, `aggressiveness` and
// `max_chars` are therefore not merely defaulted differently here, they are meaningless, and writing
// one is a config error rather than a silently ignored key (see newExtractSweep).
type ExtractSweep struct {
	minTokens      int
	minIdleSeconds int
	maxCalls       int

	modelSource   string
	modelClient   components.Model
	modelName     string
	modelMaxInput int

	mode        markerMode
	ctxMode     contextMode
	ctxMessages int

	// gate is the shared economic gate. Its arithmetic is calibrated on COMPACTION, where a call
	// removes a fraction of an output; a drop removes all of it, so the break-even for this
	// component is a different number and an unmeasured one. Rather than invent a prior, the
	// sweep carries its OWN ratioTracker (below) and lets the same exploration budget learn it:
	// what this workload's adjudicator actually removes per candidate is the drop RATE, and the
	// tracker measures exactly that. See docs/proposals/sweep-adjudicator.md, open question 3.
	gate    bool
	pricing cheapmodel.Pricing
	// ratios is deliberately NOT shared with extract_llm's. The two paths remove different
	// fractions of a candidate — a partial rewrite against all-or-nothing — so pooling them would
	// price each on the other's history, which is the same pooling error contentclass.go exists to
	// undo one level down.
	ratios ratioTracker
}

// extractSweepConfig is the sweep's whole surface. Note what is absent and why: see the
// component doc, and the rejected-key probe in newExtractSweep.
type extractSweepConfig struct {
	// MinTokens is the sweep's OWN per-output floor (0 = defaultSweepFloor). Lower than the hot
	// path's, because on this turn every candidate is being re-billed at the write rate anyway.
	MinTokens int `yaml:"min_tokens"`
	// MinIdleSeconds demands MORE idle time than the provider TTL implies (0 = just the TTL).
	// Raises the bar, never lowers it: the TTL check is the correctness condition and this is only
	// extra caution.
	MinIdleSeconds int `yaml:"min_idle_seconds"`
	// MaxCalls caps model calls for one sweep (0 = defaultSweepMaxCalls; -1 = unlimited).
	//
	// It used to default to unlimited on the cold_cache block this replaces, on the reasoning that
	// a sweep runs once per idle gap on a turn that is already expensive. MEASURED, that reasoning
	// was wrong in the way unbounded spend paths usually are: one production request made 27 calls
	// against a tenant whose llm_max_per_request was 2, spent $0.229 and added 76.6 s to a turn
	// whose upstream took 33.5 s — context-guru was 2.3x slower than the model it was saving money
	// on. The sweep deliberately draws on no other component's caps, so this is its ONLY brake.
	MaxCalls int `yaml:"max_calls"`

	Model      modelConfig `yaml:"model"`
	MarkerMode string      `yaml:"marker_mode"`
	// Context selects how much conversation the adjudication prompt carries: goal | recent
	// (default) | full. Open question 2 in the proposal: deciding "needed by NOTHING" plausibly
	// requires seeing the whole transcript, which is the expensive mode. The default is NOT `full`
	// because that pairing is the one this component's predecessor was measured losing money on —
	// 99% of the sweep's prompt was a copy of the transcript it was compacting, sent once per
	// candidate — and because nothing has yet measured whether a spent-ness judgement needs it.
	Context         string `yaml:"context"`
	ContextMessages int    `yaml:"context_messages"`
	ModelMaxInput   int    `yaml:"model_max_input_tokens"`
	EconomicGate    *bool  `yaml:"economic_gate"`
}

// defaultSweepFloor is the per-output floor when none is configured. It is the cold_cache block's
// own default, carried over: on this turn every candidate re-bills at the write rate whatever we do,
// so the bar for "worth a call" is genuinely lower than on a warm turn.
const defaultSweepFloor = 1000

// defaultSweepMaxCalls bounds one sweep when the operator names no cap. It is llmConcurrency so a
// sweep costs ONE round of calls: the (k+1)th call cannot start until one of the first k returns,
// and at a 7.1 s median that is where a sweep starts costing more wall clock than the turn it is
// shortening.
const defaultSweepMaxCalls = llmConcurrency

// sweepBannedKeys are the compaction knobs that have no meaning for an adjudicator, and the reason
// each one does not apply. They are refused rather than ignored: a silently accepted `rewrite: false`
// would read as "verified deletion-only is on" when nothing is being rewritten in the first place,
// and an operator migrating a cold_cache config by hand has no other way to find out.
//
// Detected on a SEPARATE probe struct rather than as fields of extractSweepConfig, because a field
// there would have to be declared to the settings form (components/all's field contract), which
// would put a knob on the page whose only behaviour is to fail.
var sweepBannedKeys = []struct {
	key, why string
}{
	{"strategy", "an adjudicator selects no compaction strategy — it returns a verdict, not a program"},
	{"rewrite", "nothing is rewritten, so there is no rewrite to validate; the output is kept verbatim or removed"},
	{"aggressiveness", "there is no compaction target to teach: the only question asked is whether the output is spent"},
	{"max_chars", "no projection window exists — a dropped output leaves a shape descriptor, not a truncation"},
}

func newExtractSweep(raw []byte) (components.Component, error) {
	// The banned keys FIRST, before components.Decode's KnownFields rejects them with a generic
	// yaml message. The whole point is that the error names the reason.
	if len(raw) > 0 {
		var probe map[string]yaml.Node
		if err := yaml.Unmarshal(raw, &probe); err == nil {
			for _, b := range sweepBannedKeys {
				if _, present := probe[b.key]; present {
					return nil, fmt.Errorf("extract_llm_sweep: %s does not apply here: %s "+
						"(it belongs to extract_llm, the warm/tail compactor)", b.key, b.why)
				}
			}
		}
	}
	cfg := extractSweepConfig{}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	ctxMode, err := parseContextMode(cfg.Context)
	if err != nil {
		return nil, fmt.Errorf("extract_llm_sweep: %w", err)
	}
	if cfg.MinTokens <= 0 {
		cfg.MinTokens = defaultSweepFloor
	}
	switch {
	case cfg.MaxCalls == 0:
		cfg.MaxCalls = defaultSweepMaxCalls
	case cfg.MaxCalls < 0:
		cfg.MaxCalls = 0 // an explicit opt-out of the bound
	}
	gate := true
	if cfg.EconomicGate != nil {
		gate = *cfg.EconomicGate
	}
	return &ExtractSweep{
		minTokens: cfg.MinTokens, minIdleSeconds: cfg.MinIdleSeconds, maxCalls: cfg.MaxCalls,
		modelSource: cfg.Model.Source, modelClient: cfg.Model.Client(),
		modelName: cfg.Model.Model, modelMaxInput: cfg.ModelMaxInput,
		mode: parseMarkerMode(cfg.MarkerMode), ctxMode: ctxMode, ctxMessages: cfg.ContextMessages,
		gate: gate, pricing: cheapmodel.PricingFromEnv(),
	}, nil
}

func (*ExtractSweep) Name() string                 { return "extract_llm_sweep" }
func (*ExtractSweep) Enabled(*components.Ctx) bool { return true }

// sweeping reports whether this turn gets the sweep: the provider's cache has certainly expired and
// any extra idle requirement is met. Everything the sweep unlocks — acting at depth, pricing at the
// write rate — is only correct when the cache really is gone.
func (e *ExtractSweep) sweeping(c *components.Ctx) bool {
	if c == nil || !c.ColdCache {
		return false
	}
	return e.minIdleSeconds <= 0 || c.IdleMs >= int64(e.minIdleSeconds)*1000
}

// inputLimit is the adjudication model's input budget. Same resolution order as extract_llm's: the
// operator's pin, then the static window table for a named model, then the proxied model's own
// window when the sweep runs on the incoming client.
func (e *ExtractSweep) inputLimit(c *components.Ctx) int {
	if e.modelMaxInput > 0 {
		return e.modelMaxInput
	}
	if e.modelName != "" {
		if w, ok := staticWindows.Window(c.Ctx, e.modelName); ok {
			return w
		}
		return unknownModelInputLimit
	}
	if e.modelSource != "config" && c.CtxWindow > 0 {
		return c.CtxWindow
	}
	return unknownModelInputLimit
}

// sweepUnusableSamples bounds how many unparseable replies get logged in full. Process-wide, because
// the question it answers — what is the model actually emitting? — is answered by the first few.
//
// Six rounds of the predecessor's failures were diagnosed by inferring a cause from gate counters,
// and every inference was at least partly wrong. A counter can say THAT a reply was unusable; only
// the text says WHY.
var sweepUnusableSamples atomic.Int64

// maxSweepUnusableSamples bounds it in count as well as length, because a systematic failure would
// otherwise flood the log with transcript content lifted out of the replies.
const maxSweepUnusableSamples = 5

func (e *ExtractSweep) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	sweeping := e.sweeping(c)
	if !sweeping {
		// NOT a return. The frozen replays below still run, and they are the reason a sweep's
		// saving survives past the turn that earned it: without them a warm turn would re-send
		// every dropped output verbatim, undoing the removal AND breaking the byte-stability of
		// the prefix the provider is caching.
		rep.Gate("not_a_cold_sweep")
	}
	model := e.modelClient
	if model == nil && sweeping {
		var usedSource string
		// ForModelSource, not For: `model.model` names the model to ADJUDICATE with even when the
		// source is the incoming request. Without it, a sweep on a coding agent adjudicates with
		// the agent's frontier model and the arithmetic never closes — a real cold sweep measured
		// here cut the provider bill by $0.63 and spent $1.25 of opus doing it.
		model, usedSource = c.Model.ForModelSource(e.modelSource, e.modelName)
		if model != nil && usedSource != "" && e.modelSource != "config" && usedSource == "config" {
			// A DIFFERENT credential on a DIFFERENT endpoint, so it cannot be silent: an operator
			// whose config says `source: incoming` would otherwise have no way to learn that none
			// of their calls went there.
			rep.Gate("model_source_fell_back_to_config")
		}
	}

	goal := conversationContext(req, e.ctxMode, e.ctxMessages)
	// The transcript, flattened, so a claimed obligation quote is VERIFIED against what the agent
	// was actually told rather than trusted. This is the only remaining signal that the model is
	// inventing, because nothing else it returns is content.
	flat := flattenTranscript(req)

	pricing := e.pricing
	if e.modelName == "" && e.modelSource != "config" && !c.SelfRates.Zero() {
		// The sweep is running on the agent's own model, so the agent's rates are the real ones.
		// The built-in default is haiku-class, and on a sonnet-class agent it understates every
		// call by about 3x — measured, a call recorded at $0.0276 had cost about $0.083.
		pricing = ratesPricing(c.SelfRates)
	} else if e.modelName != "" && c.RatesFor != nil {
		if rates := c.RatesFor(e.modelName); !rates.Zero() {
			pricing = ratesPricing(rates)
		}
	}
	callModel := e.modelName
	if callModel == "" {
		callModel = c.ModelName
	}

	inputLimit := e.inputLimit(c)
	promptOverhead := extractPromptOverheadTokens + schema.TextTokens(goal)
	goalOverhead := promptOverheadTokens + schema.TextTokens(goal)
	val := savedTokenValue(c)
	ratio := e.ratios.ratio()
	turnsSoFar := len(req.Input)

	type cand struct {
		i          int
		content    string
		id         string
		gateReason string
	}
	var cands []cand
	var keys []string
	changed := 0

	// Phase 1 (serial): replay frozen decisions at any depth, and collect the candidates that
	// still need a call.
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
		// If the agent recently EXPANDED this content, leave it verbatim — removing it again
		// would just trigger another expand.
		if isKeptVerbatim(c, id) {
			rep.Gate("kept_verbatim_after_expand")
			continue
		}
		// SAME-SESSION REPLAY, and it bypasses the depth gate legitimately: this session already
		// sent these exact bytes on an earlier turn, so the provider's cached prefix holds the
		// REMOVED form and replaying it is byte-identical.
		//
		// The stored value is the descriptor, which sweepDescriptor derives from the content
		// alone. That is what makes the replay safe in the sense TailOnlyCold's doc requires: the
		// DECISION came from a model, but the REPLACEMENT is a pure function of (content, config),
		// so a replay can never emit different bytes than the turn that decided it.
		if cached, hit := getResult(c, id); hit {
			metrics.RecordExtractionCacheLookup(true)
			if saved := schema.TextTokens(content) - schema.TextTokens(cached.Projected); saved > 0 {
				metrics.RecordExtractionValue(float64(saved) * val.repeatPerToken)
			}
			if k, ok := applySweepDrop(c, rep, e.mode, msg, content); ok {
				changed++
				if k != "" {
					keys = append(keys, k)
				}
				rep.Gate("reapplied_same_session")
			}
			continue
		}
		if !sweeping || model == nil {
			// A warm turn, or no client. Either way no NEW decision is taken; the replays above
			// already ran, which is all a warm turn has to do.
			if sweeping {
				rep.Gate("no_model_this_request")
			}
			continue
		}
		sz := schema.TextTokens(content)
		if sz < e.minTokens {
			rep.Gate("below_output_floor")
			metrics.RecordExtractionCacheLookup(false)
			continue
		}
		metrics.RecordExtractionCacheLookup(false)
		// The depth restriction, lifted for exactly the reason this component exists: the
		// provider's entry expired, so this turn re-writes the whole transcript into a new cache
		// entry whatever we do, and a message at depth is exactly as free to act on as one in the
		// tail. Routed through TailOnlyCold rather than skipped, so the ONE condition that makes
		// it safe is still checked here rather than assumed from `sweeping`.
		if !c.TailOnlyCold(i, true) {
			rep.Gate("cached_prefix")
			continue
		}
		if !fitsModelContext(shownBodyTokens(content), promptOverhead, inputLimit) {
			// Nothing to shed: the prompt is one tool output plus the contract. Leave it verbatim
			// rather than spend a round-trip on a request the upstream may reject.
			rep.Gate("over_model_context")
			continue
		}
		gateReason := "gate off"
		if e.gate {
			seenBefore := markSeenContent(c, id)
			explore := !tooSlowToExplore(metrics.ExtractionP50LatencyMs()) &&
				e.ratios.exploring(c.Session)
			d := evaluateGate(sz, ratio, savedTokenValueAt(c, i),
				callCost(pricing, sz, goalOverhead), seenBefore, turnsSoFar, explore, true)
			// allowCached is unconditionally true here, and it is not an override of the
			// caching-backend guard: that guard refuses candidates whose tokens are being billed
			// at the cache-READ rate, and on a cold turn none are — savedTokenValueAt prices them
			// at the cache-WRITE rate. The guard's own measurement (net-negative on caching
			// workloads) is about warm traffic, which this component never sees.
			if !d.allow {
				metrics.RecordExtractionSuppressed(d.reason)
				rep.Gate("economic_gate")
				continue
			}
			metrics.RecordExtractionReason(d.reason)
			gateReason = d.reason
		}
		cands = append(cands, cand{i: i, content: content, id: id, gateReason: gateReason})
	}

	// The sweep's own cap, drawing on no other component's allowance. The paths are switched
	// independently and have opposite economics, so a shared budget would silently disable one
	// depending on which fired first.
	if e.maxCalls > 0 && len(cands) > e.maxCalls {
		for k := e.maxCalls; k < len(cands); k++ {
			rep.Gate("over_sweep_cap")
		}
		cands = cands[:e.maxCalls]
	}

	// Phase 2 (parallel): ONE CALL PER OUTPUT. Not a batch — a shared reply can be truncated
	// mid-array, quote fidelity degraded with batch size when measured (4 of 37 quotes
	// non-verbatim at batch 16 against 0 of 16 at batch 10), and a batch-truncation counter has to
	// exist to compensate. A per-output call has none of those. The known cost of the choice is
	// that comparative judgement is unavailable; see internal/extract/adjudicate.go.
	if len(cands) > 0 {
		out := make([]sweepOutcome, len(cands))
		calls := make([]components.ModelCall, len(cands))
		sem := make(chan struct{}, llmConcurrency)
		var wg sync.WaitGroup
		for k := range cands {
			wg.Add(1)
			go func(k int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				out[k], calls[k] = e.adjudicate(c, rep.Component, cands[k].content, cands[k].id,
					cands[k].gateReason, goal, flat, model, pricing, callModel)
			}(k)
		}
		wg.Wait()
		// SERIAL FROM HERE, and the gates are why. components.Report is a plain struct whose
		// Gates map has no lock -- a Report is copied by value across this codebase and cannot
		// carry one -- so calling rep.Gate from the goroutines above is a data race, and Go's map
		// implementation turns it into `fatal error: concurrent map writes` rather than a wrong
		// count. Each call therefore accumulates its own gate names into its own slot and the
		// serial phase raises them, which is the same discipline the ModelCall records use.
		for k := range calls {
			for _, g := range out[k].gates {
				rep.Gate(g)
			}
			if calls[k].Component != "" {
				rep.Calls = append(rep.Calls, calls[k])
			}
		}
		// Phase 3 (serial): freeze + splice. Serial because the store write and the message
		// mutation are not concurrency-safe.
		for k := range cands {
			if !out[k].drop {
				continue
			}
			desc := sweepDescriptor(cands[k].content)
			// Freeze the decision so every later turn replays it byte-for-byte from the
			// same-session path above, at any depth. Session-scoped only: unlike a compaction,
			// a drop is a judgement about THIS transcript's obligations, so it must never be
			// served to another session whose agent may still need the output.
			putResult(c, cands[k].id, desc, "")
			if key, ok := applySweepDrop(c, rep, e.mode, &req.Input[cands[k].i], cands[k].content); ok {
				changed++
				if key != "" {
					keys = append(keys, key)
				}
			}
		}
	}

	if changed == 0 {
		rep.Skipped = true
	}
	return keys, nil
}

// sweepOutcome is one adjudication's result, carried back to the SERIAL phase.
//
// The gate names travel as data rather than being raised where they are decided, because
// components.Report is copied by value across this codebase and its Gates map therefore carries no
// lock. Raising a gate from a goroutine is not a slightly-wrong counter, it is
// `fatal error: concurrent map writes` -- which is how this was found.
type sweepOutcome struct {
	drop  bool
	gates []string
}

func (o *sweepOutcome) gate(name string) { o.gates = append(o.gates, name) }

// adjudicate makes ONE model call about ONE output and returns whether it may be dropped.
//
// Every failure resolves toward keep: a call error, a timeout, an unparseable reply, an unusable
// verdict, a drop that contradicts a named obligation. A wrong keep costs tokens on one turn; a
// wrong drop is a silent permanent loss the agent does not notice and cannot ask about.
func (e *ExtractSweep) adjudicate(c *components.Ctx, component string,
	content, id, gateReason, goal, flat string, model components.Model,
	pricing cheapmodel.Pricing, callModel string) (sweepOutcome, components.ModelCall) {

	var o sweepOutcome

	ctx, cancel := context.WithTimeout(c.Ctx, llmCallTimeout)
	defer cancel()
	ctx, callSink := cheapmodel.WithCallSink(ctx)
	before := schema.TextTokens(content)
	start := time.Now()

	reply, err := model.Complete(ctx, extract.BuildAdjudicationPrompt(goal,
		extract.AdjudicationItem{ID: id, SizeTokens: before, Content: content}))
	latency := float64(time.Since(start).Milliseconds())
	metrics.RecordExtractionCall(latency)
	_, inTok, outTok := callSink.Totals()
	cw, cr := callSink.CacheTotals()
	call := components.ModelCall{
		Component: component, Model: callModel, Strategy: "adjudicate",
		Cold: true, CandidateTokens: before, LatencyMs: latency,
		PromptTokens: inTok, CompletionTokens: outTok,
		CacheRead: cr, CacheWrite: cw,
		CostUSD:    pricing.Cost(inTok, outTok, cw, cr),
		GateReason: gateReason,
		Before:     content,
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			atomic.AddInt64(&llmTimeouts, 1)
		} else {
			atomic.AddInt64(&llmErrors, 1)
		}
	}
	if err != nil {
		o.gate("sweep_call_failed")
		call.Rejection = "adjudication call failed: " + err.Error()
		return o, call
	}
	o.gate("sweep_adjudicated")

	a := extract.Judge(reply, flat)
	if !a.Parsed {
		// A reply that opened the object but never closed it ran out of output budget; one that
		// never opened it is a format failure. The remedies are opposite — raise max_tokens versus
		// fix the prompt — so they get separate counters rather than one that reads as "the prompt
		// is wrong".
		if strings.Contains(reply, "{") && !strings.Contains(reply, "}") {
			o.gate("sweep_reply_truncated")
		} else {
			o.gate("sweep_unparseable")
		}
		if sweepUnusableSamples.Add(1) <= maxSweepUnusableSamples {
			head := reply
			if len(head) > 500 {
				head = head[:500]
			}
			slog.Warn("cg.sweep.unusable_reply", "reply_len", len(reply), "head", head)
		}
		call.Rejection = "reply did not parse; output kept verbatim"
		e.ratios.observe(0, before)
		return o, call
	}
	if a.QuoteFabricated {
		o.gate("sweep_quote_fabricated")
	}
	if a.CriterionMissing {
		o.gate("sweep_criterion_missing")
	}
	if a.VerdictUnusable {
		o.gate("sweep_verdict_unusable")
	}
	// The refusal is counted INSTEAD of a keep, not alongside it. Both leave the output verbatim,
	// but "the model judged this still needed" and "the model tried to remove something it had just
	// said was needed" are different events, and folding the second into the keep total is what
	// would make the alertable one invisible in the ratio an operator actually looks at.
	if a.RefusedObligation {
		o.gate("sweep_drop_refused_obligation")
		e.ratios.observe(0, before)
		call.Rejection = "drop refused: the verdict named an outstanding obligation"
		return o, call
	}
	if !a.Drop {
		o.gate("sweep_kept")
		// A keep is real evidence about this workload: the adjudicator looked at this output and
		// judged it still needed. Ratio 0, which is what it removed.
		e.ratios.observe(0, before)
		call.Rejection = "adjudicated still needed; kept verbatim"
		return o, call
	}
	after := schema.TextTokens(sweepDescriptor(content))
	if after >= before {
		// The never-worse check also lives in applySweepDrop, marker included. This one is here so
		// the ratio tracker is not fed a negative saving for a decision phase 3 will refuse.
		o.gate("sweep_drop_would_not_shrink")
		return o, call
	}
	o.gate("sweep_dropped")
	o.drop = true
	call.Accepted = true
	call.SavedTokens = before - after
	call.After = sweepDescriptor(content)
	e.ratios.observe(before-after, before)
	metrics.RecordExtractionSaving(before - after)
	metrics.RecordExtractionValue(float64(before-after) * savedTokenValue(c).perToken)
	if debugExtractLLM(c) {
		logging.From(c.Ctx).Debug("cg.sweep.drop", "candidate_tokens", before,
			"residue_tokens", after, "criterion_missing", a.CriterionMissing,
			"quote_fabricated", a.QuoteFabricated)
	}
	return o, call
}

// flattenTranscript renders the agent's own text as one string, for verifying an obligation quote.
// Every text block of every message, tool results included: an obligation can be created by a user
// instruction, by the agent's own stated next step, or by something a tool told it.
func flattenTranscript(req *bschemas.BifrostChatRequest) string {
	var b strings.Builder
	for i := range req.Input {
		b.WriteString(schema.MessageText(req.Input[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

func init() {
	f := []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: defaultSweepFloor, Min: 1,
			Hint: "The sweep's own per-output floor. Lower than extract_llm's, because on a cold turn every candidate is being re-billed at the cache-write rate whatever we do."},
		{Key: "min_idle_seconds", Type: components.FieldInt,
			Hint: "Demand MORE idle time than the provider TTL implies (0 = just the TTL). Raises the bar, never lowers it — the TTL check is the correctness condition and this is extra caution."},
		{Key: "max_calls", Type: components.FieldInt, Default: defaultSweepMaxCalls,
			Hint: "Cap model calls for one sweep (-1 = unlimited). The default is one concurrency round: past it the calls serialize and latency grows multiplicatively for a linear gain. Unbounded was measured at 27 calls, $0.229 and 76.6s added to a 33.5s turn."},
		{Key: "context", Type: components.FieldEnum, Default: "recent", Options: []string{"goal", "recent", "full"},
			Hint: "How much conversation the adjudication prompt carries. `full` is plausibly what a spent-ness judgement needs and is also what made the predecessor lose money (99% of the prompt was a copy of the transcript being compacted, once per candidate) — unmeasured either way, so the default stays recent."},
		{Key: "context_messages", Type: components.FieldInt, Default: defaultContextMessages,
			Hint: "The N for context: recent (0 = 2). The single biggest lever on what a call COSTS."},
		{Key: "economic_gate", Type: components.FieldBool, Default: true,
			Hint: "Only call the model when the expected saving exceeds the expected call cost. NOTE: the gate's arithmetic is calibrated on compaction, which removes a fraction of an output; a drop removes all of it, so the break-even here is a different and unmeasured number."},
		{Key: "model_max_input_tokens", Type: components.FieldInt,
			Hint: "Pin the adjudication model's input budget, for a model id the static table cannot name (a self-hosted id, or a gateway alias). Unset = resolved per model."},
		markerModeField(),
	}
	f = append(f, modelFields("model")...)
	components.RegisterFields("extract_llm_sweep", extractSweepConfig{}, f)
}
