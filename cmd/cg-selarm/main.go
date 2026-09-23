// cg-selarm runs one adjudication ARM against the rebuilt selection corpus and writes its decisions.
//
// WHY THIS IS GO AND NOT A SCRIPT. The thing under test is the SHIPPED prompt and the SHIPPED parser.
// A Python runner would re-implement extract.BuildPrefixAsk and extract.ParseVerdicts, and would then
// agree with my copy of them rather than with the code that runs in production — the same trap that
// made the adjudication dump's `outcome` field wrong until the real decision path was extracted into
// priceBatch. So this calls the real functions.
//
// SPENDING IS GATED THREE WAYS, because the corpus is large enough that a bug is expensive:
//
//	-dry        build every prompt, measure real token counts, project cost, make ZERO calls, and
//	            round-trip a synthetic reply through the real parser
//	-n N        run only the first N batches of a SEEDED sample, so a 30-batch slice is a strict
//	            subset of the 120-batch run and the two can be compared directly
//	-max-usd X  hard ceiling: refuse to start if the projection exceeds it, and stop mid-run if the
//	            measured spend does
//
// ARM B is arm A's prompt with ONE clause replaced, and the replacement is verified present before any
// call is made. If the clause has drifted, this exits rather than silently measuring two identical arms
// — which would read as "the change does nothing".
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/internal/cheapmodel"
	"github.com/rossoctl/context-guru/internal/extract"
)

// The clause under test, and its replacement. Exact text, so a drift in the contract is a loud failure.
const (
	clauseBefore = `If they all
look load-bearing, keep them all -- "keep everything" is a valid and often correct answer.`
	clauseAfter = `If they are all
genuinely load-bearing, keep them all -- that is a valid answer. But "keep everything" is not a way of
avoiding the comparison: if you cannot name, for each output, the specific written obligation that
needs its CONTENTS, it is spent.`
)

type cand struct {
	Conv     string         `json:"conv"`
	Decision int            `json:"decision_at"`
	MsgIdx   int            `json:"msg_idx"`
	ToolID   string         `json:"tool_id"`
	Tokens   int            `json:"tokens"`
	Content  string         `json:"content"`
	Evidence map[string]any `json:"evidence"`
}

type msg struct {
	I    int    `json:"i"`
	Role string `json:"role"`
	Text string `json:"text"`
}

type batch struct {
	Conv         string `json:"conv"`
	Decision     int    `json:"decision_at"`
	PrefixTokens int    `json:"prefix_tokens"`
	Transcript   []msg  `json:"transcript"`
	Candidates   []cand `json:"candidates"`
}

// replyRow is the raw model reply, kept alongside the verdicts so a judgement can be re-read against
// what the model actually said. Without it, "no verdicts" is unattributable.
type replyRow struct {
	Conv        string `json:"conv"`
	Decision    int    `json:"decision_at"`
	Batch       int    `json:"batch"`
	NCandidates int    `json:"n_candidates"`
	Reply       string `json:"reply"`
	Arm         string `json:"arm"`
}

// The four outcomes of asking a model to adjudicate a batch. They are separate constants rather than
// a bool because they have DIFFERENT REMEDIES: truncation means raise the token budget, unparseable
// means fix the prompt or the parser, partial means the model skipped candidates it was shown, and
// only replyOK is a measurement. Collapsing any two of them is how a run reported failures=0 while
// 17 of 30 batches produced no judgement.
const (
	replyOK          = "ok"
	replyPartial     = "partial"
	replyUnparseable = "unparseable"
	replyTruncated   = "truncated"
)

// classifyReply is the whole failure taxonomy in one place, so it can be tested without a network.
// It must not repair anything: a reply that does not parse is evidence, not an input to fix up.
func classifyReply(reply string, nCandidates int) ([]extract.Verdict, string) {
	vs, ok := extract.ParseVerdicts(reply)
	if !ok {
		// Truncation is a SUBSET of unparseable, checked second because it is the actionable case.
		if extract.ReplyWasTruncated(reply) {
			return nil, replyTruncated
		}
		return nil, replyUnparseable
	}
	// A parsed reply covering only part of the batch is the quiet failure: the candidates it omits
	// would read as deliberate keeps in any scorer that defaults absent to keep.
	if len(vs) != nCandidates {
		return vs, replyPartial
	}
	return vs, replyOK
}

type decision struct {
	Conv     string `json:"conv"`
	Decision int    `json:"decision_at"`
	MsgIdx   int    `json:"msg_idx"`
	ToolID   string `json:"tool_id"`
	Verdict  string `json:"verdict"` // drop | keep
	NeededBy string `json:"needed_by"`
	Quote    string `json:"quote"`
	Fabr     bool   `json:"quote_fabricated"`
	Refused  bool   `json:"refused_obligation"`
	Arm      string `json:"arm"`
}

// tok is the same 4-chars-per-token proxy the corpus builder and coref.py use. A proxy on purpose: it
// only has to be right enough to price a run before committing to it, and it is the SAME proxy the
// projection in the dry run reports, so the two cannot disagree.
func tok(s string) int { return len(s) / 4 }

func renderEvidence(e map[string]any) string {
	if len(e) == 0 {
		return ""
	}
	var b strings.Builder
	for _, k := range []string{"novel", "refs", "ref_age", "used_frac", "later_turns"} {
		if v, ok := e[k]; ok {
			fmt.Fprintf(&b, "%s=%v ", k, v)
		}
	}
	return strings.TrimSpace(b.String())
}

// buildAsk assembles the inventory question — the SHIPPED prompt, via extract.BuildPrefixAsk.
//
// This used to return the ask CONCATENATED ONTO the flattened transcript, as one user message, and that
// was a different question from the one production asks. Live, the transcript is the request's own
// `messages` array and the ask is a trailing user message (cheapmodel.CompletePrefixed,
// proxy/prefixask.go:115), so the model's prior outputs arrive in ASSISTANT role — they are its own
// turns. Flattened into one user message they become quoted text the user pasted, which changes three
// things at once:
//
//   - criterion (c) — "a step you have EXPLICITLY STATED you will take" — stops being a claim about
//     the model's own utterances and becomes a claim about a document.
//   - the contract's own sentence "The conversation above is your own. Read the tool outputs from it
//     directly" becomes false.
//   - "above" changes referent, from prior messages to earlier bytes of the same message.
//
// The measured symptom: the flattened harness dropped 85% of candidates on ~8-candidate batches while
// the live component dropped 33% and iteration 024 dropped 43% at comparable inventory. A harness that
// overstates drop willingness is useless for asking whether a drop is worthwhile.
func buildAsk(b batch, arm string) (string, error) {
	items := make([]extract.AdjudicationItem, 0, len(b.Candidates))
	for i, c := range b.Candidates {
		items = append(items, extract.AdjudicationItem{
			Label:      i,
			ID:         c.ToolID,
			SizeTokens: c.Tokens,
			Head:       extract.HeadLine(c.Content, extract.AdjudicationHeadChars),
			Evidence:   renderEvidence(c.Evidence),
		})
	}
	ask := extract.BuildPrefixAsk(items) // THE SHIPPED PROMPT
	if arm == "B" {
		if !strings.Contains(ask, clauseBefore) {
			return "", fmt.Errorf("arm B: the clause under test is not in the shipped contract any more.\n"+
				"Looking for:\n%s\n\nRefusing to run: without the replacement the two arms are identical "+
				"and the result would read as \"the change does nothing\"", clauseBefore)
		}
		ask = strings.Replace(ask, clauseBefore, clauseAfter, 1)
	}
	return ask, nil
}

// buildPrefixBody renders the decision point as a request body whose `messages` array carries the
// conversation, so CompletePrefixed can append the ask exactly as production does.
//
// WHAT THIS CANNOT REPRODUCE, stated because it bounds every number the tool reports: the corpus
// records each message as a role and a flat string, not as content blocks, so tool results arrive here
// as user-role text rather than as `tool_result` blocks inside a user message, and assistant tool calls
// lose their `tool_use` blocks. Role attribution is faithful; block structure is not. That is a strict
// improvement on one flattened user message and still not the live request.
//
// Consecutive same-role messages are MERGED. The provider accepts them, but merging keeps the turn
// count meaningful: an agent trace maps many tool results onto one logical user turn, and leaving them
// split would make the conversation look like dozens of user turns to a model being asked which of its
// own turns are spent.
func buildPrefixBody(b batch, model string) ([]byte, error) {
	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var msgs []msg
	for _, m := range b.Transcript {
		role := "user"
		if m.Role == "assistant" {
			role = "assistant"
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content += "\n\n" + m.Text
			continue
		}
		msgs = append(msgs, msg{Role: role, Content: m.Text})
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("decision point has no transcript")
	}
	return json.Marshal(map[string]any{"model": model, "messages": msgs})
}

func main() {
	corpus := flag.String("corpus", "", "batches.jsonl")
	out := flag.String("out", "", "decisions.jsonl")
	arm := flag.String("arm", "A", "A (shipped contract) | B (clause replaced)")
	n := flag.Int("n", 0, "run only the first N batches of the seeded sample (0 = all)")
	seed := flag.Int64("seed", 27, "sample seed; the same seed makes a small run a subset of a larger one")
	dry := flag.Bool("dry", false, "build prompts, measure, project cost, make no calls")
	maxUSD := flag.Float64("max-usd", 0, "refuse to start above this projection; stop if measured spend exceeds it")
	inRate := flag.Float64("in-rate", 3.0, "USD per million input tokens")
	outRate := flag.Float64("out-rate", 15.0, "USD per million output tokens")
	replyOut := flag.String("replies", "", "write every raw model reply here as JSONL (strongly recommended: "+
		"an unparseable reply is the only evidence of WHY a batch produced no judgement)")
	minCands := flag.Int("min-candidates", 0, "skip decision points carrying fewer than N candidates. "+
		"10 selects the regime the component's own measurements call sound (batch 10 cleared 100% of "+
		"genuinely-spent candidates; batch 3-6 dropped one 2 times in 4) and matches min_inventory")
	flag.Parse()

	raw, err := os.ReadFile(*corpus)
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus:", err)
		os.Exit(2)
	}
	var all []batch
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var b batch
		if err := json.Unmarshal([]byte(l), &b); err != nil {
			fmt.Fprintln(os.Stderr, "corpus line:", err)
			os.Exit(2)
		}
		all = append(all, b)
	}
	// INVENTORY FILTER BEFORE THE SHUFFLE, so a seeded slice of the rich population is a strict prefix
	// of a larger slice of the SAME population. Filtering after the shuffle would make -n 60 and -n 169
	// different samples and the two runs incomparable.
	if *minCands > 0 {
		kept := all[:0]
		for _, b := range all {
			if len(b.Candidates) >= *minCands {
				kept = append(kept, b)
			}
		}
		fmt.Printf("inventory filter: %d of %d decision points carry >= %d candidates\n",
			len(kept), len(all), *minCands)
		all = kept
		if len(all) == 0 {
			fmt.Fprintln(os.Stderr, "no decision point meets -min-candidates; nothing to measure")
			os.Exit(3)
		}
	}

	// SEEDED SAMPLE, so -n 30 is a strict prefix of -n 120 and the slice can be compared with the rest.
	r := rand.New(rand.NewSource(*seed))
	r.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if *n > 0 && *n < len(all) {
		all = all[:*n]
	}

	// Build every prompt FIRST, so a prompt-assembly failure costs nothing. Two parts now, matching
	// the live split: the conversation as a request body, and the ask as the trailing user message
	// CompletePrefixed appends to it.
	asks := make([]string, len(all))
	bodies := make([][]byte, len(all))
	inTok := 0
	for i, b := range all {
		a, err := buildAsk(b, *arm)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		body, err := buildPrefixBody(b, os.Getenv("CG_ARM_MODEL"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		asks[i], bodies[i] = a, body
		inTok += tok(a) + tok(string(body))
	}
	nCand := 0
	for _, b := range all {
		nCand += len(b.Candidates)
	}
	// Reply budget: ~40 tokens per verdict is generous for {"i":N,"needed_by":"x","quote":"...","verdict":"y"}.
	estOut := nCand * 60
	proj := float64(inTok)/1e6**inRate + float64(estOut)/1e6**outRate
	fmt.Printf("arm %s: %d batches, %d candidates\n", *arm, len(all), nCand)
	fmt.Printf("  input  %d tokens (measured on the assembled prompts)\n", inTok)
	fmt.Printf("  output %d tokens (estimated at 60/verdict)\n", estOut)
	fmt.Printf("  PROJECTED COST $%.2f  (in $%.2f/M, out $%.2f/M)\n", proj, *inRate, *outRate)

	if *maxUSD > 0 && proj > *maxUSD {
		fmt.Fprintf(os.Stderr, "REFUSING: projection $%.2f exceeds -max-usd $%.2f\n", proj, *maxUSD)
		os.Exit(4)
	}

	if *dry {
		// PARSER ROUND-TRIP on a synthetic reply, so a parsing bug is found for free rather than after
		// paying for 120 replies the runner cannot read.
		b0 := all[0]
		var syn strings.Builder
		syn.WriteString("[")
		for i := range b0.Candidates {
			if i > 0 {
				syn.WriteString(",")
			}
			fmt.Fprintf(&syn, `{"i":%d,"needed_by":"none","quote":"","verdict":"drop"}`, i)
		}
		syn.WriteString("]")
		vs, ok := extract.ParseVerdicts(syn.String())
		fmt.Printf("\n  parser round-trip on a synthetic reply: parsed=%v verdicts=%d (want %d)\n",
			ok, len(vs), len(b0.Candidates))
		if !ok || len(vs) != len(b0.Candidates) {
			fmt.Fprintln(os.Stderr, "PARSER ROUND-TRIP FAILED -- do not spend")
			os.Exit(5)
		}
		flat := flatten(b0)
		a := extract.Judge(vs[0], flat)
		fmt.Printf("  judge round-trip: drop=%v fabricated=%v criterionMissing=%v\n",
			a.Drop, a.QuoteFabricated, a.CriterionMissing)
		// SHAPE ASSERTIONS, so a dry run proves the request is the live shape rather than merely
		// building. Checked here because the failure they catch — the transcript leaking into the ask,
		// or the conversation collapsing to one message — is silent on the wire and expensive to find
		// after spending.
		nMsgs := len(gjson.GetBytes(bodies[0], "messages").Array())
		fmt.Printf("  prefix body: %d messages, %d bytes | ask: %d bytes\n",
			nMsgs, len(bodies[0]), len(asks[0]))
		if nMsgs < 2 {
			fmt.Fprintln(os.Stderr, "SHAPE FAILURE: the conversation collapsed to fewer than 2 messages; "+
				"this is the flattened prompt the fix exists to remove")
			os.Exit(6)
		}
		roles := map[string]int{}
		for _, m := range gjson.GetBytes(bodies[0], "messages").Array() {
			roles[m.Get("role").String()]++
		}
		fmt.Printf("  roles in the conversation: %v (assistant>0 is what makes criterion (c) meaningful)\n", roles)
		if roles["assistant"] == 0 {
			fmt.Fprintln(os.Stderr, "SHAPE WARNING: no assistant-role messages; the model is not being "+
				"shown its own turns and criterion (a)/(c) cannot be answered as intended")
		}
		if len(b0.Transcript) > 0 && strings.Contains(asks[0], b0.Transcript[0].Text) {
			fmt.Fprintln(os.Stderr, "SHAPE FAILURE: transcript text found inside the ask; the ask must "+
				"carry only the inventory")
			os.Exit(6)
		}
		fmt.Printf("  ask[0] first 200 chars: %.200s...\n", asks[0])
		fmt.Printf("  arm %s clause present in ask[0]: %v\n", *arm,
			strings.Contains(asks[0], map[string]string{"A": clauseBefore, "B": clauseAfter}[*arm]))
		fmt.Println("\nDRY RUN ONLY -- no calls made, nothing spent.")
		return
	}

	model := cheapmodel.Anthropic{
		BaseURL:    os.Getenv("CG_ARM_BASE"),
		APIKey:     os.Getenv("CG_ARM_KEY"),
		Model:      os.Getenv("CG_ARM_MODEL"),
		AuthScheme: os.Getenv("CG_ARM_AUTH"),
		// PrefixAskMaxTokens is what production gives an adjudication (16,000). The 4,096 that stood
		// here was this tool's own invention and a smaller budget than the code under test uses, which
		// would have made truncation a property of the harness.
		MaxTokens: cheapmodel.PrefixAskMaxTokens,
	}
	if model.APIKey == "" || model.Model == "" {
		fmt.Fprintln(os.Stderr, "CG_ARM_KEY and CG_ARM_MODEL are required for a live run")
		os.Exit(2)
	}
	fh, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer fh.Close()

	// Reply capture is opt-out rather than opt-in: the run is expensive and the replies are the
	// diagnostic. -replies "" disables it.
	var replies *os.File
	if *replyOut != "" {
		replies, err = os.Create(*replyOut)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer replies.Close()
	}

	spentIn, spentOut, failed := 0, 0, 0
	unparseable, truncated, partial := 0, 0, 0
	for i, b := range all {
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		reply, _, err := model.CompletePrefixed(ctx, bodies[i], asks[i])
		cancel()
		spentIn += tok(asks[i]) + tok(string(bodies[i]))
		spentOut += tok(reply)
		if err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "  batch %d/%d FAILED: %v\n", i+1, len(all), err)
			if failed > 5 {
				fmt.Fprintln(os.Stderr, "STOPPING: more than five failures; not spending further")
				break
			}
			continue
		}
		flat := flatten(b)
		// CAPTURE THE RAW REPLY BEFORE PARSING IT. The first run of this tool discarded `reply`, so
		// when 17 of 30 batches produced no verdicts there was no way to tell a refusal from a
		// truncation from a prose-wrapped array -- three faults with three different remedies. The
		// replies are the only evidence of which one happened, and they cost real money to obtain.
		if replies != nil {
			rj, _ := json.Marshal(replyRow{Conv: b.Candidates[0].Conv, Decision: b.Candidates[0].Decision,
				Batch: i, NCandidates: len(b.Candidates), Reply: reply, Arm: *arm})
			replies.Write(append(rj, '\n'))
		}
		// HONOUR THE ok FLAG. `ParseVerdicts` documents at its definition why folding "parsed to no
		// verdicts" together with "did not parse" is wrong: it makes a deliberate keep-all
		// indistinguishable from junk, and those are the two things an arm has to separate. Dropping
		// the flag here reported failures=0 on a run where more than half the batches never produced
		// a judgement at all.
		vs, kind := classifyReply(reply, len(b.Candidates))
		switch kind {
		case replyUnparseable, replyTruncated:
			unparseable++
			if kind == replyTruncated {
				truncated++
			}
			fmt.Fprintf(os.Stderr, "  batch %d/%d UNPARSEABLE (truncated=%v, %d reply chars); not counted as a decision\n",
				i+1, len(all), kind == replyTruncated, len(reply))
			continue
		case replyPartial:
			partial++
			fmt.Fprintf(os.Stderr, "  batch %d/%d PARTIAL: %d verdicts for %d candidates\n",
				i+1, len(all), len(vs), len(b.Candidates))
		}
		seen := map[int]bool{}
		for _, v := range vs {
			if v.Label < 0 || v.Label >= len(b.Candidates) || seen[v.Label] {
				continue
			}
			seen[v.Label] = true
			a := extract.Judge(v, flat)
			c := b.Candidates[v.Label]
			verdict := "keep"
			if a.Drop {
				verdict = "drop"
			}
			row := decision{Conv: c.Conv, Decision: c.Decision, MsgIdx: c.MsgIdx, ToolID: c.ToolID,
				Verdict: verdict, NeededBy: v.NeededBy, Quote: v.Quote,
				Fabr: a.QuoteFabricated, Refused: a.RefusedObligation, Arm: *arm}
			j, _ := json.Marshal(row)
			fh.Write(append(j, '\n'))
		}
		cost := float64(spentIn)/1e6**inRate + float64(spentOut)/1e6**outRate
		if (i+1)%10 == 0 || i == len(all)-1 {
			fmt.Printf("  %d/%d batches, %d verdicts this batch, running cost $%.2f\n",
				i+1, len(all), len(vs), cost)
		}
		if *maxUSD > 0 && cost > *maxUSD {
			fmt.Fprintf(os.Stderr, "STOPPING at batch %d: measured $%.2f exceeded -max-usd $%.2f\n",
				i+1, cost, *maxUSD)
			break
		}
	}
	// Report every failure mode SEPARATELY. A single "failures" number that reads 0 while half the
	// batches produced nothing is worse than no number, because it invites the reader to treat the
	// coverage as complete.
	fmt.Printf("arm %s done: in=%d out=%d tokens, cost $%.2f -> %s\n", *arm,
		spentIn, spentOut, float64(spentIn)/1e6**inRate+float64(spentOut)/1e6**outRate, *out)
	fmt.Printf("  batches %d | call errors %d | unparseable %d (of which truncated %d) | partial %d\n",
		len(all), failed, unparseable, truncated, partial)
	decided := len(all) - failed - unparseable
	fmt.Printf("  batches that produced a judgement: %d of %d (%.0f%%)\n",
		decided, len(all), 100*float64(decided)/float64(max(1, len(all))))
	if decided < len(all) {
		fmt.Fprintf(os.Stderr, "WARNING: coverage is incomplete. Candidates in the missing batches are\n"+
			"NOT keeps -- they are unmeasured, and a scorer that defaults absent to keep will\n"+
			"silently score them. Pair arms on the INTERSECTION of what both decided.\n")
	}
}

// flatten is the transcript as one string, which is what Judge checks an obligation quote against.
func flatten(b batch) string {
	var sb strings.Builder
	for _, m := range b.Transcript {
		sb.WriteString(m.Text)
		sb.WriteString("\n")
	}
	return sb.String()
}
