package claudecode

import (
	"net/http"
	"testing"
)

func TestAgentIDIsTheTrimmedHeader(t *testing.T) {
	if got := AgentID(nil); got != "" {
		t.Fatalf("nil header: AgentID = %q, want empty", got)
	}
	h := http.Header{}
	if got := AgentID(h); got != "" {
		t.Fatalf("no header: AgentID = %q, want empty", got)
	}
	h.Set("x-claude-code-agent-id", " sub1 ")
	if got := AgentID(h); got != "sub1" {
		t.Fatalf("AgentID = %q, want sub1", got)
	}
}
