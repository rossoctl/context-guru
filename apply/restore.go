package apply

import (
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// Fixed restore (#407), the apply half: decide BEFORE the pipeline runs which expanded originals
// this turn restores at their anchors, so every offloader replays its compaction of them instead of
// honouring kept-verbatim (components.Ctx.Restored), then insert the restored copies into the body
// the pipeline produced. See expand/anchor.go for the mechanism and why it exists.
//
// The decision is all-or-nothing PER ANCHOR, and made up front, because the two halves must agree:
// an original kept compacted with no copy inserted is content the model cannot see, and a copy
// inserted next to an un-compacted original is the duplication #201 removed. Every reason an anchor
// cannot be honoured — no record, the stash gone, the transcript no longer matching — drops it from
// the plan before Ctx.Restored is built, and that original is kept verbatim in place exactly as
// before.

// restoreItem is one anchor this turn honours, with the original it restores.
type restoreItem struct {
	anchor   expand.Anchor
	original string
}

type restorePlan []restoreItem

// planRestore resolves this turn's anchors against msgs, the client's transcript. It also records
// an anchor for every expand call the client itself answered (the repair path) that has none yet.
func planRestore(pipe *components.Pipeline, st store.Store, session, wire string, body []byte, msgs []gjson.Result) restorePlan {
	if !pipe.FixedRestore() || session == "" || len(msgs) == 0 {
		return nil
	}
	if schema.IsResponses(wire) && gjson.GetBytes(body, "previous_response_id").String() != "" {
		return nil // the visible input is a delta, not the transcript an anchor is stated in
	}
	// The repair path: the client received our tool_use and answered it, so the call is in its
	// history and the turn that expanded is the message holding it. The anchor goes right before
	// that message. Skipped when the repaired answer IS the original — the content was not anywhere
	// else in the transcript, so that answer is the model's only copy and nothing is compacted.
	for _, cs := range expand.CallSites(wire, body) {
		if cs.At < 1 {
			continue
		}
		orig, ok := expand.Resolve(st, cs.ID)
		if !ok || cs.Answer == orig {
			continue
		}
		expand.RecordAnchor(st, session, expand.Anchor{ID: cs.ID, At: cs.At, Fp: expand.Fingerprint(msgs[cs.At-1])})
	}
	var plan restorePlan
	for _, a := range expand.Anchors(st, session) {
		if !a.Usable(wire, msgs) {
			continue
		}
		orig, ok := expand.Resolve(st, a.ID)
		if !ok || !expand.Restorable(wire, orig) {
			continue
		}
		plan = append(plan, restoreItem{anchor: a, original: orig})
	}
	return plan
}

// restored is the plan as components.Ctx.Restored: each original's content key to its marker id.
func (p restorePlan) restored() map[string]string {
	if len(p) == 0 {
		return nil
	}
	m := make(map[string]string, len(p))
	for _, it := range p {
		m[extract.ContentKey(it.original)] = it.anchor.ID
	}
	return m
}

// insert splices the restored copies into out, the body the pipeline produced, and returns the
// tokens they added.
//
// Anchors are in the CLIENT's coordinates. When the pipeline kept the message count, those are
// out's coordinates too. When it changed it (summarize replaced a span with one message), the span
// lies before every anchor — restoredSpanGuard keeps it ending before the original, which precedes
// its anchor — so each index moves by the same amount, and the message that follows the anchor
// must be byte-identical in both bodies to prove it. A mapping that cannot be proven appends the
// copy at the end instead: never byte-stable, but the model still gets the content it asked for,
// and the original it would otherwise be reading is compacted.
func (p restorePlan) insert(wire string, out []byte, client []gjson.Result) ([]byte, int, bool) {
	if len(p) == 0 {
		return out, 0, true
	}
	outMsgs := gjson.GetBytes(out, expand.MessagesField(wire)).Array()
	shift := len(client) - len(outMsgs)
	ins := make([]expand.Insertion, 0, len(p))
	tokens := 0
	for _, it := range p {
		at := it.anchor.At - shift
		if shift != 0 && !(at >= 1 && at <= len(outMsgs) &&
			(it.anchor.At == len(client) || (at < len(outMsgs) && outMsgs[at].Raw == client[it.anchor.At].Raw))) {
			at = len(outMsgs)
		}
		text := expand.RestoredText(it.anchor.ID, it.original)
		ins = append(ins, expand.Insertion{At: at, Text: text})
		tokens += schema.TextTokens(text)
	}
	nb, ok := expand.InsertRestored(wire, out, ins)
	if !ok {
		return out, 0, false
	}
	return nb, tokens, true
}
