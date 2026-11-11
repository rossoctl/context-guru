package proxy

import (
	"context"
	"fmt"
	"strings"

	"github.com/rossoctl/context-guru/agent"
	"github.com/rossoctl/context-guru/internal/logging"
)

// agentModelTarget is a.ModelTarget, fail open: a panic gives the zero target (the host default),
// and a field the host cannot use safely is dropped, so that field keeps the host default.
func agentModelTarget(ctx context.Context, a agent.Agent) (t agent.ModelTarget) {
	defer func() {
		if rec := recover(); rec != nil {
			// The panic's type only: its message could hold a header value.
			logging.From(ctx).Error("context-guru: recovered from panic in the agent's model target; using the host default",
				"agent", fmt.Sprintf("%T", a), "panic", fmt.Sprintf("%T", rec))
			t = agent.ModelTarget{}
		}
	}()
	t = a.ModelTarget()
	if !safeTargetPath(t.Path) {
		t.Path = ""
	}
	if !httpToken(t.AuthScheme) {
		t.AuthScheme = ""
	}
	return t
}

// safeTargetPath accepts an absolute path on the route's own upstream: it starts with one "/" (a
// "//" would name another host) and has no space, control character, query or fragment.
func safeTargetPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '?' || r == '#' })
}

// httpToken reports whether s is one HTTP token (RFC 9110), the shape of an auth scheme. A space or
// a line break in a scheme could forge a header.
func httpToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}
