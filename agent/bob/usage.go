package bob

import (
	"strings"

	"github.com/rossoctl/context-guru/agent"
	"github.com/tidwall/gjson"
)

// ReadUsage reads the cache writes from a reply on Bob's model route: the top-level `usage` of a
// JSON body, or the last `usage` chunk of an OpenAI chat event stream.
func (Bob) ReadUsage(contentType string, reply []byte, base agent.Usage) agent.Usage {
	if !strings.Contains(contentType, "event-stream") {
		return readUsage(gjson.GetBytes(reply, "usage"), base)
	}
	var last gjson.Result
	for _, line := range strings.Split(string(reply), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if u := gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "usage"); present(u) {
			last = u
		}
	}
	return readUsage(last, base)
}

// readUsage corrects the shared parser's reading of a usage block from Bob's model route.
//
// Bob's gateway answers in OpenAI chat shape, but its block also carries the cache writes:
// prompt_tokens_details.cache_write_tokens and the per-TTL split under
// prompt_tokens_details.cache_creation_token_details. The host's shared parser reads that shape as
// plain OpenAI chat, so it books every written token as fresh input and sees no 1h write.
// The correction stays here, on Bob's route only, so no other provider's reading moves.
//
// base is what the shared parser produced. Any field that is absent or does not add up
// returns base unchanged (fail open).
func readUsage(u gjson.Result, base agent.Usage) agent.Usage {
	if !u.IsObject() {
		return base
	}
	prompt, ok := count(u.Get("prompt_tokens"))
	if !ok {
		return base
	}
	var write int64
	found := false
	for _, p := range []string{"prompt_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_creation_tokens", "cache_creation_input_tokens"} {
		if v := u.Get(p); v.Exists() {
			if write, ok = count(v); !ok {
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
		if write1h, ok = count(v); !ok || write1h > write {
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

// count is a non-negative whole token count, or ok=false.
func count(v gjson.Result) (int64, bool) {
	if v.Type != gjson.Number || v.Num < 0 || v.Num != float64(int64(v.Num)) {
		return 0, false
	}
	return int64(v.Num), true
}

// present is the host's test for a usage block that is there: an object with at least one field.
// A chunk with `"usage":{}` or `"usage":null` does not replace the last real block.
func present(v gjson.Result) bool {
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
