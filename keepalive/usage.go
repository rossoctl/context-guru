package keepalive

import (
	"github.com/rossoctl/context-guru/internal/providerusage"
	"github.com/tidwall/gjson"
)

// PingUsage reads what a ping's response billed. Pings always send stream:false, so this is one
// JSON document, never a stream. Anthropic's input_tokens excludes the cached tiers; OpenAI
// Responses' input_tokens includes cached_tokens, so fresh is the difference. Both rules live in
// internal/providerusage, which the proxy's own response parser uses too.
//
// Reading the response is wire format, not I/O, which is why it lives in the core: every host
// hands back the raw body and the keeper decides what it billed.
func PingUsage(raw []byte) Usage {
	t, _ := providerusage.Parse(gjson.GetBytes(raw, "usage"))
	return Usage{FreshInput: t.FreshInput, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite,
		CacheWrite1h: t.CacheWrite1h, Output: t.Output,
		StopReason: gjson.GetBytes(raw, "stop_reason").String()}
}
