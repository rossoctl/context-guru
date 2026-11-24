package agent

import (
	"net/http"
	"strings"

	"github.com/rossoctl/context-guru/agent/claudecode"
	"github.com/rossoctl/context-guru/agent/codex"
	"github.com/rossoctl/context-guru/agent/opencode"
	"github.com/rossoctl/context-guru/agent/pi"
	"github.com/rossoctl/context-guru/thread"
)

// Identify is the one library entry point that names the client behind a request. A host (the
// proxy, the bifrost adapter, Cortex) passes the request headers. Library code takes the Client it
// returns, never a raw User-Agent. It uses headers only (the User-Agent and the clients' own marks),
// never the route path: Claude Code, Codex, OpenCode and Pi share routes, and a host can mount routes
// elsewhere. The per-client rules live in agent/claudecode, agent/codex, agent/opencode and agent/pi.
// The path registry stays for an agent that owns a route (Bob's gateway).
//
// Names of the clients Client.Name can hold. "" is any other client.
const (
	ClientClaude   = "claude"
	ClientBob      = "bob"
	ClientCodex    = codex.Name
	ClientOpenCode = "opencode"
	ClientPi       = "pi"
)

// Client is what the headers of one request say about its client. Identify of no headers gives the
// generic result: Label "unknown", every other field empty.
type Client struct {
	// Label is the dashboard's agent family: "claude-code", "codex", "pi", the first token of an
	// unknown User-Agent, or "unknown". It is never empty.
	Label string
	// SDK is the User-Agent family: the label before a client mark (the Pi header) replaced it. For
	// Pi it is the provider SDK's token ("anthropic", "openai"). A reader that depends on the
	// provider, not on the harness, takes this and not Label. Never empty.
	SDK string
	// Name is the narrow identity that rules (the compaction point) key on. It is one of the
	// constants above, or "" for any other client. It is narrower than Label: a wrong match would
	// give a stranger's request another client's rule, so Bob and Codex are matched by the start of
	// their User-Agent, not anywhere in it.
	Name string
	// Entrypoint is how the client was started, from its User-Agent: "cli" (interactive),
	// "headless" (`-p`), "sdk", or "other". "" when there is no User-Agent.
	Entrypoint string
	// SessionID is the session id the client names in a header, or "". Codex: session-id.
	// OpenCode: x-opencode-session-id. Claude Code names its session in the body, not here.
	SessionID string
	// AgentID is the id of this agent's own thread, as the raw header value, or "". Claude Code:
	// x-claude-code-agent-id (subagents only). Codex: thread-id.
	AgentID string
	// AgentIDHeader names the header AgentID came from, or "".
	AgentIDHeader string
	// ParentID is the id of the parent agent's thread or session, or "" for a main thread.
	// Codex: x-codex-parent-thread-id. OpenCode: x-opencode-parent-session-id.
	ParentID string
}

// IdentifyUserAgent is Identify for a caller that has only a User-Agent (the dashboard).
func IdentifyUserAgent(ua string) Client {
	if ua == "" {
		return Identify(nil)
	}
	return Identify(http.Header{"User-Agent": {ua}})
}

// Identify names the client of a request from its headers. A nil or empty header gives the generic
// Client (see Client). It never fails and never panics.
func Identify(h http.Header) Client {
	ua := h.Get("User-Agent")
	c := Client{Label: label(ua), Entrypoint: entrypoint(ua)}
	c.SDK = c.Label
	l := strings.ToLower(strings.TrimSpace(ua))

	// The subagent header is read whatever the client: only Claude Code sends it.
	if id := claudecode.AgentID(h); thread.ValidHeaderID(id) {
		c.AgentID, c.AgentIDHeader = id, claudecode.AgentHeader
	}

	switch {
	case pi.Is(h):
		c.Label, c.Name, c.Entrypoint = ClientPi, ClientPi, "other"
	case strings.HasPrefix(l, "bobshell/"):
		c.Name = ClientBob
	default:
		if f, ok := codex.Identify(h); ok {
			c.Label, c.Name = codex.Name, codex.Name
			c.SessionID = f.SessionID
			c.ParentID = f.ParentID
			if c.AgentID == "" && f.AgentID != "" {
				c.AgentID, c.AgentIDHeader = f.AgentID, "thread-id"
			}
			break
		}
		switch c.Label {
		case "claude-code", "claude-cli":
			c.Name = ClientClaude
		case "opencode":
			c.Name = ClientOpenCode
		}
	}
	if f, ok := opencode.Identify(h); ok && c.Name != codex.Name && c.Name != ClientPi {
		c.Label, c.Name = "opencode", ClientOpenCode
		c.SessionID, c.ParentID = f.SessionID, f.ParentSessionID
	}
	return c
}

// label classifies a User-Agent into an agent family so the dashboard can filter by application.
// Unknown clients keep their raw first token rather than being lumped into "other": a filter is
// useless if everything is "other".
func label(ua string) string {
	l := strings.ToLower(ua)
	for _, known := range []string{"claude-code", "claude-cli", "codex", "cursor", "cline", "aider", "gemini-cli", "bob", "opencode"} {
		if strings.Contains(l, known) {
			return known
		}
	}
	if l == "" {
		return "unknown"
	}
	if i := strings.IndexAny(l, "/ "); i > 0 {
		return l[:i]
	}
	if len(l) > 32 {
		return l[:32]
	}
	return l
}

// entrypoint classifies how the client was started from its User-Agent. Claude Code sends
// "claude-cli/<ver> (external, <entrypoint>)"; Codex sends "codex_cli_rs/<ver> ...".
func entrypoint(ua string) string {
	l := strings.ToLower(strings.TrimSpace(ua))
	if l == "" {
		return ""
	}
	if i := strings.LastIndex(l, "("); i >= 0 && strings.HasPrefix(l, "claude-") {
		inner := strings.TrimSuffix(strings.TrimSpace(l[i+1:]), ")")
		if j := strings.LastIndex(inner, ","); j >= 0 {
			inner = inner[j+1:]
		}
		switch inner = strings.TrimSpace(inner); {
		case inner == "cli":
			return "cli"
		case inner == "sdk-cli":
			return "headless"
		case strings.HasPrefix(inner, "sdk"):
			return "sdk"
		}
		return "other"
	}
	if strings.HasPrefix(l, "codex") {
		return "cli"
	}
	return "other"
}
