package apply_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// ccReminder is the block Claude Code appends to every tool_result (#556). The counter changes
// on every call, so two runs of one command never give identical text.
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
