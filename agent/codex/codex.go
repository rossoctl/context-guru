// Package codex reads the ids OpenAI Codex (the CLI, the exec mode, the IDE extension and the
// apps built on its app server) puts in its request headers.
//
// This is a library package of plain functions: no registry, no route path. A host passes the
// request headers, so the proxy, the bifrost adapter and a plugin host such as Cortex all get
// the same answer on any path. It never imports a host. agent/imports_test.go enforces that.
package codex

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// Name is the client name on the dashboard.
const Name = "codex"

// The request headers Codex names its ids with.
const (
	SessionHeader      = "session-id"
	ThreadHeader       = "thread-id"
	ParentThreadHeader = "x-codex-parent-thread-id"
)

// Fields are the ids one Codex request carries. Each is "" when the header is absent.
type Fields struct {
	// SessionID is the `session-id` header. Codex sets it on every model request
	// (codex-rs codex-api/src/requests/headers.rs, build_session_headers). It names the whole
	// session: it stays after a compaction and after a resume, and a subagent sends its
	// parent's. A fork starts a new session with a new id. It equals the hook payload's
	// session_id and, by default, the prompt_cache_key in the body.
	SessionID string
	// AgentID is the `thread-id` header: the thread that made this request. A subagent has
	// its own thread.
	AgentID string
	// ParentID is the `x-codex-parent-thread-id` header. Only a subagent sends it.
	ParentID string
}

// Identify returns the Codex ids in h, and true, when the caller says it is Codex: its
// User-Agent or its `originator` header starts with "codex", ignoring case. The ids have no
// vendor prefix, so any other client could send them, and they are read only for Codex. A nil
// or empty h, and any other caller, give false.
//
// Codex starts its User-Agent with its originator: `codex_cli_rs/<version> (...)` by default,
// and `codex_exec`, `codex-tui`, `codex_vscode` for other front ends (codex-rs
// login/src/auth/default_client.rs). An app built on the Codex app server sets its own
// originator, and then its own User-Agent start, so the originator header is checked too.
func Identify(h http.Header) (Fields, bool) {
	if len(h) == 0 || !(hasCodexPrefix(h.Get("User-Agent")) || hasCodexPrefix(h.Get("Originator"))) {
		return Fields{}, false
	}
	return Fields{
		SessionID: strings.TrimSpace(h.Get(SessionHeader)),
		AgentID:   strings.TrimSpace(h.Get(ThreadHeader)),
		ParentID:  strings.TrimSpace(h.Get(ParentThreadHeader)),
	}, true
}

func hasCodexPrefix(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= len("codex") && strings.EqualFold(s[:len("codex")], "codex")
}

// turnMetadata is the header, and the Responses `client_metadata` key, in which Codex describes
// each request. Its JSON value holds "request_kind":"compaction" on the compaction request, local
// and remote alike (openai/codex rust-v0.172.0: core/src/responses_metadata.rs REQUEST_KIND_KEY and
// CompactionTurnMetadata, core/src/client.rs X_CODEX_TURN_METADATA_HEADER).
const turnMetadata = "x-codex-turn-metadata"

// IsCompaction reports whether Codex marked this request as its own compaction: the turn metadata in
// the header h, or in the body's `client_metadata` (the form a WebSocket request carries). It reads
// metadata only, no request text. It answers only for a caller that Identify accepts as Codex, as the
// ids do: any client could send the header. A nil h, a non-Codex caller and an unmarked request give
// false; the host then falls back to the prompt match in the thread package.
//
// `client_metadata` is a JSON object of string values (`Option<HashMap<String, String>>`, openai/codex
// codex-rs/codex-api/src/common.rs, ResponsesApiRequest and ResponseCreateWsRequest). The turn
// metadata is one such value: itself a JSON text, so the match reads it as a string.
func IsCompaction(h http.Header, body []byte) bool {
	if _, ok := Identify(h); !ok {
		return false
	}
	if marksCompaction(h.Get(turnMetadata)) {
		return true
	}
	return BodyMarksCompaction(body)
}

// BodyMarksCompaction reports whether the body's `client_metadata` holds the compaction turn metadata
// (the form a WebSocket request carries). It does not check that the caller is Codex: agent.Identify
// has done that. Metadata only, no request text.
func BodyMarksCompaction(body []byte) bool {
	return marksCompaction(gjson.GetBytes(body, "client_metadata").Get(turnMetadata).String())
}

func marksCompaction(meta string) bool {
	return meta != "" && gjson.Get(meta, "request_kind").String() == "compaction"
}

// AskHumanTools are the tools whose call waits on a person, not on the tool itself.
// "request_user_input" is Codex's question tool (openai/codex f64f003,
// codex-rs/core/src/tools/handlers/request_user_input_spec.rs, REQUEST_USER_INPUT_TOOL_NAME).
var AskHumanTools = []string{"request_user_input"}

// QuestionTools are the AskHumanTools that ask a question. Codex has no other kind.
var QuestionTools = []string{"request_user_input"}
