package bob

import (
	"testing"

	"github.com/rossoctl/context-guru/agent"
)

// Real usage blocks from Bob's model route (2026-11-04), counts only.
const (
	coldUsage = `{"cache_creation_input_tokens":20315,"cache_read_input_tokens":0,"cache_validity":false,` +
		`"completion_tokens":12,"credits":0.040662,"prompt_tokens":20319,` +
		`"prompt_tokens_details":{"cache_creation_token_details":{"ephemeral_1h_input_tokens":20157,` +
		`"ephemeral_5m_input_tokens":158},"cache_creation_tokens":20315,"cache_write_tokens":20315,` +
		`"cached_tokens":0,"text_tokens":4},"total_tokens":20331}`
	warmUsage = `{"cache_creation_input_tokens":99,"cache_read_input_tokens":20315,"cache_validity":true,` +
		`"completion_tokens":12,"prompt_tokens":20418,` +
		`"prompt_tokens_details":{"cache_creation_token_details":{"ephemeral_1h_input_tokens":0,` +
		`"ephemeral_5m_input_tokens":99},"cache_creation_tokens":99,"cache_write_tokens":99,` +
		`"cached_tokens":20315,"text_tokens":4},"total_tokens":20430}`
)

// What the shared parser makes of each block: it books every written token as fresh input.
var (
	coldBase = agent.Usage{FreshInput: 20319, Output: 12}
	warmBase = agent.Usage{FreshInput: 103, CacheRead: 20315, Output: 12}
)

func jsonReply(usage string) []byte {
	return []byte(`{"id":"c1","object":"chat.completion","choices":[],"usage":` + usage + `}`)
}

func TestReadUsageReadsTheColdWriteFromJSON(t *testing.T) {
	got := Bob{}.ReadUsage("application/json", jsonReply(coldUsage), coldBase)
	want := agent.Usage{FreshInput: 4, CacheWrite: 20315, CacheWrite1h: 20157, Output: 12}
	if got != want {
		t.Fatalf("ReadUsage(cold) = %+v, want %+v", got, want)
	}
}

func TestReadUsageReadsTheWarmWriteFromJSON(t *testing.T) {
	got := Bob{}.ReadUsage("application/json", jsonReply(warmUsage), warmBase)
	want := agent.Usage{FreshInput: 4, CacheRead: 20315, CacheWrite: 99, Output: 12}
	if got != want {
		t.Fatalf("ReadUsage(warm) = %+v, want %+v", got, want)
	}
}

// The usage block of an OpenAI chat event stream is the last chunk that has one.
func TestReadUsageReadsTheLastUsageChunkOfAStream(t *testing.T) {
	stream := []byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"ok"}}],"usage":{}}` + "\n\n" +
		`data: {"id":"c1","choices":[],"usage":` + warmUsage + `}` + "\n\n" +
		`data: {"id":"c1","choices":[],"usage":` + coldUsage + `}` + "\n\n" +
		"data: [DONE]\n\n")
	got := Bob{}.ReadUsage("text/event-stream; charset=utf-8", stream, coldBase)
	want := agent.Usage{FreshInput: 4, CacheWrite: 20315, CacheWrite1h: 20157, Output: 12}
	if got != want {
		t.Fatalf("ReadUsage(stream) = %+v, want %+v", got, want)
	}
}

// An empty or null usage block AFTER the real one must not win: only a block that carries counts
// is a reading (providerusage.Present). With the empty chunk first, as above, a wrong rule would still pass.
func TestReadUsageIgnoresAnEmptyUsageChunkAfterTheRealOne(t *testing.T) {
	for name, tail := range map[string]string{"empty": `{}`, "null": `null`} {
		stream := []byte(`data: {"id":"c1","choices":[],"usage":` + coldUsage + `}` + "\n\n" +
			`data: {"id":"c1","choices":[],"usage":` + tail + `}` + "\n\n" +
			"data: [DONE]\n\n")
		got := Bob{}.ReadUsage("text/event-stream", stream, coldBase)
		want := agent.Usage{FreshInput: 4, CacheWrite: 20315, CacheWrite1h: 20157, Output: 12}
		if got != want {
			t.Errorf("%s usage chunk last: ReadUsage = %+v, want the real block %+v", name, got, want)
		}
	}
}

func TestReadUsageFallsBackToCacheCreationFields(t *testing.T) {
	base := agent.Usage{FreshInput: 90, CacheRead: 10, Output: 1}
	for name, raw := range map[string]string{
		"cache_creation_tokens":       `{"prompt_tokens":100,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":10,"cache_creation_tokens":60}}`,
		"cache_creation_input_tokens": `{"prompt_tokens":100,"completion_tokens":1,"cache_creation_input_tokens":60,"prompt_tokens_details":{"cached_tokens":10}}`,
	} {
		got := Bob{}.ReadUsage("application/json", jsonReply(raw), base)
		want := agent.Usage{FreshInput: 30, CacheRead: 10, CacheWrite: 60, Output: 1}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}
}

func TestReadUsageReturnsBaseForAReplyItCannotRead(t *testing.T) {
	base := agent.Usage{FreshInput: 7, CacheRead: 3, Output: 2}
	for name, tc := range map[string]struct{ ct, reply string }{
		"not json":          {"application/json", `not json`},
		"no usage":          {"application/json", `{"choices":[]}`},
		"plain openai":      {"application/json", string(jsonReply(`{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3}}`))},
		"string write":      {"application/json", string(jsonReply(`{"prompt_tokens":10,"prompt_tokens_details":{"cache_write_tokens":"lots"}}`))},
		"negative write":    {"application/json", string(jsonReply(`{"prompt_tokens":10,"prompt_tokens_details":{"cache_write_tokens":-5}}`))},
		"write over prompt": {"application/json", string(jsonReply(`{"prompt_tokens":10,"prompt_tokens_details":{"cache_write_tokens":50}}`))},
		"1h over write":     {"application/json", string(jsonReply(`{"prompt_tokens":100,"prompt_tokens_details":{"cache_write_tokens":10,"cache_creation_token_details":{"ephemeral_1h_input_tokens":20}}}`))},
		"stream, no usage":  {"text/event-stream", "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"},
		"empty":             {"application/json", ``},
	} {
		if got := (Bob{}).ReadUsage(tc.ct, []byte(tc.reply), base); got != base {
			t.Errorf("%s: ReadUsage = %+v, want base %+v", name, got, base)
		}
	}
}
