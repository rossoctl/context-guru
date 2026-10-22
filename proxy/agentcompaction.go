package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// agentCompactionPhrases are the VERBATIM prompt openings agents use when they ask the
// model to summarize their own conversation so they can keep going in a smaller context.
// Each was read out of the shipped agent, not guessed:
//
//   - Claude Code 2.1.215 — `Jao()`/`GMu()` build the summary request and hand it to
//     `Ur({content})`, the user-message factory; `k2` appends it LAST
//     (`[...forkContextMessages, ...promptMessages]`). Every variant (auto-compact,
//     /compact, and the "RECENT portion" partial-compact bodies) begins the same way,
//     so the shared opening is the match. It asks for "full code snippets" and for
//     security-relevant instructions to be "preserved verbatim".
//   - Claude Code 2.1.296 (#518) — two prompt variants, chosen per session: "…summary of
//     the conversation so far" and "…summary of this conversation". The phrase therefore
//     stops at "summary of ". The prompt is no longer a message of its own: it is an extra
//     text block inside the last USER message, and a `system` message (`<total_tokens>…`)
//     still follows it. So the match skips trailing system messages (see lastTurn).
//   - Bob Shell 1.0.6 (Gemini-CLI fork) — the compression call appends exactly this one
//     fixed user turn, with the summarizer prompt in `systemInstruction`.
//
// Why these must be matched against the LAST message only: the phrase is agent-authored
// boilerplate that is always the final turn of a compaction request. A whole-body match would
// latch: the phrase would sit in the transcript for the rest of the session and mark every
// later request as a compaction.
//
// A false positive here is REACHABLE, not hypothetical: this phrase is quoted verbatim in
// docs/how-to/agent-compaction.md, so an agent that reads that page lands it in a
// trailing tool_result (#533). The match no longer changes how a request is treated, so a
// false positive now costs only a wrong count and a wrong thread mark. The one exception is a
// body over maxRequestBytes, which is bypassed (see chat).
// A phrase must contain no character encoding/json escapes (no "<", ">", "&", quotes,
// newlines): the match runs against the RAW message JSON so that a string content and a
// content-block array both work, and Go's HTML-escaping would otherwise rewrite the
// needle on the wire. Bob's full sentence ends in "<state_snapshot>." — cut there.
var agentCompactionPhrases = []string{
	"Your task is to create a detailed summary of ",
	"First, reason in your scratchpad. Then, generate the ",
}

// isAgentCompaction reports whether this request is the agent asking the model to
// summarize the conversation so far (its own context compaction).
//
// The proxy does NOT bypass such a request. Bypass forwards the original history while the
// provider cache holds the history context-guru rewrote on earlier turns, so a bypassed
// compaction missed the cache and paid for the whole request again. The request goes through
// the pipeline like any other. The summary model therefore sees the rewritten history
// (shortened tool outputs, <<cg:HASH>> markers), not the full original.
//
// The match is still used for: thread identification (internal/thread), the larger body
// limit (readGatedBody), the dashboard row (replay cap), and the agent_compaction counter in
// /stats. A compaction body over maxRequestBytes is the one case that is bypassed (see chat).
//
// The match is scoped to the last TURN's raw JSON (the phrases contain no characters
// JSON escapes, so string and content-block shapes both work) and to user-role messages,
// which is where every known agent puts it. See agentCompactionPhrases and lastTurn.
func isAgentCompaction(body []byte) bool {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return false
	}
	last, ok := lastTurn(msgs.Array())
	if !ok || last.Get("role").String() != "user" {
		return false
	}
	for _, p := range agentCompactionPhrases {
		if strings.Contains(last.Raw, p) {
			return true
		}
	}
	return false
}

// lastTurn is the last message that is not a system or developer message. Claude Code 2.1.296
// ends a request with a `system` message carrying its token budget, after the user turn that
// holds the compaction prompt. Skipping only those keeps the anti-latch rule above: an earlier
// USER or assistant turn is never looked at.
func lastTurn(arr []gjson.Result) (gjson.Result, bool) {
	for i := len(arr) - 1; i >= 0; i-- {
		switch arr[i].Get("role").String() {
		case "system", "developer":
			continue
		}
		return arr[i], true
	}
	return gjson.Result{}, false
}
