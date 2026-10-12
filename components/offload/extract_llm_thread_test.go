package offload

import (
	"testing"

	"github.com/rossoctl/context-guru/components"
)

// The growth rate is a thread's own: a short subagent request between two main-thread requests
// must not read as the main thread shrinking and then growing again (#484). Keyed by session, the
// main thread's second request compared 120,000 tokens against the subagent's 4,000.
func TestExtractLLMGrowthIsPerThread(t *testing.T) {
	c, err := newExtractLLM(nil)
	if err != nil {
		t.Fatal(err)
	}
	e := c.(*ExtractLLM)
	main := &components.Ctx{Session: "s"}
	sub := &components.Ctx{Session: "s", Thread: "agent:sub"}

	if prev := e.noteRequestSize(main, 100_000); prev != 0 {
		t.Fatalf("first main request: prev = %d, want 0", prev)
	}
	if prev := e.noteRequestSize(sub, 4_000); prev != 0 {
		t.Fatalf("first subagent request saw the main thread's size %d", prev)
	}
	if prev := e.noteRequestSize(main, 120_000); prev != 100_000 {
		t.Fatalf("second main request: prev = %d, want the main thread's own 100000", prev)
	}
	// The primary thread keys by the bare session, as before threads existed.
	if prev := e.noteRequestSize(&components.Ctx{Session: "s"}, 1); prev != 120_000 {
		t.Fatalf("primary thread key moved: prev = %d", prev)
	}
}
