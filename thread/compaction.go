package thread

import (
	"net/http"
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
//   - Codex rust-v0.172.0 — core/src/compact.rs; the template is
//     prompts/templates/compact/prompt.md ("You are performing a CONTEXT CHECKPOINT
//     COMPACTION."). Local compaction sends it as the last Responses `input` user item. This is
//     only the fallback: Codex also marks the request itself (see IsAgentCompactionRequest).
//   - OpenCode v2.4.1 (and v1.19.6, which imports the same function) — buildPrompt() in
//     packages/core/src/session/compaction.ts. v2 appends it as the last user message; v1
//     (packages/opencode/src/session/compaction.ts) sends it as the only user message. Two
//     variants: first summary, and "Update the existing checkpoint" when a summary exists.
//   - Pi (@earendil-works/pi-coding-agent 1.6.0) — dist/core/compaction/compaction.js. The
//     user message is `<conversation>…</conversation>` followed by one of three prompts:
//     SUMMARIZATION_PROMPT, UPDATE_SUMMARIZATION_PROMPT (its "<previous-summary>" is cut off:
//     "<" is JSON-escaped), or TURN_PREFIX_SUMMARIZATION_PROMPT. The prompt is the end of the
//     last turn, which is why the match is on the last turn only.
//
// Why these must be matched against the LAST message only: the phrase is agent-authored
// boilerplate that is always the final turn of a compaction request. A whole-body match would
// latch: the phrase would sit in the transcript for the rest of the session and mark every
// later request as a compaction.
//
// A false positive here is REACHABLE, not hypothetical: this phrase is quoted verbatim in
// docs/how-to/agent-compaction.md, so an agent that reads that page lands it in a
// trailing tool_result (#533). A false positive costs a wrong count and a wrong thread mark.
// A phrase must contain no character encoding/json escapes (no "<", ">", "&", quotes,
// newlines): the match runs against the RAW message JSON so that a string content and a
// content-block array both work, and Go's HTML-escaping would otherwise rewrite the
// needle on the wire. Bob's full sentence ends in "<state_snapshot>." — cut there.
var agentCompactionPhrases = []string{
	"Your task is to create a detailed summary of ",
	"First, reason in your scratchpad. Then, generate the ",
	// Codex
	"You are performing a CONTEXT CHECKPOINT COMPACTION. ",
	// OpenCode
	"You MUST summarize the conversation above into a structured summary that will be given to another agent to resume the work.",
	"Update the existing checkpoint in the conversation above into one consolidated summary.",
	// Pi
	"The messages above are a conversation to summarize. Create a structured context checkpoint summary",
	"The messages above are NEW conversation messages to incorporate into the existing summary provided in ",
	"The messages above are earlier context from an ongoing conversation. Later messages are stored separately",
}

// HeaderRequestKind is set by a client plugin on the client's own compaction request, with the value
// RequestKindCompaction. The OpenCode and Pi plugins set it. The proxy strips every x-context-guru-*
// header before it forwards a request. It is metadata only: no request text is read.
const (
	HeaderRequestKind     = "x-context-guru-request-kind"
	RequestKindCompaction = "compaction"
)

// codexTurnMetadata is the header (and the Responses `client_metadata` key) in which Codex
// describes each request. Its JSON value holds "request_kind":"compaction" on the compaction request,
// local and remote alike (Codex rust-v0.172.0, core/src/responses_metadata.rs: REQUEST_KIND_KEY,
// CompactionTurnMetadata, X_CODEX_TURN_METADATA_HEADER in core/src/client.rs).
const codexTurnMetadata = "x-codex-turn-metadata"

// IsAgentCompactionRequest is IsAgentCompaction plus the signals that need no text: the
// plugin header (HeaderRequestKind) and Codex's own turn metadata (header, or `client_metadata` in
// the body). A marked request is a compaction whatever its text says. Pass the result to Resolve as
// agentCompaction. h may be nil.
func IsAgentCompactionRequest(h http.Header, body []byte) bool {
	if h != nil {
		if strings.EqualFold(strings.TrimSpace(h.Get(HeaderRequestKind)), RequestKindCompaction) {
			return true
		}
		if codexMarksCompaction(h.Get(codexTurnMetadata)) {
			return true
		}
	}
	if codexMarksCompaction(gjson.GetBytes(body, "client_metadata").Get(codexTurnMetadata).String()) {
		return true
	}
	return IsAgentCompaction(body)
}

func codexMarksCompaction(meta string) bool {
	return meta != "" && gjson.Get(meta, "request_kind").String() == "compaction"
}

// IsAgentCompaction reports whether body is the agent asking the model to summarize the
// conversation so far (its own context compaction). Pass the result to Resolve as
// agentCompaction, with the same body: the request as the agent sent it.
//
// The match is scoped to the last TURN's raw JSON (the phrases contain no characters
// JSON escapes, so string and content-block shapes both work) and to user-role messages,
// which is where every known agent puts it. See agentCompactionPhrases and lastTurn.
func IsAgentCompaction(body []byte) bool {
	// The Chat and Messages APIs carry the conversation in `messages`; the Responses API (Codex)
	// carries it in `input`, an array of items or a plain string.
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		msgs = gjson.GetBytes(body, "input")
	}
	if msgs.Type == gjson.String {
		return hasCompactionPhrase(msgs.Raw)
	}
	if !msgs.IsArray() {
		return false
	}
	last, ok := lastTurn(msgs.Array())
	if !ok || last.Get("role").String() != "user" {
		return false
	}
	return hasCompactionPhrase(last.Raw)
}

func hasCompactionPhrase(raw string) bool {
	for _, p := range agentCompactionPhrases {
		if strings.Contains(raw, p) {
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
