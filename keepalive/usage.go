package keepalive

import "github.com/tidwall/gjson"

// PingUsage reads what a ping's response billed. Pings always send stream:false, so this is one
// JSON document, never a stream. Anthropic's input_tokens excludes the cached tiers; OpenAI
// Responses' input_tokens includes cached_tokens, so fresh is the difference — the rule the proxy's
// own response parser applies (TestPingUsageMatchesResponseUsage in proxy pins the two equal).
//
// Reading the response is wire format, not I/O, which is why it lives in the core: every host
// hands back the raw body and the keeper decides what it billed.
func PingUsage(raw []byte) Usage {
	u := gjson.GetBytes(raw, "usage")
	cached, wrote := u.Get("input_tokens_details.cached_tokens"), u.Get("input_tokens_details.cache_write_tokens")
	if cached.Exists() || wrote.Exists() {
		in, c, w := u.Get("input_tokens").Int(), cached.Int(), wrote.Int()
		return Usage{FreshInput: max(in-c-w, 0), CacheRead: c, CacheWrite: w,
			Output: u.Get("output_tokens").Int()}
	}
	return Usage{
		FreshInput:   u.Get("input_tokens").Int(),
		CacheRead:    u.Get("cache_read_input_tokens").Int(),
		CacheWrite:   u.Get("cache_creation_input_tokens").Int(),
		CacheWrite1h: u.Get("cache_creation.ephemeral_1h_input_tokens").Int(),
		Output:       u.Get("output_tokens").Int(),
		StopReason:   gjson.GetBytes(raw, "stop_reason").String(),
	}
}
