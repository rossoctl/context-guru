package components

import (
	"strings"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func mkMsg(s string) schemas.ChatMessage {
	t := s
	return schemas.ChatMessage{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &t}}
}

func TestTriggerFires(t *testing.T) {
	// ~1 message with a lot of tokens vs several tiny messages.
	big := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{mkMsg(strings.Repeat("word ", 4000))}}
	deep := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{
		mkMsg("a"), mkMsg("b"), mkMsg("c"), mkMsg("d"), mkMsg("e"),
	}}

	cases := []struct {
		name string
		tr   Trigger
		req  *schemas.BifrostChatRequest
		want bool
	}{
		{"zero fires always", Trigger{}, deep, true},
		{"token gate met", Trigger{MinRequestTokens: 1000}, big, true},
		{"token gate not met", Trigger{MinRequestTokens: 1000}, deep, false},
		{"message gate met", Trigger{MinMessages: 5}, deep, true},
		{"message gate not met", Trigger{MinMessages: 6}, deep, false},
		{"both gates ANDed (msg fails)", Trigger{MinRequestTokens: 1, MinMessages: 99}, big, false},
		{"MinOutputTokens does not affect Fires", Trigger{MinOutputTokens: 99999}, deep, true},
	}
	for _, c := range cases {
		if got := c.tr.Fires(c.req, nil); got != c.want { // nil Ctx = no window, only absolutes apply
			t.Errorf("%s: Fires=%v want %v", c.name, got, c.want)
		}
	}
}

// fillCtx is a Ctx carrying only what the request-fraction gate reads: the window, its
// provenance, and the provider's own count for the previous turn.
func fillCtx(window, billed int) *Ctx {
	return &Ctx{CtxWindow: window, CtxWindowExact: true, PrevBilledInput: billed}
}

// THE REGRESSION THIS FILE EXISTS FOR. min_request_frac shipped comparing frac*window against
// schema.MessagesTokens, which counts message TEXT only -- no system prompt, no tool
// declarations, no envelope -- while a window is stated in the units the provider bills. Measured
// on production traffic the provider's count runs a median 3.38x higher, so `0.9 * 1M` asked for
// roughly 3M provider tokens on a 1M model and could not be reached at all: the request is
// rejected upstream, or the client compacts, first.
//
// The failure was invisible because the OLD tests were written in the broken units too. They
// passed a ~4k-token request and a 200k window and asserted "0.6 does not fire", which is true
// under both the wrong arithmetic and the right one. The case below is the one that separates
// them: a request whose message text is nowhere near the threshold, on a session the provider has
// already billed past it. Under the old code this did not fire.
func TestTheRequestFractionIsMeasuredOnTheProvidersRulerNotOurs(t *testing.T) {
	small := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{mkMsg("carry on")}}
	const W = 1_000_000

	if !(Trigger{MinRequestFrac: 0.9}).Fires(small, fillCtx(W, 950_000)) {
		t.Error("a session the provider has billed 950k of a 1M window must satisfy frac 0.9, " +
			"however few tokens our own tokenizer counts in the message text")
	}
	if (Trigger{MinRequestFrac: 0.9}).Fires(small, fillCtx(W, 500_000)) {
		t.Error("500k of a 1M window is not 0.9 full and must not fire")
	}
	// And the absolute floor keeps its own ruler: it is stated in MessagesTokens and must not
	// start reading the billed figure, or every existing config that sets it changes meaning.
	if (Trigger{MinRequestTokens: 5000}).Fires(small, fillCtx(W, 950_000)) {
		t.Error("min_request_tokens counts message text, so a two-word request must not clear " +
			"5000 just because the provider billed 950k for the whole prompt")
	}
}

func TestTriggerFractions(t *testing.T) {
	small := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{mkMsg("carry on")}}
	const W = 200000

	// 0.6*200k = 120k against a session billed 4k => does not fire.
	if (Trigger{MinRequestFrac: 0.6}).Fires(small, fillCtx(W, 4000)) {
		t.Error("frac 0.6 of 200k should not fire on a session billed ~4k")
	}
	// A tiny frac fires.
	if !(Trigger{MinRequestFrac: 0.001}).Fires(small, fillCtx(W, 4000)) {
		t.Error("frac 0.001 of 200k (=200) should fire on a session billed ~4k")
	}
	// Unknown window (0) => fraction ignored, so a big frac still fires.
	if !(Trigger{MinRequestFrac: 0.9}).Fires(small, fillCtx(0, 4000)) {
		t.Error("fraction must be ignored when window unknown (0)")
	}
	// No billed figure yet (a session's first turn) => the fraction imposes nothing HERE, and
	// FracResolvable is what reports that the answer is not knowable. The two are separate on
	// purpose: Fires answers "is it too small", FracResolvable answers "can we tell".
	if !(Trigger{MinRequestFrac: 0.9}).Fires(small, fillCtx(W, 0)) {
		t.Error("with no previous billed figure the fraction must not gate inside Fires")
	}
	if (Trigger{MinRequestFrac: 0.9}).FracResolvable(fillCtx(W, 0)) {
		t.Error("FracResolvable must be false with no billed figure to compare")
	}
	if !(Trigger{MinRequestFrac: 0.9}).FracResolvable(fillCtx(W, 4000)) {
		t.Error("FracResolvable must be true once the window is exact and a billed figure exists")
	}
	// A trigger carrying no fraction is unaffected by any of it.
	if !(Trigger{}).FracResolvable(nil) {
		t.Error("a Trigger with no fraction must be resolvable against a nil Ctx")
	}

	// OutputFloor precedence: absolute > frac > legacy.
	if got := (Trigger{MinOutputTokens: 500}).OutputFloor(W, 300); got != 500 {
		t.Errorf("absolute floor should win: got %d", got)
	}
	if got := (Trigger{MinOutputFrac: 0.01}).OutputFloor(W, 300); got != 2000 {
		t.Errorf("frac floor 0.01*200k=2000: got %d", got)
	}
	if got := (Trigger{}).OutputFloor(0, 300); got != 300 {
		t.Errorf("legacy default when nothing set / window unknown: got %d", got)
	}

	// IsHuge: 0.15*200k=30k threshold.
	if !(Trigger{HugeOutputFrac: 0.15}).IsHuge(31000, W) {
		t.Error("31k output should be huge at 0.15*200k")
	}
	if (Trigger{HugeOutputFrac: 0.15}).IsHuge(31000, 0) {
		t.Error("huge must be false when window unknown")
	}
	if (Trigger{}).IsHuge(999999, W) {
		t.Error("huge must be false when HugeOutputFrac unset")
	}
}
