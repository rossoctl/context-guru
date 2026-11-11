package apply_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/store"
	"github.com/tidwall/gjson"
)

// testdata/betas_turn{1,2}.json are two requests of one Claude Code 2.1.316 session with
// CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1 (#795), every text replaced by filler of the same
// length. Turn 1 carries its last mark on the user's text in messages[0], then the 8.7 KB
// environment as a `role: "system"` message. Turn 2 adds a tool exchange and marks its tool_result.
func betasTurn(t *testing.T, n string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/betas_turn" + n + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func tailRun(t *testing.T, yaml string, body []byte) []byte {
	t.Helper()
	p, err := pipe(t, yaml).Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	res := apply.BodyOpts(context.Background(), p, store.NewMemory(store.Options{}), apply.Opts{
		Provider: bschemas.Anthropic, Body: body,
	})
	return res.Body
}

// rendered is v as the provider renders it for its cache: no cache_control, and a string content
// written as its one text block (the provider renders the two the same way; see schema/canon.go).
func rendered(t *testing.T, raw string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case map[string]any:
			delete(x, "cache_control")
			if s, ok := x["content"].(string); ok && x["role"] != nil {
				x["content"] = []any{map[string]any{"type": "text", "text": s}}
			}
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		}
		return v
	}
	out, _ := json.Marshal(walk(v))
	return string(out)
}

// #795, end to end through BodyOpts on the captured shape. Turn 1 gets a mark at its end, within
// the cap; turn 2 needs none (its client mark is at the end); and turn 2 carries turn 1's whole
// marked prefix, so it reads the entry turn 1 wrote. Live, turn 2 read 29,939 tokens where it had
// read 26,885 and written 3,200 without the step.
func TestTheNextTurnCarriesTheWholePrefixTheTailBreakpointMarked(t *testing.T) {
	const cfg = "preset: cache\n"
	in1, in2 := betasTurn(t, "1"), betasTurn(t, "2")
	if gjson.GetBytes(in1, "messages.1.role").String() != "system" ||
		!gjson.GetBytes(in1, "messages.0.content.1.cache_control").IsObject() {
		t.Fatal("fixture is not the betas shape")
	}
	out1 := tailRun(t, cfg, in1)
	if !gjson.GetBytes(out1, "messages.1.content.0.cache_control").IsObject() {
		t.Fatalf("turn 1: no mark on the environment message:\n%.400s", gjson.GetBytes(out1, "messages.1").Raw)
	}
	if n := apply.CountBreakpoints(out1).Total(); n != apply.CountBreakpoints(in1).Total()+1 || n > 4 {
		t.Fatalf("turn 1: %d breakpoints, want the client's %d plus 1, at most 4", n, apply.CountBreakpoints(in1).Total())
	}
	out2 := tailRun(t, cfg, in2)
	if a, b := apply.CountBreakpoints(out2).Total(), apply.CountBreakpoints(in2).Total(); a != b {
		t.Fatalf("turn 2: %d breakpoints, want the client's %d: its mark is at the end", a, b)
	}
	// The prefix up to turn 1's new mark: tools, system, messages[0..1].
	for _, path := range []string{"tools", "system", "messages.0", "messages.1"} {
		if a, b := rendered(t, gjson.GetBytes(out1, path).Raw), rendered(t, gjson.GetBytes(out2, path).Raw); a != b {
			t.Fatalf("%s differs between turn 1 and turn 2, so turn 2 cannot read turn 1's entry:\n%.300s\n%.300s", path, a, b)
		}
	}
	// The provider's lookback from turn 2's mark reaches turn 1's mark: 2 blocks back.
	if n := len(gjson.GetBytes(out2, "messages").Array()); n != 4 {
		t.Fatalf("turn 2 has %d messages, want 4", n)
	}
}

// The switch. `cache.tail_breakpoint: false` turns the step off, and `preset: off` never runs it,
// so the A/B control arm stays a passthrough.
func TestTheTailBreakpointIsOffWhenTheConfigSaysSo(t *testing.T) {
	in := betasTurn(t, "1")
	for _, cfg := range []string{"preset: cache\ncache:\n  tail_breakpoint: false\n", "preset: off\n"} {
		if out := tailRun(t, cfg, in); string(out) != string(in) {
			t.Fatalf("%q: the body changed:\n%.300s", cfg, gjson.GetBytes(out, "messages.1").Raw)
		}
	}
	if out := tailRun(t, "preset: off\ncache:\n  tail_breakpoint: true\n", in); string(out) != string(in) {
		t.Fatal("preset off with the switch on: the body changed; an empty pipeline must stay a passthrough")
	}
}
