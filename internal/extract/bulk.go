package extract

import (
	"encoding/json"
	"strconv"
	"strings"
)

// BULK ADJUDICATION — the only model shape the held-out selection experiment found worth pursuing.
//
// docs/results/coref-selection-experiment.md measured ten arms over 8,105 recorded decisions and
// settled two things that this file exists to implement, and one it exists to avoid:
//
//   REFUTED — the per-output merged design. One call, shown ONE output plus its reference evidence,
//   deciding drop-or-trim, scored 6% live-kept on haiku and 14% on sonnet, both inside the
//   drop-everything null model's error bar. Shown a single output, a model simply drops it.
//
//   WORKS — bulk adjudication. One call shown ~15 outputs TOGETHER lifted live-kept from 6% to 58%
//   at the LOWEST cost per output, because the overhead amortises and, more importantly, because
//   comparative judgement beats absolute judgement: ranking fifteen candidates against each other
//   is a question a model can answer, "is this one output expendable" is not.
//
//   WORKS — cost-honest framing, worth ~26 points of live-kept on its own. The first prompt
//   reassured the model that removals "stay recoverable on request"; that single clause produced
//   91% removal at 6% live-kept. Replacing it with the real consequence moved haiku to 64%/32% and
//   sonnet to 49%/58%. Telling a model its mistakes are cheap makes it careless, so this prompt
//   states the true cost and never mentions recoverability.
//
// The deterministic index still beats every model arm measured (95% live-kept at 11% false-drop, at
// zero marginal cost), and a floor-symmetric re-score plus a widened Tier-2 ground truth did not
// close that gap (docs/experiments/loca/iter009/results.md). So this is not shipped as a default —
// it exists to answer the one question decision-quality experiments structurally cannot: whether it
// moves REWARD.

// BulkItem is one candidate output offered for adjudication, with the co-reference evidence the
// deterministic index already computed for it.
type BulkItem struct {
	Index      int    // caller's message index, echoed back in the verdict
	ID         string // tool-call id, for the operator's logs
	SizeTokens int
	Evidence   string // rendered reference evidence (see RenderEvidence)
	Sample     string // the output itself, truncated
}

// BulkVerdict is one decision.
//
// NeededBy and Quote are the FORCED EVIDENCE, and they are the only thing measured to protect the
// marginal case (docs/experiments/loca/iter019/results.md §6). Arms carrying an identical criterion
// differed ONLY in whether the model had to emit which obligation still needs the output, and the
// arm that had to emit it halved the false-drop rate: instructions the model can skim past are
// inert, a required output field is not.
//
// `Kept` is GONE along with the trim verdict. Offered across 21 probe opportunities trim was chosen
// zero times, keep/drop scored identically to keep/drop/trim on every metric, and in production it
// was accepted once against eight rejected as invented -- it was the only verdict requiring the
// model to transport text, which is what it is worst at.
type BulkVerdict struct {
	Index    int    `json:"i"`
	Verdict  string `json:"verdict"`   // keep | drop
	NeededBy string `json:"needed_by"` // a | b | c | none  (see bulkContract's CRITERION)
	Quote    string `json:"quote,omitempty"`
}

// bulkContract is deliberately blunt about consequences. See the cost-honest framing note above:
// every softening of this text measured WORSE.
const bulkContract = `You are shown several tool outputs from one agent's transcript, each with evidence
about whether the agent has referred back to it since. Decide, for EACH output, whether the agent
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

JUDGE THEM AGAINST EACH OTHER. You are given several outputs precisely so you can compare. Rank them:
the ones whose information has clearly been consumed and superseded are the candidates. If they all
look load-bearing, keep them all -- "keep everything" is a valid and often correct answer.

READING THE EVIDENCE. novel = identifiers this output introduced. refs = how many later turns reused
one. ref_age = how many messages ago the last reuse was. used_frac = what share of its identifiers
were carried forward. later_turns = how many turns the output has HAD to be referenced in.

  - refs=0 with many later_turns is the strongest signal of deadness -- but it is exact-match
    evidence only, so an output whose values were TRANSFORMED (summed, reformatted, reworded) before
    being restated leaves refs=0 while still being load-bearing. Your job on those is to VETO the
    index, not to rubber-stamp it.
  - a LOW used_frac on a referenced output is ambiguous and must not be read as "the rest is chaff".
    The agent may have taken an ANCHOR -- a name, an id -- precisely in order to point at a payload it
    never copied. Keep the payload.
  - novel=0 means the index could see nothing trackable. That is absence of evidence, not evidence of
    absence. Default to keep.
  - few later_turns means the output has not yet had a chance to be used. Keep it.

FOR EACH OUTPUT, ANSWER THE CRITERION FIRST, THEN DECIDE:
  "needed_by" -- which of (a)/(b)/(c) still needs this output, or "none" if it is spent.
  "quote"     -- when needed_by is a/b/c, the transcript text that creates that obligation, copied
                 VERBATIM. Leave empty only when needed_by is "none".
  "verdict"   -- keep (still needed, or you are unsure -- this is the default) or drop (its
                 information is spent; a short descriptor of its shape will remain in its place).
                 A verdict of "drop" REQUIRES needed_by "none": if any obligation still needs the
                 output, the verdict must be keep.

Reply with ONLY a JSON array, one object per output, no prose:
[{"i": <index>, "needed_by": "a|b|c|none", "quote": "<verbatim text or empty>", "verdict": "keep|drop"}]`

// BuildBulkPrompt renders the adjudication request. goal is what the agent is currently doing, so
// relevance is judged toward the live task rather than in the abstract.
func BuildBulkPrompt(goal string, items []BulkItem) string {
	var b strings.Builder
	b.WriteString(bulkContract)
	b.WriteString("\n\nWHAT THE AGENT IS DOING NOW (judge relevance toward this):\n")
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
		b.WriteString(strconv.Itoa(it.Index))
		b.WriteString(" (")
		b.WriteString(strconv.Itoa(it.SizeTokens))
		b.WriteString(" tokens)\nevidence: ")
		b.WriteString(it.Evidence)
		b.WriteString("\ncontent:\n")
		b.WriteString(it.Sample)
		b.WriteString("\n\n")
	}
	return b.String()
}

// ParseBulkVerdicts reads the model's reply, returning the verdicts and whether the reply PARSED.
//
// The two must be distinguished. An empty array is a legitimate answer -- "keep everything", which
// the contract explicitly invites -- while unparseable output is a prompt or model failure. Folding
// both into "no verdicts" made a deliberate keep-all indistinguishable from junk in the gate
// counters, and those counters are the only way to tell "the model declined to act" from "the model
// was never successfully asked", which is exactly the distinction one live arm turned on.
func ParseBulkVerdicts(reply string) ([]BulkVerdict, bool) {
	s := stripFences(strings.TrimSpace(reply))
	i, j := strings.Index(s, "["), strings.LastIndex(s, "]")
	if i < 0 || j <= i {
		return nil, false
	}
	var out []BulkVerdict
	if err := json.Unmarshal([]byte(s[i:j+1]), &out); err != nil {
		return nil, false
	}
	keep := out[:0]
	for _, v := range out {
		switch v.Verdict {
		case "keep", "drop":
			keep = append(keep, v)
		case "trim":
			// Trim is no longer offered. A model that answers with it anyway is asking for partial
			// retention we cannot perform, so degrade to the safe direction rather than discarding
			// the verdict -- dropping it entirely would leave the output unjudged and looks
			// identical to "the model said nothing about it".
			v.Verdict = "keep"
			keep = append(keep, v)
		}
	}
	return keep, true
}
