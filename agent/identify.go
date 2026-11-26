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

// Identify is the one library entry point that names the client behind a request. Every host (the
// proxy, the bifrost adapter, Cortex) calls Identify once per request, with the request headers, and
// passes the Client down. Library code takes the Client it
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
	// Compaction is true when the headers mark this request as the client's own compaction: a context-guru
	// plugin header (OpenCode, Pi), or Codex's turn metadata header. A mark in the body (Codex over a
	// WebSocket) needs the body: see MarkedCompaction.
	Compaction bool
	// ParentID is the id of the parent agent's thread or session, or "" for a main thread.
	// Codex: x-codex-parent-thread-id. OpenCode: x-opencode-parent-session-id.
	ParentID string
}

// IdentifyUserAgent is Identify for a caller that has nothing but a stored User-Agent, such as a
// dashboard row. A request path must call Identify with the headers instead: the client marks
// (Pi, Codex ids) are in the headers, not in the User-Agent.
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
	c.Compaction = PluginMarksCompaction(h)
	l := strings.ToLower(strings.TrimSpace(ua))

	// The subagent header is read whatever the client: only Claude Code sends it.
	// thread.HeaderAgentID is the one parser of the id's form: a header that is present but unusable gives "".
	if id, _ := thread.HeaderAgentID(claudecode.AgentID(h)); id != "" {
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
			c.Compaction = c.Compaction || codex.IsCompaction(h, nil)
			if c.AgentID == "" && f.AgentID != "" {
				c.AgentID, c.AgentIDHeader = f.AgentID, "thread-id"
			}
		} else {
			if c.Label == "claude-code" || c.Label == "claude-cli" {
				c.Name = ClientClaude
			}
		}
	}
	// OpenCode: its User-Agent starts with "opencode/", or it sends its own headers. A client that
	// already has a Name keeps it. The label is not changed here: it stays the User-Agent's first
	// token ("opencode" for "opencode/<version>"), as before.
	if c.Name == "" {
		if f, ok := opencode.Identify(h); ok {
			c.Name = ClientOpenCode
			c.SessionID, c.ParentID = f.SessionID, f.ParentSessionID
		} else if ua0 := strings.FieldsFunc(l, func(r rune) bool { return r == '/' || r == ' ' }); len(ua0) > 0 && ua0[0] == "opencode" {
			c.Name = ClientOpenCode
		}
	}
	return c
}

// label classifies a User-Agent into an agent family so the dashboard can filter by application.
// Unknown clients keep their raw first token rather than being lumped into "other": a filter is
// useless if everything is "other".
func label(ua string) string {
	l := strings.ToLower(ua)
	for _, known := range []string{"claude-code", "claude-cli", "codex", "cursor", "cline", "aider", "gemini-cli", "bob"} {
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

// ModelAgentOf is the agent label the prediction model reads from a stored row. Before the Pi label,
// a Pi row held the token of the provider SDK's User-Agent, and the model's agent feature one-hots
// "openai" (and "asyncopenai", "claude-cli"). A Pi row now holds "pi", so the provider column gives
// a token back: "openai" and "anthropic" are the SDK tokens Pi's two routed providers send. Any
// other provider has no known token: the provider string is returned, which the model reads as an
// unseen value, as it read the old unknown SDK token. Every other label passes through.
func ModelAgentOf(label, provider string) string {
	if label != ClientPi {
		return label
	}
	return provider
}

// ModelAgent is the agent value the prediction model reads for a live request from c. It is the
// value training reads from the stored row of the same request: ModelAgentOf(c.Label, provider).
// A host passes the Client it found once and the request's provider, and nothing else.
func (c Client) ModelAgent(provider string) string { return ModelAgentOf(c.Label, provider) }

// MarkedCompaction reports whether the client marked this request as its own compaction, by a mark that
// needs no request text: the headers (Client.Compaction), or Codex's body metadata. body is the request
// as the client sent it.
func MarkedCompaction(c Client, body []byte) bool {
	return c.Compaction || (c.Name == ClientCodex && codex.BodyMarksCompaction(body))
}

// IsClientCompaction reports whether body is the client's own compaction request: the client's mark
// first, then the prompt match in the thread package. It is the one call for a host that holds a
// Client. Pass the result to the thread Tracker as agentCompaction. A host with no headers passes
// Identify(nil), which leaves the prompt match as the fallback.
func IsClientCompaction(c Client, body []byte) bool {
	return thread.IsAgentCompactionHint(MarkedCompaction(c, body), body)
}

// SubagentID is the id of this request's thread when the client says the request comes from a
// subagent, or "" for a main thread and for a client that gives no signal. Claude Code: the agent
// header. Codex: the request's own thread-id, when it names a parent thread. OpenCode: the request's
// own session id, when it names a parent session. A host passes it to thread.HeaderAgentID, then to
// thread.Tracker.ResolveSession.
func (c Client) SubagentID() string {
	switch {
	case c.AgentIDHeader == claudecode.AgentHeader:
		return c.AgentID
	case c.Name == ClientCodex && c.ParentID != "":
		return c.AgentID
	case c.Name == ClientOpenCode && c.ParentID != "":
		return c.SessionID
	}
	return ""
}

// TaskIDHeader is the header the Bob IDE sends its task id in. It is here, not in package bob,
// because package bob imports this package. Readers: agent/bob (SessionID), apply (the host id of
// a route) and proxy/recctx.go (the recorder's session key).
const TaskIDHeader = "x-task-id"

// CorrelationHeaders lists the request headers that only carry a client's session, thread or task
// id, read by Identify and by the agents' SessionID. They are random ids, not credentials, so a
// recorder may write them. It is built from the same constants the parsers use, so a new id
// header added to a parser is added here in one place. Claude Code names its session in the body
// (see Identify), so no session header of it is listed.
func CorrelationHeaders() []string {
	return []string{
		claudecode.AgentHeader,
		codex.SessionHeader, codex.ThreadHeader, codex.ParentThreadHeader,
		opencode.SessionHeader, opencode.ParentSessionHeader,
		TaskIDHeader,
	}
}
