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
//   - Bob Shell 1.0.6 (Gemini-CLI fork) — the compression call appends exactly this one
//     fixed user turn, with the summarizer prompt in `systemInstruction`.
//
// Why these must be matched against the LAST message only, and why that is not a free
// bypass: the phrase is agent-authored boilerplate that is always the final turn of a
// compaction request. A user who types it (or reads a file containing it) trips the
// bypass for that ONE request. On the next turn their message is no longer last and
// compaction resumes. A whole-body match WOULD latch: the phrase would sit in the
// transcript forever and silently disable compaction for the rest of the session.
//
// A false positive here is REACHABLE, not hypothetical: this phrase is quoted verbatim in
// docs/how-to/agent-compaction.md, so an agent that reads that page lands it in a
// trailing tool_result.
//
// This comment used to claim the cost was "that request's savings — nothing else, because
// the bypass path writes no session state (no cached-prefix boundary), so it cannot
// latch." Writing no boundary was the harm, not the safeguard: the provider caches the
// full body we forwarded, so failing to record its length left the next turn treating
// those messages as mutable tail, rewriting them, and forcing a cache-write of the whole
// suffix. apply.BodyOpts now records the length on a bypassed turn too, which is what
// makes the "one request only" claim actually true.
// A phrase must contain no character encoding/json escapes (no "<", ">", "&", quotes,
// newlines): the match runs against the RAW message JSON so that a string content and a
// content-block array both work, and Go's HTML-escaping would otherwise rewrite the
// needle on the wire. Bob's full sentence ends in "<state_snapshot>." — cut there.
var agentCompactionPhrases = []string{
	"Your task is to create a detailed summary of the ",
	"First, reason in your scratchpad. Then, generate the ",
}

// isAgentCompaction reports whether this request is the agent asking the model to
// summarize the conversation so far (its own context compaction).
//
// Such a request must NOT be compacted: it demands verbatim fidelity over the whole
// transcript, and any content we replaced with a <<cg:HASH>> marker simply never makes
// it into the summary — the summary is then baked into the rest of the session, so the
// loss is permanent and expansion cannot recover it. Compacting the compactor is the one
// case where the pipeline makes a request strictly worse than no proxy at all.
//
// The match is scoped to the LAST message's raw JSON (the phrases contain no characters
// JSON escapes, so string and content-block shapes both work) and to user-role messages,
// which is where every known agent puts it. See agentCompactionPhrases.
func isAgentCompaction(body []byte) bool {
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return false
	}
	arr := msgs.Array()
	if len(arr) == 0 {
		return false
	}
	last := arr[len(arr)-1]
	if last.Get("role").String() != "user" {
		return false
	}
	for _, p := range agentCompactionPhrases {
		if strings.Contains(last.Raw, p) {
			return true
		}
	}
	return false
}
