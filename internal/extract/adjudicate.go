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
// The per-item tool_use id and the co-reference `evidence` field are both in the item shape and play
// different parts here: the id is RENDERED, because it is the only exact anchor between an inventory
// line and the content (see AdjudicationItem.ID), while evidence renders only when something populates
// it, which nothing on `main` does.

// AdjudicationItem is one candidate named in the inventory. Note what it does NOT carry to the model:
// the output itself. Content is held only so the caller can resolve a label back to a message.
type AdjudicationItem struct {
	// Label is the ANSWER key: the small integer the model must put in "i", and the only thing it is
	// asked to reproduce.
	//
	// An integer for a measured reason. Asked to answer with opaque tool_use ids the model REGULARISED
	// them -- `toolu_01..07` for `toolu_probe_00..07` -- because reproducing a random identifier from
	// thousands of tokens back is a copying task, not a judgement. With integer labels it was 0 bad
	// labels in 40+ trials. The rule generalises: give the model short things it cannot get wrong and
	// keep every mapping on our side.
	Label int
	// ID is the tool-call id, and it is the EXACT anchor between an inventory line and the content the
	// model is reading from cache. Shown, but never asked for.
	//
	// THE TWO AIDS HAVE SEPARATE JOBS, and conflating them is a mistake available in both directions.
	// Label above is what the model ANSWERS with; this is what it LOCATES with, and it is never asked
	// to reproduce it. The measurement behind integer labels says the answer key must be an integer --
	// it says nothing about whether an id may appear in the prompt, and the echo risk it describes was
	// always about what the model RETURNS rather than what it reads.
	//
	// Withholding it costs the one exact anchor there is. The id appears in the transcript the model
	// reads from cache, so shipping it makes an inventory line and a tool result identifiable to each
	// other; without it, head-plus-size is the only matching signal, and that is shaky on a transcript
	// carrying a dozen near-identical `Read` results.
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
	// Evidence is a rendered reference-tracking record for this output. It goes into the inventory line
	// when non-empty and is omitted entirely when not.
	//
	// A SEAM, deliberately empty on `main`. There is no co-reference index here, so nothing populates
	// it and the contract says nothing about how to read one -- a prompt that taught the model to
	// interpret counters the prompt never carries would be teaching it to read a field that does not
	// exist. A field rather than a future signature change, because the alternative is reshaping the
	// contract when the index arrives, and the contract is the part with measurements attached to it.
	Evidence string
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
//
// TWO EDITS FROM ITERATION 027, and what they were answering. The adjudication dump showed 46 verdicts
// across 10 asks: 45 keeps, 1 drop. 28 of those keeps cited criterion (b), 17 cited (a), and 12 of 45
// rested on a quote that is not in the transcript.
//
//  1. THE RAW-FORM TEST MOVED INTO THE CRITERION. (b) reads "an instruction that is NOT YET COMPLETE",
//     which is true for the whole duration of any unfinished task -- so as written it licenses keeping
//     anything topically related to the task. The narrowing idea was already in this text ("no
//     outstanding obligation needs it in raw form") but sat in a later sentence qualifying only the
//     "captured elsewhere" case. It is now the question itself.
//
//     APPLIED TO (a) AS WELL, and that is not incidental: the dump shows the SAME output cited under
//     (b) on one turn and (a) on the next as later_turns grew. Narrow one clause and the answers
//     migrate to the other, which would produce no measurable change and read as "the prompt is not the
//     problem". The clauses have to move together.
//
//  2. THE QUOTE IS DECLARED CHECKED. It always was checked -- Judge verifies it against the flattened
//     transcript and sets QuoteFabricated -- but the model was never told, and 27% of keeps carried a
//     quote that could not be found. Stating the verification removes the incentive to reconstruct one
//     from memory. Note what this deliberately does NOT do: a fabricated quote still leaves the verdict
//     alone, because every failure path here resolves toward keep and making fabrication cause a
//     REMOVAL would invert that asymmetry in the one direction that loses task quality silently.
//
// A THIRD EDIT WAS DRAFTED AND NOT APPLIED. "keep everything is a valid and often correct answer"
// below reads as encouragement and is the line most directly implicated in a 98% keep rate -- but it is
// also the text whose cost-honest framing is worth ~26 points of live-kept, and this file's own history
// says every softening measured worse. Changing it is a measurement, not an edit — the instrument is the
// offline selection scorer (8,105 decisions, $0 to re-score), because live-kept is what the clause
// trades against and no reward run at this budget can resolve it. Tracked in #242; do not quietly
// reword it here.
const adjudicationContract = `Some of the tool outputs in the conversation above may no longer be needed. Decide,
for EACH output listed below, whether you still need it.

CRITERION. An output is SPENT only if NONE of the following would require you to RE-READ ITS
CONTENTS. Being about the task is not enough -- the question is whether you need these bytes again:
  (a) the step you are on right now;
  (b) an instruction the user has given that is NOT YET COMPLETE;
  (c) a step you have EXPLICITLY STATED you will take and have not yet taken.
Only obligations WRITTEN IN THE CONVERSATION count -- do not invent hypothetical future needs.

THE TEST, per output: could you carry out that obligation from what you have ALREADY concluded or
written down, without looking at this output again? If yes, it is spent -- even though the task is
unfinished, and even though the output is about the task. An unfinished task does not by itself make
every output it touched still needed.

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
                 obligation, copied VERBATIM. Your quote is checked against the conversation
                 character for character. Do not paraphrase or reconstruct it: if you cannot find
                 the exact text, you have not identified a written obligation.
                 Leave empty only when needed_by is "none".
  "verdict"   -- keep (still needed, or you are unsure -- this is the default) or drop (its
                 information is spent; a short descriptor of its shape will remain in its place).
                 A verdict of "drop" REQUIRES needed_by "none": if any obligation still needs the
                 output, the verdict must be keep.

ANSWER BY LABEL. Each output below is listed with a small integer label in brackets, and the "i" field
must be that integer. The tool_use id is shown only so you can find the output in the conversation
above; do not put it in your reply.

Reply with ONLY a JSON array, one object per output, no prose:
[{"i": <label>, "needed_by": "a|b|c|none", "quote": "<verbatim text or empty>", "verdict": "keep|drop"}]`

// adjudicationEvidence teaches the model to read the `evidence:` field, and is appended to the contract
// ONLY when at least one item carries one.
//
// CONDITIONAL FOR A REASON. `main` shipped Evidence as an empty seam with the note that a prompt
// teaching the model to interpret counters the prompt never carries would be teaching it to read a
// field that does not exist. The converse is equally true and is why this text exists: counters carried
// without an explanation invite the model to invent a reading of them. Both failures are avoided by
// tying the paragraph to the data.
//
// IT FRAMES THE INDEX AS FALLIBLE, in its own words, on purpose. The index is an EXACT-MATCH
// backward-looking counter: it sees an identifier reappear verbatim and nothing else. Reuse in
// transformed form -- a number reformatted, a value paraphrased, a fact carried forward in the model's
// own prose -- is invisible to it, and that blind spot is precisely what the model is here to cover.
// Presenting the index's verdict as authoritative would collapse the mechanism into the pre-filter that
// starved three iterations; presenting it as a fallible witness is the only framing under which the
// model's disagreement is worth anything.
const adjudicationEvidence = `
HOW TO READ THE "evidence" FIELD. Each output may carry counters from a mechanical index that scanned
the conversation for LITERAL reappearances of the identifiers inside that output:
  novel            -- distinct identifiers this output introduced that nothing before it had.
  refs             -- how many later messages repeated any of them, character for character.
  ref_age          -- how long ago the most recent such repeat was, in messages.
  used_frac        -- the fraction of this output's identifiers that reappeared at all.
  later_turns      -- how many of your turns came after this output. A SMALL number means the output
                      has barely had the CHANCE to be referenced, so "refs=0" says nothing about it.
  verdict_of_index -- what the index concluded on its own.

THE INDEX IS A WITNESS, NOT A JUDGE, AND IT IS BLIND IN A SPECIFIC WAY: it matches text exactly. When
you used an output's information but wrote it differently -- reformatted a number, summarised a finding,
carried a fact forward in your own words -- the index recorded NOTHING, and "refs=0" is then evidence of
its blindness rather than of the output being spent. You can see that reuse and it cannot. Where you and
the index disagree, YOUR reading of the conversation decides. Treat high refs as corroboration that an
output is still live, and treat refs=0 as a question to answer from the conversation, never as an answer.
"anything the index missed" is not a hypothetical -- it is the normal case, and it is why you are asked.`

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
	if anyEvidence(items) {
		b.WriteString(adjudicationEvidence)
	}
	b.WriteString("\n\nThe conversation above is your own. Read the tool outputs from it directly.\n")
	b.WriteString("\nTOOL OUTPUTS UNDER CONSIDERATION. Refer to them by these labels only:\n")
	for _, it := range items {
		b.WriteString("  [")
		b.WriteString(strconv.Itoa(it.Label))
		b.WriteString("] ")
		b.WriteString(strconv.Itoa(it.SizeTokens))
		b.WriteString(" tokens")
		if it.ID != "" {
			b.WriteString(", tool_use id ")
			b.WriteString(it.ID)
		}
		if it.Evidence != "" {
			b.WriteString(", evidence: ")
			b.WriteString(it.Evidence)
		}
		b.WriteString(", begins: ")
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
	if anyEvidence(items) {
		b.WriteString(adjudicationEvidence)
	}
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
	// SCAN FOR AN ARRAY THAT ACTUALLY PARSES, rather than assuming the outermost brackets are it.
	//
	// This used to take the FIRST `[` to the LAST `]` and unmarshal the span. That works only when
	// nothing else in the reply contains a bracket, and a model asked to justify twelve verdicts does
	// not oblige. MEASURED against the real gateway on aws/claude-sonnet-5: three firings, three
	// failures to produce a usable verdict, one of them a 7,191-completion-token reply at 71.2s that
	// was NOT truncated -- it simply had prose around and between the JSON, so first-to-last spanned
	// reasoning text and unmarshal failed. Net effect: zero effective compactions on real traffic
	// while the counters said `sweep_unparseable`, which reads as "the prompt is wrong".
	//
	// So try each `[` in turn with a streaming decoder, which reads exactly one value and ignores
	// whatever follows. The first span that decodes to a verdict array wins. Cost is trivial next to
	// the model call that produced the reply.
	for i := 0; i < len(s); i++ {
		if s[i] != '[' {
			continue
		}
		var out []Verdict
		if err := json.NewDecoder(strings.NewReader(s[i:])).Decode(&out); err != nil {
			continue
		}
		// A decoded array is not automatically THE array. `[{}]` and `[{"note":"x"}]` both decode into
		// []Verdict cleanly, giving a phantom verdict for label 0 -- and label 0 is a REAL candidate,
		// so a phantom there would remove the wrong output. So an element has to look like a verdict
		// object before the array is believed.
		//
		// "Looks like one" means ANY of the verdict fields is populated, NOT specifically `verdict`.
		// That distinction is load-bearing and a stricter check got it wrong: a reply of
		// `[{"i":1,"needed_by":"none","quote":""}]` -- a real verdict object whose `verdict` field the
		// model omitted -- MUST parse, so the caller can classify it as an unusable verdict and
		// default to keep. Refusing to parse it would file a model that answered badly as a model
		// that could not be read, which are different failures with different remedies and are
		// separately counted downstream for exactly that reason.
		//
		// An EMPTY array is accepted as-is: the contract explicitly invites keep-everything, and
		// conflating that with junk is what made "the model declined to act" and "the model was never
		// successfully asked" the same number for three iterations (4ca1f13).
		if len(out) == 0 || looksLikeVerdict(out[0]) {
			return out, true
		}
	}
	// NEWLINE-DELIMITED FALLBACK, and it is a parser fix rather than a prompt one because the reply it
	// recovers is CORRECT. The contract says "Reply with ONLY a JSON array"; sonnet-5 sometimes answers
	// with one verdict object per line and no brackets at all — measured on a prefix ask whose four
	// verdicts were individually well-formed, correctly reasoned, and complete for the batch. The array
	// scan above finds no `[`, so the whole ask was discarded and its cost wasted.
	//
	// ACCEPTING MORE VALID ANSWERS IS NOT LOOSENING THE GUARDS. Every check the array path applies is
	// applied here: each line must decode into a Verdict and must satisfy looksLikeVerdict, so `{}` and
	// `{"note":"x"}` are still rejected rather than becoming phantom verdicts for label 0.
	//
	// EVERY LINE MUST PARSE, which is what keeps truncation distinguishable from malformation. A reply
	// cut off mid-line leaves a final fragment that does not decode; returning false there sends the
	// caller to ReplyWasTruncated, which handles the bracket-less case below. Accepting the good lines
	// and ignoring the fragment would report a partial batch as a complete judgement — the quiet
	// failure this package already guards against on the array path.
	return parseVerdictLines(s)
}

// parseVerdictLines reads a reply of one verdict object per line. Reports false unless EVERY non-empty
// line is a plausible verdict — see the fallback note in ParseVerdicts for why partial acceptance is
// worse than rejection here.
func parseVerdictLines(s string) ([]Verdict, bool) {
	var out []Verdict
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(ln), ","))
		if ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, "{") {
			return nil, false
		}
		var v Verdict
		if json.Unmarshal([]byte(ln), &v) != nil {
			return nil, false
		}
		if !looksLikeVerdict(v) {
			return nil, false
		}
		out = append(out, v)
	}
	// AT LEAST TWO LINES, and this is a deliberate conservatism rather than an arithmetic need.
	// `TestAnUnparseableReplyYieldsNoVerdicts` refuses a BARE OBJECT as "not an array", and that guard is
	// older than this fallback: a lone object is indistinguishable from the first line of an answer that
	// stopped, whereas two or more lines are evidence of a chosen format. Every single-verdict reply that
	// follows the contract is `[{...}]` and already parses on the array path, so the only thing refused
	// here is a lone bare object — and inventories of one are gated out by min_inventory long before the
	// ask. Revisit only if one-candidate asks ever become worth recovering, and change that test
	// deliberately if so rather than as a side effect.
	if len(out) < 2 {
		return nil, false
	}
	return out, true
}

// looksLikeVerdict reports whether a decoded element is plausibly a verdict object rather than an
// arbitrary JSON object that happened to decode into the struct.
//
// Deliberately generous: any populated verdict field counts. A malformed verdict is still a verdict
// and the caller has counters to classify it (missing criterion, unusable verdict, unknown label);
// what this rejects is an object with NONE of the fields, which carries no information at all and
// would otherwise become a phantom verdict for label 0.
//
// Label is not consulted, because 0 is both a real label and the zero value, so its presence cannot
// be told from its absence without decoding twice.
func looksLikeVerdict(v Verdict) bool {
	return v.Verdict != "" || v.NeededBy != "" || v.Quote != ""
}

// ReplyWasTruncated reports whether an unparseable reply looks CUT OFF rather than malformed.
//
// A reply that opened the array and never closed it ran out of output budget; one that never opened
// it is a genuine format failure. The remedies are opposite -- raise the budget versus fix the prompt
// -- so folding them under one name hid a 70%-of-calls failure behind a label that reads as "the
// prompt is wrong" (`659e7a6`). Only meaningful when ParseVerdicts returned false.
func ReplyWasTruncated(reply string) bool {
	if strings.Contains(reply, "[") {
		return !strings.Contains(reply, "]")
	}
	// THE BRACKET-LESS CASE, which exists because ParseVerdicts now also reads one verdict per line. A
	// newline-delimited reply cut off mid-object has no bracket to be missing, so the test above calls
	// it a format failure and points the operator at the prompt when the remedy is the token budget.
	// Read as truncation only when EARLIER lines did parse: a reply whose every line is malformed is a
	// format failure, and one that got several verdicts out before stopping ran out of room.
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	if len(lines) < 2 {
		return false
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" || !strings.HasPrefix(last, "{") {
		return false
	}
	var v Verdict
	if json.Unmarshal([]byte(last), &v) == nil {
		return false // the final line is whole; nothing was cut off
	}
	if _, ok := parseVerdictLines(strings.Join(lines[:len(lines)-1], "\n")); ok {
		return true
	}
	return false
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

// anyEvidence reports whether the paragraph explaining the evidence counters has anything to explain.
// Per-ASK rather than per-item: the contract is one block of text at the top, so a mixed inventory
// (some candidates below the index's floor, some above) still gets one explanation, which is also what
// makes "no index record" a readable line rather than an unexplained one.
func anyEvidence(items []AdjudicationItem) bool {
	for _, it := range items {
		if it.Evidence != "" {
			return true
		}
	}
	return false
}
