package apply

import (
	"sort"

	"github.com/rossoctl/accountant"
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
	at := p.positions(outMsgs, client)
	ins := make([]expand.Insertion, 0, len(p))
	tokens := 0
	for i, it := range p {
		text := expand.RestoredText(it.anchor.ID, it.original)
		ins = append(ins, expand.Insertion{At: at[i], Text: text})
		tokens += schema.TextTokens(text)
	}
	nb, ok := expand.InsertRestored(wire, out, ins)
	if !ok {
		return out, 0, false
	}
	return nb, tokens, true
}

// positions is where each item of the plan goes in outMsgs, the messages of the body the pipeline
// produced: the index of the message it is inserted before, in the plan's order. See insert for
// how a count-changing pipeline moves the anchors.
func (p restorePlan) positions(outMsgs, client []gjson.Result) []int {
	shift := len(client) - len(outMsgs)
	at := make([]int, len(p))
	for i, it := range p {
		at[i] = it.anchor.At - shift
		if shift != 0 && !(at[i] >= 1 && at[i] <= len(outMsgs) &&
			(it.anchor.At == len(client) || (at[i] < len(outMsgs) && outMsgs[at[i]].Raw == client[it.anchor.At].Raw))) {
			at[i] = len(outMsgs)
		}
	}
	return at
}

// ops reports the insertions to the accountant, one operation per restored copy. The copy is the
// LAST thing done to the request, after the pipeline, so its position is in the body the pipeline
// produced and the library moves the pipeline's own operations past it (design 4.3). Inserting in
// ascending order, the copy that ranks k-th lands at its position plus k, which is also where it
// sits in the final body nb. The sizes are the wire bytes of that message. The ChangeID is the
// marker id: the same on every request that restores it, so the library keeps the cost applied
// once (accountant case rewrite/fixed-restore-cost/hot/5m, #481). ok is false when a copy cannot be
// found in nb.
func (p restorePlan) ops(at []int, nb []gjson.Result) ([]accountant.Operation, bool) {
	order := make([]int, len(p))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return at[order[a]] < at[order[b]] })
	ops := make([]accountant.Operation, 0, len(p))
	for rank, i := range order {
		pos := at[i] + rank
		if pos < 0 || pos >= len(nb) {
			return nil, false
		}
		ops = append(ops, accountant.Operation{
			Kind:          accountant.KindRewrite,
			Operator:      "context-guru.expand_restore",
			At:            accountant.Position{Section: accountant.SectionMessages, Index: pos},
			SizeBefore:    accountant.Size{Bytes: 0},
			SizeAfter:     accountant.Size{Bytes: len(nb[pos].Raw)},
			ChangeID:      "cg:restore:" + p[i].anchor.ID,
			MessageSplice: &accountant.MessageSplice{At: pos, Removed: 0, Inserted: 1},
		})
	}
	return ops, true
}
