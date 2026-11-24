package proxy

import (
	"log/slog"
	"net/http"

	"github.com/rossoctl/context-guru/agent/claudecode"
	"github.com/rossoctl/context-guru/thread"
)

// threadFor finds the session thread of one request (see package thread): the agent's header
// when it names one, prefix continuity otherwise, and the session itself as the last resort.
// The rule is thread.ResolveSession, which every embedding host uses too. This adds only the
// proxy's logs.
//
// body is the request as the agent sent it, before the pipeline ran.
//
// Fails open to the session (ResolveSession recovers): an error here must never block or change
// the request, and the session key is exactly the behaviour before threads existed.
func (h *Handler) threadFor(r *http.Request, tenantID, session string, body []byte, agentCompaction bool, lg *slog.Logger) thread.Result {
	// Only Claude Code's header names a subagent here. Identify has checked the id (thread.ValidHeaderID):
	// a header that is present but unusable gives "" and falls back to prefix continuity. Codex and
	// OpenCode ids are in the Client too; a host that uses them (#1318) widens this test.
	agentID := ""
	if c := identify(r); c.AgentIDHeader == claudecode.AgentHeader {
		agentID = c.AgentID
	}
	res, err := h.threads.ResolveSession(tenantID, session, agentID, body, agentCompaction)
	if err != nil {
		lg.Error("context-guru: recovered from panic in thread identification; using the session", "panic", err)
	}
	if res.Disagree {
		// The header path wins, but a drift between the two paths on Claude Code traffic is
		// what would tell us the fallback groups requests wrongly for other agents.
		lg.Debug("cg.thread_disagree", "thread", res.ID)
	}
	return res
}
