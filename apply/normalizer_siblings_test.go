package apply_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// siblingTextProbe is a no-op Reformat component that records every normalized
// message's role and text, in order, so a test can assert on apply's internal view
// without going through a config string or a real offloader.
type siblingTextProbe struct {
	seen []seenMsg
}

type seenMsg struct {
	role bschemas.ChatMessageRole
	text string
}

func (*siblingTextProbe) Name() string                 { return "sibling-text-probe" }
func (*siblingTextProbe) Enabled(*components.Ctx) bool { return true }
func (p *siblingTextProbe) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	p.seen = p.seen[:0]
	for _, m := range req.Input {
		p.seen = append(p.seen, seenMsg{role: m.Role, text: schema.MessageText(m)})
	}
	return nil
}

// TestNormalizeKeepsSiblingTextBlocksInOrder is issue #457: apply.normalize turned every
// tool_result block of an Anthropic user message into a synthetic role=tool message and then
// skipped the rest of that message's blocks entirely, so mid-turn user text, system reminders
// and task/teammate notifications sharing the message never reached the normalized view any
// component reads — only the raw wire body carried them.
//
// Covers both permitted shapes of a tool_result's own content (string, and an array of text
// blocks), with a sibling text block before, between, and after the tool_result blocks.
func TestNormalizeKeepsSiblingTextBlocksInOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string // the tool_result block's own `content`
	}{
		{"string content", `"first tool output"`},
		{"array content", `[{"type":"text","text":"first tool output"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"claude-x","messages":[` +
				`{"role":"user","content":"go read the log"},` +
				`{"role":"assistant","content":[` +
				`{"type":"tool_use","id":"tu_1","name":"Read","input":{}},` +
				`{"type":"tool_use","id":"tu_2","name":"Read","input":{}}]},` +
				`{"role":"user","content":[` +
				`{"type":"text","text":"BEFORE: the user typed this while tool 1 ran"},` +
				`{"type":"tool_result","tool_use_id":"tu_1","content":` + tc.content + `},` +
				`{"type":"text","text":"BETWEEN: a system reminder landed here"},` +
				`{"type":"tool_result","tool_use_id":"tu_2","content":` + tc.content + `},` +
				`{"type":"text","text":"AFTER: a teammate notification landed here"}]}]}`)

			probe := &siblingTextProbe{}
			p := components.NewPipeline([]components.Component{probe}, nil)
			out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
				bschemas.Anthropic, body, "", false)

			if changed {
				t.Fatalf("a no-op pipeline must not change the body:\n want=%s\n got =%s", body, out)
			}
			if string(out) != string(body) {
				t.Fatalf("a no-op pipeline must give byte-identical output:\n want=%s\n got =%s", body, out)
			}

			wantTexts := []string{
				"go read the log",
				"", // the assistant turn requesting the two tool calls
				"BEFORE: the user typed this while tool 1 ran",
				"first tool output",
				"BETWEEN: a system reminder landed here",
				"first tool output",
				"AFTER: a teammate notification landed here",
			}
			if len(probe.seen) != len(wantTexts) {
				t.Fatalf("normalized view has %d messages, want %d: %+v", len(probe.seen), len(wantTexts), probe.seen)
			}
			for i, want := range wantTexts {
				if probe.seen[i].text != want {
					t.Errorf("message %d text = %q, want %q", i, probe.seen[i].text, want)
				}
			}
			// The sibling text blocks are synthetic role=user messages (never role=tool —
			// that is reserved for the tool_result extraction this fix must not change).
			for _, idx := range []int{2, 4, 6} {
				if probe.seen[idx].role != bschemas.ChatMessageRoleUser {
					t.Errorf("sibling at %d has role %s, want user", idx, probe.seen[idx].role)
				}
			}
			for _, idx := range []int{3, 5} {
				if probe.seen[idx].role != bschemas.ChatMessageRoleTool {
					t.Errorf("tool_result %d has role %s, want tool", idx, probe.seen[idx].role)
				}
			}
		})
	}
}

// TestNormalizeLeavesPlainTextUserMessagesAlone is the control: a user message with no
// tool_result at all must take the ordinary whole-message path, unaffected by #457's fix.
func TestNormalizeLeavesPlainTextUserMessagesAlone(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"just text, no tool_result here"}]}]}`)

	probe := &siblingTextProbe{}
	p := components.NewPipeline([]components.Component{probe}, nil)
	out, changed := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "", false)

	if changed || string(out) != string(body) {
		t.Fatalf("a no-op pipeline must give byte-identical output:\n want=%s\n got =%s", body, out)
	}
	if len(probe.seen) != 1 || probe.seen[0].text != "just text, no tool_result here" {
		t.Fatalf("got %+v, want exactly one message with the plain text", probe.seen)
	}
}

// TestNormalizeSiblingTextIsReadOnly: a component that rewrites a sibling-text message's
// content must have that change discarded, never spliced into the body — there is no
// write-back shape for it (see the anthropicSiblingText slot kind doc).
type siblingRewriter struct{}

func (siblingRewriter) Name() string                 { return "sibling-rewriter" }
func (siblingRewriter) Enabled(*components.Ctx) bool { return true }
func (siblingRewriter) Reformat(req *bschemas.BifrostChatRequest, _ *components.Report, _ *components.Ctx) error {
	for i := range req.Input {
		if req.Input[i].Role == bschemas.ChatMessageRoleUser {
			schema.SetMessageText(&req.Input[i], "REWRITTEN")
		}
	}
	return nil
}

func TestNormalizeSiblingTextIsReadOnly(t *testing.T) {
	body := []byte(`{"model":"claude-x","messages":[` +
		`{"role":"user","content":[` +
		`{"type":"text","text":"mid-turn text"},` +
		`{"type":"tool_result","tool_use_id":"tu_1","content":"tool output"}]}]}`)

	p := components.NewPipeline([]components.Component{siblingRewriter{}}, nil)
	out, _ := apply.Body(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "", false)

	if string(out) != string(body) {
		t.Fatalf("a read-only sibling edit must never reach the body:\n want=%s\n got =%s", body, out)
	}
}

// recordingModel is a components.Model stub that records the last prompt it was asked to
// complete, so a test can assert on what the summarizer actually sent.
type recordingModel struct {
	reply  string
	prompt atomic.Value
}

func (m *recordingModel) Complete(_ context.Context, prompt string) (string, error) {
	m.prompt.Store(prompt)
	return m.reply, nil
}

func (m *recordingModel) lastPrompt() string { s, _ := m.prompt.Load().(string); return s }

// CompleteMessages makes recordingModel also satisfy components.MessagesModel, which
// cache_aware_summarizer requires (components.Model alone is not enough — see its own
// "needs either a MessagesModel or a PrefixAsker" doc). Flattened into the same prompt string
// Complete stores, so lastPrompt works for either caller.
func (m *recordingModel) CompleteMessages(ctx context.Context, system string, msgs []bschemas.ChatMessage) (string, error) {
	var b strings.Builder
	b.WriteString(system)
	for _, msg := range msgs {
		b.WriteString("\n[")
		b.WriteString(string(msg.Role))
		b.WriteString("]\n")
		b.WriteString(schema.MessageText(msg))
	}
	return m.Complete(ctx, b.String())
}

// TestSummarizeSeesMidTurnSiblingText is #457's point: before the fix, mid-turn user text
// sharing an Anthropic user message with a tool_result never reached the normalized view, so a
// summarized span that CONTAINED that text summarized the tool_result but never saw what the
// user actually said while it ran.
func TestSummarizeSeesMidTurnSiblingText(t *testing.T) {
	cfg := pipe(t, "pipeline: [summarize]\ncomponents:\n  summarize: {keep_last: 1, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})

	body, _ := json.Marshal(map[string]any{
		"model": "claude-x",
		"messages": []map[string]any{
			{"role": "user", "content": "do the task"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "call_a", "name": "Read", "input": map[string]any{}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "MIDTURN SIBLING: the user said this while the tool ran"},
				{"type": "tool_result", "tool_use_id": "call_a",
					"content": strings.Repeat("verbose tool output\n", 50)},
			}},
			{"role": "user", "content": "the final question"},
		},
	})

	model := &recordingModel{reply: "a summary"}
	// Two passes: the first commissions the summary off the hot path and forwards the body
	// untouched, the second splices the result — see TestSummarizeCountChangeLossless.
	apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false,
		components.ModelSpec{Incoming: model})
	offload.WaitForAllSummariesForTest(5 * time.Second)
	apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false,
		components.ModelSpec{Incoming: model})

	if !strings.Contains(model.lastPrompt(), "MIDTURN SIBLING: the user said this while the tool ran") {
		t.Fatalf("the summarizer's prompt did not contain the mid-turn sibling text:\n%s", model.lastPrompt())
	}
}

// buildSplitGroupBody is an Anthropic-shaped transcript (roles strictly alternating, as the
// real API requires) with one wire message (index 6) that holds BOTH a sibling text block and
// the tool_result answering the PRECEDING message's tool_use (index 5) — the exact shape #457
// is about — plus enough ordinary filler turns on each side to position the group either
// inside a summarized span or inside a kept tail, depending on keep_last.
func buildSplitGroupBody() []byte {
	// summarize's own fixed wrapper text ("=== History Summary ===\n...Do not summarize the
	// conversation again.") costs the summary message ~50-80 tokens by itself, REGARDLESS of
	// how short the model's reply is. Every filler message below must be large enough, on its
	// own, that compacting it still shrinks the request net of that fixed overhead — otherwise
	// the pipeline's never-worse guard (components/pipeline.go) correctly reverts a "summary"
	// that grew the request, and the splice this test exists to exercise never happens.
	filler := func(n string) string { return "small talk " + n + " " + strings.Repeat("filler word ", 60) }
	body, _ := json.Marshal(map[string]any{
		"model": "claude-x",
		"messages": []map[string]any{
			{"role": "user", "content": "task"},
			{"role": "assistant", "content": filler("1")},
			{"role": "user", "content": filler("2")},
			{"role": "assistant", "content": filler("3")},
			{"role": "user", "content": filler("4")},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "call_a", "name": "Read", "input": map[string]any{}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "SIBLING TEXT"},
				{"type": "tool_result", "tool_use_id": "call_a",
					"content": "tool output a " + strings.Repeat("filler ", 60)},
			}},
			{"role": "assistant", "content": "ran it"},
			{"role": "user", "content": "the final question"},
		},
	})
	return body
}

// assertSplitGroupRebuild runs the two-pass commission/splice cycle for a count-changing
// summarizer and checks: the split group (sibling text + its tool_result) survives EITHER
// wholly verbatim (kept-tail case) or wholly gone (summarized-away case) — never one half of
// each — and the result is shape-valid (#457 requirement 1 and 3: the rebuild must not split a
// wire message or orphan a tool_use/tool_result pairing).
func assertSplitGroupRebuild(t *testing.T, pipelineYAML string, groupInTail bool) {
	t.Helper()
	cfg := pipe(t, pipelineYAML)
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	body := buildSplitGroupBody()
	// Tagged, not a bare string: summarize's ensureSummaryTags wraps an untagged reply, but
	// cache_aware_summarizer requires the model's own output to already carry a closing
	// </summary> tag — see cacheAwareSummaryIncomplete — or it discards the reply as truncated
	// and never produces a checkpoint for either case to splice.
	model := &recordingModel{reply: "<summary>SUMMARY-MARKER covering the exchange</summary>"}

	// Two passes: the first commissions the summary off the hot path and forwards the body
	// untouched, the second splices the result — see TestSummarizeCountChangeLossless.
	apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false,
		components.ModelSpec{Incoming: model})
	offload.WaitForAllSummariesForTest(5 * time.Second)
	out, changed := apply.BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false,
		components.ModelSpec{Incoming: model})
	if !changed {
		t.Fatal("the summarizer never acted — the count-changing rebuild under test never ran")
	}

	hasSibling := strings.Contains(string(out), "SIBLING TEXT")
	hasTool := strings.Contains(string(out), "tool output a")
	if groupInTail {
		if !hasSibling || !hasTool {
			t.Errorf("the split group was expected verbatim in the kept tail, got sibling=%v tool=%v\n%s",
				hasSibling, hasTool, out)
		}
	} else if hasSibling || hasTool {
		t.Errorf("the split group was expected summarized away WHOLE, but found sibling=%v tool=%v (one "+
			"half survived while the other was summarized -- a split wire message)\n%s", hasSibling, hasTool, out)
	}

	// Shape must stay valid either way: no leaked role=tool message, no orphaned
	// tool_use/tool_result pairing, on the WIRE (not the re-normalized view — a sibling text
	// block legitimately sits between a tool_use's answering message and ITS tool_result
	// block now, which schema.ValidateShapeFor's own adjacency check does not expect; see the
	// PR's technical notes). assertAnthropicWirePairing checks the actual invariant the
	// provider enforces: each tool_use answered by a tool_result in the IMMEDIATELY
	// FOLLOWING wire message, wherever in that message's content array it sits.
	assertAnthropicWirePairing(t, out)
}

// assertAnthropicWirePairing checks the Anthropic wire-level invariant every tool_use must
// answer, and vice versa, allowing a sibling text block anywhere in the answering message's
// content array (#457) — unlike schema.ValidateShapeFor, which checks adjacency in the
// RE-NORMALIZED view and does not know about wire-message grouping.
func assertAnthropicWirePairing(t *testing.T, out []byte) {
	t.Helper()
	msgs := gjson.GetBytes(out, "messages").Array()
	for i, m := range msgs {
		if m.Get("role").String() == "tool" {
			t.Errorf("messages.%d: leaked role=tool message on the Anthropic wire", i)
		}
		if m.Get("role").String() != "assistant" {
			continue
		}
		m.Get("content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() != "tool_use" {
				return true
			}
			id := blk.Get("id").String()
			if i+1 >= len(msgs) {
				t.Errorf("messages.%d: tool_use %q has no following message to answer it", i, id)
				return true
			}
			next := msgs[i+1]
			answered := false
			next.Get("content").ForEach(func(_, nb gjson.Result) bool {
				if nb.Get("type").String() == "tool_result" && nb.Get("tool_use_id").String() == id {
					answered = true
				}
				return true
			})
			if !answered {
				t.Errorf("messages.%d: tool_use %q is not answered by a tool_result in messages.%d", i, id, i+1)
			}
			return true
		})
	}
	// The mirror check: every tool_result answers a tool_use that appeared in the
	// IMMEDIATELY PRECEDING message.
	for i, m := range msgs {
		if m.Get("role").String() != "user" {
			continue
		}
		m.Get("content").ForEach(func(_, blk gjson.Result) bool {
			if blk.Get("type").String() != "tool_result" {
				return true
			}
			id := blk.Get("tool_use_id").String()
			if i == 0 {
				t.Errorf("messages.%d: tool_result %q has no preceding message to answer", i, id)
				return true
			}
			prev := msgs[i-1]
			called := false
			prev.Get("content").ForEach(func(_, pb gjson.Result) bool {
				if pb.Get("type").String() == "tool_use" && pb.Get("id").String() == id {
					called = true
				}
				return true
			})
			if !called {
				t.Errorf("messages.%d: tool_result %q answers no tool_use in messages.%d", i, id, i-1)
			}
			return true
		})
	}
}

// TestSummarizeRebuildWithASplitGroup is #457 requirement 3 for `summarize`: run the
// count-changing splice with the split group positioned inside the summarized span, and again
// inside the kept tail.
func TestSummarizeRebuildWithASplitGroup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keepLast    int
		groupInTail bool
	}{
		{"group inside the summarized span", 2, false},
		{"group inside the kept tail", 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "pipeline: [summarize]\ncomponents:\n  summarize: {keep_last: " +
				strconv.Itoa(tc.keepLast) + ", start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n"
			assertSplitGroupRebuild(t, yaml, tc.groupInTail)
		})
	}
}

// TestExtractNeverTreatsASiblingTextBlockAsAToolOutput is #457 requirement 4: extract and
// extract_llm must never rewrite a sibling text block as if it were tool output to reduce.
// Both components select their targets via offload.toolIndices (role == tool), which already
// excludes a sibling (role == user) by construction — this is the regression test for that
// invariant, not a new restriction.
func TestExtractNeverTreatsASiblingTextBlockAsAToolOutput(t *testing.T) {
	cfg := pipe(t, "pipeline: [extract, extract_llm]\ncomponents:\n"+
		"  extract: {min_tokens: 1, trigger: {min_request_frac: 0}}\n"+
		"  extract_llm: {strategy: deterministic, min_tokens: 1, economic_gate: false, "+
		"allow_on_caching_backend: true, trigger: {min_request_frac: 0}}\n")
	p, _ := cfg.Build(nil)

	// The sibling text is itself full of "obvious noise" (a long run of EXACTLY repeated,
	// non-blank lines, each carrying real tokens so collapsing it is a genuine win) — if
	// extract's role filter were ever lost, THIS is what would get collapsed. A run of blank
	// lines does not do: collapseObviousNoise still collapses it, but blank lines cost ~0
	// measured tokens, so the win never clears the marker's own overhead and nothing changes
	// either message — which proves nothing about the role filter either way.
	noise := strings.Repeat("repeated noisy log line\n", 60)
	sibling := "SIBLING TEXT START\n" + noise + "SIBLING TEXT END"
	toolOut := "TOOL OUTPUT START\n" + noise + "TOOL OUTPUT END"
	body, _ := json.Marshal(map[string]any{
		"model": "claude-x",
		"messages": []map[string]any{
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "tool_use", "id": "call_a", "name": "Read", "input": map[string]any{}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": sibling},
				{"type": "tool_result", "tool_use_id": "call_a", "content": toolOut},
			}},
		},
	})

	out, _ := apply.BodyWithModel(context.Background(), p, store.NewMemory(store.Options{}),
		bschemas.Anthropic, body, "", false, components.ModelSpec{})

	// Compared via gjson (decoded strings), not a raw byte substring search on `out`: the wire
	// body JSON-escapes the newlines in `sibling`/`toolOut`, so a raw-bytes search would never
	// match even on a correct, untouched sibling.
	gotSibling := gjson.GetBytes(out, "messages.2.content.0.text").String()
	gotTool := gjson.GetBytes(out, "messages.2.content.1.content").String()
	if gotSibling != sibling {
		t.Fatalf("the sibling text block was rewritten; it must never be a tool-output candidate:\nwant=%q\ngot =%q",
			sibling, gotSibling)
	}
	if gotTool == toolOut {
		t.Fatal("the tool output was never rewritten, so this fixture proves nothing about the role filter")
	}
}

// TestCacheAwareSummarizerRebuildWithASplitGroup is the same requirement for
// cache_aware_summarizer, which shares summarizeSpan but reaches it through its own config and
// splice path.
func TestCacheAwareSummarizerRebuildWithASplitGroup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keepLast    int
		groupInTail bool
	}{
		{"group inside the summarized span", 2, false},
		{"group inside the kept tail", 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "pipeline: [cache_aware_summarizer]\ncomponents:\n  cache_aware_summarizer:\n" +
				"    keep_last_turns: " + strconv.Itoa(tc.keepLast) + "\n    min_tokens: 1\n" +
				"    trigger:\n      min_messages: 1\n      min_request_tokens: 1\n      min_request_frac: 0\n"
			assertSplitGroupRebuild(t, yaml, tc.groupInTail)
		})
	}
}
