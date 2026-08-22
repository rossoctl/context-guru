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

// BulkVerdict is one decision. Kept carries the retained text for a trim, and is ignored otherwise.
type BulkVerdict struct {
	Index   int    `json:"i"`
	Verdict string `json:"verdict"` // keep | trim | drop
	Kept    string `json:"kept,omitempty"`
}

// bulkContract is deliberately blunt about consequences. See the cost-honest framing note above:
// every softening of this text measured WORSE.
const bulkContract = `You are shown several tool outputs from one agent's transcript, each with evidence
about whether the agent has referred back to it since. Decide, for EACH output, whether the agent
still needs it.

WHAT A WRONG REMOVAL ACTUALLY COSTS. If you remove something the agent still needs, it usually does
NOT notice the gap and does not ask for the content back. It answers from worse information and gets
the task wrong. There is no safety net you should count on. A wrong removal is a silent, permanent
loss of task quality; a wrong retention costs only tokens.

JUDGE THEM AGAINST EACH OTHER. You are given several outputs precisely so you can compare. Rank them:
the ones whose information has clearly been consumed and superseded are the candidates. If they all
look load-bearing, keep them all — "keep everything" is a valid and often correct answer.

READING THE EVIDENCE. novel = identifiers this output introduced. refs = how many later turns reused
one. ref_age = how many messages ago the last reuse was. used_frac = what share of its identifiers
were carried forward. later_turns = how many turns the output has HAD to be referenced in.

  - refs=0 with many later_turns is the strongest signal of deadness — but it is exact-match
    evidence only, so an output whose values were TRANSFORMED (summed, reformatted, reworded) before
    being restated leaves refs=0 while still being load-bearing. Your job on those is to VETO the
    index, not to rubber-stamp it.
  - a LOW used_frac on a referenced output is ambiguous and must not be read as "the rest is chaff".
    The agent may have taken an ANCHOR — a name, an id — precisely in order to point at a payload it
    never copied. Keep the payload.
  - novel=0 means the index could see nothing trackable. That is absence of evidence, not evidence of
    absence. Default to keep.
  - few later_turns means the output has not yet had a chance to be used. Keep it.

VERDICTS, one per output:
  keep — still needed, or you are unsure. This is the default.
  drop — its information is spent; a short descriptor of its shape will remain in its place.
  trim — mostly spent, but some records must survive. Return those records VERBATIM in "kept":
         byte-for-byte copies of what you were shown, never paraphrased, summarised or reformatted.

Reply with ONLY a JSON array, one object per output, no prose:
[{"i": <index>, "verdict": "keep|trim|drop", "kept": "<verbatim text, trim only>"}]`

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

// ParseBulkVerdicts reads the model's reply. Anything unparseable yields no verdicts, which the
// caller treats as "change nothing" — the fail-open direction.
func ParseBulkVerdicts(reply string) []BulkVerdict {
	s := stripFences(strings.TrimSpace(reply))
	i, j := strings.Index(s, "["), strings.LastIndex(s, "]")
	if i < 0 || j <= i {
		return nil
	}
	var out []BulkVerdict
	if err := json.Unmarshal([]byte(s[i:j+1]), &out); err != nil {
		return nil
	}
	keep := out[:0]
	for _, v := range out {
		switch v.Verdict {
		case "keep", "trim", "drop":
			keep = append(keep, v)
		}
	}
	return keep
}
