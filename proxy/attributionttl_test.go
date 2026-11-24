package proxy

import "testing"

// #1350. After a free 1-hour ping the entry lives an hour, so a miss on the next request must be judged
// against 1 hour, not the 5 minutes its last write was billed at.
func TestAttributionTTLCountsAFreeOneHourPing(t *testing.T) {
	const prov, model = "anthropic", "claude-sonnet-5"
	c := &capture{}
	if got := c.attributionTTLMs(prov, model, false); got != 300_000 {
		t.Fatalf("no ping: ttl = %d ms, want 300000", got)
	}
	c.noteKeepAlive(kaArrival{refreshedOneHour: true})
	if got := c.attributionTTLMs(prov, model, false); got != 3_600_000 {
		t.Errorf("after a free 1-hour ping: ttl = %d ms, want 3600000", got)
	}
	// OpenAI GPT-5.6+: the minimum is 30 minutes whatever tier a touch names, and it is not an exact lifetime.
	if got := c.attributionTTLMs("openai", "gpt-5.6", false); got != 0 {
		t.Errorf("a minimum lifetime must not be attributed as an expiry: got %d", got)
	}
}
