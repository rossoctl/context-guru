package apply_test

import (
	"context"
	"encoding/json"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// A count-changing component (summarize) TOGETHER WITH one that rewrites tool text (format) on
// ANTHROPIC wire bytes. This combination is what live traffic always runs, and it exposed a defect
// that neither component shows alone:
//
//	400 messages: Unexpected role "tool"
//
// A synthetic role=tool message is an internal representation only; Anthropic has no such role. The
// rebuild emits a message verbatim only when it byte-matches its pre-pipeline form, so once format
// rewrote a tool message's text it fell through to a fresh marshal and leaked role="tool" onto the
// wire. Measured live at 12 of 39 runs, and it only became visible after the tool-call-id fix stopped
// these messages from being deleted outright -- one defect had been masking the other.
func TestAnthropicCountChangeNeverLeaksToolRole(t *testing.T) {
	rows := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		rows = append(rows, map[string]any{
			"id": 100000 + i, "path": "src/auth.py", "sym": "TOKEN_GRACE_41ab",
			"status": "ok", "note": "row that format can restructure",
		})
	}
	bigB, _ := json.MarshalIndent(rows, "", "  ")
	big := string(bigB)
	msgs := []map[string]any{{"role": "user", "content": "start"}}
	for i := 0; i < 8; i++ {
		a, b := "pa_"+string(rune('a'+i)), "pb_"+string(rune('a'+i))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "calling two"},
				{"type": "tool_use", "id": a, "name": "Read", "input": map[string]any{}},
				{"type": "tool_use", "id": b, "name": "Read", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": a, "content": big},
				{"type": "tool_result", "tool_use_id": b, "content": big},
			}})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "final question"})
	body, _ := json.Marshal(map[string]any{"model": "claude-x", "messages": msgs})

	rewroteAny := false
	for _, keep := range []int{1, 2, 3, 4} {
		cfg := pipe(t, "pipeline: [format, summarize]\ncomponents:\n  summarize: {keep_last: "+
			string(rune('0'+keep))+", start_from_message: 0, min_tokens: 1}\n")
		p, _ := cfg.Build(nil)
		out, changed := apply.BodyWithModel(context.Background(), p,
			store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false,
			components.ModelSpec{Incoming: stubModel{resp: "essential facts"}})
		if !changed {
			continue
		}
		// PRECONDITION. If no tool_result text was rewritten, the failing path is never entered and
		// a pass proves nothing -- the first version of this test passed with the fix removed for
		// exactly that reason. Fail loudly rather than pass vacuously.
		rewrote := false
		gjson.GetBytes(out, "messages").ForEach(func(_, m gjson.Result) bool {
			m.Get("content").ForEach(func(_, blk gjson.Result) bool {
				if blk.Get("type").String() == "tool_result" &&
					blk.Get("content").String() != big && blk.Get("content").Exists() {
					rewrote = true
				}
				return true
			})
			return true
		})
		if rewrote {
			rewroteAny = true
		}
		arr := gjson.GetBytes(out, "messages").Array()
		for i, m := range arr {
			if r := m.Get("role").String(); r != "user" && r != "assistant" && r != "system" {
				t.Fatalf("keep_last=%d: wire message %d has role %q -- Anthropic rejects this",
					keep, i, r)
			}
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
					t.Fatalf("keep_last=%d: wire message %d declares %q unanswered", keep, i, u)
				}
			}
		}
	}
	// Asserted ONCE, across all keep_last values: at least one case must have carried a rewritten
	// tool_result through a count change, or the role-leak path was never entered and a pass proves
	// nothing. The first version of this test passed with the fix removed for exactly that reason,
	// which is the same vacuous-check trap recorded twice in docs/experiments/loca/.
	if !rewroteAny {
		t.Fatal("no case produced a rewritten tool_result surviving a count change, so this test " +
			"cannot detect the defect it exists for; fix the fixture")
	}
}
