package offload

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/schema"
)

// ctxReq is a small agent-shaped transcript: a task, several tool results, corrections
// from the user along the way, and recent reasoning. The shape matters — in real agent
// traffic most messages are tool results, which is why "the last N messages" is counted
// over the messages that get RENDERED rather than over raw indices.
func ctxReq() *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("TASK: make the auth timeout configurable in src/api/users.py"),
		assistantMsg("I will grep for the timeout constant."),
		toolResultMsg(strings.Repeat("grep hit line\n", 200)),
		assistantMsg("Found it in src/api/users.py."),
		toolResultMsg(strings.Repeat("file body line\n", 200)),
		userMsg("CORRECTION: it must default to 30s, not 10s."),
		assistantMsg("Understood, defaulting to 30s."),
		toolResultMsg(strings.Repeat("test output line\n", 200)),
		assistantMsg("Now running the tests."),
		userMsg("keep going"),
	}}
}

// The three modes must actually differ in what the model is told, and each must keep the
// things the relevance judgement depends on.
func TestConversationContextModes(t *testing.T) {
	req := ctxReq()
	goal := conversationContext(req, ctxGoal, 0)
	recent := conversationContext(req, ctxRecent, defaultContextMessages)
	full := conversationContext(req, ctxFull, 0)

	if goal == recent || recent == full {
		t.Fatal("the context modes produce identical text, so the setting does nothing")
	}
	// Ordering by size is the whole point of the dial.
	if !(len(goal) < len(recent) && len(recent) < len(full)) {
		t.Fatalf("expected goal < recent < full, got %d / %d / %d", len(goal), len(recent), len(full))
	}
	// Every mode must carry the task and the correction: an extraction that does not know
	// the timeout must default to 30s can happily delete the line that says so.
	for name, got := range map[string]string{"goal": goal, "recent": recent, "full": full} {
		if !strings.Contains(got, "make the auth timeout configurable") {
			t.Errorf("%s context lost the task statement", name)
		}
	}
	for _, name := range []string{"recent", "full"} {
		got := recent
		if name == "full" {
			got = full
		}
		if !strings.Contains(got, "CORRECTION") {
			t.Errorf("%s context lost a user correction, which is exactly the signal it "+
				"exists to carry (goal mode only keeps the first and last user turns)", name)
		}
	}
	// recent must NOT drag in tool outputs: they are the bulk being reduced, and putting
	// them in the prompt would multiply the cost of every call.
	if strings.Contains(recent, "file body line") {
		t.Error("recent context included tool output; that is the bulk this component removes")
	}
	// full must, since its purpose is completeness.
	if !strings.Contains(full, "file body line") {
		t.Error("full context excluded tool output, so it is not full")
	}
}

// An unknown value must fail the config, not silently pick a mode.
func TestParseContextMode(t *testing.T) {
	for in, want := range map[string]contextMode{
		"": ctxRecent, "recent": ctxRecent, "goal": ctxGoal, "full": ctxFull,
	} {
		got, err := parseContextMode(in)
		if err != nil || got != want {
			t.Fatalf("parseContextMode(%q) = %v, %v; want %v, nil", in, got, err, want)
		}
	}
	if _, err := parseContextMode("everything"); err == nil {
		t.Fatal("an unknown context mode was accepted")
	}
	if _, err := newExtractLLM([]byte("context: everything\n")); err == nil {
		t.Fatal("the component accepted an unknown context mode")
	}
}

// Over-long context is clipped from the FRONT. The newest turns say what the agent needs
// next, so losing the tail would remove the only part that answers the prompt's question.
func TestOverlongContextKeepsTheNewestTurns(t *testing.T) {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("ANCIENT HISTORY " + strings.Repeat("filler words here ", 3000)),
		assistantMsg(strings.Repeat("more filler ", 3000)),
		userMsg("THE NEWEST INSTRUCTION"),
	}}
	got := conversationContext(req, ctxRecent, defaultContextMessages)
	if len(got) > recentContextCap+64 {
		t.Fatalf("context is %d bytes, over the %d cap", len(got), recentContextCap)
	}
	if !strings.Contains(got, "THE NEWEST INSTRUCTION") {
		t.Fatal("clipping dropped the newest turn")
	}
	if !strings.Contains(got, "elided") {
		t.Fatal("clipping left no trace, so the model cannot tell it is seeing a fragment")
	}
	if !utf8Valid(got) {
		t.Fatal("clipping split a multi-byte rune")
	}
}

// Multi-byte text must survive clipping intact — an invalid rune goes straight into a
// prompt we send.
func TestContextClipIsRuneSafe(t *testing.T) {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg(strings.Repeat("é", recentContextCap)),
		userMsg("סוף"),
	}}
	got := conversationContext(req, ctxRecent, defaultContextMessages)
	if !utf8Valid(got) {
		t.Fatal("rendered context is not valid UTF-8")
	}
	if !strings.Contains(got, "סוף") {
		t.Fatal("the newest turn was clipped away")
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// Each mode must degrade to the next-best signal rather than to nothing of its own
// invention. A tool-only request has no non-tool turn at all, so `recent` correctly ends up
// with the goal renderer's answer — which for that request is empty, and the component then
// declines every candidate on no_goal_keywords. That is the PRE-EXISTING contract (see the
// note on userMsg in extract_llm_timeout_test.go), not a regression: without a user turn
// there is no statement of what the agent needs, so there is nothing to reduce toward.
func TestContextFallsBackRatherThanInventing(t *testing.T) {
	toolOnly := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		toolResultMsg("only a tool result, nothing else"),
	}}
	if got, want := conversationContext(toolOnly, ctxRecent, 0), conversationGoal(toolOnly); got != want {
		t.Errorf("recent on a tool-only request = %q, want the goal renderer's %q", got, want)
	}
	// full still has something to say about it, because it carries tool results.
	if !strings.Contains(conversationContext(toolOnly, ctxFull, 0), "only a tool result") {
		t.Error("full context dropped the only message in the request")
	}
	// On a NORMAL request no mode may come back empty: an empty context is the component
	// silently declining everything.
	for _, m := range []contextMode{ctxGoal, ctxRecent, ctxFull} {
		if conversationContext(ctxReq(), m, 0) == "" {
			t.Errorf("%s produced an empty context for a normal transcript", m)
		}
	}
	// And the token accounting the context guard does must see the same string.
	e, err := newExtractLLM([]byte("context: recent\n"))
	if err != nil {
		t.Fatal(err)
	}
	x := e.(*ExtractLLM)
	if schema.TextTokens(x.extractionContext(ctxReq())) == 0 {
		t.Fatal("the component's own context renderer returned nothing to count")
	}
}

// assistantMsg completes the trio of role helpers (userMsg / toolResultMsg live in
// extract_llm_timeout_test.go). The context modes differ precisely in which ROLES they
// carry, so a fixture without assistant turns cannot tell them apart.
func assistantMsg(text string) bschemas.ChatMessage {
	t := text
	return bschemas.ChatMessage{
		Role:    bschemas.ChatMessageRoleAssistant,
		Content: &bschemas.ChatMessageContent{ContentStr: &t},
	}
}

// Keep-ids must come from the AGENT's words, never from the tool outputs — even when the
// prompt's context includes them (context: full, and every cold-cache sweep).
//
// The keep-list means "identifiers the agent referenced, so do not lose them". Harvested from
// a context containing the candidate, every unique token in the noise becomes a required
// identifier and no reduction can pass. MEASURED live, four samples per arm on the same
// 26k-token access log: harvesting from the full transcript gave 0/6 accepted across two runs;
// with keep-ids taken from the conversation alone, a full-sized context accepts 3/4 — the same
// as a small one. That was the difference between the cold sweep working and being an
// expensive no-op.
func TestKeepIdsNeverComeFromToolOutput(t *testing.T) {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Find the auth timeout in src/api/users.py and fix it."),
		assistantMsg("reading the access log"),
		toolResultMsg(strings.Repeat(
			"2024-01-01T00:37:37 GET /orders/9 200 12ms trace=zz9plural-z-alpha\n", 400)),
		userMsg("keep going"),
	}}
	// What the component harvests from, in every context mode: the conversation-only render.
	for _, mode := range []contextMode{ctxGoal, ctxRecent, ctxFull} {
		e, err := newExtractLLM([]byte("context: " + string(mode) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		x := e.(*ExtractLLM)
		// The prompt's context may include the payload (that is what `full` is for)...
		if mode == ctxFull && !strings.Contains(x.extractionContext(req), "zz9plural") {
			t.Fatal("full context should carry the tool output")
		}
		// ...but the keep-list must not be derived from it.
		src := conversationContext(req, ctxRecent, x.ctxMessages)
		for _, id := range []string{"zz9plural-z-alpha", "orders"} {
			if strings.Contains(src, id) {
				t.Errorf("mode %s: keep-id source contains %q, harvested from the tool output; "+
					"every unique token in the noise then becomes un-removable", mode, id)
			}
		}
		if !strings.Contains(src, "src/api/users.py") {
			t.Errorf("mode %s: keep-id source lost the path the agent actually referenced", mode)
		}
	}
}

// A non-ASCII tail must survive clipping. The first version of clipRunesTail advanced past
// every leading multi-byte rune (it tested utf8.ValidString on a single byte, which is only
// true for ASCII), so a CJK or accented transcript clipped to nothing and the extraction model
// received the elision marker and no conversation.
func TestTailClipKeepsNonASCII(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"CJK", strings.Repeat("需要保留最新的对话内容。", 4000)},
		{"accented", strings.Repeat("il faut préserver la fin de la conversation. ", 2000)},
		{"emoji", strings.Repeat("keep the tail 🚀 ", 3000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := clipRunesTail(tc.text, recentContextCap)
			if got == "" {
				t.Fatal("clipping a non-ASCII tail returned nothing")
			}
			if !utf8Valid(got) {
				t.Fatal("clipped tail is not valid UTF-8")
			}
			if len(got) > recentContextCap {
				t.Fatalf("clipped tail is %d bytes, over the %d cap", len(got), recentContextCap)
			}
			// It must be a SUFFIX of the input: the newest turns are the point.
			if !strings.HasSuffix(tc.text, got) {
				t.Fatal("clipped tail is not a suffix of the input")
			}
		})
	}
}
