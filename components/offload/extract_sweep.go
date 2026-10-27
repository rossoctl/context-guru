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

	"github.com/rossoctl/context-guru/cheapmodel"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/coref"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/internal/thread"
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
	// evidence adds the co-reference index's record to each inventory line. See renderEvidence.
	// ON by default (#404): the model weighs it, it never filters the candidates.
	evidence bool
	// shadow turns the HELD ANSWER into a measurement. The sweep still asks at the keep-alive ping and
	// holds the answer; on the next cold request it also rechecks the answer and reports what it
	// WOULD drop. It changes no request. See extract_sweep_shadow.go.
	shadow bool
	// minLaterTurns is the opportunity floor for a CANDIDATE: how many model turns must follow an
	// output before "still needed" is evidence rather than the only possible answer. See the gate in
	// Offload and extractSweepConfig.MinLaterTurns.
	minLaterTurns int
	// keepRecheckTurns is how many model turns must pass before re-offering a candidate the adjudicator
	// already judged still needed. 0 = off. See extractSweepConfig.KeepRecheckTurns.
	keepRecheckTurns int
	// maxAskBatches is how many batches one sweep asks (max_ask_batches).
	maxAskBatches int

	mode markerMode
}

// extractSweepConfig is the sweep's whole surface.
//
// Note what is absent and why. There is no `model` block: the ask goes to the REQUEST's own model by
// construction, because only that model's cache holds the transcript — naming another would read a
// different namespace and pay fresh for everything. There is no `context` / `context_messages`: the
// conversation IS the prefix, so there is nothing to choose how much of to re-send. There is no
// `max_calls`: a long list of outputs is asked in balanced batches of maxAskItems, up to max_ask_batches
// of them, rather than bounded by a call count (extract_sweep_batch.go, #132).
// And there is no `economic_gate`: the gate prices a
// per-output cheap-model call against an expected saving, and this is one cached read for the whole
// transcript, so its arithmetic does not describe this component at all — the brakes here are the
// floor below and the verified cache read.
type extractSweepConfig struct {
	// MinTokens is the per-output floor (0 = defaultSweepFloor). Candidates below it are not worth
	// naming in the inventory: each line is paid fresh, and a small output's removal cannot repay
	// the marker it leaves behind.
	MinTokens int `yaml:"min_tokens"`
	// MinInventory is the fewest candidates worth asking about (0 = defaultMinInventory, which is 1:
	// ask whenever there is a candidate). The question replaces a keep-alive ping, so the cache read
	// is already paid for and the only extra cost is the answer (#404 review, decision 3). Raise it
	// only to trade yield for fewer answers; see the floor check in Offload for the older figures on
	// small inventories.
	MinInventory int `yaml:"min_inventory"`
	// Evidence adds the co-reference index's record to each candidate's inventory line, as input the
	// model weighs rather than a filter that pre-decides. ON by default (#404 review, point 8): the
	// cost is about 500 input tokens per question, under $0.003. It changes the adjudication CONTRACT
	// (the prompt gains a paragraph teaching how to read the counters). Write `evidence: false` to
	// get the old prompt.
	Evidence *bool `yaml:"evidence"`
	// Shadow asks, holds and rechecks, but never lets the held answer change a request: it only
	// reports what it would drop. OFF by default and in no preset. See ExtractSweep.shadow.
	Shadow bool `yaml:"shadow"`
	// MinLaterTurns is the per-candidate OPPORTUNITY FLOOR: an output with fewer model turns after it
	// than this is not offered, because it has not yet had the chance to be superseded and "still
	// needed" is therefore the only answer the adjudicator can give.
	//
	// 0 (off) by default, so no deployment changes behaviour without asking. 8 mirrors
	// corefMinLaterDefault, which exists for precisely this reason one level down. Read
	// `sweep_candidate_too_new` against `sweep_offered` to see what it costs on a workload before
	// trusting it — on a transcript whose outputs are all recent it can empty the inventory, and that
	// is correct rather than a malfunction.
	MinLaterTurns int `yaml:"min_later_turns"`
	// KeepRecheckTurns is how many model turns must pass before a candidate the adjudicator already
	// judged STILL NEEDED is offered again. 0 (off) by default, which is the behaviour every iteration
	// up to 027 had: every candidate is re-offered, and re-paid for, every turn.
	//
	// WHY IT IS WORTH A KNOB. Measured on one probe pass, SEVEN of ten adjudications were the same five
	// candidates at the same 41,453 tokens, kept every time — 70% of the pass's adjudication spend
	// buying a verdict already given. Drops are frozen and replayed; keeps were forgotten.
	//
	// WHY IT MUST NOT BE LARGE, and why "cache the keep forever" is the wrong instinct: "still needed"
	// is a judgement about the conversation's CURRENT obligations, not a property of the bytes. Suppress
	// re-asking for too long and a candidate judged needed once becomes unreachable, which disables the
	// component for precisely the outputs it most wants and reads in the counters as an inventory that
	// quietly shrank. Read `sweep_keep_still_held` against `sweep_offered`.
	//
	// Counted in TURNS rather than seconds deliberately: what changes whether an output is spent is the
	// agent doing more work, and wall-clock says nothing about that.
	KeepRecheckTurns int `yaml:"keep_recheck_turns"`
	// MaxAskBatches is how many batches of 12 one sweep asks (0 = 4, at most 8). See extract_sweep_batch.go.
	MaxAskBatches int `yaml:"max_ask_batches"`
	// MarkerMode is how a removed output is referenced. `full`, the default, is the only mode that
	// keeps the removal recoverable.
	MarkerMode string `yaml:"marker_mode"`
}

// defaultSweepFloor is the per-output floor when none is configured. Carried over from the cold_cache
// block this component replaced: at 3000 the shipped preset produced ZERO extractions across 3,437
// production requests, with `below_output_floor` refusing every candidate on all 36 sweeping turns.
const defaultSweepFloor = 1000

// defaultMinInventory is the fewest candidates this component will ask about: one.
//
// It was ten. `cc1aa9f` measured the model becoming willing to act CORRECTLY at a batch of ten (at 3-6
// it dropped a genuinely-spent output 2 times in 4, at 10 it dropped it 4 in 4), and the floor
// protected against asking a question the model answers badly. #404 removed it on the argument that
// the question now replaces a keep-alive ping: the cache read is already paid for, a small inventory
// costs a small answer, and a held answer is rechecked before anything is dropped. A wrong keep costs
// one turn's tokens; the recheck and the marker bound a wrong drop. Whether the small-batch answers
// are good enough is exactly what shadow mode (shadow: true) measures. Raise `min_inventory` to bring
// the old floor back.
const defaultMinInventory = 1

// maxAskItems is the BATCH SIZE: how many candidates one question carries. Not configurable: it is a
// property of the reply budget and the model's transport limit, not of a deployment's taste. One reply
// carries a verdict for every candidate, so it grows with the count, and a reply cut off mid-array parses
// as nothing; 12 sits well inside the flat reply budget.
//
// IT WAS A HARD CAP on the whole sweep (#132): the 12 largest were asked and the rest dropped. It is now
// only the size of one batch. More candidates are split into balanced batches and asked in sequence, up
// to max_ask_batches of them (extract_sweep_batch.go).
const maxAskItems = 12

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
	{"max_calls", "a list is asked in batches of 12 (max_ask_batches bounds them), not by a call count"},
	{"economic_gate", "the gate prices a per-output cheap-model call; this is one cached read for the whole " +
		"transcript, so its arithmetic does not describe this component"},
}

// sweepRetiredKeys are settings that tuned machinery #404 removed. Unlike sweepBannedKeys they are
// IGNORED, not refused: a production config may still hold them, and a proxy that will not start
// because of a setting that no longer does anything breaks fail open. Each is dropped from the block
// before it is decoded, logged once per component build with the reason, and counted
// (SweepPingCounters().RetiredKeys), so the sweep acts exactly as if the key were absent.
var sweepRetiredKeys = []struct {
	key, why string
}{
	{"econ_trigger", "the sweep no longer asks inside a request (#404). It asks at the keep-alive ping and " +
		"every answer is held for a cold request, so an in-request ask would only cost money. The warm " +
		"path is #491 and will design its own gate; remove the key"},
	{"econ_ignore_ask_cost", "it tuned econ_trigger, which is removed (#404); remove the key"},
	{"reward_premium", "it tuned econ_trigger, which is removed (#404); remove the key"},
	{"min_pressure", "it gated the in-request ask, which is removed (#404); remove the key"},
	{"block_fallback", "there is no fallback any more (#404): the question is asked at the keep-alive ping, " +
		"which sends the ordinary ping instead of paying full price for a content-carrying completion; " +
		"remove the key"},
	{"pre_expiry_seconds", "the sweep no longer fires in a time window before the cache expires (#404). " +
		"It asks its question at the keep-alive ping, in place of the ping, and holds the answer for the " +
		"next cold request. There is no window to size; remove the key"},
}

// sweepRetiredKeyIgnored counts retired keys found in configs, by key.
var (
	sweepRetiredMu      sync.Mutex
	sweepRetiredIgnored = map[string]int64{}
)

// stripRetiredKeys removes the retired keys from a component block, warning and counting each. It
// returns raw unchanged when none is present, so a clean config is byte-for-byte what it was.
func stripRetiredKeys(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var block map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &block); err != nil || block == nil {
		return raw
	}
	found := false
	for _, r := range sweepRetiredKeys {
		if _, present := block[r.key]; !present {
			continue
		}
		found = true
		delete(block, r.key)
		slog.Warn("extract_llm_sweep: ignoring a retired setting", "key", r.key, "reason", r.why,
			"gate", "sweep_retired_key_ignored")
		sweepRetiredMu.Lock()
		sweepRetiredIgnored[r.key]++
		sweepRetiredMu.Unlock()
	}
	if !found {
		return raw
	}
	out, err := yaml.Marshal(block)
	if err != nil {
		return raw
	}
	return out
}

func newExtractSweep(raw []byte) (components.Component, error) {
	raw = stripRetiredKeys(raw)
	// The banned keys FIRST, before components.Decode's KnownFields rejects them with a generic
	// yaml message. The whole point is that the error names the reason.
	//
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
	if cfg.MaxAskBatches < 1 {
		cfg.MaxAskBatches = defaultMaxAskBatches
	}
	if cfg.MaxAskBatches > maxAskBatchesCap {
		cfg.MaxAskBatches = maxAskBatchesCap
	}
	// Evidence is ON unless the operator wrote `evidence: false`.
	evidence := cfg.Evidence == nil || *cfg.Evidence
	return &ExtractSweep{
		minTokens: cfg.MinTokens, minInventory: cfg.MinInventory,
		mode:     parseMarkerMode(cfg.MarkerMode),
		evidence: evidence, shadow: cfg.Shadow,
		minLaterTurns: cfg.MinLaterTurns, keepRecheckTurns: cfg.KeepRecheckTurns,
		maxAskBatches: cfg.MaxAskBatches,
	}, nil
}

func (*ExtractSweep) Name() string                 { return "extract_llm_sweep" }
func (*ExtractSweep) Enabled(*components.Ctx) bool { return true }

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

// admit applies the per-candidate gates that decide whether one tool output is offered to the
// adjudicator, and builds the candidate when it is. It is the part of the collection loop that is the
// same wherever the question is asked: inside the request (the econ trigger) or at the keep-alive ping
// (extract_sweep_ping.go). The checks before it (rewritable, marker present, kept verbatim, already
// decided) differ by caller and stay with the caller.
func (e *ExtractSweep) admit(c *components.Ctx, rep *components.Report, req *bschemas.BifrostChatRequest,
	i int, content, id string) (sweepCand, bool) {
	msg := &req.Input[i]
	if schema.TextTokens(content) < e.minTokens {
		rep.Gate("below_output_floor")
		return sweepCand{}, false
	}
	// NO DEPTH RESTRICTION. Candidates are the ENTIRE transcript, which is what this component
	// is for.
	//
	// The tail gate exists for the WARM-turn compactor: rewriting a message inside a live cached
	// prefix invalidates everything after it, so extract_llm must confine itself to the uncached
	// tail. This component's premise is the opposite — it applies on a cold request, where nothing
	// is cached to invalidate. Refusing depth here does not make the
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
	// WHY DEPTH IS NOT A COST HERE (#404). The time window that once justified reaching deep is gone.
	// Asking does not touch the prefix: the question is appended after it, at the ping. A drop is
	// applied only on a cold request, where the whole conversation is written anyway, so a deep drop
	// shrinks that write instead of forcing one.
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
	// TOO NEW TO BE JUDGED, which is NOT the pre-filter the comment above forbids.
	//
	// The forbidden filter removed candidates the INDEX HAD JUDGED still-referenced. It was
	// self-defeating twice: it starved the batch, and it left the model seeing only what the index
	// had already condemned. This one carries no verdict at all. An output with fewer than
	// `minLaterTurns` model turns after it has not yet HAD the opportunity to be superseded, so
	// "still needed" is the only answer available and it says nothing about whether the output is
	// spent. coref encodes exactly this distinction one level down — Classify returns `Open` ("too
	// new to have been referenced") as a separate outcome from `Unreferenced` ("the cut
	// candidate") — and corefMinLaterDefault is the same 8.
	//
	// WHY IT MATTERS HERE RATHER THAN ONLY BEING TIDY. The econ trigger fires most eagerly exactly
	// where this is worst. Measured on the iteration 026 probe, the FIRST ask of every pass landed
	// on a 2,621-token transcript: offered 2,318 of it (88% of the whole transcript was candidate
	// output), `rewritten` about zero because nothing was cached yet, and haveTurns 70 because a
	// tiny request projects a long future — need 21 against have 70, cleared by 3.3x at 4% context
	// pressure. All three such asks removed nothing, correctly, because nothing had been
	// superseded. Those are the ledger's WARM-UP samples, so the estimator's calibration is drawn
	// by construction from the moment its question is least answerable, and a floored approval
	// then suppresses the asks that would correct it.
	//
	// Starving the inventory is the INTENDED outcome when it fires: if every candidate is too new,
	// there is nothing worth asking about and declining is right. That is the opposite of
	// `4ca1f13`, where the removed candidates were judgeable and the batch was starved of them.
	// Counted either way, and `sweep_inventory_thinned` below still trips if this thins silently.
	later := laterModelTurns(req, i)
	if e.minLaterTurns > 0 && later < e.minLaterTurns {
		rep.Gate("sweep_candidate_too_new")
		return sweepCand{}, false
	}
	// ALREADY ANSWERED. A candidate the adjudicator judged still needed is not re-offered until the
	// conversation has moved on by keepRecheckTurns model turns — see sweep_keepcache.go for why the
	// interval is in turns and why it must exist rather than caching the keep outright.
	//
	// SUPPRESSING AN OFFER IS NOT THE REFUTED PRE-FILTER, and the distinction is worth stating here
	// because it sits at the exact line 4ca1f13's defect occupied. That filter substituted the
	// INDEX's opinion for the model's, so the model only ever saw what the index had already judged
	// spent. This substitutes the MODEL'S OWN PRIOR ANSWER, unchanged, and only until the state it
	// was answering about has changed. It removes a repeated question, not a veto.
	if held, since := sweepKeepStillHolds(c, id, later, e.keepRecheckTurns); held {
		rep.Gate("sweep_keep_still_held")
		_ = since
		return sweepCand{}, false
	}
	// The wire's own tool-call id, which apply.normalize sets on every synthetic tool message it
	// lifts out of an Anthropic tool_result block. Read here rather than reconstructed, because a
	// reconstructed anchor is exactly the defect #123 records.
	toolID := ""
	if msg.ChatToolMessage != nil && msg.ChatToolMessage.ToolCallID != nil {
		toolID = *msg.ChatToolMessage.ToolCallID
	}
	return sweepCand{i: i, content: content, id: id, toolID: toolID}, true
}

func (e *ExtractSweep) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	// Attribute any spend a ping-time question deferred onto the next real request (the detail row, or
	// the whole usage when the ping was sent after all). Idempotent: whichever component runs first
	// takes it.
	takeDeferredUsage(c, rep)
	// The ping-time question reads a snapshot of the conversation as this component leaves it, so it
	// is taken last, after the cold step has spliced its markers. Deferred because this function
	// returns from several places.
	defer e.registerPing(c, req)
	// Skipped is a property of the OUTCOME, not of one exit. Setting it only on one return made 29,321
	// of 29,372 production rows report `mutated=1` for a component that had DECLINED -- because
	// dash/event.go computes Mutated as !Reverted && !Skipped, and GateN is a no-op at n<=0, so
	// those turns recorded neither a gate nor a skip. A deferred guard covers every exit.
	changed := 0
	defer func() {
		if changed == 0 {
			rep.Skipped = true
		}
	}()
	// SHADOW MODE changes nothing: it reports what the apply would do (extract_sweep_shadow.go).
	if e.shadow {
		// Shadow stops NEW applies only. Drops this thread already made stay replayed, byte for byte,
		// or turning shadow on would flip them back to full inside the provider's cached prefix.
		var keys []string
		if c != nil && c.Store != nil && c.Session != "" {
			var n int
			keys, n = e.replayDrops(req, rep, c, thread.Key(c.Session, c.Thread), savedTokenValue(c))
			changed += n
		}
		e.shadowStep(c, req, rep)
		return keys, nil
	}
	// THE COLD APPLY (#404 points 3 and 4). It replays the drops this thread already made, at any
	// depth, and applies a held answer when this request is cold or its prefix changed. A warm request
	// with an unchanged prefix gets only the replays, which are byte-identical to what it sent before.
	keys, n := e.coldStep(req, rep, c)
	changed += n
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
	// answered is true once the model's reply parsed as a verdict array, including the empty one
	// ("keep everything"). It separates a question that was answered from one that failed, which both
	// return no drops. The ping-time ask holds an answer only when this is set.
	answered bool
}

func (r *sweepResult) gate(name string)  { r.gates = append(r.gates, name) }
func (r *sweepResult) event(name string) { r.events = append(r.events, name) }

// raised reports whether a gate was recorded. The gates are carried as a SLICE because the raise
// happens on the serial path, so this is a scan rather than a map lookup — the list is one entry
// per declined candidate, which is bounded by the inventory.
func (r *sweepResult) raised(name string) bool {
	for _, g := range r.gates {
		if g == name {
			return true
		}
	}
	return false
}

// askOpts is what differs between asks, kept as a type so a second caller can label its own.
type askOpts struct {
	// trigger and gateReason label the ask in the dump and in the per-call ledger row.
	trigger, gateReason string
	// lastTurnRead is the cache_read of the last real turn on the thread, 0 when unknown. See
	// prefixAskCacheOK.
	lastTurnRead int
}

// adjudicateWith makes ONE prefix ask about every candidate and returns the labels it authorised
// dropping.
//
// EVERY FAILURE PATH RESOLVES TOWARD KEEP -- no asker, no stashed prefix, a transport error, a cache
// read that did not happen, an unparseable reply, an unusable verdict, a drop that contradicts a named
// obligation, a verdict for something we did not offer. A wrong keep costs tokens on one turn; a wrong
// drop is a silent permanent loss the agent does not notice and cannot ask about. The two errors are
// not comparable, so this does not treat them symmetrically.
// The sweepResult is a NAMED result so a deferred function can still reach it on an early return:
// every early return here is `return nil, r`, and a deferred mutation of a local would happen after
// the value was copied. The first result stays blank-named — only this one needs the treatment.
func (e *ExtractSweep) adjudicateWith(req *bschemas.BifrostChatRequest, c *components.Ctx,
	rep *components.Report, cands []sweepCand, opt askOpts) (_ []int, r sweepResult) {

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
	// ONE FLUSH POINT FOR EVERY EXIT: this function returns from a dozen places, and a per-site write would miss the ones added later. A dump of a
	// FAILED ask — prompt present, reply empty — is as diagnostic as a successful one, so the deferred
	// write is not conditional on getting an answer.
	dumpDir := sweepAskDumpDir()
	var dump *sweepAskDump
	if dumpDir != "" {
		defer func() { writeSweepAskDump(dumpDir, dump) }()
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
		// The ask's own totals. See recordLeg.
		askMs, askCost float64
	)
	// recordLeg books ONE model call this component made — its wall time and its own priced spend.
	//
	// PER LEG, not once per adjudication, because the fallback was a SECOND model call on a full
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
	// STALE-PREFIX GUARD, the sweep's version of cache_aware_summarizer's own guard in
	// cache_aware_async.go. The ask below reads this session's STASHED body (proxy's sentStash,
	// through PrefixAsker.Ask) and appends askPrompt to it — the same construction, the same
	// stash, and so the same risk: a stash that holds something but not everything the ask is
	// about to claim was read.
	//
	// Checked against the DEEPEST AT-DEPTH candidate only — i <= c.MaxCachedIdx, i.e. a candidate
	// this component itself believes sits inside the already-cached prefix a PRIOR turn forwarded
	// (!c.TailOnly(i), the same test `sweep_candidate_at_depth` above uses). NOT against every
	// candidate: this turn's own tail — new content added since the last forwarded turn, which is
	// most of an ordinary turn's inventory — is never in the stash yet, by construction, because
	// the stash only gets THIS turn's body after Offload returns and the host forwards it. Checking
	// the whole candidate set would make the guard fire on every ordinary turn with any depth
	// beyond the previous turn's prefix, not only a genuinely stale one — the exact trap
	// cache_aware_summarizer's own guard comment warns about. A tail candidate the ask cannot
	// verify either way is the pre-existing, separately tracked gap (#122), not this guard's job.
	usePrefixAsk := c.PrefixAsk != nil
	stalePrefix := false
	// The span the guard verified, kept so the ask below can check it again on the SAME read of the
	// stash it asks over (PrefixCoveredAsker) instead of trusting that the stash did not change
	// between the check above and the ask.
	haveCover, coverCount, coverHash := false, 0, ""
	if usePrefixAsk {
		if pc, ok := c.PrefixAsk.(components.PrefixCoverage); ok && c.MaxCachedIdx >= 0 {
			deepest := -1
			for _, cd := range cands {
				if cd.i <= c.MaxCachedIdx && cd.i > deepest {
					deepest = cd.i
				}
			}
			if deepest >= 0 {
				requiredCount := deepest + 1
				// rawSpanForHash, not req.Input[:requiredCount]: CoversSpan compares against the
				// host's PRE-pipeline stash, and req.Input here is already whatever ran before this
				// component in the configured pipeline (textclean, format, dedup, … in housellm/
				// xhigh) — see rawSpanForHash's own comment and PR #431 round 1.
				//
				// thread.Key(c.Session, c.Thread), not bare c.Session: the host's stash (proxy's
				// sentStash) is keyed per thread (#423), and the Ask call just below already reads
				// it that way. Checking coverage under the plain session would almost always MISS
				// for a non-primary thread — CoversSpan's own missing-key convention then reports
				// "covered" and this guard would never fire for any thread but the first.
				if span, ok := rawSpanForHash(c, req, requiredCount); !ok ||
					!pc.CoversSpan(thread.Key(c.Session, c.Thread), requiredCount, components.SpanHash(span)) {
					stalePrefix = true
				} else {
					haveCover, coverCount, coverHash = true, requiredCount, components.SpanHash(span)
				}
			}
		}
	}
	if !usePrefixAsk || stalePrefix {
		// No asker at all (a non-Anthropic route, or no incoming client) and a stale stash take the
		// same fork, because both mean there is nothing SAFE to ask through: one has nothing stashed,
		// the other has the wrong thing. Gated separately so a run showing sweep_stale_prefix, rather
		// than sweep_no_asker, says which one happened.
		//
		// THERE IS NO FALLBACK (#404). The ask is made at the keep-alive ping, which has no request to
		// pay a full-price content-carrying completion for. The ping is sent instead.
		if stalePrefix {
			r.gate("sweep_stale_prefix")
			r.rec.Rejection = "stashed prefix does not cover the candidates offered"
		} else {
			r.gate("sweep_no_asker")
			r.rec.Rejection = "no prefix asker on this route"
		}
		return nil, r
	} else {
		askStart := time.Now()
		// NAMED rather than inlined, so the dump records the prompt AS SENT. A dump that rebuilt the
		// prompt from `items` would be a reconstruction, and the whole point of capturing it is to be
		// able to review the text the model actually received.
		//
		// Composed through extract.Ask so a second list (#490, #485) can be added by its own code
		// without touching this function. With one list the body is BuildPrefixAsk's, byte for byte.
		// The shared role/opening helper (#507) then frames it for the model.
		ask := extract.NewAsk()
		ask.Add(extract.SpentOutputs{Items: items})
		askPrompt := resolveAsk(nil, c.ModelName, askViaPrefix).Frame(ask.Prompt())
		if dumpDir != "" {
			dump = newSweepAskDump(req, c.Session, c.ModelName, items, cands, askPrompt)
			dump.Trigger = opt.trigger
		}
		// The THREAD's stashed body (#423), not whichever thread of the session spoke last.
		if ca, ok := c.PrefixAsk.(components.PrefixCoveredAsker); ok && haveCover {
			reply, usage, err = ca.AskCovered(ctx, thread.Key(c.Session, c.Thread), coverCount, coverHash, askPrompt)
		} else {
			reply, usage, err = c.PrefixAsk.Ask(ctx, thread.Key(c.Session, c.Thread), askPrompt)
		}
		askMs = float64(time.Since(askStart).Milliseconds())
		// ErrNoPrefix is refused LOCALLY — there is no stashed body to append to, so no request
		// leaves the process. Booking it as a call would put a 0 ms, $0 sample into the mean that
		// the exploration brake reads, and inflate `calls` with work that by definition did not
		// happen. Every other outcome, transport failure included, went to the provider.
		if !errors.Is(err, components.ErrNoPrefix) && !errors.Is(err, components.ErrStalePrefix) {
			askCost = recordLeg(askMs, usage)
		}
	}
	r.rec = components.ModelCall{
		Component: rep.Component, Model: c.ModelName, Strategy: "prefix_ask",
		CandidateTokens: before, LatencyMs: askMs,
		PromptTokens: int64(usage.Fresh), CompletionTokens: int64(usage.Output),
		CacheRead: int64(usage.CacheRead), CacheWrite: int64(usage.CacheWrite),
		CostUSD:    askCost,
		GateReason: opt.gateReason,
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			atomic.AddInt64(&llmTimeouts, 1)
		} else {
			atomic.AddInt64(&llmErrors, 1)
		}
	}
	if err != nil {
		// A first turn has no stashed prefix, which arrives here as an error. Counted separately from
		// a transport failure: one is "there was nothing to read yet", which every session does once
		// and which needs no attention, and the other is "the read failed", which does.
		if errors.Is(err, components.ErrNoPrefix) {
			r.gate("sweep_no_prefix")
			r.rec.Rejection = "no stashed prefix to ask through"
		} else if errors.Is(err, components.ErrStalePrefix) {
			// The stash changed between the guard above and the ask, and AskCovered refused to ask
			// over a body the guard had not approved. Counted as the stale prefix it is, not as a
			// failed call: no request left the process.
			r.gate("sweep_stale_prefix")
			r.rec.Rejection = "stashed prefix changed before the ask"
		} else {
			r.gate("sweep_ask_failed")
			logging.From(c.Ctx).Warn("cg.sweep.ask_failed", "session", c.Session,
				"gate", "sweep_ask_failed", "err", err)
			r.rec.Rejection = "prefix ask failed: " + err.Error()
		}
		return nil, r
	}
	// THE CACHE READ IS THE MECHANISM'S WHOLE JUSTIFICATION, so a read that did not happen is always
	// COUNTED — a silent miss looks identical to a working call except on the bill. There is no
	// fallback: the question is asked at the keep-alive ping, so a read that did not happen declines,
	// and the ordinary ping goes out.
	//
	// A WRITE trips it as surely as a zero read, and testing the read alone let the harm through.
	// Production: three asks re-created the agent's prefix for 704,925 opus cache_write tokens,
	// $3.42 and 73% of this component's entire spend -- and two of them had read 39,805 tokens of
	// a stable sub-prefix, so `CacheRead == 0` was false and the counter stayed silent on $2.26 of
	// it. A partial hit is not evidence of safety: the write is the harm, at 1.25x fresh on the
	// AGENT's model rather than a tenth of it.
	//
	// BUT THE WRITE ALONE DECIDES NOTHING (#721). The provider never reads an entry that ends at a
	// text turn, so a correct question rewrites that turn's tail. prefixAskCacheOK judges the READ
	// against the last real turn's read instead; see it for the rule.
	if ok, why := prefixAskCacheOK(usage, opt.lastTurnRead); !ok {
		reason := "read nothing from cache"
		switch why {
		case prefixAskReadZero:
			r.gate("sweep_prefix_cache_read_ZERO")
		case prefixAskReadBelowTurn:
			r.gate("sweep_prefix_cache_read_below_turn")
			reason = "read less than the last turn did, so it lost part of the prefix"
		case prefixAskWriteOverCap:
			r.gate("sweep_prefix_cache_write")
			reason = "re-created the agent's prefix instead of reading it"
		}
		// This string is persisted to extraction_calls.rejection, so it must name the real failure.
		why = reason
		r.rec.Rejection = "the prefix ask " + why + "; declining rather than acting on it"
		return nil, r
	} else {
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
	if usage.ViaTool {
		r.event("sweep_answered_via_tool")
	} else {
		r.event("sweep_answered_via_prose")
	}
	for range items {
		r.event("sweep_adjudicated")
	}

	verdicts, parsed := extract.ParseVerdicts(reply)
	r.answered = parsed
	if dump != nil {
		dump.Reply, dump.ViaTool, dump.Parsed = reply, usage.ViaTool, parsed
	}
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
	// judgedTokens is the adjudicator's OWN arithmetic, for the debug row only: what it believed
	// it was freeing, descriptor-only and before phase 3 has had a chance to refuse any of it.
	// Deliberately not the ledger's figure — see the rejection test below.
	// spentJudged counts verdicts that said "drop", whether or not we then acted on them. See its
	// increment for why the ledger's reason is derived from this rather than from the skip gates.
	var spentJudged int
	var judgedTokens int
	// RECORDED AT EACH BRANCH, never derived afterwards. Deriving the outcome from the verdict a second
	// time would mean the dump agrees with a re-implementation of this loop rather than with the loop —
	// and the divergence between what the model SAID and what the component DID is precisely the row
	// worth reading (a self-contradictory drop leaves the output in place).
	noteVerdict := func(v extract.Verdict, a extract.Adjudication, outcome string) {
		if dump == nil {
			return
		}
		dump.Verdicts = append(dump.Verdicts, sweepAskDumpVerdict{
			Label: v.Label, Drop: a.Drop, NeededBy: v.NeededBy, Quote: v.Quote,
			QuoteFabricated: a.QuoteFabricated, CriterionMissing: a.CriterionMissing,
			VerdictUnusable: a.VerdictUnusable, RefusedObligation: a.RefusedObligation,
			Outcome: outcome,
		})
	}
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
			noteVerdict(v, extract.Adjudication{}, "unknown_label")
			continue
		}
		if seen[v.Label] {
			r.gate("sweep_verdict_duplicate_label")
			noteVerdict(v, extract.Adjudication{}, "duplicate_label")
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
		// WHAT THE MODEL ANSWERED, not what validation concluded — and the difference is the whole
		// point. extract.Judge returns early on a self-contradictory drop WITHOUT setting a.Drop
		// (adjudicate.go: `RefusedObligation = true; return a`), so counting a.Drop alone misses
		// precisely the path that made this reason wrong: a model answering "drop" with a needed_by
		// for every candidate produced a ledger row saying the adjudicator found nothing spent.
		//
		// Counted before the skips below, because each of those is OUR decision not to act on a
		// judgement the model did make. VerdictUnusable is deliberately NOT counted: there the model
		// answered something other than drop or keep, so it did not judge the output spent.
		if a.Drop || a.RefusedObligation {
			spentJudged++
		}
		if a.RefusedObligation {
			r.gate("sweep_drop_refused_obligation")
			noteVerdict(v, a, "refused_obligation")
			continue
		}
		if !a.Drop {
			r.gate("sweep_kept")
			noteVerdict(v, a, "kept")
			// RECORDED AT THE BRANCH that took the decision, beside the counter, so a keep cannot be
			// counted without being remembered. Keyed by CONTENT, so the same output recurring in a
			// later turn's transcript resolves to the same record.
			recordSweepKeep(c, cands[v.Label].id, laterModelTurns(req, cands[v.Label].i))
			continue
		}
		sz := schema.TextTokens(content)
		after := schema.TextTokens(sweepDescriptor(content))
		if after >= sz {
			// The never-worse check also lives in applySweepDrop, marker included. This one is here
			// so a decision phase 3 will refuse is not counted as a removal.
			r.gate("sweep_drop_would_not_shrink")
			noteVerdict(v, a, "would_not_shrink")
			continue
		}
		r.event("sweep_dropped")
		noteVerdict(v, a, "dropped")
		drop = append(drop, v.Label)
		judgedTokens += sz - after
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
	// THE VERDICT SET, not a token total. Accepted/SavedTokens are filled by the caller after
	// phase 3, because they describe drops that actually HAPPENED and the two diverge exactly when
	// the reserve is refusing. What is this function's to state is whether the adjudicator judged
	// anything spent at all — and `drop` IS that set, so it is what the test reads.
	//
	// It used to read `removed == 0`, and moving the metrics out of this loop deleted `removed`'s
	// only assignment while leaving the read: Go then guarantees it stays 0, so EVERY adjudication
	// stamped "nothing was spent", including ones that dropped content. A row could carry
	// accepted=true, a large saved_tokens, and "nothing was spent" at once — the same
	// self-contradictory shape this change set out to remove, arrived at from the other side. The
	// compiler cannot see it because `removed` is still read.
	if len(drop) == 0 {
		// TWO QUESTIONS, and only the first is the adjudicator's: did the model judge anything
		// spent, and if it did, why did none of it become a drop?
		//
		// Enumerating the second was the mistake. An earlier version tested only the never-worse
		// pre-check and reported every other skip — refused obligation, unusable verdict, unknown
		// or duplicate label — as "the adjudicator found nothing spent", which tells an operator
		// the model saw nothing worth removing when it judged EVERY output spent and we declined
		// each one. Concretely: a model that answers drop with a needed_by for every candidate
		// trips RefusedObligation throughout and produced exactly that row.
		//
		// So the reason is derived from spentJudged, which cannot miss a skip path, and the GATES
		// carry which skip it was — they are already raised, already exported, and unlike a
		// hand-written reason string they stay correct when a skip is added.
		// A verdict we could not key to an output (unknown or duplicate label) is skipped above
		// before Judge runs, so its answer is unknown and it cannot be counted either way. It raises
		// its own gate, which is the honest record: the reason below speaks only for verdicts that
		// were actually read.
		if spentJudged == 0 {
			r.rec.Rejection = "adjudicated: nothing was spent"
		} else {
			r.rec.Rejection = fmt.Sprintf(
				"adjudicated %d spent, but none became a drop (see gates)", spentJudged)
		}
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
			// judgedTokens, not `removed`: #216 replaced the latter and it no longer exists here.
			"removed_tokens", judgedTokens,
			// ALL FOUR TOKEN TIERS, because recordLeg prices all four and this row reported two.
			// Pooled over iteration 025's 33 logged asks, cache_read and fresh accounted for $0.653
			// of $0.978 measured spend -- so a THIRD of the cost sat in the two tiers the row did not
			// carry, and "the ask paid a cache-write on its own prompt" could not be distinguished
			// from "the ask returned ~1k output tokens" after the fact. Those imply different fixes.
			"cache_read", usage.CacheRead, "fresh", usage.Fresh,
			"cache_write", usage.CacheWrite, "output", usage.Output,
			"cost_usd", askCost,
			"session", c.Session, "max_cached_idx", c.MaxCachedIdx,
			"req_tokens", schema.MessagesTokens(req), "messages", len(req.Input))
	}
	return drop, r
}

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
			Hint: "Per-output floor for naming a candidate in the inventory. Measured on iteration 022, a floor of 1000 named 4.5 of the 23.6 tool outputs a request carried and held the batch at 4.4 against a cap of 12, while a floor of 100 reaches 11.6 for 8% more mass -- candidates are nearly free (one inventory line) and are the axis the mechanism lives on. Every line is paid fresh, and a small output's removal cannot repay the marker it leaves behind. At 3000 the shipped preset produced ZERO extractions across 3,437 production requests."},
		{Key: "min_inventory", Type: components.FieldInt, Default: defaultMinInventory, Min: 1,
			Hint: "Fewest candidates worth asking about; below it the sweep does not ask. Default 1: the question replaces a keep-alive ping, so the cache read is already paid for and a small inventory costs a small answer. Earlier measurement put the model's judgement at small inventories poor (shown one output it scored 6% live-kept on haiku and 14% on sonnet, inside the drop-everything null model's error bar; at batch 3-6 it dropped a genuinely-spent output 2 times in 4, at 10 4 in 4). A held answer is rechecked before anything is dropped, and shadow mode measures the real drop and false-drop rates. Raise it to bring the old floor of 10 back."},
		{Key: "evidence", Type: components.FieldBool, Default: true,
			Hint: "Add the co-reference index's record (novel/refs/ref_age/used_frac/later_turns and the index's own verdict) to each candidate's inventory line. Default TRUE: about 500 extra input tokens per question, under $0.003. It is EVIDENCE the model weighs, never a filter over the candidates: a co-reference PRE-FILTER left about one candidate per request, which silently turned a bulk arm into the per-output shape refuted at 6% live-kept and meant the model only ever saw what the index had already judged spent — destroying the veto on the index's blind spot that the mechanism exists to provide. Enabling this also adds a paragraph to the adjudication contract teaching how to read the counters; a prompt carrying counters it never explains is worse than one carrying neither. Write false to get the old prompt."},
		{Key: "shadow", Type: components.FieldBool,
			Hint: "Shadow mode. Unset = FALSE, and not in any preset. The sweep still asks at the keep-alive ping and holds the answer, and on the next COLD request it rechecks the held answer and reports what it WOULD drop: sweep_shadow_asked, sweep_shadow_proposed, sweep_shadow_valid, and sweep_shadow_dropped_<reason> events, plus a cg.sweep.shadow log line. It changes no request. Use it to measure the drop share and the false-drop rate before the cold apply is switched on."},
		{Key: "min_later_turns", Type: components.FieldInt, Min: 0,
			Hint: "Per-candidate OPPORTUNITY FLOOR: do not offer an output with fewer than this many model turns after it. Unset = 0 = off. Such an output has not yet had the chance to be superseded, so \"still needed\" is the only answer the adjudicator can give and it is not evidence about whether the output is spent. This is not the refuted pre-filter: it carries no verdict, it says the question is premature. 8 mirrors coref's min_later_turns, which exists for the same reason. It matters because the ECONOMIC trigger fires most eagerly exactly where the question is least answerable -- measured, the first ask of a session landed on a 2,621-token transcript with 88% of it candidate output, no cached prefix to rewrite and 70 projected turns to collect over, clearing the break-even by 3.3x at 4% context pressure, and removing nothing. Those are the ask ledger's warm-up samples. Read sweep_candidate_too_new against sweep_offered before trusting a value: on all-recent transcripts it can empty the inventory, which is correct rather than broken."},
		{Key: "keep_recheck_turns", Type: components.FieldInt, Min: 0,
			Hint: "How many model turns must pass before re-offering a candidate the adjudicator already judged STILL NEEDED. Unset = 0 = off, which re-offers and re-pays for every candidate every turn. Measured on one probe pass: SEVEN of ten adjudications were the same five candidates at the same 41,453 tokens, kept every time — 70% of that pass's adjudication spend buying a verdict already given, because drops are frozen and replayed through cg:res: while keeps were forgotten. DO NOT SET IT LARGE, and do not reach for \"cache the keep forever\": \"still needed\" is a judgement about the conversation's current obligations, not a property of the bytes, so suppressing the question for too long makes a candidate judged needed once unreachable for the rest of the session — which disables the component for precisely the outputs it most wants and reads in the counters as an inventory that quietly shrank rather than as a component that stopped asking. Counted in TURNS, not seconds, because what changes whether an output is spent is the agent doing more work. A transcript that got SHORTER behind an output (agent-side compaction, a rewind) invalidates the record rather than extending it. Read sweep_keep_still_held against sweep_offered.",
		},
		{Key: "max_ask_batches", Type: components.FieldInt, Default: defaultMaxAskBatches, Min: 1,
			Hint: "How many batches of 12 outputs one sweep asks. Default 4, at most 8 (a larger value reads as 8, below 1 as 4). A list longer than 12 is split into balanced batches (16 gives 8+8, 23 gives 12+11) and asked one after the other over the same cached prefix, each judged on its own; a batch that fails (error, refused cache read, truncated or unparseable reply) keeps its outputs and the other batches still count. All answers are merged into one held answer, so the next cold request does one recheck and one cut. Each batch is one more question, so it costs one more answer. Candidates beyond 12 x max_ask_batches (the smallest) are not asked and are counted as sweep_over_ask_cap. A list of 12 or fewer is one question, as before."},
		markerModeField(),
	})
}
