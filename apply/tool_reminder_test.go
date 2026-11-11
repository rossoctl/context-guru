package apply_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// ccReminder is the block Claude Code 2.1.241 appended to every tool_result (#556). The counter
// changes on every call, so two runs of one command never give identical text. This is the
// 2.1.241 shape only: 2.1.290 to 2.1.299 send the counter as a separate role:"system" message.
func ccReminder(left int) string {
	return fmt.Sprintf("\n\n<system-reminder>\n<total_tokens>%d tokens left</total_tokens>\n</system-reminder>", left)
}

func ccServiceJSON() string {
	rows := make([]map[string]any, 110)
	for j := range rows {
		rows[j] = map[string]any{"id": fmt.Sprintf("svc-%d", j), "region": []string{"us-east", "eu-de", "jp-tok"}[j%3],
			"status": map[bool]string{true: "healthy", false: "degraded"}[j%7 != 0], "owner": fmt.Sprintf("team-%d", j%9),
			"replicas": j%12 + 1}
	}
	b, _ := json.MarshalIndent(rows, "", "  ")
	return string(b)
}

// reminderBody is a Claude Code shaped conversation: two Bash tool_results, then a question, so
// neither output is the newest message.
func reminderBody(out1, out2 any) []byte {
	tu := func(id string) map[string]any {
		return map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": "cat services.json"}}}}
	}
	tr := func(id string, c any) map[string]any {
		return map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": c}}}
	}
	b, _ := json.Marshal(map[string]any{"model": "claude-haiku-4-5", "max_tokens": 64, "messages": []any{
		map[string]any{"role": "user", "content": "Show me the services."}, tu("t1"), tr("t1", out1), tu("t2"), tr("t2", out2),
		map[string]any{"role": "assistant", "content": "Done."}, map[string]any{"role": "user", "content": "Which are degraded?"},
	}})
	return b
}

// TestTrailingReminderDoesNotBlockDedup: two outputs that differ only in Claude Code's counter
// are duplicates. The repeat collapses, and each tool_result keeps its own reminder.
func TestTrailingReminderDoesNotBlockDedup(t *testing.T) {
	p, _ := pipe(t, "pipeline: [dedup]\n").Build(nil)
	js := ccServiceJSON()
	r1, r2 := ccReminder(14989288), ccReminder(14987911)
	body := reminderBody(js+r1, js+r2)

	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false)
	if !changed {
		t.Fatal("dedup should fire on two outputs that differ only in the trailing reminder")
	}
	if got := gjson.GetBytes(out, "messages.2.content.0.content").String(); got != js+r1 {
		t.Fatalf("the first output must be left untouched, reminder included: %q", clipTail(got))
	}
	dup := gjson.GetBytes(out, "messages.4.content.0.content").String()
	if !strings.Contains(dup, "<<cg:") {
		t.Fatalf("the repeat was not collapsed: %q", clipTail(dup))
	}
	if !strings.HasSuffix(dup, r2) {
		t.Fatalf("the repeat must keep its own reminder, with its own counter: %q", clipTail(dup))
	}
	if strings.Count(dup, "<system-reminder>") != 1 {
		t.Fatalf("the reminder must appear once: %q", dup)
	}
}

// TestTrailingReminderDoesNotBlockFormat: JSON with a reminder after it is still JSON to the
// reformatters. The reminder comes back after the rewritten output, unchanged.
func TestTrailingReminderDoesNotBlockFormat(t *testing.T) {
	p, _ := pipe(t, "pipeline: [format, toon]\n").Build(nil)
	js, r := ccServiceJSON(), ccReminder(14989288)
	body := reminderBody(js+r, "short output"+ccReminder(14987911))

	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false)
	if !changed {
		t.Fatal("format/toon should fire on JSON followed by a reminder")
	}
	got := gjson.GetBytes(out, "messages.2.content.0.content").String()
	if !strings.HasSuffix(got, r) {
		t.Fatalf("the reminder must come back after the rewritten output: %q", clipTail(got))
	}
	if len(got) >= len(js+r) {
		t.Fatalf("the output was not reduced: %d >= %d bytes", len(got), len(js+r))
	}
	if strings.Contains(strings.TrimSuffix(got, r), "<system-reminder>") {
		t.Fatalf("the reminder must not be inside the rewritten output: %q", got)
	}
}

// TestTrailingReminderInTextArray: the array-shaped tool_result content gets the same
// treatment, at the text block's own path.
func TestTrailingReminderInTextArray(t *testing.T) {
	p, _ := pipe(t, "pipeline: [dedup]\n").Build(nil)
	js := ccServiceJSON()
	arr := func(r string) []any { return []any{map[string]any{"type": "text", "text": js + r}} }
	r2 := ccReminder(14987911)
	body := reminderBody(arr(ccReminder(14989288)), arr(r2))

	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false)
	if !changed {
		t.Fatal("dedup should fire on array-shaped outputs that differ only in the reminder")
	}
	dup := gjson.GetBytes(out, "messages.4.content.0.content.0.text").String()
	if !strings.Contains(dup, "<<cg:") || !strings.HasSuffix(dup, r2) {
		t.Fatalf("want a collapsed repeat that ends with its own reminder: %q", clipTail(dup))
	}
}

// TestTrailingReminderUnchangedIsByteIdentical: when no component changes an output, the body
// is the original bytes. Splitting the reminder off must not count as a change (I1).
func TestTrailingReminderUnchangedIsByteIdentical(t *testing.T) {
	p, _ := pipe(t, "pipeline: [dedup]\n").Build(nil)
	body := reminderBody("first distinct output\n"+ccReminder(1), "second distinct output\n"+ccReminder(2))

	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}), bschemas.Anthropic, body, "", false)
	if changed || !bytes.Equal(out, body) {
		t.Fatalf("an unchanged request must come out byte-identical (changed=%v)", changed)
	}
}

func clipTail(s string) string {
	if len(s) > 160 {
		return "…" + s[len(s)-160:]
	}
	return s
}

// TestTrailingReminderSurvivesCountChange: the count-changed rebuild is the other writeback
// path, and the one that produced live 400s. summarize changes the message count and extract_llm
// rewrites a tool output summarize kept. Each kept tool_result must end with its own reminder,
// once, and none may reach the wire as role=tool.
func TestTrailingReminderSurvivesCountChange(t *testing.T) {
	// Indented JSON, so extract_llm's deterministic strategy really rewrites it.
	log := func(n int) string {
		recs := make([]map[string]any, n)
		for i := range recs {
			recs[i] = map[string]any{"ts": "2024-01-01T00:00:00Z", "path": "src/api/users.py",
				"level": "INFO", "msg": "request served", "seq": i, "detail": strings.Repeat("verbose detail text ", 6)}
		}
		b, _ := json.MarshalIndent(recs, "", "  ")
		return string(b)
	}

	msgs := []map[string]any{{"role": "user", "content": "audit the request log"}}
	want := map[string]string{} // tool_use_id -> the original output, reminder included
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("call_%c", 'a'+i)
		want[id] = log(40) + ccReminder(14989288-i*1000)
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "reading the log"},
				{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": id, "content": want[id]},
			}},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "what failed?"})
	body, _ := json.Marshal(map[string]any{"model": "claude-x", "messages": msgs})

	const yaml = "pipeline: [summarize, extract_llm, cachesplit]\n" +
		"components:\n" +
		"  summarize: {keep_last: 3, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n" +
		"  extract_llm: {strategy: deterministic, min_tokens: 1, economic_gate: false, " +
		"allow_on_caching_backend: true}\n"
	p, _ := pipe(t, yaml).Build(nil)
	// Two passes over one Store: the first commissions the summary, the second splices it.
	st := store.NewMemory(store.Options{})
	models := components.ModelSpec{Incoming: stubModel{resp: "essential facts"}}
	apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false, models)
	offload.WaitForAllSummariesForTest(5 * time.Second)
	out, changed := apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false, models)
	if !changed {
		t.Fatal("no component acted, so the rebuild never ran")
	}
	arr := gjson.GetBytes(out, "messages").Array()
	if len(arr) == len(msgs) {
		t.Fatalf("message count unchanged (%d), so the count-change rebuild never ran", len(arr))
	}

	var kept, rewritten int
	for k, m := range arr {
		if m.Get("role").String() == "tool" {
			t.Errorf("message %d reached the wire with role=tool", k)
		}
		m.Get("content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() != "tool_result" {
				return true
			}
			kept++
			id, got := blk.Get("tool_use_id").String(), blk.Get("content").String()
			orig := want[id]
			r := orig[strings.LastIndex(orig, "\n\n<system-reminder>"):]
			if !strings.HasSuffix(got, r) {
				t.Errorf("%s must end with its own reminder: %q", id, clipTail(got))
			}
			if n := strings.Count(got, "<system-reminder>"); n != 1 {
				t.Errorf("%s carries %d reminders, want 1", id, n)
			}
			if got != orig {
				rewritten++
			}
			return true
		})
	}
	if kept == 0 || rewritten == 0 {
		t.Fatalf("want a kept tool_result that a component rewrote (kept=%d rewritten=%d): the "+
			"assertions above prove nothing otherwise", kept, rewritten)
	}
}
