package thread

import (
	"fmt"
	"strings"
)

// AgentHeader is the request header Claude Code names a subagent's thread with. Seen on the wire
// with Claude Code 2.1.294 through a custom ANTHROPIC_BASE_URL: present on every subagent request,
// absent on the main thread. The sibling headers its code also names
// (x-claude-code-parent-agent-id, -agent-type, -request-class) were NOT sent in that capture, so
// nothing here depends on them.
const AgentHeader = "x-claude-code-agent-id"

// HeaderAgentID turns a raw AgentHeader value into the agentID that Resolve and ResolveSession
// take. It trims the value. It gives "" when the header is absent, and also when ValidHeaderID
// refuses it: a header that is present but unusable falls back to prefix continuity. It must not
// become a key, and it must not merge the request into another thread. ok is false only for that
// refused value, so a host can log it.
func HeaderAgentID(v string) (id string, ok bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", true
	}
	if !ValidHeaderID(v) {
		return "", false
	}
	return v, true
}

// ResolveSession is Resolve for a host: the one rule the proxy and every embedding host share
// (#1247). Use it to build apply.Opts.ThreadOf.
//
//   - The scope is tenant + "\x00" + session, as the proxy keys it.
//   - An empty session gives the session itself (Primary, SourceSession), whatever the header
//     says. apply leaves Trace.Session empty when it returns before it resolves the session, so
//     a host that calls this again with Trace.Session gets the primary thread, as the proxy does.
//   - It fails open. A panic gives the session itself, and err describes the panic, so the
//     host can log it. The result is always usable; err never means "drop the request".
//
// agentID is HeaderAgentID's result. body and agentCompaction are as for Resolve: pass the
// request as the agent sent it, and agent/all.IsClientCompaction of that same headers and body (the mark, then the prompt match).
func (t *Tracker) ResolveSession(tenant, session, agentID string, body []byte, agentCompaction bool) (res Result, err error) {
	if session == "" {
		return Result{ID: Primary, Source: SourceSession}, nil
	}
	defer func() {
		if rec := recover(); rec != nil {
			res, err = Result{ID: Primary, Source: SourceSession}, fmt.Errorf("thread: recovered from panic: %v", rec)
		}
	}()
	return t.Resolve(tenant+"\x00"+session, agentID, body, agentCompaction), nil
}
