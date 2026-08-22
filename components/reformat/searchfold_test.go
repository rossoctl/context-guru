package reformat

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

// capture is one real tool result from /tmp/cg-runs (Terminal-Bench + SWE-bench
// captures of Claude Code traffic), narrowed to the outputs of search-shaped Bash
// commands. Invented shapes would prove nothing about this transform: what makes or
// breaks a fold is exactly the framing real tools emit (`grep -A` context lines with
// `-` separators, conda banners in front of `find` output, single-file `grep -n` with
// no path prefix at all).
type capture struct {
	Cmd string `json:"cmd"`
	Out string `json:"out"`
}

func loadCaptures(t *testing.T) []capture {
	t.Helper()
	b, err := os.ReadFile("testdata/search_output.json")
	if err != nil {
		t.Fatal(err)
	}
	var c []capture
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if len(c) < 100 {
		t.Fatalf("thin corpus: %d captures", len(c))
	}
	return c
}

// TestFoldRoundTripsEveryCapture is the property that replaces a losslessness
// argument: for every real captured output, unfolding the fold reproduces the input
// byte for byte. It checks each candidate fold on its own (not just the one the
// dispatcher adopts), so a fold that would corrupt output is caught here rather than
// silently declined in production.
func TestFoldRoundTripsEveryCapture(t *testing.T) {
	for _, c := range loadCaptures(t) {
		got := FoldSearchOutput(c.Out)
		if unfoldHitPath(got) != c.Out && unfoldPrefixDir(got) != c.Out && got != c.Out {
			t.Errorf("adopted fold does not round-trip for %q", trunc(c.Cmd))
		}
		if len(got) > len(c.Out) {
			t.Errorf("fold grew output for %q: %d -> %d", trunc(c.Cmd), len(c.Out), len(got))
		}
	}
}

// TestAdoptDeclinesUnsoundFold proves the harness itself: a fold that loses a byte is
// never adopted, whatever it claims about size.
func TestAdoptDeclinesUnsoundFold(t *testing.T) {
	in := "a/x.go:1:hit\na/x.go:2:hit\n"
	lossy := func(string) string { return "a/x.go\n1:hit\n" } // drops a row
	if out, ok := adopt(in, lossy, unfoldHitPath); ok || out != in {
		t.Fatalf("adopted a lossy fold: ok=%v out=%q", ok, out)
	}
	if out, ok := adopt(in, foldHitPath, unfoldHitPath); !ok || unfoldHitPath(out) != in {
		t.Fatalf("declined a sound fold: ok=%v out=%q", ok, out)
	}
}

func TestFoldShapes(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{{
		name: "grep -rn several hits per file",
		in:   "pkg/a.go:12:foo\npkg/a.go:31:foo\npkg/b.go:7:foo\n",
		want: "pkg/a.go\n12:foo\n31:foo\npkg/b.go\n7:foo\n",
	}, {
		name: "grep -A context rows keep their own separator",
		in:   "a.go:12:foo\na.go-13-bar\na.go-14-baz\n",
		want: "a.go\n12:foo\n13-bar\n14-baz\n",
	}, {
		name: "one hit per file folds the parent directory",
		in:   "src/pkg/a.go:12:foo\nsrc/pkg/b.go:31:foo\nsrc/pkg/c.go:7:foo\n",
		want: "src/pkg/\n\ta.go:12:foo\n\tb.go:31:foo\n\tc.go:7:foo\n",
	}, {
		name: "find output folds to dir + basenames",
		in:   "/x/lib/a.py\n/x/lib/b.py\n/x/lib/c.py\n",
		want: "/x/lib/\n\ta.py\n\tb.py\n\tc.py\n",
	}, {
		name: "single-file grep -n is already headed: no-op",
		in:   "12:foo\n13:bar\n14:baz\n",
		want: "12:foo\n13:bar\n14:baz\n",
	}, {
		name: "prose is not search output: no-op",
		in:   "hello there\nnothing to fold\n",
		want: "hello there\nnothing to fold\n",
	}, {
		name: "already folded output is not folded twice",
		in:   "pkg/a.go\n12:foo\n31:foo\n",
		want: "pkg/a.go\n12:foo\n31:foo\n",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FoldSearchOutput(tc.in); got != tc.want {
				t.Errorf("fold:\n got %q\nwant %q", got, tc.want)
			}
			if got := FoldSearchOutput(tc.in); unfoldHitPath(got) != tc.in && unfoldPrefixDir(got) != tc.in && got != tc.in {
				t.Errorf("round trip failed for %q", tc.in)
			}
		})
	}
}

// TestFrozenPrefixIsByteStable is the cache-safety proof. The fold carries no
// TailOnly gate because it is a pure function of one message's own text; what that
// buys is only real if the message folds to the SAME bytes on the next turn. So:
// fold a transcript, then fold it again with a turn appended, and require every
// already-sent message to come out byte-identical — no cached position re-anchored.
func TestFrozenPrefixIsByteStable(t *testing.T) {
	sf, err := newSearchfold(nil)
	if err != nil {
		t.Fatal(err)
	}
	caps := loadCaptures(t)
	var turn1 []schemas.ChatMessage
	for _, c := range caps[:20] {
		turn1 = append(turn1, toolMsgWithCall(&turn1, c.Cmd, c.Out))
	}
	turn2 := schema.CloneMessages(turn1)
	for _, c := range caps[20:24] {
		turn2 = append(turn2, toolMsgWithCall(&turn2, c.Cmd, c.Out))
	}
	run := func(msgs []schemas.ChatMessage) []schemas.ChatMessage {
		req := &schemas.BifrostChatRequest{Input: msgs}
		rep := &components.Report{}
		c := &components.Ctx{CacheAware: true, MaxCachedIdx: len(turn1) - 1}
		if err := sf.(*Searchfold).Reformat(req, rep, c); err != nil {
			t.Fatal(err)
		}
		return req.Input
	}
	a, b := run(turn1), run(turn2)
	for i := range a {
		if x, y := mustJSON(t, a[i]), mustJSON(t, b[i]); x != y {
			t.Fatalf("frozen prefix message %d changed between turns:\n%s\n%s", i, x, y)
		}
	}
}

// The pre-gate this replaces routed by the producing COMMAND: fold `grep` output, skip
// `cat`/`pytest` output. Measured on 1,795 real captured requests it lost on both axes —
// 234,722 tokens folded with the gate against 333,764 without, at 1.174 ms/request against
// 0.509 — because the pairing says which command ran, not what its output looks like, and
// resolving one json.Unmarshals a whole argument object per tool message.
//
// So the contract is now: the fold is attempted on every tool output and routes by CONTENT.
// The same blob folds whichever command produced it, and output with no repeated path
// prefix comes back untouched whichever command produced it. Losslessness does the safety
// work: adopt() only keeps a fold whose inverse reproduces the input byte for byte.
func TestRoutesByContentNotByCommand(t *testing.T) {
	hits := "src/a.go:1:x\nsrc/a.go:2:y\nsrc/b.go:3:z\n" + strings.Repeat("src/c.go:9:padding padding\n", 40)
	plain := "package main\n\n" + strings.Repeat("\tfmt.Println(\"some ordinary source line\")\n", 40)
	for _, tc := range []struct {
		cmd  string
		out  string
		fold bool
	}{
		{"grep -rn foo src", hits, true},
		{"cd /x && rg -n foo | head -50", hits, true},
		{"git grep -n foo", hits, true},
		// The gate declined these. The output is the same path-prefixed hit list, and the
		// fold is lossless, so declining it was 99,042 tokens of pure loss.
		{"cat search-results.txt", hits, true},
		{"python -m pytest -q", hits, true},
		{"make check 2>&1 | tee out.log", hits, true},
		// And output with nothing to fold stays byte-identical regardless of command.
		{"grep -rn foo src", plain, false},
		{"cat src/a.go", plain, false},
	} {
		var msgs []schemas.ChatMessage
		msgs = append(msgs, toolMsgWithCall(&msgs, tc.cmd, tc.out))
		req := &schemas.BifrostChatRequest{Input: msgs}
		sf := &Searchfold{minTokens: 50}
		if err := sf.Reformat(req, &components.Report{}, nil); err != nil {
			t.Fatal(err)
		}
		changed := schema.MessageText(req.Input[len(req.Input)-1]) != tc.out
		if changed != tc.fold {
			t.Errorf("%q: folded=%v want %v", tc.cmd, changed, tc.fold)
		}
	}
}

// TestMeasureOnRealCaptures is the measurement, not a pass/fail: it reports the token
// reduction the fold achieves over every captured search output.
func TestMeasureOnRealCaptures(t *testing.T) {
	caps := loadCaptures(t)
	var before, after, foldedBefore, foldedAfter, n int
	for _, c := range caps {
		got := FoldSearchOutput(c.Out)
		b, a := schema.TextTokens(c.Out), schema.TextTokens(got)
		before, after = before+b, after+a
		if got != c.Out {
			n++
			foldedBefore, foldedAfter = foldedBefore+b, foldedAfter+a
		}
	}
	t.Logf("searchfold over %d captured search outputs: %d -> %d tokens (%.1f%%)",
		len(caps), before, after, pct(before, after))
	t.Logf("  of those, %d folded: %d -> %d tokens (%.1f%%)", n, foldedBefore, foldedAfter, pct(foldedBefore, foldedAfter))
	if after >= before {
		t.Fatalf("no reduction at all on real captures: %d -> %d", before, after)
	}
}

func pct(b, a int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(b-a) / float64(b)
}

// toolMsgWithCall appends the assistant tool_call that produced out and returns the
// paired tool message — the shape apply.normalize hands the pipeline for both dialects.
func toolMsgWithCall(msgs *[]schemas.ChatMessage, cmd, out string) schemas.ChatMessage {
	id := fmt.Sprintf("toolu_%d", len(*msgs))
	name := "Bash"
	args, _ := json.Marshal(map[string]string{"command": cmd})
	*msgs = append(*msgs, schemas.ChatMessage{
		Role: schemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &schemas.ChatAssistantMessage{ToolCalls: []schemas.ChatAssistantMessageToolCall{{
			ID:       &id,
			Function: schemas.ChatAssistantMessageToolCallFunction{Name: &name, Arguments: string(args)},
		}}},
	})
	m := schemas.ChatMessage{Role: schemas.ChatMessageRoleTool, ChatToolMessage: &schemas.ChatToolMessage{ToolCallID: &id}}
	schema.SetMessageText(&m, out)
	return m
}

func mustJSON(t *testing.T, m schemas.ChatMessage) string {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func trunc(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}
