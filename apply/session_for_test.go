package apply

import (
	"net/http"
	"testing"

	"github.com/rossoctl/context-guru/agent"
)

type panicAgent struct{ agent.Base }

func (panicAgent) SessionID(http.Header) string { panic("boom") }

type idAgent struct{ agent.Base }

func (idAgent) SessionID(h http.Header) string { return h.Get("X-Task-Id") }

func TestExplicitSessionForOrder(t *testing.T) {
	h := http.Header{}
	h.Set("User-Agent", "codex_cli_rs/1")
	h.Set("session-id", "codex-1")
	h.Set("X-Task-Id", "task-1")
	cl := agent.Identify(h)
	body := []byte(`{}`)
	if got := ExplicitSessionFor(h, nil, cl, body); got != "codex-1" {
		t.Errorf("client id: got %q", got)
	}
	if got := ExplicitSessionFor(h, idAgent{}, cl, body); got != "task-1" {
		t.Errorf("route agent beats the client id: got %q", got)
	}
	h.Set(SessionHeader, "hdr-1")
	if got := ExplicitSessionFor(h, idAgent{}, cl, body); got != "hdr-1" {
		t.Errorf("header wins: got %q", got)
	}
	h.Del(SessionHeader)
	if got := ExplicitSessionFor(h, panicAgent{}, cl, body); got != "codex-1" {
		t.Errorf("a panicking route agent falls back to the client id: got %q", got)
	}
	if got := ExplicitSessionFor(nil, nil, agent.Identify(nil), body); got != "" {
		t.Errorf("no headers: got %q", got)
	}
}
