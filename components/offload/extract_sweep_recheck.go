package offload

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/coref"
	"github.com/rossoctl/context-guru/schema"
)

// THE RECHECK (#404, point 6).
//
// A held answer was true of one conversation at one moment. By the time a cold request arrives the
// agent may have rewound, compacted, edited an earlier turn, re-read the file the output came from,
// or expanded the output back. RecheckHeld says which of the model's drop verdicts still stand on the
// request in hand.
//
// It is built as a function of (context, request, held answer) with no side effects, so the cold
// apply (PR C) calls it on the cold request and shadow mode calls it to report. Both get the same
// verdicts for the same inputs.
//
// EVERY VERDICT IS DROPPED FOR EXACTLY ONE REASON, checked in the order below. A reason that fires
// stops the checks for that verdict, so the counts add up to the number of drop verdicts.

// Why a drop verdict was dropped. These are the suffixes of the sweep_shadow_dropped_* events.
const (
	// RecheckConversationShorter: the request has fewer messages than when the question was asked.
	// The agent rewound, ran /compact or /clear. Every verdict is void.
	RecheckConversationShorter = "conversation_shorter"
	// RecheckOutputGone: no tool message with the judged id is in the request any more.
	RecheckOutputGone = "output_gone"
	// RecheckContentChanged: the message with that id is there but its bytes differ from the ones
	// that were judged.
	RecheckContentChanged = "content_changed"
	// RecheckReferenced: a turn AFTER the question was asked reused something the output introduced.
	// The model judged before that turn existed.
	RecheckReferenced = "referenced_later"
	// RecheckExpanded: the agent asked for this content back (it is marked kept-verbatim, or a fixed
	// restore anchors it). Dropping it again would only trigger another expansion.
	RecheckExpanded = "expanded"
)

// recheckDropped is one verdict the recheck turned down, with the reason.
type recheckDropped struct {
	ToolUseID string
	Reason    string
	Saves     int
}

// recheckResult is the recheck's outcome. Valid holds the drop verdicts that still stand; Dropped
// holds the ones that do not. Kept verdicts (Drop == false) are in neither: they were never going to
// be applied.
type recheckResult struct {
	Valid   []heldOutput
	Dropped []recheckDropped
	// novel holds, per tool id, the identifiers each valid output introduced. The cold apply keeps
	// their hashes to see a later reference (the referenced_later quality signal).
	novel map[string][]string
}

// reasonCounts tallies Dropped by reason.
func (r recheckResult) reasonCounts() map[string]int {
	out := map[string]int{}
	for _, d := range r.Dropped {
		out[d.Reason]++
	}
	return out
}

// validTokens is what the surviving drops would remove.
func (r recheckResult) validTokens() int {
	n := 0
	for _, o := range r.Valid {
		n += o.Saves
	}
	return n
}

// recheckHeld checks a held answer against the request in hand.
//
// req is the request as the pipeline holds it (the expanded check reads the content the other
// components see); c.RawInput, when set, is the request as the agent SENT it, which is what the hash
// and the length are measured on. Without RawInput both come from req, which is right whenever no
// earlier component changed it, and is what the unit tests in this package use.
//
// minTokens is the co-reference index's output floor (the sweep's own min_tokens). An output below it
// has no co-reference record and so cannot be judged "referenced"; the model's answer stands.
//
// Fail-safe direction: every doubt drops a verdict. A wrongly dropped verdict loses one removal; a
// wrongly kept one removes content the agent still needs.
func recheckHeld(c *components.Ctx, req *bschemas.BifrostChatRequest, h heldAnswer, minTokens int) recheckResult {
	var res recheckResult
	raw := req.Input
	if c != nil && c.RawInput != nil {
		raw = c.RawInput
	}
	drops := make([]heldOutput, 0, len(h.Outputs))
	for _, o := range h.Outputs {
		if o.Drop {
			drops = append(drops, o)
		}
	}
	if len(drops) == 0 {
		return res
	}
	if len(raw) < h.Judged {
		for _, o := range drops {
			res.Dropped = append(res.Dropped, recheckDropped{o.ToolUseID, RecheckConversationShorter, o.Saves})
		}
		return res
	}
	// One co-reference pass over the agent's own conversation, indexed by tool id. Run on raw, not on
	// req, so the record's message indexes and h.Judged count the same messages.
	refd := map[string]coref.Record{}
	for _, r := range coref.IndexNovel(flattenForCoref(&bschemas.BifrostChatRequest{Input: raw}), minTokens, schema.TextTokens) {
		refd[r.ID] = r
	}
	res.novel = map[string][]string{}
	for _, o := range drops {
		idx, ok := rawToolMessage(raw, o.ToolUseID)
		if !ok {
			res.Dropped = append(res.Dropped, recheckDropped{o.ToolUseID, RecheckOutputGone, o.Saves})
			continue
		}
		if outputHash(raw[idx]) != o.Hash {
			res.Dropped = append(res.Dropped, recheckDropped{o.ToolUseID, RecheckContentChanged, o.Saves})
			continue
		}
		// A reference counts only if it came AFTER the question: the model already saw the earlier
		// ones. The last reference sits Idx+ConsumeLag messages in; ConsumeLag is -1 for none.
		if r, ok := refd[o.ToolUseID]; ok && r.ConsumeLag >= 0 && r.Idx+r.ConsumeLag >= h.Judged {
			res.Dropped = append(res.Dropped, recheckDropped{o.ToolUseID, RecheckReferenced, o.Saves})
			continue
		}
		if c != nil && c.Store != nil && recheckExpanded(c, req, o.ToolUseID) {
			res.Dropped = append(res.Dropped, recheckDropped{o.ToolUseID, RecheckExpanded, o.Saves})
			continue
		}
		res.Valid = append(res.Valid, o)
		if r, ok := refd[o.ToolUseID]; ok && len(r.NovelIDs) > 0 {
			res.novel[o.ToolUseID] = r.NovelIDs
		}
	}
	return res
}

// recheckExpanded reports whether the agent expanded this output back. Same test the collection loop
// uses (isKeptVerbatim), on the content the pipeline holds for the message, plus the fixed-restore
// anchors the host set for this request.
func recheckExpanded(c *components.Ctx, req *bschemas.BifrostChatRequest, toolID string) bool {
	idx, ok := rawToolMessage(req.Input, toolID)
	if !ok {
		return false
	}
	text := schema.MessageText(req.Input[idx])
	if text == "" {
		return false
	}
	if isKeptVerbatim(c, contentKey(text)) {
		return true
	}
	return restoredSpanGuard(c, text)
}
