package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// bobUsage corrects the shared parser's reading of a usage block from Bob's model route.
//
// Bob's gateway answers in OpenAI chat shape, but its block also carries the cache writes:
// prompt_tokens_details.cache_write_tokens and the per-TTL split under
// prompt_tokens_details.cache_creation_token_details. providerusage.Parse reads that shape as
// plain OpenAI chat, so it books every written token as fresh input and sees no 1h write.
// The correction stays here, on Bob's route only, so no other provider's reading moves.
//
// base is what the shared parser produced. Any field that is absent or does not add up
// returns base unchanged (fail open).
func bobUsage(u gjson.Result, base Usage) Usage {
	if !u.IsObject() {
		return base
	}
	prompt, ok := bobCount(u.Get("prompt_tokens"))
	if !ok {
		return base
	}
	var write int64
	found := false
	for _, p := range []string{"prompt_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_creation_tokens", "cache_creation_input_tokens"} {
		if v := u.Get(p); v.Exists() {
			if write, ok = bobCount(v); !ok {
				return base
			}
			found = true
			break
		}
	}
	if !found {
		return base
	}
	var write1h int64
	if v := u.Get("prompt_tokens_details.cache_creation_token_details.ephemeral_1h_input_tokens"); v.Exists() {
		if write1h, ok = bobCount(v); !ok || write1h > write {
			return base
		}
	}
	if base.CacheRead+write > prompt {
		return base
	}
	out := base
	out.CacheWrite, out.CacheWrite1h = write, write1h
	out.FreshInput = prompt - base.CacheRead - write
	return out
}

// bobCount is a non-negative whole token count, or ok=false.
func bobCount(v gjson.Result) (int64, bool) {
	if v.Type != gjson.Number || v.Num < 0 || v.Num != float64(int64(v.Num)) {
		return 0, false
	}
	return int64(v.Num), true
}

// bobResponseUsage applies bobUsage to a whole response: the top-level `usage` of a JSON body,
// or the last `usage` chunk of an OpenAI chat event stream.
func bobResponseUsage(contentType string, body []byte, base Usage) Usage {
	if !strings.Contains(contentType, "event-stream") {
		return bobUsage(gjson.GetBytes(body, "usage"), base)
	}
	var last gjson.Result
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if u := gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "usage"); usagePresent(u) {
			last = u
		}
	}
	return bobUsage(last, base)
}
