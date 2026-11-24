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
)

// MarkedCompaction reports whether the client marked this request as its own compaction, by a mark
// that needs no text: a context-guru plugin header, or a built-in agent's own request metadata
// (Codex). A host passes the result to thread.IsAgentCompactionHint, with the same body. A host that
// has no headers (the bifrost adapter today) passes a nil h and gets false: the prompt match in the
// thread package is then the fallback. A plugin host such as Cortex passes pctx.Headers.
func MarkedCompaction(h http.Header, body []byte) bool {
	return agent.PluginMarksCompaction(h) || codex.IsCompaction(h, body)
}
