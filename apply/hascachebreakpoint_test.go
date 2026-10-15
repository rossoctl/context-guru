package apply

import "testing"

// The substring prefilter in hasCacheBreakpoint must never turn a body that has a breakpoint
// into one that does not, including when the key is spelled with a JSON escape.
func TestHasCacheBreakpointPrefilterIsExact(t *testing.T) {
	cases := []struct {
		name, body string
		want       bool
	}{
		{"no marks", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"block mark", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`, true},
		{"message mark", `{"messages":[{"role":"user","content":"hi","cache_control":{"type":"ephemeral"}}]}`, true},
		{"system mark", `{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[]}`, true},
		{"tools mark", `{"tools":[{"name":"t","cache_control":{"type":"ephemeral"}}],"messages":[]}`, true},
		{"bedrock mark", `{"messages":[{"role":"user","content":[{"cachePoint":{"type":"default"}}]}]}`, true},
		{"escaped key", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi","c\u0061che_control":{"type":"ephemeral"}}]}]}`, true},
		{"only mentioned in text", `{"messages":[{"role":"user","content":"the cache_control field"}]}`, false},
	}
	for _, c := range cases {
		if got := hasCacheBreakpoint([]byte(c.body)); got != c.want {
			t.Errorf("%s: hasCacheBreakpoint = %v, want %v", c.name, got, c.want)
		}
	}
}
