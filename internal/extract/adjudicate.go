package extract

import (
	"encoding/json"
	"strconv"
	"strings"
)

// COLD-SWEEP ADJUDICATION. The model returns a VERDICT, never content.
//
// This is the contract `feat/coref-compaction` arrived at (internal/extract/bulk.go), ported here
// without its batching. The contract is general; the batch is not.
//
// WHY IT IS A VERDICT AND NOT A REWRITE. `cc1aa9f` removed the `trim` verdict from that design after
// measuring it: trim was chosen ZERO times in 21 probe opportunities, keep/drop scored identically to
// keep/drop/trim on every metric, and in production it was accepted ONCE against EIGHT rejected as
// invented. It was the only verdict that asked the model to transport text, which is what it is worst
// at. What survives is binary, and it removes the transporting OPERATION rather than merely the
// transporting STRATEGIES — the strongest available form of "never ask a model to transport text".
//
// WHY THE CRITERION IS FORCED EVIDENCE AND NOT ADVICE. Arms carrying an identical criterion differed
// ONLY in whether the model had to emit which obligation still needs the output; the arm that had to
// emit it HALVED the false-drop rate (4/4 -> 2/4). Stating the criterion alone measured inert.
// Instructions a model can skim past are inert; a required output field is not.
//
// WHAT IS DELIBERATELY ABSENT, against bulk.go. Two things, and both because there is nothing here
// for them to talk about:
//
//   - the READING THE EVIDENCE section. It taught the model to interpret a co-reference index's
//     counters, and there is no such index on this path. Shipping the section without the counters
//     would be teaching the model to read a field the prompt never carries.
//   - the JUDGE THEM AGAINST EACH OTHER section. Comparative ranking is the one thing a per-output
//     call structurally cannot do, so the paragraph would be a lie about the question being asked.
//
// The second absence is a KNOWN RISK, recorded here rather than hidden: the merged experiment
// measured comparative judgement as the difference between 6% and 58% live-kept, and `4ca1f13` found
// a live arm that had degraded to 1.02 verdicts per call and read that as "the per-output design
// already refuted at 6%". So the prior on per-output YIELD is negative and the safety machinery below
// is what carries this design — every failure mode it detects resolves toward keep.

// AdjudicationItem is the one candidate output a single adjudication call is about.
type AdjudicationItem struct {
	Index      int    // caller's message index, for the operator's logs only
	ID         string // tool-call id, likewise
	SizeTokens int
	Content    string // the output itself; BuildAdjudicationPrompt bounds what it shows
}

// Verdict is the model's whole reply. Note what is NOT in it: any field carrying output content.
// A parse that succeeds cannot produce text to splice, which is what makes the transport failure
// mode unreachable rather than merely guarded.
type Verdict struct {
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
// safety net, the model is not told about it.
const adjudicationContract = `You are shown ONE tool output from an agent's transcript. Decide whether the agent
still needs it.

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

"KEEP EVERYTHING" IS A VALID AND OFTEN CORRECT ANSWER. You are judging one output in isolation, so
you cannot tell whether something else would have been the better thing to remove. If this one looks
load-bearing, keep it. Do not reach for a removal because you were asked a question.

ANSWER THE CRITERION FIRST, THEN DECIDE:
  "needed_by" -- which of (a)/(b)/(c) still needs this output, or "none" if it is spent.
  "quote"     -- when needed_by is a/b/c, the transcript text that creates that obligation, copied
                 VERBATIM. Leave empty only when needed_by is "none".
  "verdict"   -- keep (still needed, or you are unsure -- this is the default) or drop (its
                 information is spent; a short descriptor of its shape will remain in its place).
                 A verdict of "drop" REQUIRES needed_by "none": if any obligation still needs the
                 output, the verdict must be keep.

Reply with ONLY a JSON object, no prose:
{"needed_by": "a|b|c|none", "quote": "<verbatim text or empty>", "verdict": "keep|drop"}`

// adjudicationSampleChars bounds the output shown to the model.
//
// The same 4,000 the compaction prompts use, and for the same reason: it is the largest excerpt worth
// paying for on every candidate. It is a HEAD excerpt with the cut marked, because the question is
// "is this spent", which is answered from what the output IS rather than from every row in it — and
// because an unmarked cut invites the model to reason about content it was never shown.
const adjudicationSampleChars = 4000

// BuildAdjudicationPrompt renders the request for ONE output. goal is the conversation the caller
// chose to carry (see the component's `context` setting), so spent-ness is judged against the live
// task rather than in the abstract.
func BuildAdjudicationPrompt(goal string, item AdjudicationItem) string {
	var b strings.Builder
	b.WriteString(adjudicationContract)
	b.WriteString("\n\nWHAT THE AGENT IS DOING NOW (judge relevance toward this):\n")
	g := strings.TrimSpace(goal)
	if g == "" {
		g = "(no explicit goal stated)"
	}
	if len(g) > 4000 {
		g = g[:4000]
	}
	b.WriteString(g)
	b.WriteString("\n\n=== THE OUTPUT UNDER CONSIDERATION (")
	b.WriteString(strconv.Itoa(item.SizeTokens))
	b.WriteString(" tokens)\n")
	b.WriteString(clipForAdjudication(item.Content, adjudicationSampleChars))
	b.WriteString("\n")
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

// ParseVerdict reads the model's reply, returning the verdict and whether the reply PARSED.
//
// The two must stay distinguishable. A reply that parsed and said keep is the model DECLINING to act,
// which the contract explicitly invites; a reply that did not parse is a prompt or model failure the
// component never got an answer from. Folding both into "no drop" makes those two identical in the
// counters, and telling them apart is exactly the distinction one live arm turned on (4ca1f13).
func ParseVerdict(reply string) (Verdict, bool) {
	s := stripFences(strings.TrimSpace(reply))
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j <= i {
		return Verdict{}, false
	}
	var v Verdict
	if err := json.Unmarshal([]byte(s[i:j+1]), &v); err != nil {
		return Verdict{}, false
	}
	return v, true
}

// Adjudication is what OUR code concluded, which is not the same thing as what the model said. Every
// field but Drop exists so a failure is COUNTED rather than inferred from a yield number.
type Adjudication struct {
	// Parsed is false when the reply was not a usable JSON object at all.
	Parsed bool
	// Drop is the only field that authorises an action, and it is true only when every check below
	// passed. Every other outcome leaves the output verbatim.
	Drop bool
	// VerdictUnusable marks a reply that parsed but carried no verdict we act on — absent, empty, or
	// a value the contract does not offer (`trim`, most likely, since it used to be offered).
	VerdictUnusable bool
	// RefusedObligation marks a drop we would not perform: the model named an outstanding obligation
	// and then dropped the output anyway. ALERTABLE — it means the model tried to remove something
	// it had itself just said was still needed.
	RefusedObligation bool
	// QuoteFabricated marks an obligation quote that is not in the transcript. ALERTABLE — it means
	// the model is inventing evidence, and on this design it is the ONLY such signal left, because
	// nothing else the model returns is content.
	QuoteFabricated bool
	// CriterionMissing marks a reply that never answered needed_by. Tolerated, but counted: it means
	// the forcing function did not run for this verdict.
	CriterionMissing bool
}

// Judge turns one reply into a decision, applying the four safety rules that make a verdict
// actionable. transcript is the agent's own text, flattened, and is what an obligation quote is
// checked against — verifying the quote is cheap and turns "did it make that up?" from a worry into
// a counter.
//
// EVERY FAILURE PATH RESOLVES TOWARD KEEP. That is not caution for its own sake: a wrong keep costs
// tokens on one turn, a wrong drop is a silent permanent loss the agent does not notice and cannot
// ask about. The two errors are not comparable, so the code does not treat them symmetrically.
func Judge(reply, transcript string) Adjudication {
	v, ok := ParseVerdict(reply)
	if !ok {
		// UNSURE DEFAULTS TO KEEP, and an unparseable reply is the strongest form of unsure.
		return Adjudication{}
	}
	a := Adjudication{Parsed: true}
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
		// COUNT it rather than discarding the reply — a discarded reply leaves the output unjudged,
		// which looks identical to a model that said nothing.
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
