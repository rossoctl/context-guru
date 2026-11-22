package apply

import "testing"

// Anthropic's automatic caching takes a top-level cache_control. It is one more mark: alone and 1h, the body
// asks for 1h; a 5m root mark next to 1h block marks does not (issue #1272, gap 2).
func TestAllBreakpointsAskExtendedTTL_TopLevelMark(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       bool
	}{
		{"only a top-level 1h mark",
			`{"cache_control":{"type":"ephemeral","ttl":"1h"},"messages":[{"role":"user","content":"hi"}]}`, true},
		{"a top-level 5m mark next to a 1h block mark",
			`{"cache_control":{"type":"ephemeral"},"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`, false},
		{"a top-level 1h mark next to a 1h block mark",
			`{"cache_control":{"type":"ephemeral","ttl":"1h"},"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`, true},
	} {
		if got := AllBreakpointsAskExtendedTTL([]byte(c.body)); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
