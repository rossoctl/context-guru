package offload

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// collapseFor builds a collapse with the shipped defaults unless yaml overrides them.
func collapseFor(t *testing.T, yaml string) *Collapse {
	t.Helper()
	c, err := newCollapse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return c.(*Collapse)
}

// collapseCtx is a warm turn with no cached prefix, so every message is in the tail and the
// depth gate is not what these tests are measuring (cold_gate_test.go covers that).
func collapseCtx() *components.Ctx {
	return &components.Ctx{Session: "s", Store: store.NewMemory(store.Options{})}
}

// oneLineJSON is the shape the character window exists for: a database/HTTP result
// serialised as a SINGLE line, which the line window declined outright — so nothing in the
// pipeline capped it and the provider answered 400 "prompt is too long" on a multi-megabyte
// body. No newline anywhere in it.
func oneLineJSON(approxBytes int) string {
	var b strings.Builder
	b.WriteString(`{"rows":[`)
	for i := 0; b.Len() < approxBytes; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"sku":"SKU-%06d","name":"widget %d","qty":%d,"price":%d.99}`, i, i, i, i%97, i%1000)
	}
	b.WriteString(`],"total":1}`)
	return b.String()
}

// The defect: a multi-megabyte ONE-LINE payload used to fall through collapse entirely
// (too_few_lines), and nothing downstream capped it. It must now be capped, stashed and
// marked — the three properties that make it a legal Offload.
func TestCollapseCapsSingleLineMegabytePayload(t *testing.T) {
	body := oneLineJSON(3 << 20)
	if strings.Contains(body, "\n") {
		t.Fatal("fixture must be a single line")
	}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	c := collapseCtx()
	var rep components.Report
	cl := collapseFor(t, "")
	keys, err := cl.Offload(req, &rep, c)
	if err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[0])
	if got == body {
		t.Fatalf("a %d-byte single-line output was left verbatim; gates %v", len(body), rep.Gates)
	}
	// Capped: at or under the token threshold it was judged oversized against.
	if n := schema.TextTokens(got); n > cl.maxTokens {
		t.Fatalf("collapsed output is %d tokens, above the %d-token threshold:\n%.200q", n, cl.maxTokens, got)
	}
	// Marked, and the marker resolves to the stash.
	if !expand.HasPlaceholder(got) {
		t.Fatalf("no expand marker left behind: %.300q", got)
	}
	if len(keys) != 1 {
		t.Fatalf("want 1 stash key, got %v", keys)
	}
	// Reversible: the recovered original is byte-complete.
	raw, ok := c.Store.Get(keys[0])
	if !ok {
		t.Fatalf("original not stashed under %q", keys[0])
	}
	if string(raw) != body {
		t.Fatalf("recovered original is not the input (%d bytes vs %d)", len(raw), len(body))
	}
	// Both ends kept, so the model can see WHAT it would be expanding.
	if !strings.HasPrefix(got, body[:64]) {
		t.Errorf("head of the payload not kept: %.100q", got)
	}
	if !strings.HasSuffix(got, body[len(body)-64:]) {
		t.Errorf("tail of the payload not kept: %q", got[len(got)-100:])
	}
	if rep.Events["char_window"] != 1 {
		t.Errorf("character path must be counted as an event, got events %v gates %v", rep.Events, rep.Gates)
	}
	if _, dead := rep.Gates["too_few_lines"]; dead {
		t.Error("too_few_lines must no longer count a message the character path handled")
	}
}

// A handful of long lines is the same defect: 40 lines of 100 KB each was declined too.
func TestCollapseCapsFewVeryLongLines(t *testing.T) {
	body := strings.Repeat(oneLineJSON(100<<10)+"\n", 5)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	cl := collapseFor(t, "")
	if _, err := cl.Offload(req, &rep, collapseCtx()); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[0]); got == body {
		t.Fatalf("5 lines of 100 KB left verbatim; gates %v", rep.Gates)
	}
}

// The property most likely to break: everything the LINE window already handled must come
// out byte-identical. A collapsed form is frozen and replayed at depth, so a changed layout
// re-anchors every cached position after it in live sessions. The expectation is spelled out
// here rather than captured from the implementation, so a layout change fails this test.
func TestCollapseLineWindowIsByteIdentical(t *testing.T) {
	cases := map[string]string{
		"plain log":      strings.Repeat("verbose tool output line with a bit of detail\n", 200),
		"uneven lines":   strings.Repeat("short\na much longer line carrying a path src/pkg/file.go:42 and words\n", 60),
		"trailing blank": strings.Repeat("data row\n", 100) + "\n\n",
		"no trailing nl": strings.TrimSuffix(strings.Repeat("data row\n", 100), "\n"),
	}
	for name, body := range cases {
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
		c := collapseCtx()
		var rep components.Report
		// A low threshold, so the fixtures can stay small and fast: max_tokens decides
		// WHETHER an output is collapsed, never how the line window is laid out.
		cl := collapseFor(t, "max_tokens: 50\n")
		if _, err := cl.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(body, "\n")
		want := fmt.Sprintf("%s\n... (%d lines omitted) %s [full output: call %s]\n%s",
			strings.Join(lines[:cl.headLines], "\n"),
			len(lines)-cl.headLines-cl.tailLines,
			expand.Marker(hashKey(body)), expand.ToolName,
			strings.Join(lines[len(lines)-cl.tailLines:], "\n"))
		if got := schema.MessageText(req.Input[0]); got != want {
			t.Fatalf("%s: line window is not byte-identical\n want %q\n got  %q", name, want, got)
		}
		if rep.Events["char_window"] != 0 {
			t.Fatalf("%s: multi-line output must not take the character path", name)
		}
	}
}

// never-worse, on the character path: when the window plus its marker would not actually
// shrink the output, it is left verbatim. The pipeline's own guard is per REQUEST, so
// without this a small-but-oversized message grows by the marker's tokens.
func TestCollapseCharWindowNeverWorse(t *testing.T) {
	// 300 characters on one line: above a 40-token threshold, but the window this
	// threshold buys (floored at collapseMinWindowChars) plus a <<cg:HASH>> marker and its
	// hint costs more tokens than the original.
	body := strings.Repeat("abcdefghij", 30)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	c := collapseCtx()
	var rep components.Report
	cl := collapseFor(t, "max_tokens: 40\n")
	if schema.TextTokens(body) <= cl.maxTokens {
		t.Fatalf("fixture must be oversized: %d tokens", schema.TextTokens(body))
	}
	if _, err := cl.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[0]); got != body {
		t.Fatalf("an output the window cannot shrink must stay verbatim, got %q", got)
	}
	if rep.Gates["marker_no_win"] == 0 {
		t.Fatalf("the decline must be named marker_no_win, gates %v", rep.Gates)
	}
	if !rep.Skipped {
		t.Error("a component that changed nothing must report Skipped")
	}
}

// Idempotent: content already carrying a marker is another component's stash (or an
// expansion the agent asked for). Cutting it again would orphan that stash.
func TestCollapseCharWindowSkipsMarkedContent(t *testing.T) {
	body := expand.Marker("deadbeef") + " " + oneLineJSON(1<<20)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	if _, err := collapseFor(t, "").Offload(req, &rep, collapseCtx()); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[0]); got != body {
		t.Fatalf("marker-bearing content must be untouched, got %.200q", got)
	}
	if rep.Gates["marker_or_kept_verbatim"] == 0 {
		t.Fatalf("want marker_or_kept_verbatim, gates %v", rep.Gates)
	}
}

// The window is cut on RUNE boundaries: the result is spliced back into the request body,
// so a split multibyte rune would emit invalid UTF-8 upstream.
//
// The fixture keeps ASCII separators between the CJK runs on purpose. tiktoken's merge loop
// is superlinear in the length of a single unbroken word, and a separator-free CJK blob is
// exactly that: 600 KB of it did not finish tokenizing in a ten-minute test timeout. That is
// pre-existing (collapse's own size test tokenizes the whole content), but it means a test
// fixture must not be a separator-free wall of text.
func TestCollapseCharWindowKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("日本語のログ行 ", 3000) // one line, multibyte, with separators
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	if _, err := collapseFor(t, "max_tokens: 300\n").Offload(req, &rep, collapseCtx()); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[0])
	if got == body {
		t.Fatalf("single-line CJK payload left verbatim, gates %v", rep.Gates)
	}
	if !utf8.ValidString(got) {
		t.Fatal("character window split a multibyte rune")
	}
}

// head_lines:tail_lines is the ratio the character window splits by, so the existing knobs
// keep meaning "how much of the start / the end is kept" on both paths.
func TestCollapseCharWindowHonoursHeadTailRatio(t *testing.T) {
	body := oneLineJSON(1 << 20)
	head, tail := collapseFor(t, "head_lines: 90\ntail_lines: 10\n").splitWindow(1000)
	if head != 900 || tail != 100 {
		t.Fatalf("want 900/100 split, got %d/%d", head, tail)
	}
	// And the skew shows up in the rewrite.
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	cl := collapseFor(t, "head_lines: 90\ntail_lines: 10\nmax_tokens: 500\n")
	if _, err := cl.Offload(req, &rep, collapseCtx()); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[0])
	i := strings.Index(got, "\n... (")
	if i < 0 {
		t.Fatalf("no elision notice: %.200q", got)
	}
	if kept := len([]rune(got[i+1:])); len([]rune(got[:i])) < 4*kept {
		t.Fatalf("head share not skewed: head %d runes, rest %d", len([]rune(got[:i])), kept)
	}
}

// The only genuine decline left, and it must still be named: too few lines for a line window
// AND fewer characters than the smallest character window. `too_few_lines` is gone, so this
// is where the residual gap stays visible on /stats.
func TestCollapseTooSmallForEitherWindow(t *testing.T) {
	body := strings.Repeat("x", collapseMinWindowChars-50) // one line, shorter than the floor
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	cl := collapseFor(t, "max_tokens: 1\n")
	if schema.TextTokens(body) <= cl.maxTokens {
		t.Fatal("fixture must be oversized")
	}
	if _, err := cl.Offload(req, &rep, collapseCtx()); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[0]); got != body {
		t.Fatalf("must stay verbatim, got %q", got)
	}
	if rep.Gates["too_few_lines_and_chars"] != 1 {
		t.Fatalf("want too_few_lines_and_chars, gates %v", rep.Gates)
	}
	if _, dead := rep.Gates["too_few_lines"]; dead {
		t.Error("too_few_lines is retired; it must not reappear")
	}
}

// charWindowFloored is the exact head/tail split every fixture below is cut with: at
// max_tokens 50 the character budget is min(50*4, len(r)-1) = 200, which IS
// collapseMinWindowChars, so runeWindow returns at pass 0 without a fit pass and
// splitWindow divides 200 in the default 20:20 ratio. Spelled out here so the expectations
// further down are a statement about the LAYOUT rather than a re-run of the budget code.
const charWindowFlooredHead, charWindowFlooredTail = 100, 100

// The character path's output is frozen and replayed at depth exactly like the line path's,
// so it needs the same protection the line path got in TestCollapseLineWindowIsByteIdentical:
// a changed layout re-anchors every cached position after it in every live session
// mid-flight. Without a byte-exact expectation, an off-by-one head slice (r[:h+1]) or a
// wrong omitted count passes the whole suite — both did.
//
// The expectation is written out as a literal format string rather than captured from
// window(), so a layout change fails here instead of being adopted silently.
func TestCollapseCharWindowIsByteIdentical(t *testing.T) {
	longLine := oneLineJSON(5 << 10)
	cases := map[string]string{
		// The motivating shape: one line, dense, no newline anywhere.
		"one line json": oneLineJSON(600),
		// Multibyte, so a mutation from runes to bytes moves the cut and fails here.
		"multibyte": strings.Repeat("日本語のログ行 ", 40),
		// Plenty of lines, but a huge one inside the kept head, so the line window does not
		// shrink it and the byte gate falls through to this path. The layout must be the
		// same as any other character cut.
		"huge line in head": longLine + "\n" + strings.Repeat("filler row\n", 60),
		// Same, with the huge line in the kept tail.
		"huge line in tail": strings.Repeat("filler row\n", 60) + longLine,
	}
	for name, body := range cases {
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
		c := collapseCtx()
		var rep components.Report
		cl := collapseFor(t, "max_tokens: 50\n")
		if _, err := cl.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		r := []rune(body)
		if len(r) <= charWindowFlooredHead+charWindowFlooredTail {
			t.Fatalf("%s: fixture must be longer than the floored window (%d runes)", name, len(r))
		}
		want := fmt.Sprintf("%s\n... (%d characters omitted) %s [full output: call %s]\n%s",
			string(r[:charWindowFlooredHead]),
			len(r)-charWindowFlooredHead-charWindowFlooredTail,
			expand.Marker(hashKey(body)), expand.ToolName,
			string(r[len(r)-charWindowFlooredTail:]))
		if got := schema.MessageText(req.Input[0]); got != want {
			t.Fatalf("%s: character window is not byte-identical\n want %q\n got  %q", name, want, got)
		}
		// A precondition, not a nicety: if the line path had handled the fixture the
		// comparison above would be vacuous as an assertion about THIS window.
		if rep.Events["char_window"] != 1 {
			t.Fatalf("%s: fixture did not take the character path, events %v gates %v", name, rep.Events, rep.Gates)
		}
	}
}

// The boundary this PR moves, asserted from the other side. At EXACTLY head_lines+tail_lines
// lines the line window would omit zero lines, so it must not be taken. Flipping the gate to
// `>=` survived all eight of the first round's tests: the line path computes omitted = 0,
// emits the whole content plus a marker, fails tryMark and declines marker_no_win — so a
// 40-line multi-megabyte output sails through with a green suite.
func TestCollapseCharWindowAtExactlyTheLineWindowSize(t *testing.T) {
	cl := collapseFor(t, "")
	var b strings.Builder
	for i := 0; i < cl.headLines+cl.tailLines; i++ {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(oneLineJSON(2 << 10))
	}
	body := b.String()
	if n := strings.Count(body, "\n") + 1; n != cl.headLines+cl.tailLines {
		t.Fatalf("fixture must have exactly %d lines, has %d", cl.headLines+cl.tailLines, n)
	}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	c := collapseCtx()
	var rep components.Report
	if _, err := cl.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[0])
	if got == body {
		t.Fatalf("a %d-byte output of exactly %d lines was left verbatim; gates %v events %v",
			len(body), cl.headLines+cl.tailLines, rep.Gates, rep.Events)
	}
	if rep.Events["char_window"] != 1 {
		t.Fatalf("exactly headLines+tailLines lines must take the character path, events %v gates %v", rep.Events, rep.Gates)
	}
	if n := schema.TextTokens(got); n > cl.maxTokens {
		t.Fatalf("collapsed output is %d tokens, above the %d-token threshold", n, cl.maxTokens)
	}
}

// The blocking defect the review measured: the window was chosen on the LINE COUNT, so an
// output with more than head_lines+tail_lines lines took the line path however big those
// lines were. A multi-megabyte line inside the kept head or tail survived untouched — and
// worse than being declined, because dropping the small filler lines in the middle counts as
// acting and stamps a marker, which then makes linecap decline marker_or_kept_verbatim.
// Measured: 4,195,111 B in, 4,194,922 B out, 189 bytes saved, 1.57 M tokens forwarded.
func TestCollapseCutsAHugeLineInsideTheLineWindow(t *testing.T) {
	huge := oneLineJSON(1 << 20) // one line, bigger than any window
	filler := strings.Repeat("filler row\n", 60)
	for name, body := range map[string]string{
		"huge line in head": huge + "\n" + filler,
		"huge line in tail": filler + huge,
	} {
		cl := collapseFor(t, "")
		if n := strings.Count(body, "\n") + 1; n <= cl.headLines+cl.tailLines {
			t.Fatalf("%s: fixture must have MORE lines than the line window, has %d", name, n)
		}
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
		c := collapseCtx()
		var rep components.Report
		keys, err := cl.Offload(req, &rep, c)
		if err != nil {
			t.Fatal(err)
		}
		got := schema.MessageText(req.Input[0])
		// The old behaviour: acted, marked, and still multi-megabyte.
		if len(got)*2 > len(body) {
			t.Fatalf("%s: %d-byte output came back %d bytes — the line window kept it; gates %v events %v",
				name, len(body), len(got), rep.Gates, rep.Events)
		}
		if n := schema.TextTokens(got); n > cl.maxTokens {
			t.Fatalf("%s: collapsed output is %d tokens, above the %d-token threshold", name, n, cl.maxTokens)
		}
		if rep.Events["char_window"] != 1 {
			t.Fatalf("%s: a huge kept line must fall through to the character window, events %v gates %v", name, rep.Events, rep.Gates)
		}
		// Still reversible: the fall-through goes through the same stash.
		if len(keys) != 1 {
			t.Fatalf("%s: want 1 stash key, got %v", name, keys)
		}
		if raw, ok := c.Store.Get(keys[0]); !ok || string(raw) != body {
			t.Fatalf("%s: original not recoverable byte-for-byte", name)
		}
	}
}

// A units mismatch declined a 55%-wide band of genuinely oversized content: the bail-out at
// `head+tail >= len(r)` was tested against the UN-measured chars/4 seed, and dense JSON
// tokenizes closer to 2.5 chars/token, so content over max_tokens was still shorter in runes
// than its chars/4 budget and was declined outright as too_few_lines_and_chars. max_tokens
// 3000 is what the shipped codesafe preset sets, which gave it the widest hole
// (3,006-4,491 tokens uncut) and no test covered it.
func TestCollapseCutsDenseJSONInTheOversizeBand(t *testing.T) {
	for _, maxTokens := range []int{2000, 3000} {
		cl := collapseFor(t, fmt.Sprintf("max_tokens: %d\n", maxTokens))
		// Sized into the band: over the threshold in TOKENS, under it in chars/4 runes.
		body := oneLineJSON(maxTokens * 8 / 3)
		tokens, runes := schema.TextTokens(body), len([]rune(body))
		if tokens <= maxTokens {
			t.Fatalf("max_tokens %d: fixture must be oversized, is %d tokens", maxTokens, tokens)
		}
		if runes >= maxTokens*collapseCharsPerToken {
			t.Fatalf("max_tokens %d: fixture must be INSIDE the band (%d runes vs a %d-char seed)",
				maxTokens, runes, maxTokens*collapseCharsPerToken)
		}
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
		var rep components.Report
		if _, err := cl.Offload(req, &rep, collapseCtx()); err != nil {
			t.Fatal(err)
		}
		got := schema.MessageText(req.Input[0])
		if got == body {
			t.Fatalf("max_tokens %d: %d-token single-line output left uncut; gates %v", maxTokens, tokens, rep.Gates)
		}
		if n := schema.TextTokens(got); n > maxTokens {
			t.Fatalf("max_tokens %d: collapsed output is %d tokens, still above the threshold", maxTokens, n)
		}
		if rep.Events["char_window"] != 1 {
			t.Fatalf("max_tokens %d: want the character window, events %v gates %v", maxTokens, rep.Events, rep.Gates)
		}
	}
}

// head_lines: 0, tail_lines: 0 reached the LINE path (`1 > 0+0`) and produced a bare marker
// with no content cue — an 87-byte rewrite of a 64 KB output, which is precisely what
// collapseMinWindowChars exists to prevent. With no line window configured the character
// window (even split, floored) is the only sensible reading of "cap this output".
func TestCollapseZeroLineWindowStillKeepsContent(t *testing.T) {
	body := strings.Repeat("payload line with some detail in it\n", 2000)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	cl := collapseFor(t, "head_lines: 0\ntail_lines: 0\n")
	if _, err := cl.Offload(req, &rep, collapseCtx()); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[0])
	if got == body {
		t.Fatalf("output left verbatim; gates %v", rep.Gates)
	}
	if len(got) < collapseMinWindowChars {
		t.Fatalf("a %d-byte output collapsed to %d bytes — a bare marker with no cue: %q", len(body), len(got), got)
	}
	if !strings.HasPrefix(got, body[:64]) {
		t.Errorf("head of the payload not kept: %.120q", got)
	}
	if rep.Events["char_window"] != 1 {
		t.Fatalf("no line window means the character window, events %v gates %v", rep.Events, rep.Gates)
	}
}

// A negative head_lines used to panic: `1 > -10` sent even a one-liner down the line path,
// where lines[:-10] slices out of range. Fail-open contained it, and splitWindow's own
// max(...) guard was unreachable for exactly the config it looked like it defended. The
// clamp is now in newCollapse, so it covers both paths.
func TestCollapseNegativeLineWindowIsClamped(t *testing.T) {
	cl := collapseFor(t, "head_lines: -10\ntail_lines: -5\nmax_tokens: 50\n")
	if cl.headLines != 0 || cl.tailLines != 0 {
		t.Fatalf("negative line knobs must be clamped to 0, got %d/%d", cl.headLines, cl.tailLines)
	}
	body := oneLineJSON(4 << 10)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	if _, err := cl.Offload(req, &rep, collapseCtx()); err != nil { // must not panic
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[0]); got == body {
		t.Fatalf("output left verbatim; gates %v", rep.Gates)
	}
}
