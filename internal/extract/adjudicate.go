package extract

import (
	"encoding/json"
	"strconv"
	"strings"
)

// COLD-SWEEP ADJUDICATION, as a PREFIX ASK. The question goes to the REQUEST's own model, over the
// transcript that model already has in its prompt cache, and what travels is an INVENTORY of
// candidates rather than their content.
//
// WHY NOT A CHEAP MODEL OVER COPIED CONTENT. Two measurements, both from `a9d666f`:
//
//	Need is relevance MINUS whatever has already been captured elsewhere in the transcript, and that
//	second term lives in the LATER TURNS. A judgement shown only the candidate is being asked to veto
//	an exact-match index on transformed reuse while being withheld the turns where the reuse appears.
//
//	Verbatim quoting — the one signal that says whether the model is inventing — degraded to 20.8% on
//	the cheap model at the batch sizes a bulk mechanism needs, against 0 of 59 on the request model.
//	Since a fabricated quote is the ONLY remaining check on this design, that alone settles which
//	model is asked.
//
// So the judgement wants the agent's own model AND the whole transcript, and only a cache read makes
// that affordable: appending a trailing user message to a byte-identical prefix read 19,595 tokens
// from cache and created 0, measured on the live route.
//
// WHY THE INVENTORY, AND WHY IT IS NOT A CONTRADICTION. `a9d666f`: "Paying fresh to send truncated
// copies of content the model is reading from cache would defeat the mechanism and show it an excerpt
// of something it could read in full." The model is not being shown less than before — it is being
// shown MORE, in full, from cache, and the inventory exists only to say WHICH outputs are under
// consideration and what to call them in the reply.
//
// ONE CALL FOR ALL CANDIDATES. The batch assembler, the item cap and the per-batch concurrency this
// file used to carry existed only because content was being copied and a reply had to be bounded
// against a cheap model's window. Nothing is copied now, so there is nothing to divide.
//
// WHAT IS DELIBERATELY ABSENT, against bulk.go's BuildPrefixAsk:
//
//   - the READING THE EVIDENCE section and the per-item `evidence` field. They described a
//     co-reference index's counters, and there is no such index on `main`. Shipping either would
//     teach the model to read a field the prompt never carries.
//   - the per-item tool_use id. bulk.go printed it beside the label; it is kept off the wire here
//     because a random identifier in front of the model is a string it may echo instead of the
//     label, and the labels are integers for exactly that reason. Ids stay on our side, for logs.

// AdjudicationItem is one candidate named in the inventory. Note what it does NOT carry to the model:
// the output itself. Content is held only so the caller can resolve a label back to a message.
type AdjudicationItem struct {
	// Label is the small integer the model answers with, and it is small for a measured reason.
	// Asked to answer with opaque tool_use ids, the model REGULARISED them -- `toolu_01..07` for
	// `toolu_probe_00..07` -- because reproducing a random identifier from thousands of tokens back
	// is a copying task, not a judgement. With integer labels it was 0 bad labels in 40+ trials.
	// The rule generalises: give the model short things it cannot get wrong and keep every mapping
	// on our side.
	Label int
	// ID is the tool-call id, for the operator's logs only. Never shown to the model.
	ID         string
	SizeTokens int
	// Head is a single bounded line so the model can LOCATE this output in the transcript above.
	// Not a sample of it: the model reads the output in full from cache, and an excerpt would be a
	// worse version of something it already has.
	Head string
	// Sample is a bounded excerpt used ONLY by BuildFallbackAsk, on the path where the cache read did
	// not happen and the model therefore cannot read the output from anywhere. Empty on the prefix-ask
	// path, which is what keeps the content off the wire there.
	Sample string
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
const adjudicationContract = `Some of the tool outputs in the conversation above may no longer be needed. Decide,
for EACH output listed below, whether you still need it.

CRITERION. An output is SPENT only if it is needed for NONE of the following:
  (a) the step you are on right now;
  (b) any instruction the user has given that is NOT YET COMPLETE;
  (c) any step you have EXPLICITLY STATED you will take and have not yet taken.
Only obligations WRITTEN IN THE CONVERSATION count -- do not invent hypothetical future needs. An
output whose information has already been captured elsewhere (a filed total, a recorded conclusion)
AND which no outstanding obligation needs in raw form is spent.

WHAT A WRONG REMOVAL ACTUALLY COSTS. If something you still need is removed, you will usually NOT
notice the gap and will not ask for the content back. You will answer from worse information and get
the task wrong. There is no safety net you should count on. A wrong removal is a silent, permanent
loss of task quality; a wrong retention costs only tokens.

JUDGE THEM AGAINST EACH OTHER. You are given several outputs precisely so you can compare. Rank them:
the ones whose information has clearly been consumed and superseded are the candidates. If they all
look load-bearing, keep them all -- "keep everything" is a valid and often correct answer.

FOR EACH OUTPUT, ANSWER THE CRITERION FIRST, THEN DECIDE:
  "needed_by" -- which of (a)/(b)/(c) still needs this output, or "none" if it is spent.
  "quote"     -- when needed_by is a/b/c, the text from the conversation that creates that
                 obligation, copied VERBATIM. Leave empty only when needed_by is "none".
  "verdict"   -- keep (still needed, or you are unsure -- this is the default) or drop (its
                 information is spent; a short descriptor of its shape will remain in its place).
                 A verdict of "drop" REQUIRES needed_by "none": if any obligation still needs the
                 output, the verdict must be keep.

Reply with ONLY a JSON array, one object per output, no prose:
[{"i": <label>, "needed_by": "a|b|c|none", "quote": "<verbatim text or empty>", "verdict": "keep|drop"}]`

// BuildPrefixAsk renders the adjudication question for a PREFIX ASK — a call whose prefix is the
// transcript the agent already sent, read from the provider's prompt cache.
//
// THE DIFFERENCE FROM A CONTENT PROMPT IS WHAT IS NOT HERE: the outputs themselves. A bare completion
// has no way to show them except by shipping a truncated sample of each, which caps every candidate
// and forces the model to judge an excerpt. A prefix ask has the whole transcript above it, so
// shipping samples would pay fresh tokens for content already being read from cache AND show a
// TRUNCATED copy of something the model can read in full.
//
// There is no `goal` parameter, and that is the point rather than an omission: the conversation IS the
// prefix, so what the agent is doing now is above the question already. Rendering it again would be
// paying fresh for a copy of cached bytes.
func BuildPrefixAsk(items []AdjudicationItem) string {
	var b strings.Builder
	b.WriteString(adjudicationContract)
	b.WriteString("\n\nThe conversation above is your own. Read the tool outputs from it directly.\n")
	b.WriteString("\nTOOL OUTPUTS UNDER CONSIDERATION. Refer to them by these labels only:\n")
	for _, it := range items {
		b.WriteString("  [")
		b.WriteString(strconv.Itoa(it.Label))
		b.WriteString("] ")
		b.WriteString(strconv.Itoa(it.SizeTokens))
		b.WriteString(" tokens, begins: ")
		b.WriteString(it.Head)
		b.WriteString("\n")
	}
	return b.String()
}

// FallbackSampleChars bounds each output shown in the FALLBACK prompt. The whole point of that path
// is comparative judgement across every candidate, so the per-output budget must stay small enough
// that a long list plus the contract still fits a window.
const FallbackSampleChars = 2000

// BuildFallbackAsk renders the question as a SELF-CONTAINED prompt: the contract, the goal, and a
// bounded sample of each output.
//
// THIS IS THE EXPENSIVE PATH AND IT EXISTS ON PURPOSE. It is used only when the prefix ask could not
// read the cache — a first turn with nothing stashed yet, a diverged prefix, an expired entry, a route
// with no asker. `a9d666f` chose to fall back rather than skip, and the reason is worth restating:
// treating "no prefix" as "no verdicts" would disable the component on every session's FIRST turn and
// read, in the counters, as a model that declined to act.
//
// What it costs is what the prefix ask exists to avoid: fresh tokens for content the cached path would
// have read for a tenth of the price, and a TRUNCATED view of each output rather than the whole thing.
// So it is counted every time it happens, and an operator who would rather forgo the yield than pay
// for it can switch it off with `block_fallback`.
func BuildFallbackAsk(goal string, items []AdjudicationItem) string {
	var b strings.Builder
	b.WriteString(adjudicationContract)
	b.WriteString("\n\nWHAT YOU ARE DOING NOW (judge relevance toward this):\n")
	g := strings.TrimSpace(goal)
	if g == "" {
		g = "(no explicit goal stated)"
	}
	if len(g) > 4000 {
		g = g[:4000]
	}
	b.WriteString(g)
	b.WriteString("\n\n")
	for _, it := range items {
		b.WriteString("=== OUTPUT ")
		b.WriteString(strconv.Itoa(it.Label))
		b.WriteString(" (")
		b.WriteString(strconv.Itoa(it.SizeTokens))
		b.WriteString(" tokens)\ncontent:\n")
		b.WriteString(it.Sample)
		b.WriteString("\n\n")
	}
	return b.String()
}

// ClipSample bounds one output for the fallback prompt and MARKS the cut, so the model knows it is
// judging an excerpt of something larger and does not reason as though it had seen the end.
func ClipSample(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n…[excerpt truncated; the output continues for " +
		strconv.Itoa(len(s)-max) + " more characters]"
}

// HeadLine returns a single-line, bounded opening of s, for locating an output in the transcript
// above. Newlines are collapsed so one candidate stays one line and the inventory stays readable to a
// model counting labels.
func HeadLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// AdjudicationHeadChars bounds the locating line. Small on purpose: it exists to disambiguate which
// output a label refers to, not to inform the judgement, and every character of it is paid fresh.
const AdjudicationHeadChars = 90

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
