package keepalive

import (
	"github.com/rossoctl/context-guru/providerusage"
	"github.com/rossoctl/context-guru/respfacts"
	"github.com/tidwall/gjson"
)

// PingUsage reads what a ping's response billed. Pings always send stream:false, so this is one
// JSON document, never a stream. Anthropic's input_tokens excludes the cached tiers; OpenAI
// Responses' input_tokens includes cached_tokens, so fresh is the difference. Both rules live in
// providerusage, which the proxy's own response parser uses too.
//
// The stop reason is respfacts.StopReason: Anthropic stop_reason, OpenAI finish_reason, or a
// Responses incomplete reason.
//
// Reading the response is wire format, not I/O, which is why it lives in the core: every host
// hands back the raw body and the keeper decides what it billed.
func PingUsage(raw []byte) Usage {
	t, _ := providerusage.Parse(gjson.GetBytes(raw, "usage"))
	return Usage{FreshInput: t.FreshInput, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite,
		CacheWrite1h: t.CacheWrite1h, Output: t.Output,
		StopReason: respfacts.StopReason(false, raw)}
}
