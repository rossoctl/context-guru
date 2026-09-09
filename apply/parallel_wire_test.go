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

// THE SHAPE LIVE TRAFFIC CARRIES. In Anthropic's wire format a PARALLEL tool call is ONE assistant
// message carrying several tool_use blocks, answered by ONE user message carrying several
// tool_result blocks. apply normalizes that user message into several synthetic role=tool messages,
// so N normalized messages share ONE body index.
//
// This asserts tool pairing in BOTH directions on the emitted wire, because the two fail
// independently and each hid the other:
//
//   - FORWARD (every tool_use is answered). This was PR #80's iter011 defect, where the body
//     message holding both tool_results was dropped and the parallel call went unanswered —
//     28 of 75 live runs rejected. Fixed upstream; asserted here as a regression guard.
//   - BACKWARD (every tool_result answers a call that PRECEDES it). keep_last counts messages and
//     a tool result is a message, so the tail boundary could begin mid-exchange: the assistant's
//     calls were summarized away while their results survived. At keep_last 2 and 3 this emitted
//     [user, summary, user(tool_result pa_h, tool_result pb_h), user] — two results, no call.
//
// A forward-only check passes on that wire, which is why the direction matters. Both are provider
// rejections of the entire request, not degraded output.
func TestSummarizeNeverSplitsAToolExchange(t *testing.T) {
	big := strings.Repeat("verbose parallel tool output\n", 60)
	msgs := []map[string]any{
		{"role": "user", "content": "start the task"},
	}
	for i := 0; i < 8; i++ {
		a, b := "pa_"+string(rune('a'+i)), "pb_"+string(rune('a'+i))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "calling two"},
				{"type": "tool_use", "id": a, "name": "Read", "input": map[string]any{}},
				{"type": "tool_use", "id": b, "name": "Read", "input": map[string]any{}},
			}},
			// BOTH results in ONE user message -- Anthropic's requirement for a parallel call.
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": a, "content": big},
				{"type": "tool_result", "tool_use_id": b, "content": big},
			}},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "final question"})
	body, _ := json.Marshal(map[string]any{"model": "claude-x", "messages": msgs})

	// Preconditions, so a wire that carries no tool content at all cannot pass silently.
	var sawResult, sawParallelCall, acted bool

	for _, keep := range []int{1, 2, 3, 4, 5} {
		cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_last: "+
			string(rune('0'+keep))+", start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
		p, _ := cfg.Build(nil)
		out, changed := apply.BodyWithModel(context.Background(), p,
			store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false,
			components.ModelSpec{Incoming: stubModel{resp: "essential facts"}})
		if !changed {
			continue
		}
		acted = true
		arr := gjson.GetBytes(out, "messages").Array()

		// Ids are collected AS THE TRANSCRIPT IS WALKED, so a result can only pair with a call
		// that precedes it -- the same rule schema.ToolCalls documents and the provider enforces.
		declared := map[string]bool{}
		for i, m := range arr {
			var uses []string
			m.Get("content").ForEach(func(_, blk gjson.Result) bool {
				switch blk.Get("type").String() {
				case "tool_use":
					id := blk.Get("id").String()
					uses = append(uses, id)
					declared[id] = true
				case "tool_result":
					sawResult = true
					if id := blk.Get("tool_use_id").String(); !declared[id] {
						t.Errorf("keep_last=%d: wire message %d carries tool_result %q with no "+
							"preceding tool_use -- the provider rejects this", keep, i, id)
						dumpWire(t, arr)
					}
				}
				return true
			})
			if len(uses) >= 2 {
				sawParallelCall = true
			}
			if len(uses) == 0 {
				continue
			}
			// FORWARD: the results must be in the message immediately after the call.
			answered := map[string]bool{}
			if i+1 < len(arr) {
				arr[i+1].Get("content").ForEach(func(_, blk gjson.Result) bool {
					if blk.Get("type").String() == "tool_result" {
						answered[blk.Get("tool_use_id").String()] = true
					}
					return true
				})
			}
			for _, u := range uses {
				if !answered[u] {
					t.Errorf("keep_last=%d: wire message %d declares tool_use %q with no "+
						"tool_result immediately after -- the provider rejects this", keep, i, u)
					dumpWire(t, arr)
				}
			}
		}
	}

	if !acted {
		t.Fatal("summarize never acted, so no wire was checked -- the assertions are vacuous")
	}
	if !sawResult {
		t.Fatal("no tool_result ever reached the wire, so the BACKWARD assertion never ran")
	}
	if !sawParallelCall {
		t.Fatal("no parallel tool_use pair ever reached the wire, so the FORWARD assertion " +
			"never exercised the shape this test exists for")
	}
}

func dumpWire(t *testing.T, arr []gjson.Result) {
	t.Helper()
	for k, mm := range arr {
		t.Errorf("    [%d] role=%s content_head=%.90s", k,
			mm.Get("role").String(), mm.Get("content").Raw)
	}
}
