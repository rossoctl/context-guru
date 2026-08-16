package apply

import (
	"testing"

	"github.com/tidwall/sjson"
)

// The metadata-write exception must be exactly that: added cache_control keys and
// nothing else. Anything wider would put us back to splicing a lossy re-marshal
// over a message bifrost cannot round-trip.
func TestMetadataOnlyWritesRejectsNonMetadataChanges(t *testing.T) {
	pre := `{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use"}]}`
	cases := []struct {
		name, post string
		want       bool
	}{
		{"added cache_control", `{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","cache_control":{"type":"ephemeral"}}]}`, true},
		{"two added", `{"role":"assistant","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}},{"type":"tool_use","cache_control":{"type":"ephemeral"}}]}`, true},
		{"no change", pre, false},
		{"text edited too", `{"role":"assistant","content":[{"type":"text","text":"HELLO"},{"type":"tool_use","cache_control":{"type":"ephemeral"}}]}`, false},
		{"block removed", `{"role":"assistant","content":[{"type":"tool_use","cache_control":{"type":"ephemeral"}}]}`, false},
		{"role changed", `{"role":"user","content":[{"type":"text","text":"hello"},{"type":"tool_use","cache_control":{"type":"ephemeral"}}]}`, false},
		{"string content", `{"role":"assistant","content":"hello"}`, false},
	}
	for _, tc := range cases {
		if _, ok := metadataOnlyWrites([]byte(pre), []byte(tc.post)); ok != tc.want {
			t.Errorf("%s: ok=%v want %v", tc.name, ok, tc.want)
		}
	}
}

// applyMetaWrites must refuse when the raw body's block layout disagrees with what
// the writes assume, so a key can never land on the wrong block.
func TestApplyMetaWritesRefusesOnShapeMismatch(t *testing.T) {
	msg := []byte(`{"role":"assistant","content":[{"type":"tool_use","id":"t"}]}`)
	w := []metaWrite{{path: "content.1.cache_control", raw: `{"type":"ephemeral"}`}}
	if _, ok := applyMetaWrites(msg, 2, w); ok {
		t.Fatal("expected a refusal: the raw message has 1 block, the writes assume 2")
	}
	// Never overwrite a breakpoint the caller already set.
	set := []byte(`{"role":"assistant","content":[{"type":"tool_use","cache_control":{"type":"ephemeral","ttl":"1h"}}]}`)
	w = []metaWrite{{path: "content.0.cache_control", raw: `{"type":"ephemeral"}`}}
	if _, ok := applyMetaWrites(set, 1, w); ok {
		t.Fatal("expected a refusal: the caller's own cache_control must not be overwritten")
	}
}

// Bedrock spells the breakpoint `cachePoint` and puts it as its OWN entry in `system`
// and `tools` — the two locations defect 2 is about. Missing those paths left the cap
// breachable on Bedrock exactly as it was on Anthropic.
func TestBedrockCachePointCounted(t *testing.T) {
	body := []byte(`{
		"system":[{"text":"a"},{"cachePoint":{"type":"default"}},{"text":"b"},{"cachePoint":{"type":"default"}}],
		"tools":[{"toolSpec":{"name":"x"}},{"cachePoint":{"type":"default"}}],
		"messages":[{"role":"user","content":[{"text":"hi"},{"cachePoint":{"type":"default"}}]}]}`)
	// 2 in system + 1 in tools + 1 in message content = 4, all of them at the cap. The
	// pre-fix counter saw only the content one, so it believed 3 slots were free.
	if got := wireBreakpoints(body); got != 4 {
		t.Fatalf("wireBreakpoints = %d, want 4 (2 system + 1 tool + 1 content); a Bedrock cap breach would go unseen", got)
	}
}

func TestBreakpointCounting(t *testing.T) {
	body := []byte(`{
		"system":[{"type":"text","cache_control":{"type":"ephemeral"}},{"type":"text","cache_control":{"type":"ephemeral"}},{"type":"text"}],
		"tools":[{"name":"x"},{"name":"y","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"user","content":"a tool output that merely mentions cache_control in its text"},
			{"role":"assistant","content":[{"type":"tool_use","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","cache_control":{"type":"ephemeral"},"content":"q"}
		]}`)
	if got := wireBreakpoints(body); got != 5 {
		t.Errorf("wireBreakpoints = %d, want 5", got)
	}
}

// A request that arrives already over the cap is the client's problem — we forward it
// untouched. Shouting ERROR about it would blame context-guru for a request it never
// changed, so the breach check compares against the inbound count. Asserted at the
// condition level: the log call itself is not observable from here.
func TestBreachIsOursOnly(t *testing.T) {
	over := []byte(`{"system":[
		{"type":"text","cache_control":{"type":"ephemeral"}},{"type":"text","cache_control":{"type":"ephemeral"}},
		{"type":"text","cache_control":{"type":"ephemeral"}},{"type":"text","cache_control":{"type":"ephemeral"}},
		{"type":"text","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`)
	inbound := wireBreakpoints(over)
	if inbound != 5 {
		t.Fatalf("fixture should carry 5 inbound breakpoints, got %d", inbound)
	}
	// Unchanged body: over the cap, but out == inbound, so it is not ours to report.
	if out := wireBreakpoints(over); out > maxWireBreakpoints && out > inbound {
		t.Fatal("a client-caused breach we did not add to must not be reported as ours")
	}
	// If we DID add one, it must be reported.
	worse, err := sjson.SetRawBytes(over, "messages.0.cache_control", []byte(`{"type":"ephemeral"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out := wireBreakpoints(worse); !(out > maxWireBreakpoints && out > inbound) {
		t.Fatalf("we added a breakpoint over the cap (%d -> %d); that must be reported", inbound, out)
	}
}
