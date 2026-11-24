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

func TestServerHeldHistoryHasTwoForms(t *testing.T) {
	for _, c := range []struct {
		body         string
		wide, narrow bool
	}{
		{`{"input":"x"}`, false, false},
		{`{"previous_response_id":"r1","input":"x"}`, true, true},
		{`{"conversation":"c1","input":"x"}`, true, false},
		{`{"conversation":{"id":"c1"},"input":"x"}`, true, false},
		{`{"conversation":"","input":"x"}`, false, false},
		{`{"input":[{"type":"item_reference","id":"i"}]}`, true, false},
		{`{"input":[{"type":"message"}]}`, false, false},
	} {
		if got := ServerHeldHistory([]byte(c.body), true); got != c.wide {
			t.Errorf("wide %s: %v want %v", c.body, got, c.wide)
		}
		if got := ServerHeldHistory([]byte(c.body), false); got != c.narrow {
			t.Errorf("narrow %s: %v want %v", c.body, got, c.narrow)
		}
	}
}
