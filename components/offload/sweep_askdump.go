package offload

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
)

// WHY THIS EXISTS, in one sentence: the component pays a model to make a judgement and recorded only
// how many times it said yes.
//
// `sweep_kept: 32` out of 34 adjudicated was the entire record of one probe's adjudication. Which
// candidate the model kept, what it claimed the output was still needed by, which quote it anchored on,
// and — decisively — WHAT IT WAS SHOWN when it decided, were all absent. So the two questions anyone
// actually has about this component were unanswerable from any iteration's artifacts, including every
// run already completed:
//
//	was the judgement RIGHT?          needs the transcript the model saw, beside its verdict
//	is the PROMPT the problem?        needs the prompt as sent, not as intended
//
// Neither is recoverable afterwards. A prefix ask appends the inventory to the provider's CACHED
// transcript, so nothing in the outgoing HTTP body contains the conversation the model reasoned over —
// the rig's own response capture holds a response head, and the debug log holds two summary rows. The
// only place both halves exist together is here, in the process, at the moment of the call.
//
// OFF BY DEFAULT AND OPT-IN BY PATH. This writes FULL PROMPT AND TRANSCRIPT CONTENT to disk in the
// clear, which is exactly what makes it useful and exactly why it must never be on by accident: on a
// real workload those bytes are the user's conversation. It is a bench instrument for a controlled
// eval box, not an operational feature, and nothing about it is reachable without an operator naming a
// directory:
//
//	CG_SWEEP_ASK_DUMP=/path/to/dir
//
// Failures are swallowed. A full disk or an unwritable path must degrade to "no dump", never to a
// failed adjudication — the component's job is not to produce diagnostics.
type sweepAskDump struct {
	Time    string `json:"time"`
	Session string `json:"session"`
	Seq     int64  `json:"seq"`
	Model   string `json:"model"`

	// Econ is the decision that authorised paying for this ask, carried alongside so a judgement can be
	// read against the economics that bought it rather than in isolation.
	//
	// A POINTER, so it is ABSENT rather than zero-valued when there was no econ decision. The pre-expiry
	// trigger fires on the clock and never evaluates the break-even, so on those asks there is no need,
	// no have and no premium — and a zero-valued block would read as "the premium was 0", which is a
	// different and wrong statement. Absent-versus-zero has produced three separate misreadings in this
	// component's history; this is the cheap place to refuse the ambiguity.
	Econ *sweepAskDumpEcon `json:"econ,omitempty"`
	// Trigger names which one fired, so an absent Econ block is explained rather than merely empty.
	Trigger string `json:"trigger"`

	// AskPrompt is the text appended to the cached prefix — verbatim, as sent. The prompt under
	// review, not a reconstruction of it.
	AskPrompt string `json:"ask_prompt"`
	// Inventory is what the prompt offered, one entry per candidate, with the label the model answers
	// by and the FULL content it was being asked to judge.
	Inventory []sweepAskDumpCand `json:"inventory"`
	// Transcript is the conversation as the pipeline had left it when the ask was made — which is what
	// the provider serves from cache, and therefore what the model reasoned over. Without it a verdict
	// cannot be called right or wrong.
	Transcript []sweepAskDumpMsg `json:"transcript"`

	// Reply is the raw answer. ViaTool records how it arrived; both parse identically, so a difference
	// here is not a difference in outcome.
	Reply   string `json:"reply"`
	ViaTool bool   `json:"via_tool"`
	Parsed  bool   `json:"parsed"`

	// Verdicts is the model's answer per candidate, plus what validation made of it. `Outcome` is what
	// the component DID, which is not always what the model said — a self-contradictory drop leaves the
	// output in place, and that divergence is the interesting row.
	Verdicts []sweepAskDumpVerdict `json:"verdicts"`
}

type sweepAskDumpEcon struct {
	NeedTurns int     `json:"need_turns"`
	HaveTurns int     `json:"have_turns"`
	Premium   float64 `json:"premium"`
	Approval  float64 `json:"approval"`
	AskUSD    float64 `json:"ask_usd"`
	ReqTokens int     `json:"req_tokens"`
	Pressure  float64 `json:"pressure"`
	Offered   int     `json:"offered_tokens"`
}

type sweepAskDumpCand struct {
	Label   int    `json:"label"`
	ToolID  string `json:"tool_id"`
	Index   int    `json:"index"`
	Tokens  int    `json:"tokens"`
	Content string `json:"content"`
	// Evidence is the co-reference index's statement about this candidate, as the inventory line
	// carried it. Included because a verdict that contradicts the evidence and one that follows it are
	// different findings about the prompt, and the line is what the model actually read.
	Evidence string `json:"evidence,omitempty"`
}

type sweepAskDumpMsg struct {
	Index  int    `json:"index"`
	Role   string `json:"role"`
	Tokens int    `json:"tokens"`
	Text   string `json:"text"`
}

type sweepAskDumpVerdict struct {
	Label    int    `json:"label"`
	Drop     bool   `json:"model_said_drop"`
	NeededBy string `json:"needed_by"`
	Quote    string `json:"quote"`
	// The validation flags, named as the questions they answer rather than as booleans.
	QuoteFabricated   bool `json:"quote_fabricated"`
	CriterionMissing  bool `json:"criterion_missing"`
	VerdictUnusable   bool `json:"verdict_unusable"`
	RefusedObligation bool `json:"refused_obligation"`
	// Outcome is the component's action: dropped | kept | refused_obligation | unknown_label |
	// duplicate_label | would_not_shrink.
	Outcome string `json:"outcome"`
}

var sweepAskDumpSeq atomic.Int64

// sweepAskDumpDir returns the configured directory, or "" when dumping is off. Read per call rather
// than cached, so switching it on does not require a restart on a bench box.
func sweepAskDumpDir() string { return os.Getenv("CG_SWEEP_ASK_DUMP") }

// writeSweepAskDump persists one adjudication in full. Best effort by construction: every error path
// returns silently after at most one warning, because a diagnostic that can fail a request is worse
// than no diagnostic.
func writeSweepAskDump(dir string, d *sweepAskDump) {
	if dir == "" || d == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil { // 0700: this file holds conversation content
		slog.Warn("cg.sweep.ask_dump: directory unusable, dumping disabled for this call",
			"dir", dir, "err", err)
		return
	}
	name := fmt.Sprintf("ask-%s-%03d.json", d.Session, d.Seq)
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		slog.Warn("cg.sweep.ask_dump: could not encode", "err", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		slog.Warn("cg.sweep.ask_dump: could not write", "file", name, "err", err)
		return
	}
	slog.Debug("cg.sweep.ask_dump", "file", name, "candidates", len(d.Inventory),
		"verdicts", len(d.Verdicts), "bytes", len(b), "session", d.Session)
}

// newSweepAskDump captures everything known BEFORE the reply — the prompt, the inventory and the
// transcript. Split from the verdict half because the ask can fail, and a dump of a failed ask (prompt
// present, reply empty) is exactly as diagnostic as a successful one.
func newSweepAskDump(req *bschemas.BifrostChatRequest, session, model string,
	items []extract.AdjudicationItem, cands []sweepCand, prompt string) *sweepAskDump {

	d := &sweepAskDump{
		Time:      time.Now().UTC().Format(time.RFC3339Nano),
		Session:   session,
		Seq:       sweepAskDumpSeq.Add(1),
		Model:     model,
		AskPrompt: prompt,
	}
	for _, it := range items {
		if it.Label < 0 || it.Label >= len(cands) {
			continue
		}
		cd := cands[it.Label]
		d.Inventory = append(d.Inventory, sweepAskDumpCand{
			Label: it.Label, ToolID: cd.toolID, Index: cd.i,
			Tokens: schema.TextTokens(cd.content), Content: cd.content,
			Evidence: it.Evidence,
		})
	}
	if req != nil {
		for i := range req.Input {
			text := schema.MessageText(req.Input[i])
			d.Transcript = append(d.Transcript, sweepAskDumpMsg{
				Index: i, Role: string(req.Input[i].Role),
				Tokens: schema.TextTokens(text), Text: text,
			})
		}
	}
	return d
}
