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
		{"mark is not an object", `{"messages":[{"content":[{"cache_control":null}]}]}`, 0},
		{"both spellings on one block", `{"messages":[{"content":[{"cache_control":` + ce + `,"cachePoint":{}}]}]}`, 2},
	} {
		if got := CountBreakpoints([]byte(c.body)).Total(); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
		if got := CountBreakpointsEscaped([]byte(c.body)).Total(); got != c.want {
			t.Errorf("%s (escaped count): %d, want %d", c.name, got, c.want)
		}
	}
}

// A key spelled with a \u escape: only the exact count sees it. The hot-path count must not
// (its prefilter is "cache" only, on purpose), and a \u in unrelated text must not make it parse.
func TestCountBreakpointsEscapedKey(t *testing.T) {
	ce := `{"type":"ephemeral"}`
	for _, c := range []struct{ name, body string }{
		{"escaped cache_control", `{"messages":[{"content":[{"type":"text","c\u0061che_control":` + ce + `}]}]}`},
		{"escaped cachePoint", `{"messages":[{"content":[{"c\u0061chePoint":{"type":"default"}}]}]}`},
		{"escaped in system", `{"system":[{"c\u0061che_control":` + ce + `}],"messages":[]}`},
		{"escaped on message", `{"messages":[{"content":"x","c\u0061che_control":` + ce + `}]}`},
	} {
		if got := CountBreakpointsEscaped([]byte(c.body)).Total(); got != 1 {
			t.Errorf("%s: escaped count %d, want 1", c.name, got)
		}
		if got := CountBreakpoints([]byte(c.body)).Total(); got != 0 {
			t.Errorf("%s: hot-path count %d, want 0 (cache-only prefilter)", c.name, got)
		}
	}
	if MayHoldBreakpoint(`{"text":"a \u003c b"}`) {
		t.Error("MayHoldBreakpoint accepts a bare \\u escape; the hot path would parse most tool output")
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

// #1279: the top-level cache_control of automatic caching is one breakpoint, in Root.
func TestCountBreakpointsCountsTheRootMark(t *testing.T) {
	body := []byte(`{"cache_control":{"type":"ephemeral","ttl":"1h"},` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`)
	got := CountBreakpoints(body)
	if want := (Breakpoints{Blocks: 1, Root: 1}); got != want || got.Total() != 2 {
		t.Errorf("CountBreakpoints = %+v (total %d), want %+v", got, got.Total(), want)
	}
	if CountBreakpoints([]byte(`{"cache_control":"x","messages":[]}`)).Total() != 0 {
		t.Error("a non-object root cache_control counted")
	}
}
