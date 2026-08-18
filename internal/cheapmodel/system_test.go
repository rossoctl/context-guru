package cheapmodel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Anthropic backend must send the invariant preamble as a `system` block carrying a
// cache_control breakpoint, with the variable part left in the user message. Wrong shape
// = no caching, silently (issue #28 part A).
func TestAnthropicSendsCachedSystemBlock(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":900}}`)
	}))
	defer srv.Close()

	// Long enough to clear the model minimum, so the breakpoint is asked for. A SHORT
	// preamble deliberately gets no mark — see TestCacheBreakpointOnlyWhenItWouldCache.
	preamble := strings.Repeat("INVARIANT PREAMBLE. ", 1200)
	_, err := Anthropic{BaseURL: srv.URL, Model: "m"}.
		CompleteSystem(context.Background(), preamble, "VARIABLE PART")
	if err != nil {
		t.Fatal(err)
	}

	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("expected a 1-block system array, got %#v", body["system"])
	}
	blk := sys[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != preamble {
		t.Fatalf("system block must carry the preamble as text: %#v", blk)
	}
	cc, ok := blk["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Fatalf("system block must carry an ephemeral cache_control breakpoint: %#v", blk)
	}
	// The variable part must stay in the user message — putting it in the cached block
	// would make the prefix differ every call and cache nothing.
	msgs := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("expected exactly one user message, got %d", len(msgs))
	}
	if m := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "VARIABLE PART" {
		t.Fatalf("variable part must be the user message: %#v", m)
	}
}

// Complete (no system) must keep the original single-user-message shape, so nothing that
// relies on it changes behavior.
func TestAnthropicCompleteKeepsSingleMessageShape(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}]}`)
	}))
	defer srv.Close()

	if _, err := (Anthropic{BaseURL: srv.URL, Model: "m"}).Complete(context.Background(), "P"); err != nil {
		t.Fatal(err)
	}
	if _, present := body["system"]; present {
		t.Fatal("Complete without a system part must not send a system field")
	}
}

// The OpenAI backend has no explicit breakpoints, so it must degrade CLEANLY: a leading
// system message (the cacheable-prefix idiom there) and NO invented cache_control field,
// which the API would reject.
func TestOpenAIDegradesToLeadingSystemMessage(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OUT"}}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":80}}}`)
	}))
	defer srv.Close()

	if _, err := (OpenAI{BaseURL: srv.URL, Model: "m"}).
		CompleteSystem(context.Background(), "PREAMBLE", "VARIABLE"); err != nil {
		t.Fatal(err)
	}
	if _, present := body["system"]; present {
		t.Fatal("OpenAI must not send a top-level system field")
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("expected system+user messages, got %d", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "PREAMBLE" {
		t.Fatalf("preamble must be a LEADING system message: %#v", first)
	}
	if _, bad := first["cache_control"]; bad {
		t.Fatal("must not invent cache_control on the OpenAI backend")
	}
	if second := msgs[1].(map[string]any); second["role"] != "user" || second["content"] != "VARIABLE" {
		t.Fatalf("variable part must be the user message: %#v", second)
	}
}

// OpenAI counts cached tokens INSIDE prompt_tokens; Anthropic reports the tiers
// disjointly. Normalize, or the "fresh input" figure means different things per backend
// and the cost model silently double-counts.
func TestOpenAICachedTokensAreNotDoubleCounted(t *testing.T) {
	resetUsage()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OUT"}}],"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":80}}}`)
	}))
	defer srv.Close()

	if _, err := (OpenAI{BaseURL: srv.URL, Model: "m"}).Complete(context.Background(), "P"); err != nil {
		t.Fatal(err)
	}
	_, in, out := Usage()
	_, read := CacheUsage()
	if in != 20 { // 100 prompt - 80 cached
		t.Fatalf("fresh input tokens = %d, want 20 (cached excluded)", in)
	}
	if read != 80 {
		t.Fatalf("cache read tokens = %d, want 80", read)
	}
	if out != 5 {
		t.Fatalf("output tokens = %d, want 5", out)
	}
}

// A read of 0 across calls is the signal that a breakpoint is being silently ignored (the
// prefix is under the model's minimum cacheable length) — the measured reality on
// claude-haiku-4-5, whose minimum is 4096 tokens against our ~1463-token preamble. The
// accounting must make that visible rather than implying a win from placement alone.
func TestCacheReadZeroIsVisible(t *testing.T) {
	resetUsage()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Mirrors the gateway's real response for a sub-minimum prefix: no write, no read.
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":1808,"output_tokens":10}}`)
	}))
	defer srv.Close()

	for i := 0; i < 3; i++ {
		if _, err := (Anthropic{BaseURL: srv.URL, Model: "m"}).
			CompleteSystem(context.Background(), "SHORT PREAMBLE", "VAR"); err != nil {
			t.Fatal(err)
		}
	}
	write, read := CacheUsage()
	if write != 0 || read != 0 {
		t.Fatalf("sub-minimum prefix must record no cache activity, got write=%d read=%d", write, read)
	}
	calls, in, _ := Usage()
	if calls != 3 || in != 3*1808 {
		t.Fatalf("all input must be billed fresh: calls=%d in=%d", calls, in)
	}
	// AvgCallCost must reflect that: no cache benefit, full input price every call.
	avg, ok := AvgCallCost(HaikuPricing())
	if !ok || avg <= 0 {
		t.Fatalf("AvgCallCost must be observable and positive, got %v ok=%v", avg, ok)
	}
}

// resetUsage clears the process counters so usage assertions are independent.
func resetUsage() {
	llmCalls.Store(0)
	llmInputTokens.Store(0)
	llmOutputTokens.Store(0)
	llmCacheWrite.Store(0)
	llmCacheRead.Store(0)
}

// The breakpoint must be placed only when the provider would actually cache the prefix.
//
// This is a MEASURED boundary, not a style choice: a mark below the model's minimum
// cacheable prefix returns cache_creation_input_tokens: 0 with no error, so the old
// unconditional mark was inert on haiku-class models — and where it is NOT inert but never
// read, it is a 1.25x write paid for nothing. An unnameable model gets the conservative
// (larger) floor so we do not write a cache we cannot verify.
func TestCacheBreakpointOnlyWhenItWouldCache(t *testing.T) {
	short := strings.Repeat("word ", 200) // ~200 tokens
	mid := strings.Repeat("word ", 1500)  // ~1.5k tokens: the extractor's old preamble
	long := strings.Repeat("word ", 6000) // ~6k tokens: clears every floor
	for _, tc := range []struct {
		name, model, system string
		wantMark            bool
	}{
		{"haiku, short prefix", "claude-haiku-4-5", short, false},
		{"haiku, 1.5k prefix is below its 4096 minimum", "claude-haiku-4-5", mid, false},
		{"haiku, 6k prefix caches", "claude-haiku-4-5", long, true},
		{"sonnet, 1.5k prefix clears its 1024 minimum", "aws/claude-sonnet-5", mid, true},
		{"sonnet, short prefix", "claude-sonnet-5", short, false},
		{"unnameable model gets the conservative floor", "qwen3-coder-30b", mid, false},
		{"unnameable model, big prefix", "qwen3-coder-30b", long, true},
		{"empty model name is treated as unknown", "", mid, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &body)
				_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}]}`)
			}))
			defer srv.Close()
			if _, err := (Anthropic{BaseURL: srv.URL, Model: tc.model}).
				CompleteSystem(context.Background(), tc.system, "VARIABLE"); err != nil {
				t.Fatal(err)
			}
			sys, _ := body["system"].([]any)
			if len(sys) != 1 {
				t.Fatalf("expected one system block, got %#v", body["system"])
			}
			_, marked := sys[0].(map[string]any)["cache_control"]
			if marked != tc.wantMark {
				t.Fatalf("cache_control present = %v, want %v (model %q, ~%d tokens)",
					marked, tc.wantMark, tc.model, len(tc.system)/5)
			}
		})
	}
}

// Two ordered blocks must arrive as two blocks, in order, with the single breakpoint on
// the LAST one — the whole preamble is the prefix worth caching, and a mark in the middle
// would cache only the first half.
func TestCompleteBlocksKeepsOrderAndMarksTheLastBlock(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}]}`)
	}))
	defer srv.Close()

	general := strings.Repeat("general contract ", 1000)
	aggro := strings.Repeat("compaction target ", 300)
	if _, err := (Anthropic{BaseURL: srv.URL, Model: "aws/claude-sonnet-5"}).
		CompleteBlocks(context.Background(), []string{general, "", aggro}, "VARIABLE"); err != nil {
		t.Fatal(err)
	}
	sys, _ := body["system"].([]any)
	if len(sys) != 2 {
		t.Fatalf("expected 2 blocks (the blank one dropped), got %d: %#v", len(sys), body["system"])
	}
	if sys[0].(map[string]any)["text"] != general || sys[1].(map[string]any)["text"] != aggro {
		t.Fatal("block order changed; the shared half must come first or it is not a shared prefix")
	}
	if _, marked := sys[0].(map[string]any)["cache_control"]; marked {
		t.Fatal("the first block must not carry the breakpoint (that caches only half the preamble)")
	}
	if _, marked := sys[1].(map[string]any)["cache_control"]; !marked {
		t.Fatal("the last block must carry the breakpoint")
	}
}

// A blank-only system must omit the field entirely, leaving a request byte-identical to
// one that never had a system prompt (the API rejects an empty text block).
func TestBlankSystemSendsNoSystemField(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"OK"}]}`)
	}))
	defer srv.Close()
	if _, err := (Anthropic{BaseURL: srv.URL, Model: "m"}).
		CompleteBlocks(context.Background(), []string{"", "   "}, "VARIABLE"); err != nil {
		t.Fatal(err)
	}
	if _, present := body["system"]; present {
		t.Fatalf("system field present for a blank preamble: %#v", body["system"])
	}
}

// A cache entry that is only ever WRITTEN is worse than no breakpoint at all: the write costs
// 1.25x fresh input and buys nothing. Measured on a live session — two extraction calls in one
// request ran concurrently, so neither could read what neither had written yet, and both paid:
// cache_write=5228, cache_read=0.
//
// So the first call in flight takes the write slot and marks; its concurrent siblings do not.
// Once an entry demonstrably exists, every later call marks again, because then the mark is a
// READ.
func TestOnlyOneConcurrentCallPaysForTheCacheWrite(t *testing.T) {
	resetPrefixCache()
	t.Cleanup(resetPrefixCache)
	const model = "aws/claude-sonnet-5"
	prefix := []string{strings.Repeat("stable preamble ", 400)}

	// Nothing written yet: the first claim marks, a concurrent second does not.
	first, releaseFirst := systemBlocks(prefix, model)
	second, releaseSecond := systemBlocks(prefix, model)
	if !marked(first) {
		t.Fatal("the first call did not ask for the cache, so nothing is ever written")
	}
	if marked(second) {
		t.Fatal("a concurrent sibling also asked for the cache: both pay the 1.25x write " +
			"premium for the same entry, which is the measured waste this prevents")
	}

	// The write happened. Now a mark is a READ, so every call should carry one.
	releaseFirst(true, false)
	releaseSecond(false, false)
	third, _ := systemBlocks(prefix, model)
	if !marked(third) {
		t.Fatal("no mark after the entry exists, so the write is never amortised by a read")
	}

	// A DIFFERENT prefix is a different entry and must be claimed separately.
	other, _ := systemBlocks([]string{strings.Repeat("different preamble ", 400)}, model)
	if !marked(other) {
		t.Fatal("a distinct prefix was denied its own first write")
	}
}

// A claimed slot must be freed even when the call fails, or one transport error stops the
// prefix from ever being cached again for the life of the process.
func TestAFailedCallReleasesTheWriteSlot(t *testing.T) {
	resetPrefixCache()
	t.Cleanup(resetPrefixCache)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	sys := strings.Repeat("stable preamble ", 400)
	if _, err := (Anthropic{BaseURL: srv.URL, Model: "aws/claude-sonnet-5"}).
		CompleteSystem(context.Background(), sys, "x"); err == nil {
		t.Fatal("expected the 500 to be an error")
	}
	blocks, _ := systemBlocks([]string{sys}, "aws/claude-sonnet-5")
	if !marked(blocks) {
		t.Fatal("the write slot was still held after a failed call, so this prefix can " +
			"never be cached again")
	}
}

func marked(blocks []any) bool {
	if len(blocks) == 0 {
		return false
	}
	_, ok := blocks[len(blocks)-1].(map[string]any)["cache_control"]
	return ok
}

// Believing an entry exists must EXPIRE. The provider's ephemeral entry dies after a few
// minutes unused; a sticky `written` flag meant that after any idle gap every concurrent
// caller marked and each paid a full creation charge — the waste the protocol prevents,
// returning on the first burst after every quiet period.
func TestPrefixBeliefExpires(t *testing.T) {
	resetPrefixCache()
	t.Cleanup(resetPrefixCache)
	const model = "aws/claude-sonnet-5"
	prefix := []string{strings.Repeat("stable preamble ", 400)}

	first, release := systemBlocks(prefix, model)
	if !marked(first) {
		t.Fatal("the first call did not claim the write")
	}
	release(true, false) // the entry now exists

	// Age it past the TTL, as the provider would.
	prefixMu.Lock()
	for _, st := range prefixCache {
		st.at = st.at.Add(-2 * prefixEntryTTL)
	}
	prefixMu.Unlock()

	a, _ := systemBlocks(prefix, model)
	b, _ := systemBlocks(prefix, model)
	if !marked(a) {
		t.Fatal("no call re-claimed the write after the entry expired, so it is never re-cached")
	}
	if marked(b) {
		t.Fatal("both callers marked after expiry: back to paying twice for one entry")
	}
}
