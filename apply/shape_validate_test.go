package apply

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	_ "github.com/rossoctl/context-guru/components/all"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// schema.ValidateShape applied where the four historical summarize defects actually surfaced:
// the message list a request is rebuilt from. Each of 2edb9d4, fb5c460, e7d1aa8 was found by a
// provider rejecting a live request; all three are properties of this list, so they are
// assertable here with no provider and no model.
//
// The list checked is the NORMALIZED view — the shape components mutate, with Anthropic
// `tool_use` blocks lifted into ToolCalls and each `tool_result` block its own synthetic
// role=tool message. Re-normalizing the EMITTED wire is what makes this an end-to-end check:
// it starts from the bytes the proxy would actually send, after apply's rebuild, rather than
// from the in-memory slice a component happened to hand back.
//
// WHAT THE ROUND-TRIP CANNOT SEE, and why the raw-body check below exists. normalize() is
// lossy in exactly the direction that matters for one defect: it maps a legal Anthropic wire
// and a wire carrying the illegal `"role":"tool"` onto the IDENTICAL normalized list, because
// a tool_result block and a leaked role=tool message both become one synthetic role=tool
// message. So the one wire-level defect this repo has actually paid a 400 for
// (`Unexpected role "tool"`, fixed on main, recorded in apply/toolrole_wire_test.go) is
// structurally invisible to ValidateShape here — the round-trip destroys the evidence before
// the validator runs. Roles are therefore asserted on the RAW BODY, before normalization, by
// assertWireRolesLegal. Shape rules go through the validator; byte-level role legality does
// not, and cannot.

// shapeModel is a canned summarizer, so these tests assert on SHAPE and never on wording.
type shapeModel struct{}

func (shapeModel) Complete(context.Context, string) (string, error) {
	return "essential facts from the earlier trajectory", nil
}

// assertShapeValid judges the normalized list WITH its wire grouping (#461). The groups
// come from normalize's own slots, via wireGroups: a sibling text block sharing a wire
// message with the tool_result that answers a call is then read as the answer it is,
// instead of as a break in the run that answers the call — the false positive the grouped
// validator exists to fix. nil groups (a slot path that did not parse) is the legacy
// every-message-its-own-wire-message reading.
func assertShapeValid(t *testing.T, what string, msgs []bschemas.ChatMessage, groups []int) {
	t.Helper()
	if vs := schema.ValidateShapeGrouped(bschemas.Anthropic, msgs, groups); len(vs) != 0 {
		t.Errorf("%s: %d shape violation(s) — a provider rejects this request:\n%s",
			what, len(vs), schema.FormatShapeViolations(vs, msgs))
	}
}

// assertWireRolesLegal checks role legality on the RAW body, which is the only place it is
// decidable: Anthropic accepts exactly "user", "assistant" and "system" in `messages`, and
// normalize() cannot distinguish a legal wire from one carrying this package's internal
// role=tool. Three lines, no walk, and it closes the gap the comment above describes.
func assertWireRolesLegal(t *testing.T, what string, body []byte) {
	t.Helper()
	for i, m := range gjson.GetBytes(body, "messages").Array() {
		switch r := m.Get("role").String(); r {
		case "user", "assistant", "system":
		default:
			t.Errorf("%s: messages.%d role %q reaches the Anthropic wire — a 400 "+
				"(`Unexpected role`), and normalize() would hide it", what, i, r)
		}
	}
}

// REAL CAPTURED TRAFFIC MUST PASS. This is the guard against the failure mode that would make
// the validator worthless: firing on ordinary requests. Both fixtures are real traffic through
// the proxy — an Anthropic tool-use exchange and five turns of Claude-Agent-SDK conversation
// whose per-turn `<system-reminder>` budget messages sit at indices 1, 4 and 7 of `messages`.
// A validator that demanded "system only at index 0" (the first version did) rejects every one
// of those turns.
func TestRealCapturedTrafficIsShapeValid(t *testing.T) {
	checked := 0
	check := func(name string, body []byte) {
		msgs := gjson.GetBytes(body, "messages").Array()
		if len(msgs) == 0 {
			t.Fatalf("%s: fixture carries no messages", name)
		}
		norm, slots := normalize(bschemas.Anthropic, msgs)
		if len(norm) == 0 {
			t.Fatalf("%s: normalized to nothing", name)
		}
		assertShapeValid(t, name, norm, wireGroups(slots))
		checked++
	}

	raw, err := os.ReadFile("testdata/anthropic_tool_use.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	check("anthropic_tool_use", fx.Body)

	raw, err = os.ReadFile("testdata/session_head_agentsdk.json")
	if err != nil {
		t.Fatal(err)
	}
	var turns map[string]json.RawMessage
	if err := json.Unmarshal(raw, &turns); err != nil {
		t.Fatal(err)
	}
	sawSystemAwayFromHead := false
	for name, body := range turns {
		norm, _ := normalize(bschemas.Anthropic, gjson.GetBytes(body, "messages").Array())
		for i, m := range norm {
			if i > 0 && m.Role == bschemas.ChatMessageRoleSystem {
				sawSystemAwayFromHead = true
			}
		}
		check("agentsdk/"+name, body)
	}
	if !sawSystemAwayFromHead {
		t.Fatal("no fixture carried a system-role message away from index 0, so the " +
			"false-positive guard this test exists for never ran")
	}
	if checked == 0 {
		t.Fatal("no fixture was validated")
	}
}

// THE END-TO-END GUARD. summarize is run over an Anthropic transcript of PARALLEL tool calls
// (one assistant message with two tool_use blocks, both results in the single user message
// that follows — the shape live traffic carries), across every keep_last that lands the span
// boundary in a different place, and the emitted wire is re-normalized and validated.
//
// This is the test the four historical defects would have failed:
//
//	2edb9d4  a system-role summary spliced in front of the kept tail  -> system-position
//	fb5c460  the tail beginning on a tool_result whose call was cut   -> paired-tool-result
//	e7d1aa8  an assistant tool-call head kept while its results were cut -> answered-tool-use
func TestSummarizeEmittedWireIsShapeValid(t *testing.T) {
	// INDENTED JSON, so the second pipeline's extract_llm performs a real rewrite of a RETAINED
	// tool output. Prose or already-compact content is left alone, which is what made two
	// earlier versions of the toolrole test vacuous — see apply/toolrole_wire_test.go.
	rec := make([]map[string]any, 0, 40)
	for i := 0; i < 40; i++ {
		rec = append(rec, map[string]any{
			"ts": "2024-01-01T00:00:00Z", "path": "src/api/users.py", "level": "INFO",
			"msg": "request served", "seq": i,
			"detail": strings.Repeat("verbose parallel tool output ", 6),
		})
	}
	rb, _ := json.MarshalIndent(rec, "", "  ")
	big := string(rb)

	msgs := []map[string]any{{"role": "user", "content": "start the task"}}
	for i := 0; i < 8; i++ {
		a, b := "pa_"+string(rune('a'+i)), "pb_"+string(rune('a'+i))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []map[string]any{
				{"type": "text", "text": "calling two tools"},
				{"type": "tool_use", "id": a, "name": "Read", "input": map[string]any{}},
				{"type": "tool_use", "id": b, "name": "Read", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": a, "content": big},
				{"type": "tool_result", "tool_use_id": b, "content": big},
			}},
		)
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "final question"})
	body, _ := json.Marshal(map[string]any{"model": "claude-x", "messages": msgs})

	// TWO PIPELINES, because the role predicate is only non-vacuous in the second one. The
	// role="tool" leak needs TWO components in one turn: summarize to change the message count
	// (so rebuildCountChanged runs at all) and a second component to rewrite a tool message
	// summarize KEPT (so that message no longer byte-matches its pre-image and gets marshaled
	// fresh, internal role and all). With summarize alone every retained tool message still
	// byte-matches and is emitted from its original bytes, so no role can leak and the predicate
	// proves nothing. Verified by mutation: reverting 6e503e2 leaves [summarize] passing and
	// fails [summarize, extract_llm].
	//
	// extract_llm with strategy=deterministic keeps this hermetic — a real rewrite of the kept
	// message's bytes with no model reply to stub.
	pipelines := []struct{ name, yaml string }{
		{"summarize", "pipeline: [summarize]\ncomponents:\n" +
			"  summarize: {keep_last: %d, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n"},
		{"summarize+extract_llm", "pipeline: [summarize, extract_llm]\ncomponents:\n" +
			"  summarize: {keep_last: %d, start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n" +
			"  extract_llm: {strategy: deterministic, min_tokens: 1, economic_gate: false, " +
			"allow_on_caching_backend: true}\n"},
	}

	acted, sawParallelCall, sawResult, sawRewritingRun := false, false, false, false
	for _, pl := range pipelines {
		for _, keep := range []int{1, 2, 3, 4, 5} {
			what := fmt.Sprintf("%s keep_last=%d", pl.name, keep)
			cfg, err := config.LoadBytes([]byte(fmt.Sprintf(pl.yaml, keep)))
			if err != nil {
				t.Fatal(err)
			}
			// TWO PASSES OVER ONE STORE. summarize commissions its summary off the hot path, so
			// the first pass starts the model call and forwards the body untouched; the second
			// splices the checkpoint the first one left. The Store must be SHARED between them —
			// a fresh one per pass would discard the checkpoint and neither pass would ever
			// change the message count, which is the condition every assertion here rests on.
			p, _ := cfg.Build(nil)
			st := store.NewMemory(store.Options{})
			models := components.ModelSpec{Incoming: shapeModel{}}
			BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false, models)
			offload.WaitForAllSummariesForTest(5 * time.Second)
			out, changed := BodyWithModel(context.Background(), p, st,
				bschemas.Anthropic, body, "", false, models)
			if !changed {
				continue
			}
			acted = true
			assertWireRolesLegal(t, what, out)
			arr := gjson.GetBytes(out, "messages").Array()
			if len(arr) != len(msgs) && pl.name == "summarize+extract_llm" {
				sawRewritingRun = true
			}
			norm, slots := normalize(bschemas.Anthropic, arr)
			for _, m := range norm {
				if a := m.ChatAssistantMessage; a != nil && len(a.ToolCalls) >= 2 {
					sawParallelCall = true
				}
				if m.Role == bschemas.ChatMessageRoleTool {
					sawResult = true
				}
			}
			assertShapeValid(t, what, norm, wireGroups(slots))
		}
	}
	// Vacuity guards: this test is worthless if summarize never acted, and it does not
	// exercise the shape it exists for unless a parallel exchange survived onto the wire.
	if !acted {
		t.Fatal("summarize never acted, so no wire was validated — the assertions are vacuous")
	}
	if !sawParallelCall {
		t.Fatal("no parallel tool_use pair reached the wire, so the run-scanning half of " +
			"answered-tool-use was never exercised")
	}
	if !sawResult {
		t.Fatal("no tool_result reached the wire, so paired-tool-result was never exercised")
	}
	// The role predicate is only meaningful over a count-changing run of the two-component
	// pipeline; without one it is a check that cannot fail.
	if !sawRewritingRun {
		t.Fatal("the two-component pipeline never changed the message count, so the " +
			"count-change rebuild never ran and assertWireRolesLegal cannot fail")
	}
}

// buildSplitGroupBody is an Anthropic-shaped transcript (roles strictly alternating, as the
// real API requires) with one wire message (index 6) that holds BOTH a sibling text block and
// the tool_result answering the PRECEDING message's tool_use (index 5) — the exact shape #457
// is about — plus enough ordinary filler turns on each side to position the group either
// inside a summarized span or inside a kept tail, depending on keep_last.
//
// This fixture and its test live INSIDE the apply package (not beside the other sibling tests
// in normalizer_siblings_test.go) because the shape assertion needs normalize's slots and
// wireGroups, which are package-internal — the grouping-aware validation that replaced the
// hand-rolled wire-pairing check the external file used to carry.
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

// replyModel is shapeModel with a settable reply, for the split-group rebuild. Tagged, not a
// bare string: summarize's ensureSummaryTags wraps an untagged reply, but cache_aware_summarizer
// requires the model's own output to already carry a closing </summary> tag — see
// cacheAwareSummaryIncomplete — or it discards the reply as truncated and never produces a
// checkpoint for either case to splice.
type replyModel struct{ reply string }

func (m replyModel) Complete(context.Context, string) (string, error) { return m.reply, nil }

// CompleteMessages makes replyModel also satisfy components.MessagesModel, which
// cache_aware_summarizer requires — components.Model alone is not enough for it. The prompt
// is flattened away: this model's job is only to answer with one fixed string.
func (m replyModel) CompleteMessages(ctx context.Context, _ string, _ []bschemas.ChatMessage) (string, error) {
	return m.Complete(ctx, "")
}

// assertSplitGroupRebuild runs the two-pass commission/splice cycle for a count-changing
// summarizer and checks: the split group (sibling text + its tool_result) survives EITHER
// wholly verbatim (kept-tail case) or wholly gone (summarized-away case) — never one half of
// each — and the result is shape-valid (#457 requirement 1 and 3: the rebuild must not split a
// wire message or orphan a tool_use/tool_result pairing).
func assertSplitGroupRebuild(t *testing.T, pipelineYAML string, groupInTail bool) {
	t.Helper()
	cfg, err := config.LoadBytes([]byte(pipelineYAML))
	if err != nil {
		t.Fatal(err)
	}
	p, _ := cfg.Build(nil)
	st := store.NewMemory(store.Options{})
	body := buildSplitGroupBody()
	model := replyModel{reply: "<summary>SUMMARY-MARKER covering the exchange</summary>"}

	// Two passes: the first commissions the summary off the hot path and forwards the body
	// untouched, the second splices the result — see TestSummarizeCountChangeLossless.
	BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false,
		components.ModelSpec{Incoming: model})
	offload.WaitForAllSummariesForTest(5 * time.Second)
	out, changed := BodyWithModel(context.Background(), p, st, bschemas.Anthropic, body, "", false,
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
	// tool_use/tool_result pairing. Roles are checked on the RAW BODY (assertWireRolesLegal
	// — the re-normalized view cannot see them, see its comment); the pairing is judged on
	// the re-normalized view WITH its wire grouping. That grouping is what made the old
	// hand-rolled assertAnthropicWirePairing redundant: a sibling text block legitimately
	// sits between a tool_use's answering message and its tool_result block, and the grouped
	// validator reads the pair as answered because both normalize out of the ONE wire
	// message that follows the call (#461).
	assertWireRolesLegal(t, "split-group rebuild", out)
	norm, slots := normalize(bschemas.Anthropic, gjson.GetBytes(out, "messages").Array())
	assertShapeValid(t, "split-group rebuild", norm, wireGroups(slots))
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
				fmt.Sprintf("%d", tc.keepLast) + ", start_from_message: 0, min_tokens: 1, trigger: {min_request_frac: 0}}\n"
			assertSplitGroupRebuild(t, yaml, tc.groupInTail)
		})
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
		// The split group is the transcript's LAST tool exchange. cache_aware_summarizer keeps that
		// exchange verbatim after a splice (#723), so with keep_last_turns 2 it is kept WHOLE too: the
		// "never split" invariant still holds, and the shape check below still runs.
		{"group is the last tool exchange, so it is kept", 2, true},
		{"group inside the kept tail", 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "pipeline: [cache_aware_summarizer]\ncomponents:\n  cache_aware_summarizer:\n" +
				"    keep_last_turns: " + fmt.Sprintf("%d", tc.keepLast) + "\n    min_tokens: 1\n" +
				"    trigger:\n      min_messages: 1\n      min_request_tokens: 1\n      min_request_frac: 0\n"
			assertSplitGroupRebuild(t, yaml, tc.groupInTail)
		})
	}
}
