package tooldefer

import "strings"

// Explain says whether the history rule would defer name in tenant's next session, and why, for
// save-more tools (#869). name is a tool, or a whole MCP server as "mcp__<server>". It reads the
// same history and the same thresholds as Decide, but without a request: the project veto, a use
// in the thread, tool_choice and the 200-token floor are decided per request and are not checked
// here, so a "yes" means "the user rule holds", not "this thread defers it". A name on always (the
// always-defer list, #878) is a "yes" without history, unless keep or the never-defer list holds it.
func Explain(h History, tenant, name string, keep, always []string) (defer_ bool, reason string) {
	// An MCP tool is decided with its whole server, as Decide does.
	key := name
	if srv := mcpServer(name); srv != "" {
		key = mcpPrefix + srv + "__"
	} else if strings.HasPrefix(name, mcpPrefix) {
		key = strings.TrimSuffix(name, "__") + "__"
	}
	switch {
	case NeverDefer(name):
		return false, "never deferred: a core tool"
	case inList(keep, name) || inList(keep, strings.TrimSuffix(key, "__")):
		return false, "on your keep list"
	case inList(always, name) || inList(always, strings.TrimSuffix(key, "__")):
		return true, "on your always-defer list"
	case h == nil:
		return false, "no tool history on this proxy yet"
	}
	past := h.UserSessions(tenant, "", UserSessions)
	if len(past) < UserSessions {
		return false, "fewer than 3 sessions of history"
	}
	if unusedIn(past, key) {
		return true, "declared and not used in your last 3 sessions"
	}
	for _, s := range past {
		if inSet(s.Used, key) {
			return false, "used or loaded in your last 3 sessions"
		}
	}
	return false, "not declared in each of your last 3 sessions"
}

func inList(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}
