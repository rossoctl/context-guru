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
)

// Name is the client name on the dashboard.
const Name = "codex"

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
		SessionID: strings.TrimSpace(h.Get("session-id")),
		AgentID:   strings.TrimSpace(h.Get("thread-id")),
		ParentID:  strings.TrimSpace(h.Get("x-codex-parent-thread-id")),
	}, true
}

func hasCodexPrefix(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= len("codex") && strings.EqualFold(s[:len("codex")], "codex")
}
