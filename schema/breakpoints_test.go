package schema

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

func TestCountBreakpointsMatchesTheCapRule(t *testing.T) {
	body := []byte(`{"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"s","cachePoint":{"type":"default"}}],` +
		`"messages":[{"role":"user","content":"u","cache_control":{"type":"ephemeral"}},` +
		`{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}},{"type":"text","text":"y"}]}]}`)
	got := CountBreakpoints(body)
	if want := (Breakpoints{System: 1, Tools: 1, Messages: 1, Blocks: 1}); got != want {
		t.Errorf("CountBreakpoints = %+v, want %+v", got, want)
	}
	if got.Total() > MaxBreakpoints {
		t.Errorf("total %d over cap %d", got.Total(), MaxBreakpoints)
	}
}

// Each table row is a form that one of the old copies counted and another missed.
func TestCountBreakpointsEveryForm(t *testing.T) {
	ce := `{"type":"ephemeral"}`
	for _, c := range []struct {
		name, body string
		want       int
	}{
		{"text names the field", `{"messages":[{"role":"user","content":"the cache_control field"}]}`, 0},
		{"tool_result text names it", `{"messages":[{"role":"user","content":[{"type":"tool_result","content":"cache_control cachePoint"}]}]}`, 0},
		{"system cachePoint", `{"system":[{"cachePoint":{"type":"default"}}],"messages":[]}`, 1},
		{"tools cachePoint", `{"tools":[{"cachePoint":{"type":"default"}}],"messages":[]}`, 1},
		{"block cachePoint", `{"messages":[{"content":[{"cachePoint":{"type":"default"}}]}]}`, 1},
		{"message-level cachePoint is not a mark", `{"messages":[{"content":"x","cachePoint":{"type":"default"}}]}`, 0},
		{"escaped key", `{"messages":[{"content":[{"type":"text","cache_control":` + ce + `}]}]}`, 1},
		{"mark is not an object", `{"messages":[{"content":[{"cache_control":null}]}]}`, 0},
		{"both spellings on one block", `{"messages":[{"content":[{"cache_control":` + ce + `,"cachePoint":{}}]}]}`, 2},
	} {
		if got := CountBreakpoints([]byte(c.body)).Total(); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestHasBlockBreakpoint(t *testing.T) {
	if HasBlockBreakpoint(nil) || HasBlockBreakpoint(&bschemas.ChatMessage{}) {
		t.Error("nil message or content reported a mark")
	}
	m := &bschemas.ChatMessage{Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{{}, {CacheControl: &bschemas.CacheControl{}}}}}
	if !HasBlockBreakpoint(m) {
		t.Error("marked block not seen")
	}
}
