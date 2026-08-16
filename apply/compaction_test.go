package apply_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/modes"
	"github.com/rossoctl/context-guru/store"
)

// The agent's own compaction (Claude Code auto-compact / Bob Shell compression) replaces
// the transcript with a summary and keeps going under the SAME session id — that stability
// is deliberate, so one conversation is one session in the dashboard. These tests pin the
// consequence: the cached-prefix boundary must restart on the new prefix, or every message
// of the shorter transcript reads as already-cached and no component can act again for the
// rest of the session.

// bobSession is the stable id (metadata.taskId) that survives the compaction.
const bobSession = "b2f1c0de-4a51-4d0e-9f30-77a1c9d5e412"

// compactBody builds a request under the stable session id carrying `tools` tool outputs
// whose text is derived from tag, so a pre- and a post-compaction transcript hold DIFFERENT
// content (a frozen decision for one cannot silently satisfy the other).
func compactBody(t *testing.T, tag string, tools int) []byte {
	t.Helper()
	msgs := []map[string]any{
		{"role": "system", "content": "You are Bob."},
		{"role": "user", "content": "ship the thing"},
	}
	for i := 0; i < tools; i++ {
		msgs = append(msgs, map[string]any{
			"role":         "tool",
			"tool_call_id": tag + string(rune('a'+i%26)) + strings.Repeat("x", i/26),
			"content":      tag + " tool output #" + strings.Repeat("payload line\n", 40+i),
		})
	}
	b, err := json.Marshal(map[string]any{
		"model":    "claude-sonnet-4-5",
		"metadata": map[string]any{"taskId": bobSession},
		"messages": msgs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runTurn sends one turn through the real entry point with a shared store + tracker.
func runTurn(t *testing.T, o apply.Opts, st store.Store, body []byte) apply.Result {
	t.Helper()
	cfg := pipe(t, "pipeline: [mask]\ncomponents:\n  mask: {keep_recent: 3, min_tokens: 50}\n")
	p, _ := cfg.Build(nil)
	o.Body = body
	return apply.BodyOpts(context.Background(), p, st, o)
}

// TestCompactionRestartsPrefix is the regression. Turn 1+2 are an append-only stream (the
// boundary grows, the original invariant); turn 3 is the post-compaction request — the
// same session id, 50 messages down to 5 — and turn 4 the turn after it. Before the fix,
// turns 3 AND 4 reported MaxCachedIdx 51 with AttemptedTokens 0: the whole session frozen,
// permanently.
func TestCompactionRestartsPrefix(t *testing.T) {
	for _, withTracker := range []bool{true, false} {
		name := "tracker"
		if !withTracker {
			name = "store-backed" // library callers / /compact
		}
		t.Run(name, func(t *testing.T) {
			st := store.NewMemory(store.Options{})
			o := apply.Opts{Provider: bschemas.Anthropic, CacheMode: "on"}
			if withTracker {
				o.Tracker = modes.NewTracker(0)
			}

			// Turn 1: 50 tool outputs, nothing cached yet — whole request is the tail.
			r1 := runTurn(t, o, st, compactBody(t, "pre", 50))
			if r1.Trace.Session != bobSession {
				t.Fatalf("session = %q, want the stable taskId %q", r1.Trace.Session, bobSession)
			}
			if r1.Trace.MaxCachedIdx != -1 {
				t.Fatalf("turn 1 MaxCachedIdx = %d, want -1", r1.Trace.MaxCachedIdx)
			}
			if !r1.Changed {
				t.Fatal("turn 1: mask did not act on a 50-message transcript")
			}

			// Turn 2: append-only. The boundary must have GROWN to turn 1's length, so only
			// the new tail is eligible.
			r2 := runTurn(t, o, st, compactBody(t, "pre", 51))
			if want := 52 - 1; r2.Trace.MaxCachedIdx != want { // 2 head + 50 tools
				t.Fatalf("turn 2 MaxCachedIdx = %d, want %d (boundary must grow on an append-only stream)",
					r2.Trace.MaxCachedIdx, want)
			}

			// Turn 3: the agent compacted. Same session id, 53 messages -> 7.
			r3 := runTurn(t, o, st, compactBody(t, "post", 5))
			if r3.Trace.Session != bobSession {
				t.Fatalf("session id moved across compaction: %q", r3.Trace.Session)
			}
			if r3.Trace.MaxCachedIdx != -1 {
				t.Fatalf("post-compaction MaxCachedIdx = %d, want -1 (the whole new prefix must be eligible)",
					r3.Trace.MaxCachedIdx)
			}
			if r3.Trace.AttemptedTokens == 0 {
				t.Fatal("post-compaction: every message frozen, no component could act")
			}
			if !r3.Changed {
				t.Fatal("post-compaction: mask acted on nothing")
			}

			// Turn 4: the session keeps working — the boundary is now rebased on the SHORT
			// transcript, not stuck at the pre-compaction length.
			r4 := runTurn(t, o, st, compactBody(t, "post", 6))
			if want := 7 - 1; r4.Trace.MaxCachedIdx != want { // 2 head + 5 tools
				t.Fatalf("turn 4 MaxCachedIdx = %d, want %d (boundary did not rebase on the new prefix)",
					r4.Trace.MaxCachedIdx, want)
			}
			if r4.Trace.AttemptedTokens == 0 {
				t.Fatal("turn 4 after compaction: still wholly frozen")
			}
		})
	}
}

// TestCompactionResetIsCounted: the reset must be visible to an operator. modes owns the
// counter and the host merges it into /stats as compaction_resets (whose presence in the
// payload is pinned by proxy.statsGoldenTopLevel).
func TestCompactionResetIsCounted(t *testing.T) {
	st := store.NewMemory(store.Options{})
	o := apply.Opts{Provider: bschemas.Anthropic, CacheMode: "on", Tracker: modes.NewTracker(0)}

	before := modes.CompactionResets()
	runTurn(t, o, st, compactBody(t, "pre", 50))
	runTurn(t, o, st, compactBody(t, "pre", 51)) // append-only: not a reset
	if got := modes.CompactionResets() - before; got != 0 {
		t.Fatalf("an append-only stream reported %d compaction resets, want 0", got)
	}
	runTurn(t, o, st, compactBody(t, "post", 5)) // compaction
	if got := modes.CompactionResets() - before; got != 1 {
		t.Fatalf("compaction_resets delta = %d, want 1", got)
	}
}

// TestPreCompactionContentStaysReadable: the reset must not orphan history. Everything
// offloaded BEFORE the compaction has to stay expandable afterwards — the dashboard shows
// the old diffs, and a <<cg:HASH>> marker that survived into the agent's summary must still
// resolve. Both are content-hash keyed and deliberately not scoped to a prefix generation;
// this test is what keeps them that way.
func TestPreCompactionContentStaysReadable(t *testing.T) {
	st := store.NewMemory(store.Options{})
	o := apply.Opts{Provider: bschemas.Anthropic, CacheMode: "on", Tracker: modes.NewTracker(0)}

	r1 := runTurn(t, o, st, compactBody(t, "pre", 50))
	keys := markerKeys(t, r1.Body)
	if len(keys) == 0 {
		t.Fatal("turn 1 minted no expand markers; this test would prove nothing")
	}
	originals := map[string]string{}
	for _, k := range keys {
		orig, ok := expand.Resolve(st, k)
		if !ok {
			t.Fatalf("pre-compaction marker %q did not resolve even before the compaction", k)
		}
		originals[k] = orig
	}

	runTurn(t, o, st, compactBody(t, "post", 5)) // compaction resets the prefix

	for _, k := range keys {
		orig, ok := expand.Resolve(st, k)
		if !ok {
			t.Errorf("pre-compaction marker %q stopped resolving after the compaction", k)
			continue
		}
		if orig != originals[k] {
			t.Errorf("pre-compaction marker %q resolves to different bytes after the compaction", k)
		}
	}
}

// TestCompactionKeepsFrozenDecisions: a frozen decision is keyed by a CONTENT hash, not a
// message position, so it stays valid across a compaction — and replaying it is exactly
// what keeps bytes the provider already cached stable. Content that recurs in the
// post-compaction transcript must therefore come out byte-identical to before, rather than
// being re-derived.
func TestCompactionKeepsFrozenDecisions(t *testing.T) {
	st := store.NewMemory(store.Options{})
	o := apply.Opts{Provider: bschemas.Anthropic, CacheMode: "on", Tracker: modes.NewTracker(0)}

	r1 := runTurn(t, o, st, compactBody(t, "pre", 50))
	was := maskedTexts(t, r1.Body)
	if len(was) == 0 {
		t.Fatal("turn 1 masked nothing; this test would prove nothing")
	}

	// The post-compaction transcript re-sends the SAME early content (a partial compaction
	// keeps the recent portion, and "pre" content recurs).
	r2 := runTurn(t, o, st, compactBody(t, "pre", 8))
	if r2.Trace.MaxCachedIdx != -1 {
		t.Fatalf("shrink did not restart the prefix: MaxCachedIdx = %d", r2.Trace.MaxCachedIdx)
	}
	now := maskedTexts(t, r2.Body)
	shared := 0
	for k, text := range now {
		prev, ok := was[k]
		if !ok {
			continue
		}
		shared++
		if text != prev {
			t.Errorf("tool_call_id %q: frozen decision was re-derived across the compaction\n before=%q\n after =%q", k, prev, text)
		}
	}
	if shared == 0 {
		t.Fatal("no masked content recurred across the compaction; this test would prove nothing")
	}
}

// markerKeys returns every expand marker key present in a rewritten body.
func markerKeys(t *testing.T, body []byte) []string {
	t.Helper()
	var seen []string
	dedup := map[string]bool{}
	for _, m := range messagesOf(t, body) {
		for _, k := range expand.ParseMarkers(textOf(m["content"])) {
			if !dedup[k] {
				dedup[k], seen = true, append(seen, k)
			}
		}
	}
	return seen
}

// maskedTexts maps tool_call_id -> rewritten text, for messages mask actually replaced.
func maskedTexts(t *testing.T, body []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range messagesOf(t, body) {
		id, _ := m["tool_call_id"].(string)
		text := textOf(m["content"])
		if id != "" && expand.HasPlaceholder(text) {
			out[id] = text
		}
	}
	return out
}

func messagesOf(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Messages
}

// textOf flattens a message's content, which is a string or a block array on the wire.
func textOf(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, blk := range c {
			if m, ok := blk.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					b.WriteString(s)
				}
				if s, ok := m["content"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}
