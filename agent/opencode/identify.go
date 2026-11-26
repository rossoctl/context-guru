// Package opencode holds OpenCode's header rules. They are plain functions of the request headers,
// so any host can call them. agent/client calls Identify.
package opencode

import (
	"net/http"
	"strings"
)

// Fields are the ids OpenCode sends in headers.
type Fields struct {
	// SessionID is x-opencode-session-id.
	SessionID string
	// ParentSessionID is x-opencode-parent-session-id: set only on a subagent's session.
	ParentSessionID string
}

// Identify reads OpenCode's ids from h. ok is true when OpenCode sent one of its headers. A nil
// header gives the zero Fields and false.
func Identify(h http.Header) (f Fields, ok bool) {
	f = Fields{
		SessionID:       strings.TrimSpace(h.Get("x-opencode-session-id")),
		ParentSessionID: strings.TrimSpace(h.Get("x-opencode-parent-session-id")),
	}
	return f, f != (Fields{})
}
