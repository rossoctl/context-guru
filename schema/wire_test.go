package schema

import "testing"

func TestWireFor(t *testing.T) {
	for _, c := range []struct{ provider, api, want string }{
		{"openai", "", WireOpenAI},
		{"openai", "responses", WireResponses},
		{"openai", "/v1/responses", WireResponses},
		{"openai", "/v1/chat/completions", WireOpenAI},
		{"anthropic", "", WireAnthropic},
		{"anthropic", "/v1/messages", WireAnthropic},
	} {
		if got := WireFor(c.provider, c.api); got != c.want {
			t.Errorf("WireFor(%q,%q) = %q, want %q", c.provider, c.api, got, c.want)
		}
	}
}
