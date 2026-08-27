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
// ONE CALL PER BATCH, NOT PER OUTPUT, and this was got wrong once. The per-output shape is the one
// docs/results/coref-selection-experiment.md REFUTED at 6% live-kept, inside the drop-everything null
// model's error bar; the batch shape lifted that to 58% at the lowest cost per output, because
// comparative judgement beats absolute judgement. `4ca1f13` diagnosed a live arm answering 1.02
// verdicts per call as exactly that refuted design wearing the bulk name. And `cc1aa9f` names the
// direction of the failure, which is the one a sweep cannot tolerate: small batches "do not make it
// wrong, they make it UNWILLING TO ACT, which is what a 94.6% keep rate looks like from inside". A
// sweep exists because the entire transcript is re-billing at the write rate; a timid adjudicator is
// an expensive no-op there. See internal/extract/adjudicate.go for the full evidence.
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
	// MaxCalls caps BATCH calls for one sweep (0 = defaultSweepMaxCalls; -1 = unlimited).
	//
	// It bounds CALLS, and each call now adjudicates up to extract.MaxAdjudicationItems candidates
	// together, so the two brakes are independent: the item cap is a measured quote-fidelity ceiling
	// and this is a spend/latency bound.
	//
	// Why it still exists at all, when the measured shape made exactly one call per request: with a
	// single call a transcript carrying 40 candidates would have 12 adjudicated and 28 left verbatim
	// — on the one turn whose whole point is that everything is re-billing at the write rate, that is
	// leaving most of the money. Nothing measured says four batches of 12 is worse than one batch of
	// 12; the per-batch shape is byte-identical, and batch SIZE is the variable the experiments moved.
	// The default is one concurrency round for the same reason the per-output default was: past it
	// the calls serialize and latency grows multiplicatively for a linear gain.
	//
	// The unbounded default this replaces was measured wrong in the way unbounded spend paths usually
	// are: one production request made 27 calls against a tenant whose llm_max_per_request was 2,
	// spent $0.229 and added 76.6 s to a turn whose upstream took 33.5 s — context-guru was 2.3x
	// slower than the model it was saving money on. The sweep draws on no other component's caps, so
	// this is its only spend brake.
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

	var cands []sweepCand
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
		// NO PER-CANDIDATE ECONOMIC GATE HERE. It is evaluated once for the whole batch below,
		// for two reasons that both point the same way.
		//
		// ARITHMETIC: the gate compares one candidate's expected saving against one CALL's cost, and
		// a call now covers up to twelve candidates. Charging each of them a whole call priced the
		// batch at ~12x its real cost and would suppress batches that pay comfortably.
		//
		// STARVATION, which is the worse failure. `4ca1f13` found the merged arm's real defect was an
		// upstream per-candidate filter: prefix_still_referenced removed 149,681 candidates, leaving
		// about one per request, so a "bulk" arm was running the per-output design refuted at 6%
		// live-kept. A per-candidate gate on this path is the same mechanism — it thins the batch one
		// output at a time until comparative judgement has nothing to compare — and it would return
		// this component to batch-of-one silently. See sweep_batch_of_one, which counts it if any
		// future filter reintroduces the shape.
		cands = append(cands, sweepCand{i: i, content: content, id: id})
	}

	// THE ECONOMIC GATE, ONCE FOR THE SWEEP. Priced the way the call is actually made: the batch's
	// total candidate tokens against ONE call's cost. That is the same comparison evaluateGate always
	// made, just given the unit that now corresponds to a call.
	gateReason := "gate off"
	if e.gate && len(cands) > 0 {
		var total int
		seenBefore := true
		for k := range cands {
			total += schema.TextTokens(cands[k].content)
			// Recurrence is a property of the CONTENT, so it is recorded for every candidate — a
			// suppressed batch still counts as seen. The batch is treated as recurring only when
			// every member is, which is the conservative direction for a spending decision.
			if !markSeenContent(c, cands[k].id) {
				seenBefore = false
			}
		}
		explore := !tooSlowToExplore(metrics.ExtractionP50LatencyMs()) &&
			e.ratios.exploring(c.Session)
		// allowCached is unconditionally true, and it is not an override of the caching-backend
		// guard: that guard refuses candidates whose tokens bill at the cache-READ rate, and on a
		// cold turn none do — savedTokenValue prices them at the cache-WRITE rate. The guard's own
		// measurement (net-negative on caching workloads) is about warm traffic, which this
		// component never sees.
		d := evaluateGate(total, ratio, val, callCost(pricing, total, goalOverhead),
			seenBefore, turnsSoFar, explore, true)
		if !d.allow {
			metrics.RecordExtractionSuppressed(d.reason)
			// Counted per candidate the batch would have covered, so the figure is comparable with
			// the other per-candidate gates above rather than reading as a single refusal.
			rep.GateN("economic_gate", len(cands))
			cands = nil
		} else {
			metrics.RecordExtractionReason(d.reason)
			gateReason = d.reason
		}
	}
	for k := range cands {
		cands[k].gateReason = gateReason
	}

	// Phase 2: BATCHED ADJUDICATION. Candidates are chunked into batches of at most
	// extract.MaxAdjudicationItems and each batch is ONE call, so the model ranks a dozen outputs
	// against each other rather than being asked "is this one expendable" twelve times over.
	//
	// The batches fan out, so the gate discipline from #119 applies here too: a Report's Gates map
	// carries no lock, so each call accumulates its own gate names into its own slot and the serial
	// phase raises them.
	if len(cands) > 0 {
		batches := make([][]int, 0, len(cands)/extract.MaxAdjudicationItems+1)
		for lo := 0; lo < len(cands); lo += extract.MaxAdjudicationItems {
			hi := lo + extract.MaxAdjudicationItems
			if hi > len(cands) {
				hi = len(cands)
			}
			idx := make([]int, 0, hi-lo)
			for k := lo; k < hi; k++ {
				idx = append(idx, k)
			}
			batches = append(batches, idx)
		}
		// The spend brake. NO SILENT CAPS: a truncated sweep is a bounded-coverage decision and must
		// be visible, or "we judged everything" and "we judged the first twelve" read identically.
		if e.maxCalls > 0 && len(batches) > e.maxCalls {
			for _, b := range batches[e.maxCalls:] {
				for range b {
					rep.Gate("sweep_batch_truncated")
				}
			}
			batches = batches[:e.maxCalls]
		}
		rep.GateN("sweep_offered", len(cands))

		out := make([]sweepOutcome, len(batches))
		calls := make([]components.ModelCall, len(batches))
		sem := make(chan struct{}, llmConcurrency)
		var wg sync.WaitGroup
		for bi := range batches {
			wg.Add(1)
			go func(bi int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				items := make([]extract.AdjudicationItem, 0, len(batches[bi]))
				for _, k := range batches[bi] {
					items = append(items, extract.AdjudicationItem{
						// The LABEL is the candidate's position in cands, so the mapping back is
						// ours and the model only ever handles a small integer. See
						// AdjudicationItem.Label for why that is not a style choice.
						Label:      k,
						ID:         cands[k].id,
						SizeTokens: schema.TextTokens(cands[k].content),
						Content:    cands[k].content,
					})
				}
				out[bi], calls[bi] = e.adjudicateBatch(c, rep.Component, items, cands,
					goal, flat, model, pricing, callModel)
			}(bi)
		}
		wg.Wait()

		// Serial from here.
		drop := map[int]bool{}
		for bi := range calls {
			for _, g := range out[bi].gates {
				rep.Gate(g)
			}
			for _, k := range out[bi].drop {
				drop[k] = true
			}
			if calls[bi].Component != "" {
				rep.Calls = append(rep.Calls, calls[bi])
			}
		}
		// Phase 3 (serial): freeze + splice. Serial because the store write and the message
		// mutation are not concurrency-safe.
		for k := range cands {
			if !drop[k] {
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

// sweepCand is one candidate the sweep collected: where it sits, what it holds, and what the economic
// gate concluded for the batch it belongs to. A package-level type because adjudicateBatch needs to
// resolve a model-supplied LABEL back to content, and that mapping must stay on our side of the wire.
type sweepCand struct {
	i          int
	content    string
	id         string
	gateReason string
}

// sweepOutcome is one adjudication's result, carried back to the SERIAL phase.
//
// The gate names travel as data rather than being raised where they are decided, because
// components.Report is copied by value across this codebase and its Gates map therefore carries no
// lock. Raising a gate from a goroutine is not a slightly-wrong counter, it is
// `fatal error: concurrent map writes` -- which is how this was found.
type sweepOutcome struct {
	// drop lists the candidate labels this batch authorised removing. A slice rather than a bool
	// because one call now decides for many outputs.
	drop  []int
	gates []string
}

func (o *sweepOutcome) gate(name string) { o.gates = append(o.gates, name) }

// adjudicateBatch makes ONE model call about a BATCH of outputs and returns the labels it authorised
// dropping.
//
// EVERY FAILURE PATH RESOLVES TOWARD KEEP -- a call error, a timeout, an unparseable reply, an
// unusable verdict, a drop that contradicts a named obligation, a verdict for something we did not
// offer. A wrong keep costs tokens on one turn; a wrong drop is a silent permanent loss the agent
// does not notice and cannot ask about. The two errors are not comparable, so this does not treat
// them symmetrically.
func (e *ExtractSweep) adjudicateBatch(c *components.Ctx, component string,
	items []extract.AdjudicationItem, cands []sweepCand, goal, flat string,
	model components.Model, pricing cheapmodel.Pricing, callModel string) (sweepOutcome, components.ModelCall) {

	var o sweepOutcome
	// A SINGLE-ITEM BATCH IS THE REFUTED DESIGN WEARING THE NEW NAME, so it is counted rather than
	// silently accepted. `4ca1f13` found a live "bulk" arm answering 1.02 verdicts per call and
	// diagnosed it as the per-output shape refuted at 6% live-kept; asserting one CALL was not enough
	// to catch it, because one call carrying one item is exactly that. The call still proceeds -- a
	// transcript can legitimately have one candidate above the floor -- but a workload where this
	// fires routinely has an upstream filter starving the batch, which is the failure that cost three
	// iterations.
	if len(items) < 2 {
		o.gate("sweep_batch_of_one")
	}

	ctx, cancel := context.WithTimeout(c.Ctx, llmCallTimeout)
	defer cancel()
	ctx, callSink := cheapmodel.WithCallSink(ctx)
	var before int
	for _, it := range items {
		before += it.SizeTokens
	}
	start := time.Now()

	// RAISE THE REPLY BUDGET. A verdict array over a full batch, each entry carrying an obligation
	// label and a verbatim quote, is long, and `659e7a6` measured what happens when the budget is the
	// client's default: 24 of 34 replies cut off mid-array, which parses as nothing and is
	// indistinguishable from a model declining to act. Optional interface, so a client that does not
	// support it is used as-is rather than refused.
	if b, ok := model.(components.Budgeter); ok {
		if m := b.WithMaxTokens(extract.AdjudicationReplyTokens); m != nil {
			model = m
		}
	} else {
		// Counted, because the alternative is a silent return to the truncation regime on whatever
		// client shape this is.
		o.gate("sweep_reply_budget_not_raised")
	}

	reply, err := model.Complete(ctx, extract.BuildAdjudicationPrompt(goal, items))
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
		GateReason: cands[items[0].Label].gateReason,
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
	for range items {
		o.gate("sweep_adjudicated")
	}

	verdicts, parsed := extract.ParseVerdicts(reply)
	if !parsed {
		// TRUNCATION IS NOT JUNK, and the two need opposite fixes -- raise the budget versus fix the
		// prompt -- so one name for both hid a 70%-of-calls failure behind a label that reads as "the
		// prompt is wrong".
		if extract.ReplyWasTruncated(reply) {
			o.gate("sweep_reply_truncated")
		} else {
			o.gate("sweep_unparseable")
		}
		// LOG A BOUNDED SAMPLE OF THE ACTUAL TEXT. A counter can say THAT a reply was unusable; only
		// the text says WHY, and six rounds of the predecessor's failures were diagnosed by inferring
		// a cause from counters with every inference at least partly wrong.
		if sweepUnusableSamples.Add(1) <= maxSweepUnusableSamples {
			head := reply
			if len(head) > 500 {
				head = head[:500]
			}
			slog.Warn("cg.sweep.unusable_reply", "reply_len", len(reply),
				"offered", len(items), "head", head)
		}
		call.Rejection = "reply did not parse; every output kept verbatim"
		e.ratios.observe(0, before)
		return o, call
	}
	if len(verdicts) == 0 {
		// A well-formed EMPTY array: the model read the batch and kept all of it. The contract
		// explicitly invites that, so it must not be filed as a failure -- that conflation is what
		// made "the model declined to act" and "the model was never successfully asked" the same
		// number for three iterations (4ca1f13).
		o.gate("sweep_kept_whole_batch")
		e.ratios.observe(0, before)
		call.Rejection = "adjudicated: keep the whole batch"
		return o, call
	}

	offered := map[int]bool{}
	for _, it := range items {
		offered[it.Label] = true
	}
	var removed int
	seen := map[int]bool{}
	for _, v := range verdicts {
		if !offered[v.Label] {
			// A verdict for something this batch did not offer. Never acted on: the label is how a
			// decision is keyed to an output, so a wrong label is a decision about an unknown
			// message and acting on it would remove the wrong content.
			o.gate("sweep_verdict_unknown_label")
			continue
		}
		if seen[v.Label] {
			o.gate("sweep_verdict_duplicate_label")
			continue
		}
		seen[v.Label] = true
		content := cands[v.Label].content
		a := extract.Judge(v, flat)
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
		// but "the model judged this still needed" and "the model tried to remove something it had
		// just said was needed" are different events, and folding the second into the keep total is
		// what would make the alertable one invisible in the ratio an operator actually looks at.
		if a.RefusedObligation {
			o.gate("sweep_drop_refused_obligation")
			continue
		}
		if !a.Drop {
			o.gate("sweep_kept")
			continue
		}
		sz := schema.TextTokens(content)
		after := schema.TextTokens(sweepDescriptor(content))
		if after >= sz {
			// The never-worse check also lives in applySweepDrop, marker included. This one is here
			// so the ratio tracker is not fed a negative saving for a decision phase 3 will refuse.
			o.gate("sweep_drop_would_not_shrink")
			continue
		}
		o.gate("sweep_dropped")
		o.drop = append(o.drop, v.Label)
		removed += sz - after
		metrics.RecordExtractionSaving(sz - after)
		metrics.RecordExtractionValue(float64(sz-after) * savedTokenValue(c).perToken)
	}
	// An output this batch offered that no verdict mentioned is UNJUDGED, and it must not look like a
	// keep: `4ca1f13` found a live arm where the model silently omitted labels and the missing answers
	// were invisible, so "the batch is starved" and "the model answered for a third of it" were the
	// same number.
	for _, it := range items {
		if !seen[it.Label] {
			o.gate("sweep_verdict_missing")
		}
	}
	// Feed the observed ratio so the gate prices FUTURE batches on what this workload actually
	// achieves. Fed once per CALL over the whole batch, which is the unit the gate now prices.
	e.ratios.observe(removed, before)
	if removed > 0 {
		call.Accepted = true
		call.SavedTokens = removed
	} else {
		call.Rejection = "adjudicated: nothing in this batch was spent"
	}
	if debugExtractLLM(c) {
		logging.From(c.Ctx).Debug("cg.sweep.batch", "offered", len(items),
			"verdicts", len(verdicts), "dropped", len(o.drop),
			"candidate_tokens", before, "removed_tokens", removed)
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
