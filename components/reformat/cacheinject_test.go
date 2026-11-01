package reformat

import (
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func blockMsg(role schemas.ChatMessageRole, text string) schemas.ChatMessage {
	return schemas.ChatMessage{
		Role: role,
		Content: &schemas.ChatMessageContent{
			ContentBlocks: []schemas.ChatContentBlock{{
				Type: schemas.ChatContentBlockTypeText,
				Text: &text,
			}},
		},
	}
}

func convo(n int) []schemas.ChatMessage {
	out := make([]schemas.ChatMessage, n)
	for i := 0; i < n; i++ {
		role := schemas.ChatMessageRoleUser
		if i%2 == 1 {
			role = schemas.ChatMessageRoleAssistant
		}
		out[i] = blockMsg(role, "message-"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	return out
}

func marked(msgs []schemas.ChatMessage) []int {
	var out []int
	for i := range msgs {
		if schema.HasBlockBreakpoint(&msgs[i]) {
			out = append(out, i)
		}
	}
	return out
}

func run(t *testing.T, c *components.Ctx, msgs []schemas.ChatMessage) ([]int, *components.Report) {
	t.Helper()
	req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic, Input: msgs}
	rep := &components.Report{}
	if err := (Cacheinject{}).Reformat(req, rep, c); err != nil {
		t.Fatalf("Reformat: %v", err)
	}
	return marked(req.Input), rep
}

func ctx() *components.Ctx {
	return &components.Ctx{Session: "s1", Store: store.NewMemory(store.Options{})}
}

// The trailing message must ALWAYS get a breakpoint: rule 1, the only one that is
// unconditionally worth money (a token resent even once is cheaper written).
func TestTrailingBreakpointAlways(t *testing.T) {
	c := ctx()
	for _, n := range []int{1, 2, 5, 25, 60} {
		got, rep := run(t, c, convo(n))
		if rep.Skipped {
			t.Fatalf("n=%d: skipped", n)
		}
		if len(got) == 0 || got[len(got)-1] != n-1 {
			t.Fatalf("n=%d: trailing message not marked, got %v", n, got)
		}
	}
}

// Never exceed the provider's hard cap. Exceeding it is a 400, not a slow request.
func TestNeverExceedsCap(t *testing.T) {
	for _, n := range []int{1, 3, 20, 40, 100, 300} {
		got, _ := run(t, ctx(), convo(n))
		if len(got) > maxBreakpoints {
			t.Fatalf("n=%d: %d breakpoints > cap %d (%v)", n, len(got), maxBreakpoints, got)
		}
	}
}

// An agent that already sets its own 4 breakpoints (claude-code does) is at the
// optimum. We must not add a 5th — that is the +9.2% regression v1 caused.
func TestRespectsExistingBreakpoints(t *testing.T) {
	msgs := convo(30)
	for _, i := range []int{0, 1, 14, 29} {
		mark(&msgs[i], nil)
	}
	before := marked(msgs)
	got, rep := run(t, ctx(), msgs)
	if !rep.Skipped {
		t.Fatalf("expected skip when all %d slots are taken", maxBreakpoints)
	}
	if len(got) != len(before) {
		t.Fatalf("added breakpoints over a full budget: %v -> %v", before, got)
	}
}

// With some slots free, fill only the remainder.
func TestPartialBudget(t *testing.T) {
	msgs := convo(30)
	mark(&msgs[0], nil)
	mark(&msgs[1], nil)
	got, _ := run(t, ctx(), msgs)
	if len(got) > maxBreakpoints {
		t.Fatalf("over cap: %v", got)
	}
	if len(got) < 3 {
		t.Fatalf("left free slots idle (they are billed as a span, so they cost nothing): %v", got)
	}
}

// The rule that earns the money: when an EARLY message mutates, a breakpoint must
// land just below the divergence so the stable head still bills at 0.1x rather
// than 1.0x. Turn 1 primes the digest history; turn 2 mutates message 1.
func TestAnchorsBelowDivergence(t *testing.T) {
	c := ctx()
	first := convo(24)
	run(t, c, first)

	second := convo(26)
	second[1] = blockMsg(schemas.ChatMessageRoleAssistant, "MUTATED running scratchpad")
	got, rep := run(t, c, second)
	if rep.Skipped {
		t.Fatal("skipped on a diverging turn — this is the case that pays")
	}
	// divergence at message 1 => anchor at index 0
	found := false
	for _, i := range got {
		if i == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no anchor below the divergence (want index 0), got %v", got)
	}
}

// Append-only growth is the common case (100% of claude-code's turns, 98% of
// Bob's). The divergence anchor must then land at the previous tail, i.e. inside
// the span that is already read — costing nothing.
func TestAppendOnlyIsCheap(t *testing.T) {
	c := ctx()
	run(t, c, convo(10))
	got, _ := run(t, c, convo(12))
	for _, i := range got {
		if i > 11 {
			t.Fatalf("breakpoint past the end: %v", got)
		}
	}
	if len(got) > maxBreakpoints {
		t.Fatalf("over cap: %v", got)
	}
	// index 9 is the previous tail: the divergence anchor for an append-only turn.
	hit := false
	for _, i := range got {
		if i == 9 {
			hit = true
		}
	}
	if !hit {
		t.Fatalf("append-only turn did not anchor at the previous tail (9): %v", got)
	}
}

// Anchor indices must not drift as the conversation grows, or they are never
// pre-warmed and so never readable. Same conversation, three lengths: the deep
// anchors must be identical.
func TestAnchorsAreTurnStable(t *testing.T) {
	deep := func(n int) []int {
		got, _ := run(t, ctx(), convo(n))
		var out []int
		for _, i := range got {
			if i < n-2 { // exclude the trailing/divergence pair
				out = append(out, i)
			}
		}
		return out
	}
	a, b := deep(60), deep(64)
	for i := range a {
		if i < len(b) && a[i] != b[i] {
			t.Fatalf("anchor drifted with length: %v vs %v", a, b)
		}
	}
}

// Bedrock Mantle serves Claude on the native Anthropic surface, so cacheinject must place
// breakpoints there as it does on Bedrock (#809). Before the fix it skipped the request.
func TestPlacesBreakpointsOnEveryCacheControlProvider(t *testing.T) {
	for _, p := range []schemas.ModelProvider{schemas.Anthropic, schemas.Bedrock, schemas.BedrockMantle, schemas.Vertex} {
		req := &schemas.BifrostChatRequest{Provider: p, Input: convo(30)}
		rep := &components.Report{}
		if err := (Cacheinject{}).Reformat(req, rep, ctx()); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if rep.Skipped || len(marked(req.Input)) == 0 {
			t.Errorf("%s: want breakpoints, got skipped=%v marked=%v", p, rep.Skipped, marked(req.Input))
		}
	}
}

// Non-cache-aware providers must be untouched: cache_control is meaningless (and
// on Gemini/OpenAI-shaped wires, not even expressible).
func TestSkipsNonCacheAwareProviders(t *testing.T) {
	for _, p := range []schemas.ModelProvider{schemas.OpenAI, schemas.Gemini} {
		req := &schemas.BifrostChatRequest{Provider: p, Input: convo(10)}
		rep := &components.Report{}
		if err := (Cacheinject{}).Reformat(req, rep, ctx()); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !rep.Skipped || len(marked(req.Input)) != 0 {
			t.Fatalf("%s: should be inert, got %v", p, marked(req.Input))
		}
	}
}

// String-content messages cannot carry a block-level directive; skipping them
// (rather than restructuring) is what keeps this component lossless.
func TestStringContentIsSkippedNotRestructured(t *testing.T) {
	s := "plain string content"
	msgs := []schemas.ChatMessage{
		blockMsg(schemas.ChatMessageRoleUser, "a"),
		{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &s}},
	}
	req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic, Input: msgs}
	if err := (Cacheinject{}).Reformat(req, &components.Report{}, ctx()); err != nil {
		t.Fatal(err)
	}
	if req.Input[1].Content.ContentStr == nil || *req.Input[1].Content.ContentStr != s {
		t.Fatal("string content was restructured — no longer lossless")
	}
}

// When the trailing message is string-content (unmarkable), rule 1 still wants the
// prefix written, so the breakpoint must fall back DOWN to the nearest markable
// block rather than being dropped — otherwise the whole prefix bills at 1.0x.
func TestFallsBackWhenTrailingUnmarkable(t *testing.T) {
	s := "newest turn as a plain string"
	msgs := append(convo(6), schemas.ChatMessage{
		Role:    schemas.ChatMessageRoleUser,
		Content: &schemas.ChatMessageContent{ContentStr: &s},
	})
	got, rep := run(t, ctx(), msgs)
	if rep.Skipped || len(got) == 0 {
		t.Fatalf("dropped the breakpoint instead of falling back: %v", got)
	}
	if got[len(got)-1] != 5 {
		t.Fatalf("want fallback to the last markable block (5), got %v", got)
	}
}

// No store (Nop) must degrade gracefully: no divergence history, but rule 1 still
// applies and nothing panics.
func TestWorksWithoutStore(t *testing.T) {
	got, rep := run(t, &components.Ctx{}, convo(12))
	if rep.Skipped || len(got) == 0 {
		t.Fatalf("should still place the trailing breakpoint without a store: %v", got)
	}
}

// TTL is configuration, not a heuristic: 5m must emit no explicit ttl (byte-identical
// to what the agent sends), 1h must emit it, and anything else must be rejected at
// config load rather than silently ignored.
func TestTTLConfig(t *testing.T) {
	ttlOf := func(raw string) *string {
		comp, err := newCacheinject([]byte(raw))
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		msgs := convo(4)
		req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic, Input: msgs}
		if err := comp.(*Cacheinject).Reformat(req, &components.Report{}, ctx()); err != nil {
			t.Fatal(err)
		}
		for i := range req.Input {
			if cc := req.Input[i].Content.ContentBlocks[0].CacheControl; cc != nil {
				return cc.TTL
			}
		}
		t.Fatal("no breakpoint emitted")
		return nil
	}
	if got := ttlOf(""); got != nil {
		t.Fatalf("default should emit no explicit ttl (5m is the provider default), got %q", *got)
	}
	if got := ttlOf("ttl: 5m\n"); got != nil {
		t.Fatalf("5m should emit no explicit ttl, got %q", *got)
	}
	if got := ttlOf("ttl: 1h\n"); got == nil || *got != "1h" {
		t.Fatalf("1h not emitted: %v", got)
	}
	if _, err := newCacheinject([]byte("ttl: 30m\n")); err == nil {
		t.Fatal("an unsupported ttl must be rejected, not silently accepted")
	}
}

// The provider's cap counts breakpoints this component cannot see: `system` and
// `tools` never reach it, and the host's normalize step drops the mark on tool_result
// blocks it rewrites. Real Claude Code traffic carries (system=2, tools=0, messages=1), so the
// true remaining budget is 1, not 3 (issue #32, defect 2). The synthetic 60-message
// shape below is exactly the probe that produced 4 message marks — 6 on the wire.
func TestBudgetCountsInvisibleBreakpoints(t *testing.T) {
	msgs := convo(60)
	mark(&msgs[59], nil) // the caller's own message breakpoint

	c := ctx()
	c.ExistingBreakpoints = 3 // (system=2, tools=0, messages=1) — the real shape
	req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic, Input: msgs}
	rep := &components.Report{}
	if err := (Cacheinject{}).Reformat(req, rep, c); err != nil {
		t.Fatalf("Reformat: %v", err)
	}
	got := marked(req.Input)
	// The caller's own message breakpoint is one of the 3 counted, so the wire total is
	// c.ExistingBreakpoints plus whatever we added on top.
	added := 0
	for _, i := range got {
		if i != 59 {
			added++
		}
	}
	if wire := c.ExistingBreakpoints + added; wire > maxBreakpoints {
		t.Fatalf("wire breakpoints %d > cap %d (marked %v, existing %d)",
			wire, maxBreakpoints, got, c.ExistingBreakpoints)
	}
}

// Four breakpoints already on the wire leave no budget at all: adding any would be a 400.
func TestExistingBreakpointsCanExhaustTheBudget(t *testing.T) {
	c := ctx()
	c.ExistingBreakpoints = maxBreakpoints
	req := &schemas.BifrostChatRequest{Provider: schemas.Anthropic, Input: convo(30)}
	rep := &components.Report{}
	if err := (Cacheinject{}).Reformat(req, rep, c); err != nil {
		t.Fatalf("Reformat: %v", err)
	}
	if !rep.Skipped {
		t.Fatal("expected a skip: the caller's own breakpoints already fill the cap")
	}
	if got := marked(req.Input); len(got) != 0 {
		t.Fatalf("marked %v over a full budget", got)
	}
}
