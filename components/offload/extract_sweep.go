package offload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
// `max_calls`: one call covers every candidate. And there is no `economic_gate`: the gate prices a
// per-output cheap-model call against an expected saving, and this is one cached read for the whole
// transcript, so its arithmetic does not describe this component at all — the brakes here are the
// floor below and the verified cache read.
type extractSweepConfig struct {
	// MinTokens is the per-output floor (0 = defaultSweepFloor). Candidates below it are not worth
	// naming in the inventory: each line is paid fresh, and a small output's removal cannot repay
	// the marker it leaves behind.
	MinTokens int `yaml:"min_tokens"`
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
	pre := defaultPreExpiry
	if cfg.PreExpirySeconds > 0 {
		pre = time.Duration(cfg.PreExpirySeconds) * time.Second
	}
	return &ExtractSweep{
		minTokens: cfg.MinTokens, preExpiry: pre, mode: parseMarkerMode(cfg.MarkerMode),
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
		if !sweeping {
			// Outside the window no NEW decision is taken; the replays above already ran, which is
			// all such a turn has to do.
			continue
		}
		metrics.RecordExtractionCacheLookup(false)
		if schema.TextTokens(content) < e.minTokens {
			rep.Gate("below_output_floor")
			continue
		}
		// The depth restriction, lifted for the reason this component exists: the prefix this turn
		// invalidates is nearly expired, so a message at depth is very nearly as free to act on as
		// one in the tail. Routed through TailOnlyCold rather than skipped so the condition is
		// CHECKED here rather than assumed from `sweeping`.
		//
		// It reads ColdCache, which is false in the pre-expiry window by construction — so this is
		// the ordinary tail gate, and a candidate deep in the prefix is refused by it. That is the
		// conservative reading and it is deliberate: lifting the gate on a prefix that is merely
		// NEARLY expired would need a measurement of what the early invalidation costs, and there is
		// none. The sweep therefore acts on the uncached tail in this window, and the counter below
		// says how much it is turning away.
		if !c.TailOnlyCold(i, true) {
			rep.Gate("cached_prefix")
			continue
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
		cands = append(cands, sweepCand{i: i, content: content, id: id})
	}
	// WHAT WAS SHOWN, counted apart from what was answered. A per-candidate loop cannot express "this
	// many were OFFERED", and the distinction is not cosmetic: a live arm reported 2.80 verdicts per
	// call and that was read as the batch size, when it counted what the model chose to ANSWER rather
	// than what it was SHOWN. Without this, "the inventory is starved" and "the model answered for a
	// third of it" are the same number.
	rep.GateN("sweep_offered", len(cands))
	if eligible > len(cands) {
		rep.GateN("sweep_inventory_thinned", eligible-len(cands))
	}

	// Phase 2: ONE ASK for every candidate. Not a batch and not a call per output — nothing is
	// copied per candidate, so there is nothing to divide.
	if len(cands) > 0 && sweeping {
		drop, call := e.adjudicate(req, c, rep, cands)
		for _, g := range call.gates {
			rep.Gate(g)
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
	id      string
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
	rec   components.ModelCall
}

func (r *sweepResult) gate(name string) { r.gates = append(r.gates, name) }

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
		r.gate("sweep_inventory_of_one")
	}

	items := make([]extract.AdjudicationItem, 0, len(cands))
	for k := range cands {
		items = append(items, extract.AdjudicationItem{
			Label:      k,
			ID:         cands[k].id,
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
	r.rec = components.ModelCall{
		Component: rep.Component, Model: c.ModelName, Strategy: "prefix_ask",
		CandidateTokens: before, LatencyMs: latency,
		PromptTokens: int64(usage.Fresh), CompletionTokens: int64(usage.Output),
		CacheRead: int64(usage.CacheRead), CacheWrite: int64(usage.CacheWrite),
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
		r.gate("sweep_prefix_cache_read_ok")
	}
	for range items {
		r.gate("sweep_adjudicated")
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
		r.gate("sweep_kept_everything")
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
		r.gate("sweep_dropped")
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
	r.gate("sweep_fallback_used")
	reply, err := model.Complete(ctx, extract.BuildFallbackAsk(conversationGoal(req), withSamples))
	if err != nil {
		r.gate("sweep_fallback_failed")
		r.rec.Rejection = "fallback completion failed: " + err.Error()
		return "", err
	}
	return reply, nil
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
		{Key: "pre_expiry_seconds", Type: components.FieldInt, Default: int(defaultPreExpiry / time.Second),
			Hint: "How long before the prompt cache's believed expiry the sweep may fire. The window is where BOTH halves are cheap: the ask still reads a live cache, and the prefix it invalidates has little life left. The TTL itself is read from the request, never assumed. This WIDTH is the component's one unmeasured number — wider fires more often and invalidates more remaining TTL, narrower fires rarely, and nothing measures either side."},
		{Key: "block_fallback", Type: components.FieldBool,
			Hint: "Decline instead of falling back when the prefix ask could not read the cache. Unset = FALSE: the fallback asks again with a bounded sample of each output copied into the prompt, which keeps the component working on a session's first turn and whenever a cache entry has gone — but pays fresh for content the cached path reads for a tenth of the price. Set true where the bill matters more than the removal. The miss is counted either way."},
		markerModeField(),
	})
}
