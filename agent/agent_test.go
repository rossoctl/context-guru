package agent

import (
	"net/http"
	"testing"
)

func TestBaseNameIsEmpty(t *testing.T) {
	if got := (Base{}).Name(); got != "" {
		t.Fatalf("Name() = %q, want %q", got, "")
	}
}

func TestBaseClaimsNoPaths(t *testing.T) {
	if got := (Base{}).Paths(); got != nil {
		t.Fatalf("Paths() = %#v, want nil", got)
	}
}

func TestBaseSessionIDIsEmptyEvenWhenHostHeaderIsSet(t *testing.T) {
	h := http.Header{"X-Task-Id": {"task-1"}}
	if got := (Base{}).SessionID(h); got != "" {
		t.Fatalf("SessionID() = %q, want %q", got, "")
	}
}

func TestBaseReadUsageReturnsBaseUnchanged(t *testing.T) {
	base := Usage{FreshInput: 10, CacheRead: 20, CacheWrite: 30, CacheWrite1h: 4, Output: 5}
	reply := []byte(`{"usage":{"prompt_tokens":99,"prompt_tokens_details":{"cache_write_tokens":50}}}`)
	if got := (Base{}).ReadUsage("application/json", reply, base); got != base {
		t.Fatalf("ReadUsage() = %+v, want %+v", got, base)
	}
}

// sessionOnly overrides SessionID and nothing else.
type sessionOnly struct{ Base }

func (sessionOnly) SessionID(h http.Header) string { return h.Get("X-Task-Id") }

func TestEmbeddingBaseKeepsNeutralResultForEveryMethodNotOverridden(t *testing.T) {
	var a Agent = sessionOnly{}
	h := http.Header{"X-Task-Id": {"task-1"}}
	base := Usage{FreshInput: 1, CacheRead: 2, CacheWrite: 3, CacheWrite1h: 1, Output: 4}

	if got := a.SessionID(h); got != "task-1" {
		t.Errorf("SessionID() = %q, want the override %q", got, "task-1")
	}
	if got := a.Name(); got != "" {
		t.Errorf("Name() = %q, want %q", got, "")
	}
	if got := a.Paths(); got != nil {
		t.Errorf("Paths() = %#v, want nil", got)
	}
	if got := a.ReadUsage("text/event-stream", []byte("data: {}\n"), base); got != base {
		t.Errorf("ReadUsage() = %+v, want %+v", got, base)
	}
}
