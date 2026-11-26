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
)

// MarkedCompaction reports whether the client marked this request as its own compaction. It is a thin
// wrapper: it identifies the client from h, then calls agent.MarkedCompaction. A host that already
// holds the Client calls agent.MarkedCompaction. A nil h gives false.
func MarkedCompaction(h http.Header, body []byte) bool {
	return agent.MarkedCompaction(agent.Identify(h), body)
}

// IsClientCompaction is agent.IsClientCompaction for a host that has headers and no Client yet (a
// plugin host such as Cortex passes pctx.Headers; the bifrost adapter passes nil, which leaves the
// prompt match as the fallback).
func IsClientCompaction(h http.Header, body []byte) bool {
	return agent.IsClientCompaction(agent.Identify(h), body)
}
