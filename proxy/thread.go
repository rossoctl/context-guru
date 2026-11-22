package proxy

import (
	"log/slog"
	"net/http"
	"strings"

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
	raw := r.Header.Get(thread.AgentHeader)
	agentID, ok := thread.HeaderAgentID(raw)
	if !ok && session != "" {
		lg.Debug("cg.thread_header_invalid", "len", len(strings.TrimSpace(raw)))
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
