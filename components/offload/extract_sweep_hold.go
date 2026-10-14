package offload

import (
	"encoding/json"
	"strings"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// THE HELD ANSWER (#404, points 3 and 5).
//
// The sweep asks its question at the keep-alive ping and gets verdicts back: drop this output, keep
// that one. Applying a drop while the cache is warm rewrites the cache (#491 records why that almost
// never pays), so the answer is HELD, per thread, until a cold request arrives, when the whole
// conversation is written anyway and a drop makes that write smaller.
//
// THIS PR ONLY HOLDS. Nothing reads a held answer to change a request. The record lives under its own
// store namespace (store.SweepHeldPrefix) and not under cg:res:, because the same-session replay in
// ExtractSweep.Offload reads cg:res: and splices what it finds at ANY depth: a verdict stored there
// would be applied on the very next request, warm or cold, and would force the rewrite this design
// exists to avoid. TestAHeldAnswerChangesNoRequest pins that.
//
// WHAT IS KEPT PER JUDGED OUTPUT is its tool_use id and a content hash of the message as the AGENT
// sent it (components.SpanHash over the raw message, #431). The id finds the output again on a later
// request wherever it has moved to; the hash says whether it is still the same bytes. The message
// index is deliberately not kept: an earlier component that changes the message count (#452) moves
// every index, and an id does not move.

// heldVersion is bumped when heldAnswer changes shape. A record of another version reads as absent,
// so an old process's answer is never interpreted by a new one.
const heldVersion = 1

// heldOutput is one output the adjudicator was asked about.
type heldOutput struct {
	// ToolUseID is the wire's id for the tool call. Always set: an output with no id cannot be found
	// again, so it is not held (counted as sweep_hold_no_tool_id).
	ToolUseID string `json:"id"`
	// Hash is components.SpanHash of the tool message in the agent's own request.
	Hash string `json:"hash"`
	// Tokens is the output's own size; Saves is what dropping it would remove (the size minus the
	// descriptor that replaces it). Both from schema.TextTokens, the sweep's own ruler.
	Tokens int `json:"tokens"`
	Saves  int `json:"saves"`
	// Drop is the model's verdict after Judge: the quote checks and the would-it-shrink test have
	// already run. A kept output is recorded too, so "asked" and "proposed" are both countable.
	Drop bool `json:"drop"`
}

// heldAnswer is one thread's held answer.
type heldAnswer struct {
	V int `json:"v"`
	// Thread is thread.Key(session, thread), the same key the keep-alive, the prefix stash and the
	// per-thread cold verdict (#487) use.
	Thread    string `json:"thread"`
	AskedAtMs int64  `json:"asked_at_ms"`
	Model     string `json:"model,omitempty"`
	// Judged is the length of the agent's conversation when the question was asked. A later request
	// shorter than this is a different conversation (a rewind, /compact, /clear) and the answer is
	// void: see RecheckConversationShorter.
	Judged  int          `json:"judged_msgs"`
	Outputs []heldOutput `json:"outputs"`
	// WaitingFor and WaitTool are the thread's wait cause (#453) when the question was asked: what
	// the thread was waiting on. STORED ONLY. Nothing here uses it to decide; it is an input to the
	// warm-path gate in #491.
	WaitingFor string  `json:"waiting_for,omitempty"`
	WaitTool   string  `json:"wait_tool,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	// Reported is set once a request has reported the ask in shadow mode, so two requests do not
	// count one question twice.
	Reported bool `json:"reported,omitempty"`
	// Spent marks a consumed answer. store.Store has no Delete, so a consumed record is overwritten
	// with a tombstone and reads as absent.
	Spent bool `json:"spent,omitempty"`
}

// proposed counts the drops the model voted for and the tokens they would remove.
func (h heldAnswer) proposed() (n, tokens int) {
	for _, o := range h.Outputs {
		if o.Drop {
			n++
			tokens += o.Saves
		}
	}
	return n, tokens
}

func heldKey(threadKey string) string { return store.SweepHeldPrefix + threadKey }

// putHeld writes the answer, replacing any earlier one for the thread. A newer question is about a
// newer conversation, so it supersedes the older answer rather than merging with it.
func putHeld(st store.Store, h heldAnswer) {
	if st == nil || h.Thread == "" {
		return
	}
	h.V = heldVersion
	if b, err := json.Marshal(h); err == nil {
		st.Put(heldKey(h.Thread), b)
	}
}

// getHeld reads a thread's held answer. A missing, unreadable, consumed or other-version record is
// absent: never act on a record that cannot be fully read.
func getHeld(st store.Store, threadKey string) (heldAnswer, bool) {
	if st == nil || threadKey == "" {
		return heldAnswer{}, false
	}
	b, ok := st.Get(heldKey(threadKey))
	if !ok || len(b) == 0 {
		return heldAnswer{}, false
	}
	var h heldAnswer
	if json.Unmarshal(b, &h) != nil || h.V != heldVersion || h.Spent || h.Thread != threadKey {
		return heldAnswer{}, false
	}
	return h, true
}

// spendHeld consumes the answer. It keeps the record's identity so a reader can tell "consumed" from
// "never asked", then reads as absent.
func spendHeld(st store.Store, h heldAnswer) {
	h.Spent = true
	h.Outputs = nil
	putHeld(st, h)
}

// markHeldReported records, once, that a request has reported this ask.
func markHeldReported(st store.Store, h heldAnswer) {
	h.Reported = true
	putHeld(st, h)
}

// rawToolMessage finds the tool message that answers toolID. By id, never by index: see the note on
// heldOutput.
func rawToolMessage(msgs []bschemas.ChatMessage, toolID string) (int, bool) {
	if toolID == "" {
		return -1, false
	}
	for i := range msgs {
		m := &msgs[i]
		if m.Role == bschemas.ChatMessageRoleTool && m.ChatToolMessage != nil &&
			m.ChatToolMessage.ToolCallID != nil && *m.ChatToolMessage.ToolCallID == toolID {
			return i, true
		}
	}
	return -1, false
}

// outputHash is the content hash kept for a judged output: SpanHash of the one message, so it shares
// the cache-breakpoint-insensitive definition of "the same bytes" that the stale-prefix guard uses.
func outputHash(m bschemas.ChatMessage) string {
	return components.SpanHash([]bschemas.ChatMessage{m})
}

// buildHeld turns one finished question into a held answer. cands are the candidates the question
// carried, drop the indexes into cands the model voted to drop, raw the agent's own conversation.
// It returns how many candidates it could not hold because the agent's request has no tool message
// with their id.
func buildHeld(threadKey, model string, raw []bschemas.ChatMessage, cands []sweepCand, drop []int,
	askedAtMs int64, wait SweepPingWait, costUSD float64) (h heldAnswer, unholdable int) {

	voted := make(map[int]bool, len(drop))
	for _, k := range drop {
		voted[k] = true
	}
	h = heldAnswer{V: heldVersion, Thread: threadKey, AskedAtMs: askedAtMs, Model: model, Judged: len(raw),
		WaitingFor: wait.WaitingFor, WaitTool: wait.WaitTool, CostUSD: costUSD}
	for k, cd := range cands {
		idx, ok := rawToolMessage(raw, cd.toolID)
		if !ok {
			unholdable++
			continue
		}
		tokens := schema.TextTokens(cd.content)
		saves := tokens - schema.TextTokens(sweepDescriptor(cd.content))
		if saves < 0 {
			saves = 0
		}
		h.Outputs = append(h.Outputs, heldOutput{ToolUseID: cd.toolID, Hash: outputHash(raw[idx]),
			Tokens: tokens, Saves: saves, Drop: voted[k]})
	}
	return h, unholdable
}

// trimReason keeps a reason or gate name safe to use as a metric/event suffix.
func trimReason(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		}
		return '_'
	}, strings.ToLower(s))
}
