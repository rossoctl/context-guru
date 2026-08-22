package apply

import (
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// TestTTLTierNamesTheLifetimeTheBodyAsked pins the value that cacheTTL always computed and
// always discarded, and which the dashboard now stores.
//
// The third answer is the one that matters: a body with no cache_control at all is NOT a
// 5-minute-TTL request, it is an uncached one, and folding it into the 5m bucket would report
// prompt caching on traffic that does none.
func TestTTLTierNamesTheLifetimeTheBodyAsked(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"no cache_control anywhere is not a 5m request",
			`{"messages":[{"role":"user","content":"hi"}]}`, ""},
		{"a plain breakpoint is the default 5m tier",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral"}}]}]}`,
			TTLEphemeral5m},
		{"an explicit 1h ttl is the extended tier",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"x","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`,
			TTLEphemeral1h},
		{"a 1h breakpoint on the system block is found too",
			`{"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`,
			TTLEphemeral1h},
		// Structural, not textual: the literal "1h" inside tool output must not extend our
		// idea of the cache lifetime. Same reason hasCacheBreakpoint is structural.
		{"the text 1h in content does not make it a 1h request",
			`{"messages":[{"role":"user","content":[{"type":"text","text":"ttl is 1h","cache_control":{"type":"ephemeral"}}]}]}`,
			TTLEphemeral5m},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ttlTier(bschemas.Anthropic, []byte(tc.body)); got != tc.want {
				t.Errorf("ttlTier = %q, want %q", got, tc.want)
			}
		})
	}
}
