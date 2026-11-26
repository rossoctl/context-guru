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

func TestServerHeldHistory(t *testing.T) {
	for _, c := range []struct {
		body string
		want bool
	}{
		{`{"input":"x"}`, false},
		{`{"previous_response_id":"r1","input":"x"}`, true},
		{`{"conversation":"c1","input":"x"}`, true},
		{`{"conversation":{"id":"c1"},"input":"x"}`, true},
		{`{"conversation":"","input":"x"}`, false},
		{`{"input":[{"type":"item_reference","id":"i"}]}`, true},
		{`{"input":[{"type":"message"}]}`, false},
	} {
		if got := ServerHeldHistory([]byte(c.body)); got != c.want {
			t.Errorf("%s: %v want %v", c.body, got, c.want)
		}
	}
}
