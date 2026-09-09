package apply_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// Anthropic has NO tool role. A synthetic role=tool message is this package's internal
// representation of a tool_result content block, and serializing one onto the wire is a hard
// provider rejection:
//
//	400 messages: Unexpected role "tool". Allowed roles are "user" or "assistant."
//
// rebuildCountChanged emits a message from its ORIGINAL body bytes only when it byte-matches its
// pre-pipeline form, and marshals it fresh otherwise -- a branch its own comment describes as being
// for "a new message (e.g. the summary)". A RETAINED tool-result message that a LATER component
// also rewrites lands in that same branch, and gets marshaled with its internal role intact.
//
// Two components in one turn are required, which is why this was never seen before: summarize
// changes the message count (so the rebuild runs at all), and a second component rewrites the text
// of a tool message that summarize kept. Observed as a real 400 on a live session.
func TestNoToolRoleOnAnthropicWireAfterCountChange(t *testing.T) {
	// Indented JSON, so a reducing component actually rewrites it. Compact or prose content is
	// left alone, and the test would pass while proving nothing -- the exact trap that made an
	// earlier version of this test vacuous twice.
	toolBody := func(n int) string {
		recs := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			recs = append(recs, map[string]any{
				"ts": "2024-01-01T00:00:00Z", "path": "src/api/users.py",
				"level": "INFO", "msg": "request served", "seq": i,
				"detail": strings.Repeat("verbose detail text ", 6),
			})
		}
		b, _ := json.MarshalIndent(recs, "", "  ")
		return string(b)
	}

	msgs := []map[string]any{{"role": "user", "content": "audit the request log"}}
	for i := 0; i < 6; i++ {
		id := "call_" + string(rune('a'+i))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "reading the log"},
				{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": id, "content": toolBody(40)},
			}},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "what failed?"})
	body, _ := json.Marshal(map[string]any{"model": "claude-x", "messages": msgs})

	// summarize changes the count; extract_llm rewrites a RETAINED tool output in the same turn.
	// strategy: deterministic keeps this hermetic -- the reduction is a real rewrite of the kept
	// message's bytes with no model reply to stub.
	const comps = "components:\n" +
		"  summarize: {keep_last: 3, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n" +
		"  extract_llm: {strategy: deterministic, min_tokens: 1, economic_gate: false, " +
		"allow_on_caching_backend: true}\n"

	// Both the reduced pipeline and the one this was reported against. cachesplit is not a
	// spectator: it is a no-op in Reformat and the split it names happens inside THIS package,
	// which rewrites the envelope before the rebuild runs and changes the rebuild's control flow
	// (a declined rebuild is still forwarded when systemSplit is set). So a fix verified only
	// without it is not verified for the configuration that produced the live 400.
	for _, tc := range []struct{ name, pipeline string }{
		{"summarize+extract_llm", "pipeline: [summarize, extract_llm]\n"},
		{"reported: summarize+extract_llm+cachesplit", "pipeline: [summarize, extract_llm, cachesplit]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkNoToolRole(t, tc.pipeline+comps, body, len(msgs))
		})
	}
}

func checkNoToolRole(t *testing.T, yaml string, body []byte, inCount int) {
	t.Helper()
	cfg := pipe(t, yaml)
	p, _ := cfg.Build(nil)
	out, changed := apply.BodyWithModel(context.Background(), p,
		store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false,
		components.ModelSpec{Incoming: stubModel{resp: "essential facts"}})
	if !changed {
		t.Fatal("no component acted, so the rebuild never ran -- assertion is vacuous")
	}
	arr := gjson.GetBytes(out, "messages").Array()

	// PRECONDITION 1: the count must actually have changed, or rebuildCountChanged never ran.
	if len(arr) == inCount {
		t.Fatalf("message count unchanged (%d), so the count-change rebuild never ran", len(arr))
	}
	// Count both shapes in ONE pass, because they are alternatives rather than independent
	// facts: a tool exchange that survived is either a well-formed tool_result block (correct)
	// or a leaked role=tool message (the defect). Counting only tool_result blocks would make
	// the precondition fire on the very output the assertion exists to catch -- the leaked
	// message has no tool_result block to find, so "no tool content survived" and "the bug
	// happened" would be indistinguishable, and the test would abort as vacuous instead of
	// failing.
	var toolRoleMsgs, toolResultBlocks int
	for _, m := range arr {
		if m.Get("role").String() == "tool" {
			toolRoleMsgs++
		}
		m.Get("content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() == "tool_result" {
				toolResultBlocks++
			}
			return true
		})
	}

	// PRECONDITION 2: some tool exchange must have survived into the output, in either shape.
	// If summarize swallowed them all there is no retained tool message to mis-serialize.
	if toolRoleMsgs+toolResultBlocks == 0 {
		t.Fatal("no tool exchange survived into the output, so no retained tool message could " +
			"reach the rebuild -- assertion is vacuous")
	}

	// THE ASSERTION: no message may carry the internal tool role.
	if toolRoleMsgs > 0 {
		t.Errorf(`%d wire message(s) have role="tool" -- Anthropic rejects the request with `+
			`"Unexpected role \"tool\". Allowed roles are \"user\" or \"assistant\{-}."`,
			toolRoleMsgs)
		for k, mm := range arr {
			t.Errorf("    [%d] role=%s content_head=%.80s", k,
				mm.Get("role").String(), mm.Get("content").Raw)
		}
	}
}
