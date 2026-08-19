package apply_test

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// turn builds request N of a session: the same catalogue and system prompt every time (as
// Claude Code sends them) with one more exchange on the transcript.
func turn(n int) string {
	var b strings.Builder
	b.WriteString(`{"model":"claude-opus-5","tools":[` +
		`{"name":"Bash","description":"run a command","input_schema":{"type":"object"}},` +
		`{"name":"CronCreate","description":"schedule a job","input_schema":{"type":"object"}},` +
		`{"name":"Workflow","description":"never used","input_schema":{"type":"object"}}],` +
		`"system":[{"type":"text","text":"You are an agent. Prefer Bash for shell work.\nCurrent branch: main\n"}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"start"}]}`)
	for i := 0; i < n; i++ {
		b.WriteString(`,{"role":"assistant","content":[{"type":"tool_use","id":"t` + string(rune('0'+i)) +
			`","name":"Bash","input":{"command":"ls"}}]}`)
		b.WriteString(`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t` +
			string(rune('0'+i)) + `","content":"a b c"}]}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

// TestToolFilterThroughApply is the wiring test: gated on the component name, reading the
// component's own list, and rewriting `tools` without disturbing `messages` — the ordering
// that matters because the writeback takes byte offsets into the body afterwards.
func TestToolFilterThroughApply(t *testing.T) {
	for _, tc := range []struct {
		name, pipeline string
		want           int
	}{
		{"absent without the component", "pipeline: [format]\n", 3},
		{"empty list removes nothing", "pipeline: [format, toolfilter]\n", 3},
		{"removes what it is told", "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [CronCreate, Workflow]\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := pipe(t, tc.pipeline).Build(nil)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(turn(2))
			out, _ := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
				bschemas.Anthropic, body, "s1", false)
			if got := len(gjson.GetBytes(out, "tools").Array()); got != tc.want {
				t.Errorf("tools = %d, want %d (%s)", got, tc.want, gjson.GetBytes(out, "tools").Raw)
			}
			if a, b := gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(out, "messages").Raw; a != b {
				t.Errorf("messages changed:\n%s\n%s", a, b)
			}
		})
	}
}

// TestToolFilterSameToolsEveryTurn is the cache-invalidation test, and it is the one that
// matters most: `tools` renders at position 0 with no breakpoint on it, so a filter that
// acted on some turns of a session and not others would re-anchor the entire prefix on every
// one of them. A session filtered from turn 1 must send byte-identical `tools` on every turn.
func TestToolFilterSameToolsEveryTurn(t *testing.T) {
	p, err := pipe(t, "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [CronCreate, Workflow]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory(store.Options{})
	want := ""
	for n := 0; n < 8; n++ {
		out, _ := apply.Body(context.Background(), p, st, bschemas.Anthropic, []byte(turn(n)), "s1", false)
		got := gjson.GetBytes(out, "tools").Raw
		if n == 0 {
			want = got
			if len(gjson.Parse(got).Array()) != 1 {
				t.Fatalf("turn 0 did not filter: %s", got)
			}
			continue
		}
		if got != want {
			t.Fatalf("turn %d sent different tools:\n got %s\nwant %s", n, got, want)
		}
	}
}

// TestToolFilterBypassed: a bypassed request promises a byte-identical forward.
func TestToolFilterBypassed(t *testing.T) {
	p, err := pipe(t, "pipeline: [format, toolfilter]\ncomponents:\n  toolfilter:\n    remove: [CronCreate]\n").Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(turn(1))
	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "s1", true)
	if changed || string(out) != string(body) {
		t.Error("a bypassed request was filtered")
	}
}

// TestToolFilterRejectsAJunkName: validation at write time, so a bad list is a 400 on the
// settings page rather than a filter that silently matches nothing forever.
func TestToolFilterRejectsAJunkName(t *testing.T) {
	for _, name := range []string{"*", "Cron*", "a b", "Cron;drop"} {
		if _, err := pipe(t, "pipeline: [toolfilter]\ncomponents:\n  toolfilter:\n    remove: [\""+name+"\"]\n").Build(nil); err == nil {
			t.Errorf("accepted %q as a declaration name", name)
		}
	}
}
