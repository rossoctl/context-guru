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
		blockFallback: cfg.BlockFallback,
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
			metrics.RecordExtractionCacheLookup(rep.Component, true)
			// Inside the ok branch, with rep.Replay. It used to sit above the call, so a drop
			// applySweepDrop declined still booked its token savings — and after #188 that
			// call can decline for a new reason (the reserve), on precisely the runs where the
			// savings figure is being measured. rep.Replay on this path was already guarded
			// correctly; this is the metric matching it.
			if k, ok := applySweepDropReplay(c, rep, e.mode, msg, content); ok {
				if saved := schema.TextTokens(content) - schema.TextTokens(cached.Projected); saved > 0 {
					metrics.RecordExtractionValue(rep.Component, float64(saved)*val.repeatPerToken)
				}
				changed++
				if k != "" {
					keys = append(keys, k)
				}
				rep.Replay("reapplied_same_session")
			}
			continue
		}
		if !sweeping {
			// Outside the window no NEW decision is taken; the replays above already ran, which is
			// all such a turn has to do.
			continue
		}
		metrics.RecordExtractionCacheLookup(rep.Component, false)
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
	if len(cands) > 0 && sweeping {
		drop, call := e.adjudicate(req, c, rep, cands)
		for _, g := range call.gates {
			rep.Gate(g)
		}
		for _, ev := range call.events {
			rep.Event(ev)
		}
		// Phase 3 (serial): freeze + splice.
		applied := 0
		for _, k := range drop {
			// THE DROP FIRST, then the decision. putResult ran ahead of applySweepDrop, and
			// once the reserve could refuse that left a frozen cg:res: record for a drop that
			// never happened — the same dangling-decision-then-late-splice shape as
			// extract_llm's phase 3: the same-session replay path above reads the record,
			// bypasses the depth gate because the bytes were "already sent", and removes the
			// output from inside the provider's cached prefix on some later turn.
			key, ok := applySweepDrop(c, rep, e.mode, &req.Input[cands[k].i], cands[k].content)
			if !ok {
				continue
			}
			// Freeze the decision so every later turn replays it byte-for-byte from the same-session
			// path above, at any depth. Session-scoped only: unlike a compaction, a drop is a
			// judgement about THIS transcript's obligations, so it must never be served to another
			// session whose agent may still need the output.
			putResult(c, cands[k].id, sweepDescriptor(cands[k].content), "")
			// Booked here, where the drop is a fact. rep.Replay on the replay path above was
			// already guarded this way; the fresh path was not.
			saved := schema.TextTokens(cands[k].content) -
				schema.TextTokens(sweepDescriptor(cands[k].content))
			if saved > 0 {
				applied += saved
				metrics.RecordExtractionSaving(rep.Component, saved)
				metrics.RecordExtractionValue(rep.Component, float64(saved)*val.perToken)
			}
			changed++
			if key != "" {
				keys = append(keys, key)
			}
		}
		// AFTER phase 3: rep.Calls takes a COPY of the row, and Accepted/SavedTokens are only
		// knowable once the drops have been applied. `applied` is what reached the wire, which is
		// the figure the ledger's own doc claims it carries.
		if applied > 0 {
			call.rec.Accepted = true
			call.rec.SavedTokens = applied
		} else if call.rec.Rejection == "" {
			// The adjudicator named spent outputs and NONE of them could be dropped. Distinct
			// from "nothing was spent", and it is the reserve-exhausted shape: without its own
			// reason the row read as a plain rejection.
			call.rec.Rejection = "adjudicated spent, but no drop could be applied"
		}
		if call.rec.Component != "" {
			rep.Calls = append(rep.Calls, call.rec)
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
// The sweepResult is a NAMED result purely so `defer foldFallback()` below can reach it. It has to
// be: every early return here is `return nil, r` on a LOCAL, and a deferred mutation of a local
// happens after the return value has already been copied, so the fold would be silently lost on
// exactly the error paths it exists to cover. The first result stays blank-named — only this one
// needs the treatment, and saying so beats leaving a reader to wonder what `dropped` is for.
func (e *ExtractSweep) adjudicate(req *bschemas.BifrostChatRequest, c *components.Ctx,
	rep *components.Report, cands []sweepCand) (_ []int, r sweepResult) {

	// A SINGLE-CANDIDATE ASK IS THE REFUTED SHAPE WEARING A NEW NAME, so it is counted rather than
	// silently accepted. Shown one output, a model simply drops it: 6% live-kept on haiku and 14% on
	// sonnet, both inside the drop-everything null model's error bar. The ask still proceeds — a
	// transcript can legitimately have one candidate above the floor — but a workload where this
	// fires routinely has an upstream filter starving the inventory, which is the failure that cost
	// three iterations (4ca1f13).
	if len(cands) < 2 {
		r.event("sweep_inventory_of_one")
	}

	items := make([]extract.AdjudicationItem, 0, len(cands))
	for k := range cands {
		items = append(items, extract.AdjudicationItem{
			Label:      k,
			ID:         cands[k].toolID, // the wire's id, not our content key — see #123
			SizeTokens: schema.TextTokens(cands[k].content),
			Head:       extract.HeadLine(cands[k].content, extract.AdjudicationHeadChars),
		})
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
	//
	// Resolved BEFORE the ask because both legs below price themselves with it. The fallback goes to
	// the same model the prefix ask addresses (c.Model.For("incoming")), so one rate card is right
	// for both.
	pricing := cheapmodel.PricingFromEnv()
	if !c.SelfRates.Zero() {
		pricing = ratesPricing(c.SelfRates)
	}
	var (
		reply string
		usage components.PrefixUsage
		err   error
		// fellBack records that the expensive path has already run, so the cache-read check below does
		// not fire a second time for the same call. Without it a failed ask both fell back AND then
		// reported a zero cache read, double-counting one event as two.
		fellBack bool
		// The ask's own totals, accumulated PER LEG. An adjudication is not one model call: the
		// prefix ask and the fallback are two, on two prompts, and either can be the only one that
		// happens. See recordLeg.
		askMs, askCost float64
		fbUsage        components.PrefixUsage
		fbMs, fbCost   float64
	)
	// recordLeg books ONE model call this component made — its wall time and its own priced spend.
	//
	// PER LEG, not once per adjudication, because `fallbackAsk` is a SECOND model call on a full
	// sampled transcript and it used to be accounted at $0.00 and 0 ms. The single record was built
	// from the PREFIX ask's usage and assigned once, while two of the three fallback points fire
	// after that assignment — so on every route without an Anthropic prefix asker (where the prefix
	// ask never happens at all), on every session's first turn (ErrNoPrefix) and on every mistimed
	// window (CacheRead == 0), the component's real frontier-model spend was reported as free.
	//
	// That was survivable before this PR only by accident: the fallback's tokens still reached
	// /stats through cheapmodel's process totals, which the host passed in as the `cost` argument.
	// Making the components price their own spend removed that accident, so the leg has to book
	// itself. This is the same defect class as #176, in the component this PR re-scoped.
	recordLeg := func(ms float64, u components.PrefixUsage) float64 {
		metrics.RecordExtractionCall(rep.Component, ms)
		cost := pricing.Cost(int64(u.Fresh), int64(u.Output),
			int64(u.CacheWrite), int64(u.CacheRead))
		metrics.RecordExtractionSpend(rep.Component, cost)
		return cost
	}
	// runFallback runs the expensive path and books it as its own leg, at all three call sites, so a
	// fourth one cannot be added without the accounting coming with it. It records even when the
	// call ERRORS: a failed completion still burned wall time, and on some failures tokens.
	runFallback := func() (string, error) {
		out, u, ms, err := e.fallbackAsk(ctx, req, c, &r, items, cands)
		fbUsage, fbMs = u, ms
		fbCost = recordLeg(ms, u)
		return out, err
	}
	// foldFallback adds the fallback leg's tokens, dollars and wall time into the ask's ledger row.
	//
	// Idempotent by construction — at most one fallback runs per adjudication and this READS the
	// accumulators rather than adding to them — which is what lets it be deferred and also called
	// explicitly on the happy path, where the row is built after the no-asker fallback has already
	// run and would otherwise overwrite the fold.
	//
	// DEFERRED, so it also covers the three fallback ERROR paths. Each of those is
	// `if reply, err = runFallback(); err != nil { return nil, r }`, and `runFallback` has already
	// booked the leg into metrics via recordLeg — so /stats counted the call and its seconds while
	// the ledger row kept only the prefix ask's LatencyMs and a Strategy naming a leg that was no
	// longer the only one that ran. Latency rather than dollars, because on an error the sink is
	// empty (recordUsageCache is reached only after a successful decode on both backends, and
	// neither returns an error after billing) — but the fallback is the SLOW leg by construction, so
	// a failed one contributes tens of seconds to avg_latency_ms against a row showing milliseconds.
	foldFallback := func() {
		if fbMs == 0 && fbCost == 0 && fbUsage == (components.PrefixUsage{}) {
			return
		}
		// IDENTITY FIRST, and this is the half of the fix that is easy to miss. On the no-asker
		// path the fallback runs BEFORE r.rec exists, so if it errors the row is never built at
		// all — Component stays "" and the caller drops the whole row on the
		// `call.rec.Component != ""` guard. /stats then reports a call the ledger has no row for.
		// That path never built a row before either, so the row is not a regression; the recorded
		// spend and latency are new, so the DIVERGENCE is.
		if r.rec.Component == "" {
			r.rec.Component = rep.Component
			r.rec.Model = c.ModelName
			r.rec.CandidateTokens = before
			r.rec.GateReason = "pre-expiry window: the cache still exists and is nearly worthless"
		}
		r.rec.LatencyMs = askMs + fbMs
		r.rec.PromptTokens = int64(usage.Fresh + fbUsage.Fresh)
		r.rec.CompletionTokens = int64(usage.Output + fbUsage.Output)
		r.rec.CacheRead = int64(usage.CacheRead + fbUsage.CacheRead)
		r.rec.CacheWrite = int64(usage.CacheWrite + fbUsage.CacheWrite)
		r.rec.CostUSD = askCost + fbCost
		// Name what actually ran. On the no-asker route the prefix ask never happens, so calling
		// the row "prefix_ask+fallback" would report a leg that did not exist.
		if c.PrefixAsk == nil {
			r.rec.Strategy = "fallback"
		} else {
			r.rec.Strategy = "prefix_ask+fallback"
		}
	}
	// One arming point for all four exits that can carry a fallback — the three error returns and
	// the happy path. A per-site call was what left the error paths uncovered, and a fifth site
	// added later would have been missed the same way.
	defer foldFallback()
	if c.PrefixAsk == nil {
		// No asker at all: a non-Anthropic route, or no incoming client. Not a failure of the ask —
		// there was nothing to ask through — so it takes the same fork as a missed read.
		r.gate("sweep_no_asker")
		if e.blockFallback {
			r.gate("sweep_fallback_blocked")
			r.rec.Rejection = "no prefix asker on this route and block_fallback is set"
			return nil, r
		}
		if reply, err = runFallback(); err != nil {
			return nil, r
		}
		fellBack = true
	} else {
		askStart := time.Now()
		reply, usage, err = c.PrefixAsk.Ask(ctx, c.Session, extract.BuildPrefixAsk(items))
		askMs = float64(time.Since(askStart).Milliseconds())
		// ErrNoPrefix is refused LOCALLY — there is no stashed body to append to, so no request
		// leaves the process. Booking it as a call would put a 0 ms, $0 sample into the mean that
		// the exploration brake reads, and inflate `calls` with work that by definition did not
		// happen. Every other outcome, transport failure included, went to the provider.
		if !errors.Is(err, components.ErrNoPrefix) {
			askCost = recordLeg(askMs, usage)
		}
	}
	r.rec = components.ModelCall{
		Component: rep.Component, Model: c.ModelName, Strategy: "prefix_ask",
		CandidateTokens: before, LatencyMs: askMs,
		PromptTokens: int64(usage.Fresh), CompletionTokens: int64(usage.Output),
		CacheRead: int64(usage.CacheRead), CacheWrite: int64(usage.CacheWrite),
		CostUSD:    askCost,
		GateReason: "pre-expiry window: the cache still exists and is nearly worthless",
	}
	// The no-asker path's fallback ran BEFORE this row existed, and the assignment above just
	// overwrote the deferred fold's work-in-progress. Re-folded here rather than relying on the
	// defer alone because the defer's ordering relative to this assignment is what makes that
	// reliance fragile — and folding twice is free, since foldFallback reads the accumulators.
	foldFallback()
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
		if reply, err = runFallback(); err != nil {
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
		if reply, err = runFallback(); err != nil {
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
		// NOT BOOKED HERE. A verdict is a decision, not a removal: phase 3 still has to apply it,
		// and that can decline — the reserve refuses the payload, or the marker-inclusive
		// never-worse check fails. The descriptor-only pre-check above catches neither (it is not
		// marker-inclusive, and it cannot see the reserve at all), so a saturated reserve booked
		// the full saving for outputs that went upstream untouched. Recorded in phase 3 instead,
		// per candidate, once the drop is a fact.
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
	// `removed` is what the ADJUDICATOR judged spent, which is why Accepted/SavedTokens are filled
	// by the caller after phase 3 rather than here: they describe drops that actually happened, and
	// the two numbers diverge exactly when the reserve is refusing. The rejection reason is still
	// this function's to state — it is about the verdict, not about the splice.
	if removed == 0 {
		r.rec.Rejection = "adjudicated: nothing was spent"
	}
	if debugExtractLLM(c) {
		logging.From(c.Ctx).Debug("cg.sweep.ask", "offered", len(items),
			"verdicts", len(verdicts), "dropped", len(drop), "candidate_tokens", before,
			"removed_tokens", removed, "cache_read", usage.CacheRead, "fresh", usage.Fresh)
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
// It returns its OWN usage and wall time so the caller can price it. `model.Complete` reports
// neither, so the usage is read from a per-call cheapmodel sink nested inside whatever scope already
// wraps ctx — the same construction extract_llm uses to attribute one call. The sink also reaches
// every ancestor, so the request's own bill does not lose these tokens.
func (e *ExtractSweep) fallbackAsk(ctx context.Context, req *bschemas.BifrostChatRequest,
	c *components.Ctx, r *sweepResult, items []extract.AdjudicationItem,
	cands []sweepCand) (string, components.PrefixUsage, float64, error) {
	model := c.Model.For("incoming")
	if model == nil {
		r.gate("sweep_fallback_no_model")
		r.rec.Rejection = "the prefix ask could not read the cache and no request model is available"
		return "", components.PrefixUsage{}, 0, errNoFallbackModel
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
	ctx, sink := cheapmodel.WithCallSink(ctx)
	start := time.Now()
	reply, err := model.Complete(ctx, extract.BuildFallbackAsk(sweepIntent(req), withSamples))
	ms := float64(time.Since(start).Milliseconds())
	// Read the sink whatever happened: a completion that failed after the provider billed it still
	// cost money, and returning zeros there is how spend goes missing.
	_, inTok, outTok := sink.Totals()
	cw, cr := sink.CacheTotals()
	u := components.PrefixUsage{Fresh: int(inTok), Output: int(outTok),
		CacheWrite: int(cw), CacheRead: int(cr)}
	if err != nil {
		r.gate("sweep_fallback_failed")
		r.rec.Rejection = "fallback completion failed: " + err.Error()
		return "", u, ms, err
	}
	return reply, u, ms, nil
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
			Hint: "Per-output floor for naming a candidate in the inventory. Every line is paid fresh, and a small output's removal cannot repay the marker it leaves behind. At 3000 the shipped preset produced ZERO extractions across 3,437 production requests."},
		{Key: "min_inventory", Type: components.FieldInt, Default: defaultMinInventory, Min: 1,
			Hint: "Fewest candidates worth asking about; below it the sweep declines without asking. The model's judgement is a function of how many candidates it COMPARES, and the numbers are far apart: shown one output it scored 6% live-kept on haiku and 14% on sonnet, both inside the drop-everything null model's error bar, while ~15 together reached 58% at the lowest cost per output. At batch 3-6 it dropped a genuinely-spent output 2 times in 4; at 10, 4 in 4. Below the floor a removal is a guess, and a wrong removal costs content the agent still needs while a wrong keep costs one turn's tokens. Lower it only to trade that asymmetry away deliberately."},
		{Key: "pre_expiry_seconds", Type: components.FieldInt, Default: int(defaultPreExpiry / time.Second),
			Hint: "How long before the prompt cache's believed expiry the sweep may fire. The window is where BOTH halves are cheap: the ask still reads a live cache, and the prefix it invalidates has little life left. The TTL itself is read from the request, never assumed. This WIDTH is the component's one unmeasured number — wider fires more often and invalidates more remaining TTL, narrower fires rarely, and nothing measures either side."},
		{Key: "block_fallback", Type: components.FieldBool,
			Hint: "Decline instead of falling back when the prefix ask could not read the cache. Unset = FALSE: the fallback asks again with a bounded sample of each output copied into the prompt, which keeps the component working on a session's first turn and whenever a cache entry has gone — but pays fresh for content the cached path reads for a tenth of the price. Set true where the bill matters more than the removal. The miss is counted either way."},
		markerModeField(),
	})
}
