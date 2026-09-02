package offload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/coref"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
	"gopkg.in/yaml.v3"
)

func init() { components.Register("extract_llm_sweep", newExtractSweep) }

// ExtractSweep is the SWEEP ADJUDICATOR: it asks the request's OWN model, over the transcript that
// model already has in its prompt cache, which tool outputs are spent — and removes those, leaving a
// shape descriptor plus a recoverable marker. It never rewrites anything and it never copies output
// content into a prompt.
//
// WHY IT IS A SEPARATE COMPONENT FROM extract_llm. The two situations want different operations. On a
// warm turn extract_llm works the uncached tail: the output is recent, the agent may still want most
// of it, and a smaller version of it is more useful than none of it. The sweep works DEEP HISTORY,
// where rewriting is the wrong operation on either branch of the only question that matters — deep
// history is either still load-bearing, in which case rewriting corrupts content the model has
// already reasoned about, or it is spent, in which case the answer is to remove it.
//
// WHY IT ASKS THE REQUEST'S MODEL OVER THE CACHE, in one call, shipping an inventory:
//
//	Need is relevance MINUS what has already been captured elsewhere, and that second term lives in
//	the LATER TURNS. A judgement shown only the candidate cannot see them.
//	Verbatim quoting — the only signal that says whether the model is inventing — degraded to 20.8%
//	on the cheap model at bulk batch sizes, against 0 of 59 on the request model.
//	Appending a trailing user message to a byte-identical prefix read 19,595 tokens from cache and
//	created 0, so the whole transcript is affordable exactly once: as a cache read.
//
// See internal/extract/adjudicate.go for the full evidence and components.PrefixAsker for the
// construction.
//
// WHAT IT NEVER DOES. It selects no compaction strategy, produces no rewritten text, and there is no
// reply field a model could return content through. `strategy`, `rewrite`, `aggressiveness` and
// `max_chars` are therefore not merely defaulted differently here, they are meaningless, and writing
// one is a config error rather than a silently ignored key (see newExtractSweep).
type ExtractSweep struct {
	minTokens int
	// minInventory is the fewest candidates worth asking about. See defaultMinInventory.
	minInventory int
	// blockFallback refuses the content-copying fallback. See extractSweepConfig.BlockFallback.
	blockFallback bool
	// preExpiry is how long before the prompt cache's believed expiry the sweep may fire. See
	// sweeping() for why the window is where it is, and why its WIDTH is the one number here that
	// no measurement settles.
	preExpiry time.Duration
	// evidence adds the co-reference index's record to each inventory line. See renderEvidence.
	evidence bool
	// econTrigger enables the SECOND trigger: fire on economics even while the cache is live. See
	// econPays() for the break-even, and why one trigger is not a superset of the other.
	econTrigger bool

	mode markerMode
}

// extractSweepConfig is the sweep's whole surface.
//
// Note what is absent and why. There is no `model` block: the ask goes to the REQUEST's own model by
// construction, because only that model's cache holds the transcript — naming another would read a
// different namespace and pay fresh for everything. There is no `context` / `context_messages`: the
// conversation IS the prefix, so there is nothing to choose how much of to re-send. There is no
// `max_calls`: there is exactly one ask per turn, bounded by maxAskItems rather than by a call count
// — and that bound is not configurable, because it follows from the reply budget rather than from a
// deployment's preference (see the cap in Offload, and #132 for the coverage question it leaves open).
// And there is no `economic_gate`: the gate prices a
// per-output cheap-model call against an expected saving, and this is one cached read for the whole
// transcript, so its arithmetic does not describe this component at all — the brakes here are the
// floor below and the verified cache read.
type extractSweepConfig struct {
	// MinTokens is the per-output floor (0 = defaultSweepFloor). Candidates below it are not worth
	// naming in the inventory: each line is paid fresh, and a small output's removal cannot repay
	// the marker it leaves behind.
	MinTokens int `yaml:"min_tokens"`
	// MinInventory is the fewest candidates worth asking about (0 = defaultMinInventory). Below it
	// the sweep declines entirely rather than asking, because the model's judgement at small
	// inventory sizes is measured poor — see the floor check in Offload for the figures.
	MinInventory int `yaml:"min_inventory"`
	// PreExpirySeconds is the width of the pre-expiry window (0 = defaultPreExpiry).
	PreExpirySeconds int `yaml:"pre_expiry_seconds"`
	// Evidence adds the co-reference index's record to each candidate's inventory line, as input the
	// model weighs rather than a filter that pre-decides. OFF by default: it changes the adjudication
	// CONTRACT (the prompt gains a paragraph teaching how to read the counters), and the contract is
	// the part with measurements attached to it.
	Evidence bool `yaml:"evidence"`
	// EconTrigger adds the economic trigger alongside the pre-expiry window. OFF by default: it
	// deliberately invalidates a LIVE cached prefix, which is a cost the pre-expiry trigger exists to
	// avoid, and it is only worth paying when the saving is collected over enough remaining turns.
	EconTrigger bool `yaml:"econ_trigger"`
	// BlockFallback refuses the fallback path: when the prefix ask cannot read the cache, decline
	// instead of asking again with the output content copied into the prompt.
	//
	// OFF by default, which is a deliberate choice between two real costs. Falling back keeps the
	// component working on a session's FIRST turn and whenever an entry has gone — treating "no
	// prefix" as "no verdicts" would disable it there and read, in the counters, as a model that
	// declined to act. But the fallback pays fresh for content the cached path reads for a tenth of
	// the price, which is where this component's predecessor lost money. Default on the side of
	// working; switch it off where the bill matters more than the yield. Counted either way.
	BlockFallback bool `yaml:"block_fallback"`
	// MarkerMode is how a removed output is referenced. `full`, the default, is the only mode that
	// keeps the removal recoverable.
	MarkerMode string `yaml:"marker_mode"`
}

// defaultSweepFloor is the per-output floor when none is configured. Carried over from the cold_cache
// block this component replaced: at 3000 the shipped preset produced ZERO extractions across 3,437
// production requests, with `below_output_floor` refusing every candidate on all 36 sweeping turns.
const defaultSweepFloor = 1000

// defaultMinInventory is the fewest candidates this component will ask about. Ten, because that is
// where `cc1aa9f` measured the model becoming willing to act CORRECTLY: at batch 3-6 it dropped a
// genuinely-spent output 2 times in 4, at batch 10 it dropped it 4 in 4 and cleared 100% of
// genuinely-spent candidates. Below that the mechanism is not a timid version of itself, it is
// answering the question the selection experiment refuted at 6% live-kept.
const defaultMinInventory = 10

// maxAskItems bounds how many candidates one ask may carry. Not configurable: it is a property of the
// reply budget and the model's transport limit, not of a deployment's taste, and an operator raising
// it would be trading a partial sweep for no sweep at all. See the cap in Offload for the arithmetic.
const maxAskItems = 12

// defaultPreExpiry is the pre-expiry window's width when none is configured.
//
// IT IS AN ASSUMPTION, AND THE ONLY UNMEASURED NUMBER IN THIS COMPONENT. One minute is
// apply.coldMargin, which is the single figure in this codebase with a stated purpose for clock
// uncertainty around cache expiry: the gap between when a turn was recorded here and when the
// provider last touched the entry. A window one margin wide therefore sits inside the interval where
// our clock and the provider's are believed to agree to within that margin.
//
// What is NOT known is the yield/cost trade-off of widening it. A wider window fires on more turns
// and invalidates prefixes with more remaining TTL; a narrower one fires rarely. Nothing measures
// either side, so this is deliberately narrow and configurable rather than tuned.
const defaultPreExpiry = time.Minute

// sweepBannedKeys are the compaction knobs that have no meaning for an adjudicator, and the reason
// each one does not apply. They are refused rather than ignored: a silently accepted `rewrite: false`
// would read as "verified deletion-only is on" when nothing is being rewritten in the first place,
// and an operator migrating an older config by hand has no other way to find out.
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
	// THE MODEL IS NOT A FREE CHOICE HERE, and this is the one place that asymmetry with extract_llm
	// is visible, so it is spelled out rather than left to look like an oversight.
	//
	// extract_llm may compact with any model, because its prompt carries the output it is compacting:
	// any model can read it. This component's prompt carries an INVENTORY, and the outputs are read
	// from the prompt cache of the model being asked. Only the REQUEST's model has that cache. So
	// `source: config` — a separate cheap model — is not a cheaper configuration of this component, it
	// is a broken one: the ask would read nothing and degrade to paying fresh for the whole
	// transcript, which is precisely the cost that made the predecessor lose money.
	//
	// Refused rather than accepted-and-corrected, because an operator who wrote it meant something,
	// and silently substituting a different model is how a configuration comes to disagree with the
	// bill.
	{"model", "the ask goes to the REQUEST's own model by construction: only that model's prompt cache " +
		"holds the transcript the inventory refers to. `source: config` is incoherent here rather than " +
		"merely suboptimal — a separate cheap model has no such cache, so the ask would read nothing " +
		"and pay fresh for the entire transcript. extract_llm's model IS a free choice because its " +
		"prompt carries the output itself"},
	{"context", "the conversation IS the cached prefix, so there is no amount of it to choose to re-send"},
	{"context_messages", "the conversation IS the cached prefix; see `context`"},
	{"max_calls", "one call adjudicates every candidate, because nothing is copied per candidate"},
	{"economic_gate", "the gate prices a per-output cheap-model call; this is one cached read for the whole " +
		"transcript, so its arithmetic does not describe this component"},
}

func newExtractSweep(raw []byte) (components.Component, error) {
	// The banned keys FIRST, before components.Decode's KnownFields rejects them with a generic
	// yaml message. The whole point is that the error names the reason.
	if len(raw) > 0 {
		var probe map[string]yaml.Node
		if err := yaml.Unmarshal(raw, &probe); err == nil {
			for _, b := range sweepBannedKeys {
				if _, present := probe[b.key]; present {
					return nil, fmt.Errorf("extract_llm_sweep: %s does not apply here: %s", b.key, b.why)
				}
			}
		}
	}
	cfg := extractSweepConfig{}
	if err := components.Decode(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg.MinTokens <= 0 {
		cfg.MinTokens = defaultSweepFloor
	}
	if cfg.MinInventory <= 0 {
		cfg.MinInventory = defaultMinInventory
	}
	pre := defaultPreExpiry
	if cfg.PreExpirySeconds > 0 {
		pre = time.Duration(cfg.PreExpirySeconds) * time.Second
	}
	return &ExtractSweep{
		minTokens: cfg.MinTokens, minInventory: cfg.MinInventory,
		preExpiry: pre, mode: parseMarkerMode(cfg.MarkerMode),
		blockFallback: cfg.BlockFallback, econTrigger: cfg.EconTrigger,
		evidence: cfg.Evidence,
	}, nil
}

func (*ExtractSweep) Name() string                 { return "extract_llm_sweep" }
func (*ExtractSweep) Enabled(*components.Ctx) bool { return true }

// sweeping reports whether this turn falls in the PRE-EXPIRY WINDOW: the prompt cache still exists,
// and it is close enough to expiring that invalidating it costs little.
//
// THIS IS THE RESOLUTION OF A CONTRADICTION, and it is the whole reason the trigger is not the cold
// gate it started as. The two halves of this component want opposite cache states:
//
//	the ASK needs a WARM cache — a prefix ask reads an entry that must still exist, or the call pays
//	fresh for the whole transcript, which is the cost the design exists to avoid;
//	the REMOVAL wants a COLD cache — rewriting deep history invalidates a live prefix and forces a
//	cache-write of the whole suffix at 1.25x fresh.
//
// Both are cheap in the window where the entry still exists but has little life left: the ask still
// reads it, and what the removal invalidates is nearly worthless. So the trigger is
// `0 < remaining <= preExpiry`, where remaining is the cache's believed lifetime minus this session's
// idle time.
//
// THE TTL IS DERIVED, NOT ASSUMED. Ctx.CacheTTLMs is the same figure apply's cold decision uses, read
// out of the request itself: a bare `ephemeral` mark is 5 minutes, an explicit `ttl: "1h"` is an hour,
// widened to the longest lifetime this prefix has ever asked for. 0 means the cache-aware path did not
// run, i.e. unknown, and unknown must not fire — a window computed from a guessed TTL would invalidate
// live prefixes on exactly the deployments whose TTL we could not read.
//
// !ColdCache is redundant against `remaining > 0` and kept anyway: it is apply's own verdict, computed
// with its clock-skew margin, and one cheap agreement check costs nothing next to a wrongly
// invalidated prefix.
func (e *ExtractSweep) sweeping(c *components.Ctx) bool {
	if c == nil || c.ColdCache || c.CacheTTLMs <= 0 || c.IdleMs <= 0 {
		return false
	}
	remaining := time.Duration(c.CacheTTLMs-c.IdleMs) * time.Millisecond
	return remaining > 0 && remaining <= e.preExpiry
}

// econPays is the SECOND trigger, and the claim this component's econ_trigger mode exists to test:
// that deferring an output's removal to a deep sweep pays for itself even when the cached prefix is
// still LIVE, because the removal is collected on every remaining turn while the cache-write it forces
// is paid once.
//
// NEITHER TRIGGER IS A SUPERSET OF THE OTHER, which is why both are kept:
//
//	pre-expiry  fires on TIME and knows nothing about mass. It is nearly free — what it invalidates
//	            is about to expire anyway — but it cannot fire at all on a session whose cache keeps
//	            being refreshed, which is exactly the long agent run with the most to save.
//	econ        fires on MASS and knows nothing about the clock. It reaches those sessions, and it
//	            pays a real cache-write to do it, so it must clear S*T > 11.5*W first.
//
// See prefix_econ.go for the break-even itself; it is shared with coref rather than restated, because
// two components pricing the same cache-write differently would be two answers to one question.
//
// S IS AN UPPER BOUND, and this is the trigger's known optimism. S is the inventory's whole token
// mass, but the model drops only some of it, and how much is not known until after the call this test
// is deciding whether to make. The counter-bias is in W: prefixRewriteWindow assumes the WHOLE
// transcript is cached whenever the boundary is unknown, over-stating what the mutation rewrites. The
// two lean opposite ways and neither is calibrated, so read a fired econ trigger as "this batch was
// worth asking about", not as a realised saving. `prefix_rewrite_not_repaid` vs
// `prefix_rewrite_repaid` is what makes the split observable.
func (e *ExtractSweep) econPays(req *bschemas.BifrostChatRequest, c *components.Ctx, cands []sweepCand) (need, have int, ok bool) {
	if !e.econTrigger || len(cands) == 0 {
		return 0, 0, false
	}
	saved, shallowest := 0, cands[0].i
	for _, cd := range cands {
		saved += schema.TextTokens(cd.content)
		if cd.i < shallowest {
			shallowest = cd.i
		}
	}
	return prefixRewritePays(req, saved, shallowest, c)
}

// selectAffordableDrops chooses WHICH of the model's votes to actually apply.
//
// THE PROBLEM THIS FIXES. `min_tokens` was doing two unrelated jobs at once: deciding which outputs are
// worth NAMING in the inventory, and standing in for whether a drop is worth its cost. Those want
// opposite settings. Naming is nearly free -- the model reads each output from the cached prefix, so one
// more candidate costs one inventory line, on the order of thirty tokens -- and more candidates is the
// axis the mechanism lives on (6% live-kept shown one output, 58% at ~15). But a high floor was the only
// thing standing between the model and an expensive drop, so it had to be set for the second job, which
// starved the first. Measured on iteration 022: the shipped floor of 1000 named 4.5 of the 23.6 tool
// outputs a request carried, holding the batch at 4.4 against a cap of 12.
//
// WHY SIZE WAS THE WRONG DISCRIMINATOR ANYWAY. A drop's real cost is not its marker -- tryMark already
// refuses any drop whose replacement would not shrink the message, marker-inclusive. It is the
// cache-WRITE that mutating the prefix forces, and that is charged on the span from the EARLIEST dropped
// index to the cached boundary, once per pass. So a small output dropped after something already being
// dropped costs its descriptor and nothing more; the same output dropped EARLIER than everything else
// sets W for the whole batch and must repay the entire rewrite by itself. Depth relative to the rest of
// the batch decides, not size.
//
// THE WALK. Votes are ordered latest-first and accumulated. Each step reaches further back, so S and W
// both grow, and the subset maximising S*T - 11.5*W is chosen. Not the largest clearing subset: the
// objective is not monotonic in k -- one early, tiny drop can extend W past what several later ones
// repay -- so it is maximised rather than thresholded.
//
// ONLY ON THE ECON PATH, and that is not an oversight. The pre-expiry window fires precisely because the
// cached prefix is about to expire, which makes W nearly worthless; pruning drops to protect a cache
// entry with seconds to live would forgo real savings to preserve nothing. Under pre-expiry every vote
// is applied, exactly as on `main`.
func (e *ExtractSweep) selectAffordableDrops(req *bschemas.BifrostChatRequest, c *components.Ctx,
	cands []sweepCand, drop []int) (kept []int, pruned int) {

	if len(drop) < 2 || c == nil || c.CtxWindow <= 0 {
		return drop, 0
	}
	ord := append([]int(nil), drop...)
	sort.Slice(ord, func(a, b int) bool { return cands[ord[a]].i > cands[ord[b]].i }) // latest first
	bestNet, bestK := 0.0, 0
	saved, shallowest := 0, 0
	for k := 1; k <= len(ord); k++ {
		cd := cands[ord[k-1]]
		saved += schema.TextTokens(cd.content)
		if k == 1 || cd.i < shallowest {
			shallowest = cd.i
		}
		net, _, _ := prefixRewriteNet(req, saved, shallowest, c)
		if k == 1 || net > bestNet {
			bestNet, bestK = net, k
		}
	}
	if bestK == len(ord) {
		return drop, 0
	}
	kept = append(kept, ord[:bestK]...)
	return kept, len(ord) - bestK
}

// renderEvidence formats one output's co-reference record for the inventory line. Counts only — no
// identifier lists — because the measured win came from comparative RANKING, not from more detail, and
// every token here is paid on every candidate on every sweeping turn.
//
// The classifier's own verdict is included deliberately. It is the index stating its conclusion, which
// the model is free to overrule; that disagreement is the signal the design wants, and it is
// unavailable if the index only ships raw counters and keeps its judgement to itself.
func renderEvidence(r *coref.Record, laterTurns int) string {
	if r == nil {
		// No record: the output was below the index's size floor, so the index has no opinion. Say so
		// plainly rather than emitting zeros, which would read as "nothing referenced it" — the one
		// misreading that could turn a silent index into a drop.
		return fmt.Sprintf("no index record (below size floor); later_turns=%d", laterTurns)
	}
	age := "never"
	if r.RefAge >= 0 {
		age = fmt.Sprintf("%d messages ago", r.RefAge)
	}
	return fmt.Sprintf("novel=%d refs=%d ref_age=%s used_frac=%.2f later_turns=%d verdict_of_index=%s",
		r.Novel, r.Refs, age, r.UsedFrac, r.LaterTurns,
		coref.Classify(*r, corefClosedDistDefault, corefOpenRepsDefault, corefMinLaterDefault))
}

// laterModelTurns counts assistant messages after index i — the opportunity an output has HAD to be
// referenced. Only used when the index has no record, where it is the one honest thing still sayable:
// an output with few later turns has not yet had a chance to be referenced, so "unreferenced" says
// nothing about it.
func laterModelTurns(req *bschemas.BifrostChatRequest, i int) int {
	n := 0
	for j := i + 1; j < len(req.Input); j++ {
		if req.Input[j].Role == bschemas.ChatMessageRoleAssistant {
			n++
		}
	}
	return n
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
	// preExpiry is trigger one. Trigger two (econ) cannot be evaluated yet: it prices the candidate
	// mass, and the mass is not known until the collection loop below has run. So collection runs
	// whenever EITHER trigger could fire, and the econ decision is taken at the ask.
	preExpiry := e.sweeping(c)
	collecting := preExpiry || e.econTrigger
	if !preExpiry {
		// NOT a return. The frozen replays below still run, and they are the reason a sweep's saving
		// survives past the turn that earned it: without them a later turn would re-send every
		// removed output verbatim, undoing the removal AND breaking the byte-stability of the prefix
		// the provider is caching.
		rep.Gate("not_in_pre_expiry_window")
	}

	val := savedTokenValue(c)
	var cands []sweepCand
	var keys []string
	changed := 0
	// eligible counts candidates that cleared every gate this component knows about. Compared with the
	// inventory's size below, to catch a pre-filter that thinned it -- see the comment at the append
	// site for why that is the failure worth a tripwire.
	eligible := 0

	// Phase 1 (serial): replay frozen decisions at any depth, and collect the candidates to name in
	// the inventory.
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
		// If the agent recently EXPANDED this content, leave it verbatim — removing it again would
		// just trigger another expand.
		if isKeptVerbatim(c, id) {
			rep.Gate("kept_verbatim_after_expand")
			continue
		}
		// SAME-SESSION REPLAY, and it bypasses the depth gate legitimately: this session already
		// sent these exact bytes on an earlier turn, so the provider's cached prefix holds the
		// REMOVED form and replaying it is byte-identical.
		//
		// The stored value is the descriptor, which sweepDescriptor derives from the content alone.
		// That is what makes the replay safe in the sense TailOnlyCold's doc requires: the DECISION
		// came from a model, but the REPLACEMENT is a pure function of (content, config), so a replay
		// can never emit different bytes than the turn that decided it.
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
				rep.Event("reapplied_same_session")
			}
			continue
		}
		if !collecting {
			// Outside the window no NEW decision is taken; the replays above already ran, which is
			// all such a turn has to do.
			continue
		}
		metrics.RecordExtractionCacheLookup(false)
		if schema.TextTokens(content) < e.minTokens {
			rep.Gate("below_output_floor")
			continue
		}
		// NO DEPTH RESTRICTION. Candidates are the ENTIRE transcript, which is what this component
		// is for, and the sweep window is the whole justification.
		//
		// The tail gate exists for the WARM-turn compactor: rewriting a message inside a live cached
		// prefix invalidates everything after it, so extract_llm must confine itself to the uncached
		// tail. This component's premise is the opposite — it ACCEPTS that invalidation, because it
		// only ever runs on a prefix with almost no TTL left. Refusing depth here does not make the
		// sweep conservative, it makes it pointless: the candidates would be exactly the messages the
		// prefix ask CANNOT SEE, since the ask reads the previous turn's sent body (everything up to
		// the cached boundary) while the tail is everything past it. Disjoint by construction.
		//
		// That is not hypothetical. It shipped, and live verification found the model judging outputs
		// it had never read: one turn kept two outputs citing "Reply with the word ACK only." as the
		// obligation for each — a real transcript string, so no fabrication counter fired — and
		// another DROPPED an output having seen only `begins: # ledger_b` and a token count. See #122.
		//
		// It is also what made the inventory degenerate. Candidates confined to the tail means one
		// candidate on an ordinary agent turn, which is the per-output shape `4ca1f13` records as
		// refuted at 6% live-kept — reached by default, and acted on.
		//
		// WHY THIS NEEDS NO MEASUREMENT OF EARLY INVALIDATION, which is the objection the previous
		// version of this comment raised against itself. The cost of invalidating early is bounded by
		// the window width, not by the TTL: inside the window the prefix has at most `window` left to
		// live, so at most that much cache value is being given up, and the window is deliberately
		// small (one minute against a 5-minute TTL by default). The trigger is what buys the
		// permission — that is the entire reason it exists — so keying the permission on ColdCache
		// instead of on the window withdrew it exactly where it had just been paid for.
		//
		// Counted positively rather than as a refusal: `sweep_candidate_at_depth` says the component
		// is genuinely reaching past the cached boundary. It going to zero is the signal that this
		// regressed again, which a `cached_prefix` refusal counter could not distinguish from
		// "nothing was deep this turn".
		if !c.TailOnly(i) {
			rep.Event("sweep_candidate_at_depth")
		}
		// EVERY CANDIDATE PAST THIS POINT MUST REACH THE INVENTORY, and sweep_inventory_thinned below
		// is the tripwire for the day one does not.
		//
		// `4ca1f13`'s real defect was a per-candidate PRE-FILTER sitting exactly here.
		// prefix_still_referenced removed 149,681 candidates and left about one per request, which
		// silently turned a bulk adjudication arm into the per-output shape refuted at 6% live-kept --
		// and the arm reported itself as bulk throughout. It was self-defeating twice over: it starved
		// the comparison, and it meant the model only ever saw what the index had ALREADY judged spent,
		// which destroys the veto on the index's blind spot that the mechanism exists to provide.
		//
		// `main` has no such thinner, so `eligible` and the inventory size are equal by construction
		// and this counter cannot fire today. THAT IS THE POINT: PR #80 rebases onto this branch and
		// brings index-driven candidate selection with it, and a filter added between this line and the
		// append below trips the counter on its first request. If you are adding one, the index's
		// verdict belongs in the prompt as EVIDENCE for the model to weigh
		// (extract.AdjudicationItem.Evidence), never as a gate that pre-decides the answer.
		eligible++
		// The wire's own tool-call id, which apply.normalize sets on every synthetic tool message it
		// lifts out of an Anthropic tool_result block. Read here rather than reconstructed, because a
		// reconstructed anchor is exactly the defect #123 records.
		toolID := ""
		if msg.ChatToolMessage != nil && msg.ChatToolMessage.ToolCallID != nil {
			toolID = *msg.ChatToolMessage.ToolCallID
		}
		cands = append(cands, sweepCand{i: i, content: content, id: id, toolID: toolID})
	}
	// WHAT WAS SHOWN, counted apart from what was answered. A per-candidate loop cannot express "this
	// many were OFFERED", and the distinction is not cosmetic: a live arm reported 2.80 verdicts per
	// call and that was read as the batch size, when it counted what the model chose to ANSWER rather
	// than what it was SHOWN. Without this, "the inventory is starved" and "the model answered for a
	// third of it" are the same number.
	// CAP THE ASK, because an uncapped one risks losing every verdict rather than some.
	//
	// The reply carries one verdict per candidate, each with a VERBATIM transcript quote, and the
	// budget is PrefixAskMaxTokens (16,000). Live: a 12-candidate ask produced a 7,191-token reply —
	// about 600 tokens per verdict once the model's reasoning is included — so roughly 26 candidates
	// exhausts the budget. Past that the reply truncates, and truncation is ALL-OR-NOTHING: the array
	// never closes, nothing parses, and every verdict in it is discarded. A 50-candidate transcript
	// would therefore sweep nothing at all, having paid for the call.
	//
	// Twelve, and the two independent arguments agree on it, which is the only reason to trust a
	// number here. Reply-budget arithmetic says ~26 is the ceiling and something well inside it is
	// prudent. And `cc1aa9f` measured quote fidelity degrading with size — 4 of 37 quotes non-verbatim
	// at 16 against 0 of 16 at 10 — so 12 was already the conservative end of the transport limit.
	// That fidelity measurement was taken when content was copied into the prompt, which it no longer
	// is, so it does not straightforwardly transfer; it is cited as corroboration, not as proof.
	//
	// LARGEST FIRST, so the cap keeps the candidates worth the most. And what is left over is
	// COUNTED: a component that silently swept 12 of 50 while reporting success would be the same
	// class of defect as the starved inventory this file already guards against.
	//
	// What this does NOT do is make a second ask to cover the remainder. That is a real coverage gap
	// on a transcript-heavy session and it needs a measurement — whether N asks over one transcript
	// beat one ask, and at what cost — so it is tracked rather than guessed at. See #132.
	// Counted BEFORE the cap, and the thinning tripwire measured against this rather than against the
	// post-cap length. The cap is a deliberate ceiling; sweep_inventory_thinned exists to catch a
	// pre-filter quietly starving the comparison (`4ca1f13`), and letting the cap trip it would turn
	// that alarm into noise on exactly the transcripts where it should be loudest.
	assembled := len(cands)
	if len(cands) > maxAskItems {
		sort.SliceStable(cands, func(i, j int) bool {
			return schema.TextTokens(cands[i].content) > schema.TextTokens(cands[j].content)
		})
		rep.GateN("sweep_over_ask_cap", len(cands)-maxAskItems)
		cands = cands[:maxAskItems]
	}
	rep.EventN("sweep_offered", len(cands))
	if eligible > assembled {
		rep.EventN("sweep_inventory_thinned", eligible-assembled)
	}
	// DO NOT ASK AT ALL BELOW THE INVENTORY FLOOR. The yield of this mechanism is a property of how
	// many candidates the model compares, and the numbers are not close:
	//
	//	shown 1 output    6% live-kept on haiku, 14% on sonnet — both inside the
	//	                  drop-everything null model's error bar (8,105 recorded decisions)
	//	shown ~15         58% live-kept, at the LOWEST cost per output
	//	batch 3-6         dropped a genuinely-spent output 2 times in 4
	//	batch 10          dropped it 4 in 4, and cleared 100% of genuinely-spent candidates
	//
	// So `cc1aa9f`'s conclusion — "small batches do not make it wrong, they make it UNWILLING TO
	// ACT" — has a corollary this component needs: below about ten, the model is not merely timid,
	// it is answering a question the measurements say it answers badly, and a `drop` from it is a
	// guess. Declining is strictly better than asking, because a wrong keep costs one turn's tokens
	// and a wrong drop costs content the agent still needs.
	//
	// Ten is the measured inflection above, not a round number. Configurable because a deployment
	// whose transcripts are shorter may prefer to trade the yield away entirely rather than act on
	// small inventories.
	//
	// Counted, because a component that declines is indistinguishable from one that is broken unless
	// the decline is recorded — the failure mode that hid the `economic_gate: false` blind spot in
	// this same component and three vacuous trim tests before it.
	if len(cands) < e.minInventory {
		rep.GateN("sweep_inventory_below_min", len(cands))
		return keys, nil
	}

	// Phase 2: ONE ASK for every candidate. Not a batch and not a call per output — nothing is
	// copied per candidate, so there is nothing to divide.
	// Trigger two is decided HERE, where the mass exists. Only consulted when the pre-expiry window
	// did not already fire: the two are OR'd, and pricing a rewrite the first trigger has already
	// justified would let an unrepaid verdict veto a nearly-free removal.
	asking := preExpiry
	if !asking {
		need, have, ok := e.econPays(req, c, cands)
		if ok {
			asking = true
			rep.Event("prefix_rewrite_repaid")
		} else if e.econTrigger {
			rep.Gate("prefix_rewrite_not_repaid")
		}
		if e.econTrigger {
			slog.Debug("cg.sweep.econ", "decision", ok, "needTurns", need, "haveTurns", have,
				"candidates", len(cands))
		}
	}
	if len(cands) > 0 && asking {
		drop, call := e.adjudicate(req, c, rep, cands)
		// Which votes are affordable is a separate question from which outputs were worth asking about.
		// See selectAffordableDrops -- and note it runs ONLY when econ fired, because the pre-expiry
		// window's whole premise is that the prefix it invalidates is nearly worthless.
		if !preExpiry {
			var pruned int
			drop, pruned = e.selectAffordableDrops(req, c, cands, drop)
			if pruned > 0 {
				rep.GateN("drop_unaffordable_pruned", pruned)
			}
		}
		for _, g := range call.gates {
			rep.Gate(g)
		}
		for _, ev := range call.events {
			rep.Event(ev)
		}
		if call.rec.Component != "" {
			rep.Calls = append(rep.Calls, call.rec)
		}
		// Phase 3 (serial): freeze + splice.
		for _, k := range drop {
			desc := sweepDescriptor(cands[k].content)
			// Freeze the decision so every later turn replays it byte-for-byte from the same-session
			// path above, at any depth. Session-scoped only: unlike a compaction, a drop is a
			// judgement about THIS transcript's obligations, so it must never be served to another
			// session whose agent may still need the output.
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

// sweepCand is one candidate the sweep collected. A package-level type because adjudicate needs to
// resolve a model-supplied LABEL back to content, and that mapping must stay on our side of the wire.
type sweepCand struct {
	i       int
	content string
	// id is the CONTENT KEY (extract.ContentKey): the store/stash key and the result-cache key. It is
	// ours, and it appears nowhere in the transcript.
	id string
	// toolID is the wire's own tool-call id, lifted from the normalized message. It is the only string
	// here that also occurs in the transcript the model reads, so it is the only one that can serve as
	// a locating anchor — which is why it is a SEPARATE field rather than a reuse of `id`.
	//
	// They were conflated once, and the effect was worse than omitting the anchor: the inventory
	// announced "tool_use id 300c312d1492952219bfb1c4" while the real id in that transcript was
	// `toolu_d2`, so the contract told the model to locate content by a key that cannot be found
	// anywhere. See #123. Empty when the dialect carries no id, in which case nothing is claimed.
	toolID string
}

// sweepResult is one adjudication's outcome, carried back to the SERIAL phase.
//
// The gate names travel as data rather than being raised where they are decided, because
// components.Report is copied by value across this codebase and its Gates map therefore carries no
// lock. Raising a gate off the serial path is not a slightly-wrong counter, it is
// `fatal error: concurrent map writes` — which is how #119 was found. Kept even though this path now
// makes ONE call: the discipline is what stops the next concurrent thing here from reintroducing it.
type sweepResult struct {
	gates []string
	// events are the names that go to Report.Events rather than Report.Gates: work PERFORMED or
	// neutral observation, as against a candidate turned away. Carried separately for the same
	// reason gates are carried at all — the raise happens on the serial path — and split for the
	// reason Report.Events exists: exported under a metric named "declines", a success made the
	// series climb as the component worked better.
	events []string
	rec    components.ModelCall
}

func (r *sweepResult) gate(name string)  { r.gates = append(r.gates, name) }
func (r *sweepResult) event(name string) { r.events = append(r.events, name) }

// adjudicate makes ONE prefix ask about every candidate and returns the labels it authorised
// dropping.
//
// EVERY FAILURE PATH RESOLVES TOWARD KEEP -- no asker, no stashed prefix, a transport error, a cache
// read that did not happen, an unparseable reply, an unusable verdict, a drop that contradicts a named
// obligation, a verdict for something we did not offer. A wrong keep costs tokens on one turn; a wrong
// drop is a silent permanent loss the agent does not notice and cannot ask about. The two errors are
// not comparable, so this does not treat them symmetrically.
func (e *ExtractSweep) adjudicate(req *bschemas.BifrostChatRequest, c *components.Ctx,
	rep *components.Report, cands []sweepCand) ([]int, sweepResult) {

	var r sweepResult
	// A SINGLE-CANDIDATE ASK IS THE REFUTED SHAPE WEARING A NEW NAME, so it is counted rather than
	// silently accepted. Shown one output, a model simply drops it: 6% live-kept on haiku and 14% on
	// sonnet, both inside the drop-everything null model's error bar. The ask still proceeds — a
	// transcript can legitimately have one candidate above the floor — but a workload where this
	// fires routinely has an upstream filter starving the inventory, which is the failure that cost
	// three iterations (4ca1f13).
	if len(cands) < 2 {
		r.event("sweep_inventory_of_one")
	}

	// FILL THE EVIDENCE SEAM. The co-reference index's record for each candidate goes into the
	// inventory line as EVIDENCE for the model to weigh — never as a filter over `cands`. That
	// distinction is the whole lesson of the `prefix_still_referenced` thinner documented above: a
	// pre-filter left about one candidate per request, which silently turned a bulk arm into the
	// per-output shape refuted at 6% live-kept, AND meant the model only ever saw what the index had
	// already judged spent, destroying the veto on the index's blind spot that the mechanism exists to
	// provide. Evidence preserves the veto: the index states what it saw, the model may disagree.
	//
	// Keyed by message index, which is what both sides already agree on — Record.Idx and sweepCand.i
	// are the same coordinate. A candidate with no record is normal, not an error: the index applies
	// its own size floor, and saying so beats emitting zeros that read as "nothing referenced it".
	byIdx := map[int]*coref.Record{}
	if e.evidence {
		recs := coref.Index(flattenForCoref(req), e.minTokens, schema.TextTokens)
		for i := range recs {
			byIdx[recs[i].Idx] = &recs[i]
		}
	}
	items := make([]extract.AdjudicationItem, 0, len(cands))
	for k := range cands {
		it := extract.AdjudicationItem{
			Label:      k,
			ID:         cands[k].toolID, // the wire's id, not our content key — see #123
			SizeTokens: schema.TextTokens(cands[k].content),
			Head:       extract.HeadLine(cands[k].content, extract.AdjudicationHeadChars),
		}
		if e.evidence {
			it.Evidence = renderEvidence(byIdx[cands[k].i], laterModelTurns(req, cands[k].i))
		}
		items = append(items, it)
	}
	if e.evidence {
		// Counted, because "the index had an opinion" and "the index was silent" produce the same
		// inventory line length and would otherwise be indistinguishable in a run's counters.
		for k := range cands {
			if byIdx[cands[k].i] == nil {
				r.event("evidence_no_index_record")
			}
		}
	}
	// The transcript, flattened, so a claimed obligation quote is VERIFIED against what the agent was
	// actually told rather than trusted. This is the only remaining signal that the model is
	// inventing, because nothing else it returns is content.
	//
	// Built from the INCOMING request, while the ask reads the PREVIOUS turn's sent body. The two
	// differ, and in the safe direction: the incoming transcript is a superset in content (nothing
	// removed) and one turn newer, so a quote the model took from the cached prefix is still findable
	// here. A quote it invented is still not.
	flat := flattenTranscript(req)

	ctx, cancel := context.WithTimeout(c.Ctx, llmCallTimeout)
	defer cancel()
	var before int
	for _, it := range items {
		before += it.SizeTokens
	}
	start := time.Now()
	var (
		reply string
		usage components.PrefixUsage
		err   error
		// fellBack records that the expensive path has already run, so the cache-read check below does
		// not fire a second time for the same call. Without it a failed ask both fell back AND then
		// reported a zero cache read, double-counting one event as two.
		fellBack bool
	)
	if c.PrefixAsk == nil {
		// No asker at all: a non-Anthropic route, or no incoming client. Not a failure of the ask —
		// there was nothing to ask through — so it takes the same fork as a missed read.
		r.gate("sweep_no_asker")
		if e.blockFallback {
			r.gate("sweep_fallback_blocked")
			r.rec.Rejection = "no prefix asker on this route and block_fallback is set"
			return nil, r
		}
		if reply, err = e.fallbackAsk(ctx, req, c, &r, items, cands); err != nil {
			return nil, r
		}
		fellBack = true
	} else {
		reply, usage, err = c.PrefixAsk.Ask(ctx, c.Session, extract.BuildPrefixAsk(items))
	}
	latency := float64(time.Since(start).Milliseconds())
	metrics.RecordExtractionCall(latency)
	// COST, which this record carried as $0.00 forever. It never set CostUSD at all, so the per-call
	// ledger the dashboard shows for this component reported zero on every firing — measured live at
	// $0.00 against real cache reads of 449,304 and 449,376 tokens and real completion tokens, while
	// the request-level rollup (proxy/dashcapture.go, cg_llm_cost_usd) had the true $0.0940 and
	// $0.1652. Two recorded totals disagreeing, one of them structurally zero, is worse than either
	// alone: a component whose whole justification is cost looked free.
	//
	// Priced from the REQUEST's model, not a cheap-model card, because that is what this component
	// calls by construction — and from the same rates the request-level figure uses, so the two agree
	// rather than being two independent guesses. c.SelfRates is the model the request came in on;
	// falling back to the env card keeps a figure when the host supplies no rates, the same
	// convention extract_llm.pricingFor uses.
	pricing := cheapmodel.PricingFromEnv()
	if !c.SelfRates.Zero() {
		pricing = ratesPricing(c.SelfRates)
	}
	r.rec = components.ModelCall{
		Component: rep.Component, Model: c.ModelName, Strategy: "prefix_ask",
		CandidateTokens: before, LatencyMs: latency,
		PromptTokens: int64(usage.Fresh), CompletionTokens: int64(usage.Output),
		CacheRead: int64(usage.CacheRead), CacheWrite: int64(usage.CacheWrite),
		CostUSD: pricing.Cost(int64(usage.Fresh), int64(usage.Output),
			int64(usage.CacheWrite), int64(usage.CacheRead)),
		GateReason: "pre-expiry window: the cache still exists and is nearly worthless",
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			atomic.AddInt64(&llmTimeouts, 1)
		} else {
			atomic.AddInt64(&llmErrors, 1)
		}
	}
	if !fellBack && err != nil {
		// A first turn has no stashed prefix, which arrives here as an error. Counted separately from
		// a transport failure: one is "there was nothing to read yet", which every session does once
		// and which needs no attention, and the other is "the read failed", which does.
		if errors.Is(err, components.ErrNoPrefix) {
			r.gate("sweep_no_prefix")
		} else {
			r.gate("sweep_ask_failed")
		}
		if e.blockFallback {
			r.gate("sweep_fallback_blocked")
			r.rec.Rejection = "prefix ask failed and block_fallback is set: " + err.Error()
			return nil, r
		}
		if reply, err = e.fallbackAsk(ctx, req, c, &r, items, cands); err != nil {
			return nil, r
		}
		fellBack = true
	}
	// THE CACHE READ IS THE MECHANISM'S WHOLE JUSTIFICATION, so a read that did not happen is always
	// COUNTED — a silent miss is what hid this class of problem before, and it looks identical to a
	// working call except on the bill.
	//
	// What happens next is a choice between two real costs, and the default is to keep working:
	//
	//	FALL BACK (default). Ask again with the outputs copied into the prompt. That pays fresh for
	//	content the cached path reads for a tenth of the price, and shows the model a TRUNCATED view of
	//	each output — but it keeps the component alive on a session's first turn and whenever an entry
	//	has gone. Treating "no prefix" as "no verdicts" would disable it there and read, in the
	//	counters, as a model that declined to act.
	//	DECLINE (`block_fallback: true`). Forgo the yield rather than pay for it. The right choice
	//	where the bill matters more than the removal, and the honest one to reach for if
	//	sweep_prefix_cache_read_ZERO turns out to be common.
	//
	// Note what neither mode can do: prevent the fresh read that already happened on THIS call. The
	// counter is what tells an operator the window is mistimed.
	if !fellBack && usage.CacheRead == 0 {
		r.gate("sweep_prefix_cache_read_ZERO")
		if e.blockFallback {
			r.gate("sweep_fallback_blocked")
			r.rec.Rejection = "the prefix ask read nothing from cache and block_fallback is set; " +
				"declining rather than paying again for a full-price transcript read"
			return nil, r
		}
		if reply, err = e.fallbackAsk(ctx, req, c, &r, items, cands); err != nil {
			return nil, r
		}
		fellBack = true
	} else if !fellBack {
		r.event("sweep_prefix_cache_read_ok")
	}
	// HOW THE ANSWER ARRIVED: the proxy's structured-answer tool, or reply prose. Split because
	// nothing else in this component can tell the two apart. ParseVerdicts reads a tool_use `input`
	// and a JSON array in text identically -- by design, so that declaring the tool is additive --
	// which means a run where the declared tool is never touched produces the same verdicts, the same
	// savings and the same gate counts as one where it is used every time. A review of PR #137
	// measured exactly that divergence live (0 of 5 asks used the tool, all 5 answered in prose)
	// against that PR's claim of 6 of 6, and no published counter could adjudicate between them.
	//
	// Only for the PREFIX ask. The fallback calls Complete(), which has no tool to declare, so
	// counting it as prose would report a shape that was never on offer.
	if !fellBack {
		if usage.ViaTool {
			r.event("sweep_answered_via_tool")
		} else {
			r.event("sweep_answered_via_prose")
		}
	}
	for range items {
		r.event("sweep_adjudicated")
	}

	verdicts, parsed := extract.ParseVerdicts(reply)
	if !parsed {
		// TRUNCATION IS NOT JUNK, and the two need opposite fixes -- raise the budget versus fix the
		// prompt -- so one name for both hid a 70%-of-calls failure behind a label that reads as "the
		// prompt is wrong".
		if extract.ReplyWasTruncated(reply) {
			r.gate("sweep_reply_truncated")
		} else {
			r.gate("sweep_unparseable")
		}
		if sweepUnusableSamples.Add(1) <= maxSweepUnusableSamples {
			head := reply
			if len(head) > 500 {
				head = head[:500]
			}
			slog.Warn("cg.sweep.unusable_reply", "reply_len", len(reply),
				"offered", len(items), "head", head)
		}
		r.rec.Rejection = "reply did not parse; every output kept verbatim"
		return nil, r
	}
	if len(verdicts) == 0 {
		// A well-formed EMPTY array: the model read the inventory and kept all of it. The contract
		// explicitly invites that, so it must not be filed as a failure -- that conflation is what
		// made "the model declined to act" and "the model was never successfully asked" the same
		// number for three iterations (4ca1f13).
		// SPLIT BY PATH, because a keep-all means different things on each and averaging them hides
		// the more interesting one. The fallback has no transcript, so it cannot see that a task
		// closed and resolves toward keep structurally -- measured at 12 of 12 kept where the prefix
		// ask dropped 12 of 12 on the same content (#125). Without this split a run's numbers read as
		// "the component sometimes acts and sometimes does not", when the real variable is whether
		// the cache read happened. It is also how the goal-ordering fix in sweepIntent gets checked
		// against real traffic rather than argued about.
		if fellBack {
			r.gate("sweep_fallback_kept_everything")
		} else {
			r.gate("sweep_kept_everything")
		}
		r.rec.Rejection = "adjudicated: keep everything"
		return nil, r
	}

	var drop []int
	seen := map[int]bool{}
	var removed int
	for _, v := range verdicts {
		if v.Label < 0 || v.Label >= len(cands) {
			// A verdict for something we did not offer. NEVER acted on: the label is how a decision is
			// keyed to an output, so a wrong label is a decision about an unknown message — and
			// indexing on it would panic rather than merely act wrongly.
			//
			// WHAT THIS CANNOT CATCH is a label that is IN RANGE but wrong: a verdict meant for output
			// 4 arriving as output 5 removes the wrong content and looks perfectly valid from here, and
			// nothing downstream can detect it either. That is the failure the tool_use id in the
			// inventory exists to PREVENT rather than to detect -- an exact anchor between the line and
			// the content makes mis-keying less likely in the first place, which is the only defence
			// available against a plausible-but-wrong label.
			r.gate("sweep_verdict_unknown_label")
			continue
		}
		if seen[v.Label] {
			r.gate("sweep_verdict_duplicate_label")
			continue
		}
		seen[v.Label] = true
		content := cands[v.Label].content
		a := extract.Judge(v, flat)
		if a.QuoteFabricated {
			r.gate("sweep_quote_fabricated")
		}
		if a.CriterionMissing {
			r.gate("sweep_criterion_missing")
		}
		if a.VerdictUnusable {
			r.gate("sweep_verdict_unusable")
		}
		// The refusal is counted INSTEAD of a keep, not alongside it. Both leave the output verbatim,
		// but "the model judged this still needed" and "the model tried to remove something it had
		// just said was needed" are different events, and folding the second into the keep total is
		// what would make the alertable one invisible in the ratio an operator actually looks at.
		if a.RefusedObligation {
			r.gate("sweep_drop_refused_obligation")
			continue
		}
		if !a.Drop {
			r.gate("sweep_kept")
			continue
		}
		sz := schema.TextTokens(content)
		after := schema.TextTokens(sweepDescriptor(content))
		if after >= sz {
			// The never-worse check also lives in applySweepDrop, marker included. This one is here
			// so a decision phase 3 will refuse is not counted as a removal.
			r.gate("sweep_drop_would_not_shrink")
			continue
		}
		r.event("sweep_dropped")
		drop = append(drop, v.Label)
		removed += sz - after
		metrics.RecordExtractionSaving(sz - after)
		metrics.RecordExtractionValue(float64(sz-after) * savedTokenValue(c).perToken)
	}
	// An output named in the inventory that no verdict mentioned is UNJUDGED, and it must not look
	// like a keep: 4ca1f13 found a live arm where the model silently omitted labels and the missing
	// answers were invisible, so "the inventory is starved" and "the model answered for a third of
	// it" were the same number.
	for _, it := range items {
		if !seen[it.Label] {
			r.gate("sweep_verdict_missing")
		}
	}
	if removed > 0 {
		r.rec.Accepted = true
		r.rec.SavedTokens = removed
	} else {
		r.rec.Rejection = "adjudicated: nothing was spent"
	}
	if debugExtractLLM(c) {
		// SESSION AND BOUNDARY ARE HERE SO THIS LINE CAN BE JOINED, which it could not be before.
		// This is the only record carrying the ask's economics, and without a session it cannot be tied
		// to the request that produced it -- so "why was 40% of this ask charged fresh?" was
		// unanswerable from a completed run's logs. It has exactly one benign explanation (the client's
		// last cache_control breakpoint sits before the end of the body, so the tail past it is
		// uncached and our appended question pays for it) and one alarming one (the prefix we send no
		// longer matches what the provider cached), and they are distinguished by max_cached_idx and
		// the request size, both of which are already in hand here.
		logging.From(c.Ctx).Debug("cg.sweep.ask", "offered", len(items),
			"verdicts", len(verdicts), "dropped", len(drop), "candidate_tokens", before,
			"removed_tokens", removed, "cache_read", usage.CacheRead, "fresh", usage.Fresh,
			"session", c.Session, "max_cached_idx", c.MaxCachedIdx,
			"req_tokens", schema.MessagesTokens(req), "messages", len(req.Input))
	}
	return drop, r
}

// fallbackAsk is the EXPENSIVE path: a self-contained completion carrying a bounded sample of every
// candidate, for when the prefix ask could not read the cache.
//
// It goes to the REQUEST's own model, the same one the prefix ask would have addressed. Not a cheap
// one: the measurement that chose this model is about faithful quoting, not about caching — verbatim
// quoting degraded to 20.8% on the cheap model against 0 of 59 on the request model — and a fabricated
// quote is the only remaining signal that the model is inventing. That reason survives the loss of the
// cache read intact, so the fallback must not quietly downgrade the judge as well as the prompt.
//
// The reply budget is raised through components.Budgeter where the client supports it, for the same
// reason the prefix ask raises it: one reply carries a verdict for every candidate.
func (e *ExtractSweep) fallbackAsk(ctx context.Context, req *bschemas.BifrostChatRequest,
	c *components.Ctx, r *sweepResult, items []extract.AdjudicationItem,
	cands []sweepCand) (string, error) {
	model := c.Model.For("incoming")
	if model == nil {
		r.gate("sweep_fallback_no_model")
		r.rec.Rejection = "the prefix ask could not read the cache and no request model is available"
		return "", errNoFallbackModel
	}
	if b, ok := model.(components.Budgeter); ok {
		if m := b.WithMaxTokens(cheapmodel.PrefixAskMaxTokens); m != nil {
			model = m
		}
	}
	// The samples are attached HERE and nowhere else, which is what keeps content off the prefix-ask
	// path by construction rather than by care.
	withSamples := make([]extract.AdjudicationItem, len(items))
	for i, it := range items {
		it.Sample = extract.ClipSample(cands[it.Label].content, extract.FallbackSampleChars)
		withSamples[i] = it
	}
	r.event("sweep_fallback_used")
	reply, err := model.Complete(ctx, extract.BuildFallbackAsk(sweepIntent(req), withSamples))
	if err != nil {
		r.gate("sweep_fallback_failed")
		r.rec.Rejection = "fallback completion failed: " + err.Error()
		return "", err
	}
	return reply, nil
}

// sweepIntent renders the conversation's intent for a SPENT-NESS judgement, which wants it ordered
// differently from every other component's relevance question.
//
// conversationGoal joins firstUser, lastAsst, lastUser in that order, unlabelled. That is right for
// extract_llm, which asks "is this output relevant to the task" — the opening instruction IS the
// task. It is wrong here, and measurably so. This component asks whether an output is SPENT, and the
// opening instruction describes what the session set out to do, which is precisely what may now be
// finished. Leading with it makes everything look needed.
//
// MEASURED, on two near-identical transcripts with twelve candidates each: the prefix ask dropped
// 12 of 12, while the fallback — same content, goal-string only — kept 12 of 12 and cited the
// original read instruction as the obligation for every one. See #125. The fallback has no
// transcript by construction, so it cannot see that the task closed; the goal string is the only
// place that can tell it.
//
// So: same three parts, ordered current-FIRST and LABELLED, with the original instruction explicitly
// marked as possibly already satisfied. The parts map onto the contract's own criteria — (a) the
// current step, (b) an unfinished user instruction, (c) a next step the agent stated — rather than
// arriving as one undifferentiated blob the model has to guess the structure of.
//
// The original instruction is kept rather than dropped, deliberately: criterion (b) is an unfinished
// USER instruction, and a standing "…and summarise all of them at the end" lives in exactly that
// message. Removing it would trade a bias toward keeping for a bias toward dropping, which is the
// direction that loses content the agent still needs.
func sweepIntent(req *bschemas.BifrostChatRequest) string {
	var firstUser, lastUser, lastAsst string
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleUser {
			firstUser = strings.TrimSpace(schema.MessageText(req.Input[i]))
			break
		}
	}
	for i := len(req.Input) - 1; i >= 0; i-- {
		switch req.Input[i].Role {
		case bschemas.ChatMessageRoleUser:
			if lastUser == "" {
				lastUser = strings.TrimSpace(schema.MessageText(req.Input[i]))
			}
		case bschemas.ChatMessageRoleAssistant:
			if lastAsst == "" {
				lastAsst = strings.TrimSpace(schema.MessageText(req.Input[i]))
			}
		}
		if lastUser != "" && lastAsst != "" {
			break
		}
	}
	var b strings.Builder
	add := func(label, text string) {
		if text == "" {
			return
		}
		b.WriteString(label)
		b.WriteString("\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	add("MOST RECENT USER TURN — this is the step the agent is on now:", lastUser)
	add("THE AGENT'S OWN LAST STATEMENT — any next step it named is an obligation:", lastAsst)
	// Last, and flagged. Its position in the prompt is the fix.
	if firstUser != "" && firstUser != lastUser {
		add("THE SESSION'S ORIGINAL INSTRUCTION — MAY ALREADY BE SATISFIED; treat it as an "+
			"obligation only if some part of it is still outstanding:", firstUser)
	}
	return clipRunes(strings.TrimSpace(b.String()), goalCap)
}

// errNoFallbackModel is returned when the fallback has nowhere to go. Its own type so the caller can
// distinguish "we chose not to" from "we could not".
var errNoFallbackModel = errors.New("no request model for the sweep fallback")

// flattenTranscript renders the agent's own text as one string, for verifying an obligation quote.
// Every text block of every message, tool results included: an obligation can be created by a user
// instruction, by the agent's own stated next step, or by something a tool told it.
func flattenTranscript(req *bschemas.BifrostChatRequest) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for i := range req.Input {
		b.WriteString(schema.MessageText(req.Input[i]))
		b.WriteByte('\n')
	}
	return b.String()
}

func init() {
	components.RegisterFields("extract_llm_sweep", extractSweepConfig{}, []components.Field{
		{Key: "min_tokens", Type: components.FieldInt, Default: defaultSweepFloor, Min: 1,
			Hint: "Per-output floor for naming a candidate in the inventory. NOTE this no longer has to price a drop as well: selectAffordableDrops decides which votes are worth their cache-write, on DEPTH relative to the rest of the batch rather than on size, so a low floor is safe on the econ path. Measured on iteration 022, a floor of 1000 named 4.5 of the 23.6 tool outputs a request carried and held the batch at 4.4 against a cap of 12, while a floor of 100 reaches 11.6 for 8% more mass -- candidates are nearly free (one inventory line) and are the axis the mechanism lives on. Every line is paid fresh, and a small output's removal cannot repay the marker it leaves behind. At 3000 the shipped preset produced ZERO extractions across 3,437 production requests."},
		{Key: "min_inventory", Type: components.FieldInt, Default: defaultMinInventory, Min: 1,
			Hint: "Fewest candidates worth asking about; below it the sweep declines without asking. The model's judgement is a function of how many candidates it COMPARES, and the numbers are far apart: shown one output it scored 6% live-kept on haiku and 14% on sonnet, both inside the drop-everything null model's error bar, while ~15 together reached 58% at the lowest cost per output. At batch 3-6 it dropped a genuinely-spent output 2 times in 4; at 10, 4 in 4. Below the floor a removal is a guess, and a wrong removal costs content the agent still needs while a wrong keep costs one turn's tokens. Lower it only to trade that asymmetry away deliberately."},
		{Key: "pre_expiry_seconds", Type: components.FieldInt, Default: int(defaultPreExpiry / time.Second),
			Hint: "How long before the prompt cache's believed expiry the sweep may fire. The window is where BOTH halves are cheap: the ask still reads a live cache, and the prefix it invalidates has little life left. The TTL itself is read from the request, never assumed. This WIDTH is the component's one unmeasured number — wider fires more often and invalidates more remaining TTL, narrower fires rarely, and nothing measures either side."},
		{Key: "block_fallback", Type: components.FieldBool,
			Hint: "Decline instead of falling back when the prefix ask could not read the cache. Unset = FALSE: the fallback asks again with a bounded sample of each output copied into the prompt, which keeps the component working on a session's first turn and whenever a cache entry has gone — but pays fresh for content the cached path reads for a tenth of the price. Set true where the bill matters more than the removal. The miss is counted either way."},
		{Key: "evidence", Type: components.FieldBool,
			Hint: "Add the co-reference index's record (novel/refs/ref_age/used_frac/later_turns and the index's own verdict) to each candidate's inventory line. Unset = FALSE. It is EVIDENCE the model weighs, never a filter over the candidates: a co-reference PRE-FILTER left about one candidate per request, which silently turned a bulk arm into the per-output shape refuted at 6% live-kept and meant the model only ever saw what the index had already judged spent — destroying the veto on the index's blind spot that the mechanism exists to provide. Enabling this also adds a paragraph to the adjudication contract teaching how to read the counters; a prompt carrying counters it never explains is worse than one carrying neither."},
		{Key: "econ_trigger", Type: components.FieldBool,
			Hint: "Add the ECONOMIC trigger alongside the pre-expiry window: sweep a LIVE cached prefix when the removal's saving, collected over the turns estimated to remain, exceeds the cache-write it forces (S*T > 11.5*W). Unset = FALSE, because it deliberately invalidates a prefix the provider still holds. The two triggers are OR'd and neither contains the other — pre-expiry fires on the clock and cannot reach a session whose cache keeps being refreshed, which is the long run with the most to save; econ fires on mass and cannot know how much time is left. S is the inventory's whole mass and so an upper bound on the batch's real saving; read prefix_rewrite_repaid / prefix_rewrite_not_repaid rather than assuming a fired trigger banked anything."},
		markerModeField(),
	})
}
