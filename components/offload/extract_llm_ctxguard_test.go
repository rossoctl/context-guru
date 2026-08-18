package offload

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// silentModel records every call and never returns a compaction, so a test can assert
// purely on WHETHER the extraction model was consulted.
type silentModel struct{ calls int64 }

func (m *silentModel) Complete(context.Context, string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	return "", nil
}

// newCtxGuardComponent builds the component through its registered constructor (see
// newTimeoutTestComponent for why a struct literal is wrong here) with the context guard
// as the only gate left standing: min_tokens: 1 and economic_gate: false, so a declined
// call can only be the guard's doing.
func newCtxGuardComponent(t *testing.T, model components.Model, extraYAML string) *ExtractLLM {
	t.Helper()
	c, err := newExtractLLM([]byte("min_tokens: 1\nstrategy: code\neconomic_gate: false\n" + extraYAML))
	if err != nil {
		t.Fatalf("newExtractLLM: %v", err)
	}
	e := c.(*ExtractLLM)
	e.modelClient = model
	e.mode = markerFull
	return e
}

// budgetFor is the total budget one call for a contentTok-sized output needs — the same
// arithmetic fitsModelContext applies, so a test can sit EXACTLY on the boundary.
func budgetFor(contentTok, overheadTok int) int {
	return int(float64(contentTok+overheadTok)*extractContextMargin) +
		cheapExtractOutputTokens + cheapExtractSlack
}

// The arithmetic, including both sides of the boundary. The reply reservation and the
// tokenizer margin are the parts most likely to be "simplified" away later.
func TestFitsModelContext(t *testing.T) {
	content, overhead := 10_000, 2_000
	need := budgetFor(content, overhead)
	cases := []struct {
		name  string
		limit int
		want  bool
	}{
		{"far below the limit", 200_000, true},
		{"exactly at the limit", need, true},
		{"one token over the limit", need - 1, false},
		{"small self-hosted window", 8192, false},
		{"unknown-model default", unknownModelInputLimit, true},
		{"zero limit never fits", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := fitsModelContext(content, overhead, tc.limit); got != tc.want {
				t.Fatalf("fitsModelContext(%d, %d, %d) = %v, want %v (need %d)",
					content, overhead, tc.limit, got, tc.want, need)
			}
		})
	}
	// The reply must be reserved: an input that only fits when the output is assumed free
	// is exactly the request an input+output-bounded API rejects.
	promptOnly := int(float64(content+overhead) * float64(extractContextMargin))
	if fitsModelContext(content, overhead, promptOnly) {
		t.Fatal("a prompt that fills the whole window was accepted: the model's reply is " +
			"no longer reserved, so the upstream will reject the call")
	}
}

// A body beyond extract's shown-body bound must not scale the estimate: its prompt carries
// a bounded head+tail, so counting the whole thing would decline calls on exactly the very
// large outputs this component exists for (#28's escape-hatch path uses a ~480k-token body).
func TestShownBodyTokensIsBounded(t *testing.T) {
	cases := []struct{ name, body string }{
		{"under the bound", strings.Repeat("log line here\n", 100)},
		{"at the bound", strings.Repeat("x", extractShownBodyChars)},
		{"far over the bound", strings.Repeat("padding ", 60_000)},
	}
	// A token is at least one character, so the sample can never exceed its own char bound.
	ceiling := extractShownBodyChars
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shownBodyTokens(tc.body)
			if got > ceiling {
				t.Fatalf("shownBodyTokens = %d for a %d-char body, above the %d-token "+
					"ceiling of the sample the prompt shows", got, len(tc.body), ceiling)
			}
			full := schema.TextTokens(tc.body)
			if len(tc.body) <= extractShownBodyChars && got != full {
				t.Fatalf("a body within the bound must be counted whole: %d != %d", got, full)
			}
			if len(tc.body) > 4*extractShownBodyChars && got > full/4 {
				t.Fatalf("a %d-token body estimated at %d: the estimate is still scaling "+
					"with the whole output instead of the shown sample", full, got)
			}
		})
	}
}

// The limit is DATA (modelinfo's table / the host-resolved window / an explicit pin), never
// a magic number, and an unnameable model gets the conservative default rather than the
// benefit of the doubt.
func TestExtractLLMInputLimit(t *testing.T) {
	cases := []struct {
		name      string
		yaml      string
		ctxWindow int
		want      int
	}{
		{"config pin wins", "model_max_input_tokens: 4096\nmodel:\n  model: claude-haiku-4-5\n", 200_000, 4096},
		{"pinned model resolved from the table", "model:\n  model: claude-haiku-4-5\n", 0, 200_000},
		{"unnameable pinned model falls back", "model:\n  model: qwen3-coder-30b-local\n", 999_999, unknownModelInputLimit},
		{"incoming model uses the host-resolved window", "", 128_000, 128_000},
		{"incoming model, window unknown", "", 0, unknownModelInputLimit},
		{"source config hides the model id", "model:\n  source: config\n", 128_000, unknownModelInputLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newCtxGuardComponent(t, &silentModel{}, tc.yaml)
			c := &components.Ctx{Ctx: context.Background(), CtxWindow: tc.ctxWindow}
			if got := e.inputLimit(c); got != tc.want {
				t.Fatalf("inputLimit = %d, want %d", got, tc.want)
			}
		})
	}
}

// End to end: a tool output whose prompt cannot fit the extraction model's context must be
// left VERBATIM, with the refusal recorded — and one that fits must still be compacted, so
// the guard cannot pass by switching the component off.
//
// PROVEN TO FAIL WITHOUT THE GUARD: with the fitsModelContext check removed, the
// "over the limit" subtest calls the model (calls=1) and records no gate.
func TestExtractLLMContextGuard(t *testing.T) {
	const first, last = "Fix the failing handler in src/mod/file.py and run the tests.", "and keep going"
	// Big enough that a small window genuinely cannot hold it.
	output := strings.Repeat("src/mod/file.py:12: def handler(request, context): # noise\n", 400)
	newReq := func() *bschemas.BifrostChatRequest {
		return &bschemas.BifrostChatRequest{
			Input: []bschemas.ChatMessage{userMsg(first), toolResultMsg(output), userMsg(last)},
		}
	}
	// The exact budget this fixture needs, derived through the component's own goal
	// derivation so "exactly at the limit" really sits on the boundary.
	// Through the component's OWN context renderer, in the mode it will actually use, so
	// "exactly at the limit" really sits on the boundary. Deriving this from
	// conversationGoal was correct only while that was the one possible context.
	overhead := extractPromptOverheadTokens +
		schema.TextTokens(conversationContext(newReq(), ctxRecent, defaultContextMessages))
	need := budgetFor(shownBodyTokens(output), overhead)

	cases := []struct {
		name     string
		limit    int
		wantCall bool
	}{
		{"comfortably fits", 200_000, true},
		{"exactly at the limit", need, true},
		{"one token over the limit", need - 1, false},
		{"small self-hosted window", 8192, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &silentModel{}
			e := newCtxGuardComponent(t, model,
				"model_max_input_tokens: "+strconv.Itoa(tc.limit)+"\n")
			req := newReq()
			rep := &components.Report{}
			c := &components.Ctx{
				Session: "ctxguard-" + tc.name,
				Store:   store.NewMemory(store.Options{}),
				Ctx:     context.Background(),
				Model:   components.ModelSpec{Static: model, Incoming: model},
			}
			if _, err := e.Offload(req, rep, c); err != nil {
				t.Fatalf("Offload must fail open, got error: %v", err)
			}

			calls := atomic.LoadInt64(&model.calls)
			gates := rep.Gates["over_model_context"]
			if tc.wantCall {
				if calls == 0 {
					t.Fatalf("no model call at limit %d (need %d): the guard is declining "+
						"calls that fit, which silently disables the component", tc.limit, need)
				}
				if gates != 0 {
					t.Fatalf("over_model_context recorded %d times for a candidate that fits", gates)
				}
				return
			}
			if calls != 0 {
				t.Fatalf("the model was called %d time(s) with a prompt that cannot fit "+
					"limit %d (need %d): the oversized request goes on the wire", calls, tc.limit, need)
			}
			// A silent skip is not acceptable — the refusal must be diagnosable at
			// /stats as components.extract_llm.gates.
			if gates != 1 {
				t.Fatalf("over_model_context gate recorded %d times, want 1 (gates: %v)",
					gates, rep.Gates)
			}
			if !rep.Skipped {
				t.Fatal("nothing was compacted but the report is not marked Skipped")
			}
			// FAIL OPEN: the tool output is untouched, so nothing had to be shed and
			// nothing was truncated.
			if got := msgText(t, req.Input[1]); got != output {
				t.Fatalf("tool output changed (%d -> %d bytes) although no call was made",
					len(output), len(got))
			}
			// And every USER message survives verbatim — the component only ever
			// considers tool-role messages (toolIndices), so it cannot drop one.
			if got := msgText(t, req.Input[0]); got != first {
				t.Fatalf("first user message was modified: %q", got)
			}
			if got := msgText(t, req.Input[2]); got != last {
				t.Fatalf("trailing user message was modified: %q", got)
			}
			if len(req.Input) != 3 {
				t.Fatalf("messages were dropped: %d, want 3", len(req.Input))
			}
		})
	}
}

func msgText(t *testing.T, m bschemas.ChatMessage) string {
	t.Helper()
	if m.Content == nil || m.Content.ContentStr == nil {
		t.Fatal("message lost its text content")
	}
	return *m.Content.ContentStr
}
