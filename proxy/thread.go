package proxy

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/rossoctl/context-guru/internal/thread"
)

// agentIDHeader is Claude Code's subagent id. Seen on the wire with Claude Code 2.1.294 through a
// custom ANTHROPIC_BASE_URL: present on every subagent request, absent on the main thread. The
// sibling headers its code also names (x-claude-code-parent-agent-id, -agent-type,
// -request-class) were NOT sent in that capture, so nothing here depends on them.
const agentIDHeader = "x-claude-code-agent-id"

// threadFor finds the session thread of one request (see internal/thread): the agent's header
// when it names one, prefix continuity otherwise, and the session itself as the last resort.
//
// body is the request as the agent sent it, before the pipeline ran.
//
// Fails open to the session: an error here must never block or change the request, and the
// session key is exactly the behaviour before threads existed.
func (h *Handler) threadFor(r *http.Request, tenantID, session string, body []byte, agentCompaction bool, lg *slog.Logger) (res thread.Result) {
	if session == "" {
		return thread.Result{ID: thread.Primary, Source: thread.SourceSession}
	}
	defer func() {
		if rec := recover(); rec != nil {
			lg.Error("context-guru: recovered from panic in thread identification; using the session", "panic", rec)
			res = thread.Result{ID: thread.Primary, Source: thread.SourceSession}
		}
	}()
	agentID := strings.TrimSpace(r.Header.Get(agentIDHeader))
	if agentID != "" && !thread.ValidHeaderID(agentID) {
		// A header that is present but unusable falls back to prefix continuity. It must not
		// become a key, and it must not merge this request into another thread.
		lg.Debug("cg.thread_header_invalid", "len", len(agentID))
		agentID = ""
	}
	res = h.threads.Resolve(tenantID+"\x00"+session, agentID, body, agentCompaction)
	if res.Disagree {
		// The header path wins, but a drift between the two paths on Claude Code traffic is
		// what would tell us the fallback groups requests wrongly for other agents.
		lg.Debug("cg.thread_disagree", "thread", res.ID)
	}
	return res
}
