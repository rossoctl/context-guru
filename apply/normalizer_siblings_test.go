package apply_test

import (
	"context"
	"encoding/json"
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

// The split-group REBUILD test (assertSplitGroupRebuild,
// TestSummarizeRebuildWithASplitGroup) lives in shape_validate_test.go, inside the apply
// package itself: its shape assertion needs normalize's slots and wireGroups, which are
// package-internal — exactly the grouping-aware validation that replaced the hand-rolled
// wire-pairing check this file used to carry.

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
