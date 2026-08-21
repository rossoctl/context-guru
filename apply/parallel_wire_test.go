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

// THE SHAPE LIVE TRAFFIC CARRIES. In Anthropic's wire format a PARALLEL tool call is one assistant
// message with several tool_use blocks, answered by ONE user message holding several tool_result
// blocks. apply normalizes that user message into several synthetic role=tool messages, so N
// normalized messages share ONE body index -- the case the  guard in apply exists for.
//
// Live LOCA runs failed 28 of 75 with the provider reporting an unanswered tool_use, and a capture
// hop between the proxy and the gateway showed a trailing assistant(tool_use, tool_use) with nothing
// after it. summarize is clean on the bifrost message list at every keep_last from 1 to 5 (see
// components/offload/summarize_parallel_test.go), so the wire rebuild is the remaining suspect.
//
// CONFIRMED: this test FAILS at keep_last=4 while the message-list test passes with the IDENTICAL
// summarize config, which localises the defect to rebuildCountChanged rather than to summarize. The
// emitted wire is [user, summary, assistant(tool_use pa_a, tool_use pb_a), user("final question")] --
// the body message holding BOTH tool_results is dropped entirely, so the assistant's parallel call
// goes unanswered and the provider rejects the request.
//
// Not yet fixed, deliberately. The suspect area is the slot/body-index mapping that lets several
// normalized messages share one body index (the `emitted` guard), and a careless change there risks
// the byte-losslessness guarantee this package exists to provide. This component family has produced
// four shape defects already; a blind fifth fix is not the way to close it. The reproduction is the
// deliverable.
func TestSummarizeKeepsParallelToolResultsOnTheWire(t *testing.T) {
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

	for _, keep := range []int{1, 2, 3, 4} {
		t.Run("keep_last", func(t *testing.T) {
			cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_last: "+
				string(rune('0'+keep))+", start_from_message: 0, min_tokens: 1}\n")
			p, _ := cfg.Build(nil)
			out, changed := apply.BodyWithModel(context.Background(), p,
				store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false,
				components.ModelSpec{Incoming: stubModel{resp: "essential facts"}})
			if !changed {
				t.Skip("summarize did not act")
			}
			// Walk the EMITTED wire messages and check every tool_use is answered in the next one.
			arr := gjson.GetBytes(out, "messages").Array()
			for i, m := range arr {
				var uses []string
				m.Get("content").ForEach(func(_, blk gjson.Result) bool {
					if blk.Get("type").String() == "tool_use" {
						uses = append(uses, blk.Get("id").String())
					}
					return true
				})
				if len(uses) == 0 {
					continue
				}
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
						for k, mm := range arr {
							t.Errorf("    [%d] role=%s content_head=%.90s", k,
								mm.Get("role").String(), mm.Get("content").Raw)
						}
						return
					}
				}
			}
		})
	}
}
