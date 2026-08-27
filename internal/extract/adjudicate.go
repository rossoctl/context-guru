package extract

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/rossoctl/context-guru/internal/tokens"
)

// COLD-SWEEP ADJUDICATION. The model returns VERDICTS, never content.
//
// This is the contract `feat/coref-compaction` arrived at (internal/extract/bulk.go), ported here
// with its batching, because the batch is the part that was measured good.
//
// WHY IT IS A BATCH AND NOT ONE CALL PER OUTPUT. This was got wrong once already, so the evidence is
// recorded rather than summarised. docs/results/coref-selection-experiment.md measured ten arms over
// 8,105 recorded decisions:
//
//	REFUTED — the per-output shape. One call shown ONE output, deciding its fate, scored 6% live-kept
//	on haiku and 14% on sonnet, both inside the drop-everything null model's error bar. Shown a single
//	output, a model simply drops it.
//
//	WORKS — bulk adjudication. One call shown ~15 outputs TOGETHER lifted live-kept from 6% to 58% at
//	the LOWEST cost per output, because the overhead amortises and, more importantly, because
//	COMPARATIVE judgement beats absolute judgement: ranking a dozen candidates against each other is a
//	question a model can answer, "is this one output expendable" is not.
//
// `4ca1f13` is the commit that makes this concrete, and it is the one that was previously misread as
// evidence FOR per-output. It found a live arm reporting 1.02 verdicts per call and diagnosed that as
// a DEFECT — "that is the per-output design refuted at 6% live-kept, not the bulk shape that measured
// 58%, so iteration 014 measured something other than what it claimed". A single call carrying a
// single item is the refuted design wearing a new name, which is why buildBatch's caller must assert
// the batch offers more than one.
//
// `cc1aa9f` adds the direction of the failure, and it is the direction a SWEEP cannot tolerate: "at
// batch 3-6 the model dropped a genuinely-spent output only 2 times in 4, at batch 10 it dropped it 4
// in 4 and cleared 100% of genuinely-spent candidates. Small batches do not make it wrong, they make
// it UNWILLING TO ACT, which is what a 94.6% keep rate looks like from inside." A cold sweep exists
// because the whole transcript is re-billing at the write rate; an adjudicator too timid to remove
// anything is an expensive no-op there.
//
// WHAT THE BATCH DOES NOT COST. The two failure modes that once argued for per-output calls were both
// solved on the branch this is ported from, so they no longer distinguish the shapes:
//
//   - a shared reply truncated mid-array. `659e7a6` traced 24 of 34 unparseable replies to an output
//     budget of 2048 and raised it; a verdict array over 12 items each carrying an obligation label
//     and a verbatim quote is simply long. Output bills as generated, not as budgeted, so the ceiling
//     costs nothing until used. See ParseVerdicts for why truncation is now counted separately from a
//     format failure — the two need opposite fixes.
//   - quote fidelity decaying with batch size. `cc1aa9f` measured the ceiling and capped the batch
//     below it: 4 of 37 quotes non-verbatim at batch 16 against 0 of 16 at batch 10. See
//     MaxAdjudicationItems.
//
// And the transport principle never distinguished them at all: post-trim, NO verdict carries content
// in either shape. There is no reply field a model could return output text through.
//
// WHY IT IS A VERDICT AND NOT A REWRITE. `cc1aa9f` removed the `trim` verdict after measuring it:
// chosen ZERO times in 21 probe opportunities, keep/drop scored identically to keep/drop/trim on
// every metric, and in production it was accepted ONCE against EIGHT rejected as invented. It was the
// only verdict that asked the model to transport text, which is what it is worst at. Removing it
// removes the transporting OPERATION rather than merely the transporting STRATEGIES.
//
// WHY THE CRITERION IS FORCED EVIDENCE AND NOT ADVICE. Arms carrying an identical criterion differed
// ONLY in whether the model had to emit which obligation still needs the output; the arm that had to
// emit it HALVED the false-drop rate (4/4 -> 2/4). Stating the criterion alone measured inert.
// Instructions a model can skim past are inert; a required output field is not.
//
// WHAT IS DELIBERATELY ABSENT, against bulk.go: the READING THE EVIDENCE section. It taught the model
// to interpret a co-reference index's counters (novel / refs / ref_age / used_frac), and there is no
// such index on `main`. Shipping the section would be teaching the model to read fields the prompt
// never carries.

// MaxAdjudicationItems caps one adjudication batch.
//
// 12, not 15, and the number is a measured ceiling rather than a round figure. Batch size is a
// YIELD/SAFETY trade-off (docs/experiments/loca/iter019/results.md, "batch size"): at batch 3-6 the
// model dropped a genuinely-spent output only half the time, at batch 10 it cleared 100% of the
// genuinely-spent candidates. But at batch 16 the transport burden started to tell -- 4 of 37
// required quotes came back non-verbatim, against 0 of 16 at batch 10. So the ceiling sits between 10
// and 16 and this takes the conservative end of it.
const MaxAdjudicationItems = 12

// AdjudicationSampleChars bounds each output shown in the prompt.
//
// The whole point is comparative judgement across many outputs, so the per-output budget must stay
// small enough that a full batch plus the contract still fits the adjudication model's window.
// Exported because the caller sizes its context check against the same number.
const AdjudicationSampleChars = 4000

// AdjudicationReplyTokens is the reply budget one batched adjudication needs.
//
// 16000, from `659e7a6`, which is the commit that found this: the merged arm's replies were being cut
// off at a 2048-token default and the parse failure was misread as a model declining to act for three
// iterations. A verdict array over 12 items, each carrying an obligation label and a VERBATIM quote,
// is long -- and a request model running adaptive thinking spends part of the budget before emitting
// any text at all (a probe at max_tokens 900 returned thinking blocks and no text whatsoever). Output
// bills as generated and not as budgeted, so the ceiling costs nothing until it is used.
const AdjudicationReplyTokens = 16000

// AdjudicationItem is one candidate output offered for adjudication.
type AdjudicationItem struct {
	// Label is the small integer the model answers with, and it is small for a measured reason.
	// Asked to answer with opaque tool_use ids, the model REGULARISED them -- `toolu_01..07` for
	// `toolu_probe_00..07` -- because reproducing a random identifier from thousands of tokens back
	// is a copying task, not a judgement. With integer labels it was 0 bad labels in 40+ trials.
	// The rule generalises: give the model short things it cannot get wrong and keep every mapping
	// on our side.
	Label      int
	ID         string // tool-call id, for the operator's logs only — never shown to the model
	SizeTokens int
	Content    string // the output itself; BuildAdjudicationPrompt bounds what it shows
}

// Verdict is one decision. Note what is NOT in it: any field carrying output content. A parse that
// succeeds cannot produce text to splice, which is what makes the transport failure mode unreachable
// rather than merely guarded.
type Verdict struct {
	Label    int    `json:"i"`
	Verdict  string `json:"verdict"`   // keep | drop
	NeededBy string `json:"needed_by"` // a | b | c | none  (see adjudicationContract's CRITERION)
	Quote    string `json:"quote,omitempty"`
}

// adjudicationContract is deliberately blunt about consequences.
//
// The cost-honest framing is worth ~26 points of live-kept on its own, measured: an earlier prompt
// reassured the model that removals "stay recoverable on request" and produced 91% removal at 6%
// live-kept; replacing that clause with the real consequence moved haiku to 64%/32% and sonnet to
// 49%/58%. Telling a model its mistakes are cheap makes it careless. So this text states the true
// cost and NEVER mentions recoverability — even though, on this path, the drop genuinely is
// recoverable through the marker and the stash. That asymmetry is intentional: the operator gets the
// safety net, the model is not told about it. Every softening of this text measured WORSE.
const adjudicationContract = `You are shown several tool outputs from one agent's transcript. Decide, for EACH
output, whether the agent still needs it.

CRITERION. An output is SPENT only if it is needed for NONE of the following:
  (a) the step the agent is on right now;
  (b) any instruction the user has given that is NOT YET COMPLETE;
  (c) any step the agent has EXPLICITLY STATED it will take and has not yet taken.
Only obligations WRITTEN IN THE TRANSCRIPT count -- do not invent hypothetical future needs. An
output whose information has already been captured elsewhere (a filed total, a recorded conclusion)
AND which no outstanding obligation needs in raw form is spent.

WHAT A WRONG REMOVAL ACTUALLY COSTS. If you remove something the agent still needs, it usually does
NOT notice the gap and does not ask for the content back. It answers from worse information and gets
the task wrong. There is no safety net you should count on. A wrong removal is a silent, permanent
loss of task quality; a wrong retention costs only tokens.

JUDGE THEM AGAINST EACH OTHER. You are given several outputs precisely so you can compare. Rank them:
the ones whose information has clearly been consumed and superseded are the candidates. If they all
look load-bearing, keep them all -- "keep everything" is a valid and often correct answer.

FOR EACH OUTPUT, ANSWER THE CRITERION FIRST, THEN DECIDE:
  "needed_by" -- which of (a)/(b)/(c) still needs this output, or "none" if it is spent.
  "quote"     -- when needed_by is a/b/c, the transcript text that creates that obligation, copied
                 VERBATIM. Leave empty only when needed_by is "none".
  "verdict"   -- keep (still needed, or you are unsure -- this is the default) or drop (its
                 information is spent; a short descriptor of its shape will remain in its place).
                 A verdict of "drop" REQUIRES needed_by "none": if any obligation still needs the
                 output, the verdict must be keep.

Reply with ONLY a JSON array, one object per output, no prose:
[{"i": <label>, "needed_by": "a|b|c|none", "quote": "<verbatim text or empty>", "verdict": "keep|drop"}]`

// THE PROMPT IS SPLIT so the invariant half can be cached, and the sibling batches of one sweep can
// read it instead of each paying fresh for the same bytes.
//
// It was NOT split at first, and that was a real defect rather than a missed optimisation: the
// component called Model.Complete, which routes to CompleteSystem(ctx, "", prompt) and thence to
// CompleteBlocks(ctx, nil, prompt) — no system field, so systemBlocks places NO cache_control mark
// at all. Every batch of a sweep sent the whole contract as fresh input and none could read
// another's write, because there was nothing to read. The identical defect is documented on
// extract_llm's own path.
//
// WHAT GOES WHERE, and it matters: the CONTRACT is invariant across every request and every tenant,
// so it is a system block. The GOAL is invariant across the batches of ONE request, so it is a second
// system block when the caller says there is more than one batch to share it. THE ITEMS ARE NEVER IN
// THE PREFIX — they differ per batch, so a cache entry containing them could never be read, which is
// strictly worse than no breakpoint.
//
// SIZE IS THE LIMIT, AND IT IS NOT MET ON HAIKU. A cache_control below the model's minimum cacheable
// prefix is silently ignored — no error, cache_creation_input_tokens: 0 — and the minimum is 4,096
// PROVIDER tokens on haiku-class (3,413 o200k after the unit conversion) against 1,024 on
// sonnet-class (853 o200k). MEASURED with internal/tokens: this contract is 504 o200k tokens, and
// with a two-message `recent` context the whole prefix is ~536. So:
//
//	sonnet-class    reachable — it needs only ~349 tokens of conversation on top of the contract,
//	                which a real two-message context often supplies.
//	haiku-class     PROVABLY NOT at context: recent — it would need ~2,909 tokens of conversation,
//	                i.e. context: full or a very large context_messages. That is proposal open
//	                question 2 and deliberately not changed to chase a cache.
//	unnameable id   treated as haiku-class, which is minCacheablePrefix's own conservative default.
//
// Split anyway. systemBlocks withholds the mark below the floor, so the split is free and correct on
// haiku and wins on sonnet — the same conclusion CompleteSystem's comment reaches for the same
// reason. The caller must not ASSUME the win: see AdjudicationPrefixTokens, and the
// sweep_prefix_uncacheable / sweep_prefix_cache_read_ZERO counters that make the outcome visible
// rather than hoped for.

// adjudicationGoalPrefix labels the conversation context. Kept as a constant because it is part of
// the cached prefix's bytes, and a prefix whose framing drifts is a prefix with a new cache key.
const adjudicationGoalPrefix = "WHAT THE AGENT IS DOING NOW (judge relevance toward this):\n"

// BuildAdjudicationSplit renders one batch as (cacheable system blocks, per-batch user message).
//
// cacheContext moves the goal into a trailing system block. Worth it only when the request will make
// MORE THAN ONE call — the goal is identical across a sweep's batches, so batches 2..N read it
// instead of re-sending it, but a cache WRITE costs 1.25x fresh, so paying it for a single call is a
// 25% loss. The caller decides, exactly as extract.Cfg.CacheContext works for the compaction path.
func BuildAdjudicationSplit(goal string, items []AdjudicationItem, cacheContext bool) (system []string, user string) {
	system = []string{adjudicationContract}
	g := clipAdjudicationGoal(goal)
	if cacheContext {
		system = append(system, adjudicationGoalPrefix+g)
		return system, adjudicationItemsBlock(items)
	}
	return system, adjudicationGoalPrefix + g + "\n\n" + adjudicationItemsBlock(items)
}

// AdjudicationPrefixTokens is the o200k size of the system prefix BuildAdjudicationSplit would
// produce, so a caller can ask cheapmodel.CacheablePrefix whether the provider will honour a
// breakpoint on it before paying a serialized round to earn one. Same unit as internal/tokens, which
// is the unit CacheablePrefix expects — comparing a provider count with an o200k count is the unit
// error that silently cost every haiku call its cache once already.
func AdjudicationPrefixTokens(goal string, cacheContext bool) int {
	system, _ := BuildAdjudicationSplit(goal, nil, cacheContext)
	n := 0
	for _, b := range system {
		n += tokens.Count(b)
	}
	return n
}

// AskAdjudication sends one batch through the best caching capability the client has: ordered system
// blocks, else one joined system field, else a single user message. Identical content in all three
// cases — only the caching differs — so a Model implementing neither optional interface still works.
//
// It lives here rather than in the component because completeSplit is unexported and because the
// decision about which half of the prompt is cacheable belongs next to the contract it is splitting.
func AskAdjudication(ctx context.Context, model Model, goal string, items []AdjudicationItem,
	cacheContext bool) (string, error) {
	system, user := BuildAdjudicationSplit(goal, items, cacheContext)
	return completeSplit(ctx, model, system, user)
}

// BuildAdjudicationPrompt renders the whole request as ONE string — the shape a client with no
// system capability receives, and what the split's two halves concatenate to. Kept so a test can
// assert on the whole thing the model sees.
func BuildAdjudicationPrompt(goal string, items []AdjudicationItem) string {
	system, user := BuildAdjudicationSplit(goal, items, false)
	return strings.Join(system, "\n\n") + "\n\n" + user
}

// clipAdjudicationGoal bounds the conversation context. Also the reason the goal is a separate
// block: clipping it inside the cached prefix keeps the prefix's size bounded and its bytes stable.
func clipAdjudicationGoal(goal string) string {
	g := strings.TrimSpace(goal)
	if g == "" {
		return "(no explicit goal stated)"
	}
	if len(g) > 4000 {
		g = g[:4000]
	}
	return g
}

// adjudicationItemsBlock renders the candidates. NEVER cacheable: they differ per batch, which is
// what makes them the user half.
func adjudicationItemsBlock(items []AdjudicationItem) string {
	var b strings.Builder
	for _, it := range items {
		b.WriteString("=== OUTPUT ")
		b.WriteString(strconv.Itoa(it.Label))
		b.WriteString(" (")
		b.WriteString(strconv.Itoa(it.SizeTokens))
		b.WriteString(" tokens)\ncontent:\n")
		b.WriteString(clipForAdjudication(it.Content, AdjudicationSampleChars))
		b.WriteString("\n\n")
	}
	return b.String()
}

// clipForAdjudication bounds one output and MARKS the cut, so the model knows it is judging an
// excerpt of something larger and does not reason as though it had seen the end.
func clipForAdjudication(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[excerpt truncated; the output continues for " +
		strconv.Itoa(len(s)-max) + " more characters]"
}

// ParseVerdicts reads the model's reply, returning the verdicts and whether the reply PARSED.
//
// THE TWO MUST STAY DISTINGUISHABLE, and `4ca1f13` is the commit that separated them. An EMPTY array
// is a legitimate answer -- "keep everything", which the contract explicitly invites -- while
// unparseable output is a prompt or model failure. Folding both into "no verdicts" made a deliberate
// keep-all indistinguishable from junk in the counters, and those counters are the only way to tell
// "the model declined to act" from "the model was never successfully asked", which is exactly the
// distinction one live arm turned on.
func ParseVerdicts(reply string) ([]Verdict, bool) {
	s := stripFences(strings.TrimSpace(reply))
	i, j := strings.Index(s, "["), strings.LastIndex(s, "]")
	if i < 0 || j <= i {
		return nil, false
	}
	var out []Verdict
	if err := json.Unmarshal([]byte(s[i:j+1]), &out); err != nil {
		return nil, false
	}
	return out, true
}

// ReplyWasTruncated reports whether an unparseable reply looks CUT OFF rather than malformed.
//
// A reply that opened the array and never closed it ran out of output budget; one that never opened
// it is a genuine format failure. The remedies are opposite -- raise the budget versus fix the prompt
// -- so folding them under one name hid a 70%-of-calls failure behind a label that reads as "the
// prompt is wrong" (`659e7a6`). Only meaningful when ParseVerdicts returned false.
func ReplyWasTruncated(reply string) bool {
	return strings.Contains(reply, "[") && !strings.Contains(reply, "]")
}

// Adjudication is what OUR code concluded, which is not the same thing as what the model said. Every
// field but Drop exists so a failure is COUNTED rather than inferred from a yield number.
type Adjudication struct {
	// Drop is the only field that authorises an action, and it is true only when every check below
	// passed. Every other outcome leaves the output verbatim.
	Drop bool
	// VerdictUnusable marks a verdict we do not act on — absent, empty, or a value the contract does
	// not offer (`trim`, most likely, since it used to be offered).
	VerdictUnusable bool
	// RefusedObligation marks a drop we would not perform: the model named an outstanding obligation
	// and then dropped the output anyway. ALERTABLE — it means the model tried to remove something
	// it had itself just said was still needed.
	RefusedObligation bool
	// QuoteFabricated marks an obligation quote that is not in the transcript. ALERTABLE — it means
	// the model is inventing evidence, and on this design it is the ONLY such signal left, because
	// nothing else the model returns is content.
	QuoteFabricated bool
	// CriterionMissing marks a verdict that never answered needed_by. Tolerated, but counted: it
	// means the forcing function did not run for this verdict.
	CriterionMissing bool
}

// Judge turns ONE verdict into a decision, applying the four safety rules that make it actionable.
// transcript is the agent's own text, flattened, and is what an obligation quote is checked against
// — verifying the quote is cheap and turns "did it make that up?" from a worry into a counter.
//
// EVERY FAILURE PATH RESOLVES TOWARD KEEP. That is not caution for its own sake: a wrong keep costs
// tokens on one turn, a wrong drop is a silent permanent loss the agent does not notice and cannot
// ask about. The two errors are not comparable, so the code does not treat them symmetrically. This
// is orthogonal to batch size — the same rules applied per-verdict whatever the batch was.
func Judge(v Verdict, transcript string) Adjudication {
	var a Adjudication
	nb := strings.ToLower(strings.TrimSpace(v.NeededBy))
	if nb == "" {
		// Counted on keeps as well as drops. The name is what it says: the criterion was not
		// answered. A model that never answers it is one the forcing function is not reaching at
		// all, and that is the same defect whichever way the verdict fell.
		a.CriterionMissing = true
	}
	if q := strings.TrimSpace(v.Quote); q != "" && !transcriptHasQuote(transcript, q) {
		a.QuoteFabricated = true
	}
	switch strings.ToLower(strings.TrimSpace(v.Verdict)) {
	case "drop":
		// COHERENCE, AND IT POINTS THE DANGEROUS WAY. The criterion states that a drop requires
		// needed_by "none". A verdict that names an outstanding obligation and drops the output
		// anyway contradicts itself in the direction of silent loss, so it is REFUSED rather than
		// performed. Anything that is neither empty nor "none" counts as naming one, including a
		// value the contract never offered — an unrecognised criterion answer is not evidence of
		// spent-ness.
		if nb != "" && nb != "none" {
			a.RefusedObligation = true
			return a
		}
		a.Drop = true
	case "keep":
	default:
		// Missing, empty, or a verdict we do not perform. `trim` is the one to expect: it was
		// offered by the previous design and a model may still answer with it. Degrade to keep and
		// COUNT it rather than discarding the verdict — a discarded verdict leaves the output
		// unjudged, which looks identical to a model that said nothing about it.
		a.VerdictUnusable = true
	}
	return a
}

// transcriptHasQuote reports whether q appears in the transcript.
//
// Exact containment first, which is what the merged design checked. Then ONE whitespace-insensitive
// retry, because a model that re-wrapped a long line has copied the text faithfully and is not
// inventing — and this counter is meant to be alertable, so a routine re-wrap firing it would train
// the operator to ignore the signal that says the model is fabricating. The retry runs only on a
// miss, so the transcript is normalised at most once per fabrication-suspect quote rather than once
// per candidate.
func transcriptHasQuote(transcript, q string) bool {
	if strings.Contains(transcript, q) {
		return true
	}
	return strings.Contains(wsRe.ReplaceAllString(transcript, " "), wsRe.ReplaceAllString(q, " "))
}
