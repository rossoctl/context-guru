package keepalive

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/tidwall/sjson"
)

// pingBody turns the last real request into the cheapest accepted cache READ of the
// same prefix. Responses uses max_output_tokens:16 because the IBM LiteLLM
// Azure route rejects smaller values; Anthropic Messages uses max_tokens:1.
//
// Only two fields change, and NEITHER is inside the hashed prefix. Anthropic hashes
// `tools` → `system` → `messages` cumulatively up to the breakpoint; `max_tokens` and
// `stream` are request-level knobs outside that sequence, so the prefix stays byte-identical
// and the ping matches the entry the real request wrote. Everything a change to WOULD
// invalidate the prefix — the model, the tools, `tool_choice`, the thinking parameters, any
// system or message content — is left exactly as it was sent.
//
// max_tokens is 1 rather than 0, and the reason is now MEASURED rather than assumed.
//
// `max_tokens: 0` is the provider's documented cache pre-warm shape, and on THIS gateway it is
// MODEL-DEPENDENT, which is the whole reason 1 wins:
//
//   - aws/claude-sonnet-5: 200, `cache_read_input_tokens: 5851`, `cache_creation_input_tokens: 0`,
//     `output_tokens: 0` — a full cache read for no output at all.
//   - claude-haiku-4-5:    **400**, `max_tokens: must be greater than or equal to 1`.
//
// So the cheaper shape is real on some routes and a hard failure on others, and the routing is not
// this function's to know: `model` comes from the caller's own body. A ping that 400s is a cache
// entry allowed to lapse, which is the one outcome this request exists to prevent.
//
// The prize for taking that risk is ONE output token: $0.0000076 against the $0.0073304 the read
// itself costs on a 48k-token prefix, i.e. 0.1% of a ping. The docs additionally list four bodies
// 0 is rejected for — `stream: true`, extended thinking, structured outputs, and `tool_choice` of
// type `tool` or `any`. This ping forces `stream: false` and refuses thinking-enabled sessions, but
// it resends the caller's body verbatim and does not touch `tool_choice`, because touching it would
// change the hashed prefix and miss the entry. 0.1% does not buy that many ways to fail.
//
// stream is false so the response is one small JSON body with its usage block in it, rather
// than an SSE stream this would have to read to the end to price.
func pingOutputBudget(route string) int {
	if route == "/v1/responses" {
		// The IBM LiteLLM Azure route rejects max_output_tokens below 16.
		return 16
	}
	return 1
}

func pingBody(body []byte, route string) ([]byte, bool) { return pingBodyN(body, route, 0) }

// pingVary is how many distinct output budgets the pings of one entry cycle through.
const pingVary = 8

// pingBodyN is pingBody for the entry's n-th ping (0 for the first): its output budget is the base
// plus n mod pingVary, so no two pings in a row on one entry are byte-identical.
//
// IDENTICAL PINGS NEVER REACH THE PROVIDER (#706). The IBM LiteLLM gateway keeps a response cache
// keyed on the request, and answers a byte-identical request from it: the same response id, the
// same usage (a "cache read" that never happened), and no call to the provider, so the provider's
// prompt cache was not refreshed. The first ping always differs from the real turn it follows (the
// turn asks for thousands of output tokens), so it got through; the second was a copy of the first,
// and the prefix expired 5 minutes after the first. Live: after 1 ping the next turn missed 0 of
// 146 times, after 2 pings 50 of 67 times. Measured on the gateway: `max_tokens`, `temperature`
// and `stop_sequences` each change its cache key, and `metadata.user_id` does not.
//
// The output budget is the field to vary because the ping already sets it and it is outside the
// prefix the provider hashes, so the ping is still a pure cache read. A larger budget costs nothing
// on its own: the reply to a keep-alive ping is one token, and output is billed as generated.
func pingBodyN(body []byte, route string, n int) ([]byte, bool) {
	field := "max_tokens"
	if route == "/v1/responses" {
		field = "max_output_tokens"
	}
	if n < 0 {
		n = 0
	}
	out, err := sjson.SetBytes(body, field, pingOutputBudget(route)+n%pingVary)
	if err != nil {
		return nil, false
	}
	out, err = sjson.SetBytes(out, "stream", false)
	if err != nil {
		return nil, false
	}
	return out, true
}

// pingTimeout bounds one ping. Generous relative to what a max_tokens:1 answer
// takes and far short of the request path's own header timeout.
const pingTimeout = 60 * time.Second

// reserveFrac is the share of a tenant's rate and concurrency budget a ping may
// never touch. A quarter, so a tenant at three quarters of its limit stops being pinged
// while its agent keeps working — the requirement is that a ping never crowds out a real
// request, and the only way to honour that with a fixed-window counter is to stay away from
// the edge of it.
const reserveFrac = 0.25

// eligible only admits cache rules whose lifetime and ping wire shape
// we know. OpenAI Responses uses implicit caching on GPT-5.6+; earlier models
// have model/retention-dependent lifetimes and are not safe to schedule here.
func eligible(p bschemas.ModelProvider, model, route string) bool {
	switch p {
	case bschemas.Anthropic, bschemas.Bedrock, bschemas.BedrockMantle, bschemas.Vertex:
		return true
	case bschemas.OpenAI:
		return route == "/v1/responses" && modelinfo.OpenAI30MinuteCache(string(p), model)
	}
	return false
}
