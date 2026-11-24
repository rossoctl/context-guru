// Package all blank-imports every built-in agent so their init() registrations run. A host (the
// proxy, a sidecar plugin) imports this package for its side effects, then asks the agent package
// for the agents. Import it for effect:
//
//	import _ "github.com/rossoctl/context-guru/agent/all"
package all

import (
	"net/http"

	"github.com/rossoctl/context-guru/agent"
	_ "github.com/rossoctl/context-guru/agent/bob"
	"github.com/rossoctl/context-guru/agent/codex"
	"github.com/rossoctl/context-guru/thread"
)

// MarkedCompaction reports whether the client marked this request as its own compaction, by a mark
// that needs no text: a context-guru plugin header, or a built-in agent's own request metadata
// (Codex). A host that has no headers (the bifrost adapter today) passes a nil h and gets false.
//
// Temporary home: when agent.Identify lands (#1312), it returns this mark as part of the Client
// (for example Client.Compaction), and this function becomes a thin wrapper or goes away.
func MarkedCompaction(h http.Header, body []byte) bool {
	return agent.PluginMarksCompaction(h) || codex.IsCompaction(h, body)
}

// IsClientCompaction reports whether body is the client's own compaction request: the client's
// mark first, then the prompt match in the thread package. It is the ONE call for a host. The proxy
// passes r.Header, a plugin host such as Cortex passes pctx.Headers, and the bifrost adapter passes
// nil, which leaves the prompt match as the fallback. Pass the result to the thread Tracker as
// agentCompaction.
func IsClientCompaction(h http.Header, body []byte) bool {
	return thread.IsAgentCompactionHint(MarkedCompaction(h, body), body)
}
