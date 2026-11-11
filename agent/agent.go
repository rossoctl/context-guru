// Package agent is the extension point for a client agent (an IDE, a CLI) that talks to a host
// in its own way. The shared request path asks the agent for the route's answer and does not
// branch on which agent it is. A new agent is one package that embeds Base, overrides only what
// differs, and registers its route paths.
//
// This is a library package. A host (the proxy, the bifrost adapter) imports it; it never
// imports a host. imports_test.go enforces that.
package agent

import "net/http"

// Agent is what the shared request path asks of the client agent on a route.
type Agent interface {
	// Name is the agent label on the dashboard. "" means the shared code picks the label.
	Name() string
	// Paths are the model routes this agent owns, as UPSTREAM paths: the path the host forwards to
	// (for example /inference/v1/chat/completions), not the path the client calls on the host
	// (which can carry a host prefix such as /openai). The registry finds the agent by them, by the
	// exact path: no method, no query string, the same trailing slash.
	Paths() []string
	// SessionID is a session id that only this agent's client sends in a header. The host tries
	// it LAST: after x-context-guru-session and the ids in the body. It is per agent because such
	// a header often has no vendor prefix, so any client can send it, and only the route that
	// owns it may read it. "" means none.
	SessionID(h http.Header) string
	// ReadUsage corrects the shared parser's reading of a reply's usage, for an agent whose
	// upstream reports fields the shared parser does not know. base is that reading. A reply it
	// cannot read must return base unchanged (fail open).
	ReadUsage(contentType string, reply []byte, base Usage) Usage
	// ModelTarget is where the host sends its own calls to this route's model (a summary, the
	// sweep's question). The agent never sees the key: the host builds the header from it.
	ModelTarget() ModelTarget
}

// ModelTarget is the chat path on the route's upstream, and the scheme of the Authorization header
// that carries the caller's key. "" in a field means the host default for it (/v1/chat/completions,
// Bearer). The host ignores a field it cannot use safely, and keeps its default for that field.
type ModelTarget struct {
	Path       string // for example /inference/v1/chat/completions
	AuthScheme string // for example Apikey
}

// Usage is the token counts ReadUsage reads and returns. It is a neutral copy of the counts in
// the host's own usage type: a library cannot import the host to use that type. The host maps
// its type to this one and back.
type Usage struct {
	FreshInput int64
	CacheRead  int64
	CacheWrite int64
	// CacheWrite1h is the part of CacheWrite billed at the one-hour write tier.
	CacheWrite1h int64
	Output       int64
}

// Base is the neutral agent: it changes nothing on the shared path. An agent embeds it and
// overrides only the methods where it differs, so a new method on Agent does not break it. That is
// why Agent holds only what a host wires today: a method is added when its first caller is.
type Base struct{}

func (Base) Name() string                                   { return "" }
func (Base) Paths() []string                                { return nil }
func (Base) SessionID(http.Header) string                   { return "" }
func (Base) ReadUsage(_ string, _ []byte, base Usage) Usage { return base }
func (Base) ModelTarget() ModelTarget                       { return ModelTarget{} }
