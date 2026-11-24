// Package claudecode holds Claude Code's header rules. They are plain functions of the request
// headers, so any host (the proxy, the bifrost adapter, Cortex) can call them. agent/client calls
// them from Identify.
//
// This is a library package. It never imports a host. imports_test.go enforces that.
package claudecode

import (
	"net/http"
	"strings"
)

// AgentHeader is the request header Claude Code names a subagent's thread with. Seen on the wire
// with Claude Code 2.1.294 through a custom ANTHROPIC_BASE_URL: present on every subagent request,
// absent on the main thread. The sibling headers its code also names
// (x-claude-code-parent-agent-id, -agent-type, -request-class) were NOT sent in that capture, so
// nothing here depends on them.
const AgentHeader = "x-claude-code-agent-id"

// AgentID is the raw AgentHeader value, trimmed: Claude Code's id for a subagent's thread. "" for
// the main thread, and for a nil header. The host validates it (thread.HeaderAgentID) before it
// uses it as a key.
func AgentID(h http.Header) string { return strings.TrimSpace(h.Get(AgentHeader)) }
