// Package providerusage reads the token tiers out of a provider's `usage` block.
//
// One rule, one place. Four hosts need it (the proxy, the keep-alive keeper, the cheap-model
// clients and the transcript reader), and each used to keep its own copy. The copies handled
// different dialects, so a gap showed up as a silent zero, not as an error.
//
// This package reads ONE block. It does not read a stream: the proxy merges the blocks of a
// server-sent-event stream itself, because that is transport.
package providerusage

import "github.com/tidwall/gjson"

// Tiers is one response's billed token counts.
//
// FreshInput, CacheRead and CacheWrite are disjoint. CacheWrite1h is a SUBSET of CacheWrite,
// never an addition to it.
type Tiers struct {
	FreshInput   int64
	CacheRead    int64
	CacheWrite   int64
	CacheWrite1h int64
	Output       int64
}

// Zero reports whether every count is zero.
func (t Tiers) Zero() bool {
	return t.FreshInput|t.CacheRead|t.CacheWrite|t.Output == 0
}

// Parse reads a `usage` object. ok is false when the object has no field of a known dialect.
//
// Dialects:
//
//	Anthropic        input_tokens (fresh only), cache_read_input_tokens,
//	                 cache_creation_input_tokens, cache_creation.ephemeral_1h_input_tokens
//	OpenAI Responses input_tokens (includes cache), input_tokens_details.{cached_tokens,
//	                 cache_write_tokens}
//	OpenAI chat      prompt_tokens (includes cache), prompt_tokens_details.cached_tokens
//	Output only      output_tokens alone. Anthropic's streaming message_delta carries exactly
//	                 this, and it holds the final completion count.
//
// OpenAI counts cached tokens INSIDE its input total, so fresh is the difference. Anthropic
// reports the tiers apart. Reading one as the other double-counts the whole transcript.
func Parse(u gjson.Result) (t Tiers, ok bool) {
	switch {
	case u.Get("input_tokens").Exists():
		input := u.Get("input_tokens").Int()
		t.Output = u.Get("output_tokens").Int()
		if u.Get("input_tokens_details.cached_tokens").Exists() || u.Get("input_tokens_details.cache_write_tokens").Exists() {
			// Responses includes both cache tiers in input_tokens.
			t.CacheRead = u.Get("input_tokens_details.cached_tokens").Int()
			t.CacheWrite = u.Get("input_tokens_details.cache_write_tokens").Int()
			t.FreshInput = max(input-t.CacheRead-t.CacheWrite, 0)
		} else {
			t.CacheRead = u.Get("cache_read_input_tokens").Int()
			t.FreshInput = input
			t.CacheWrite = u.Get("cache_creation_input_tokens").Int()
		}
		// The per-TTL split, when the provider reports one.
		t.CacheWrite1h = u.Get("cache_creation.ephemeral_1h_input_tokens").Int()
	case u.Get("prompt_tokens").Exists():
		t.Output = u.Get("completion_tokens").Int()
		t.CacheRead = u.Get("prompt_tokens_details.cached_tokens").Int()
		t.FreshInput = max(u.Get("prompt_tokens").Int()-t.CacheRead, 0)
	case u.Get("output_tokens").Exists():
		t.Output = u.Get("output_tokens").Int()
	default:
		return Tiers{}, false
	}
	return t, true
}

// Present reports whether a usage block is there: an object with at least one field. A JSON null
// or `{}` is absent, not a block in an unknown dialect. The proxy and agent/bob both use this
// rule, so the two always pick the same stream chunk.
func Present(v gjson.Result) bool {
	if !v.IsObject() {
		return false
	}
	empty := true
	v.ForEach(func(gjson.Result, gjson.Result) bool {
		empty = false
		return false // one field is enough
	})
	return !empty
}
