package components

import (
	"strings"
	"testing"
	"time"

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

// EVERY CONSTRUCTOR THAT EMBEDS A TRIGGER VALIDATES IT, which CacheAllows' docstring has always
// promised and exactly one of the three actually did.
func TestTriggerValidateRejectsWhatTheFormWouldAccept(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tr      Trigger
		wantErr bool
	}{
		{"the zero trigger is valid", Trigger{}, false},
		{"an empty cache_state means no constraint", Trigger{CacheState: ""}, false},
		{"every declared cache state is accepted", Trigger{CacheState: CacheStatePreExpiry}, false},
		{"a typo in cache_state is refused, not read as `any`",
			Trigger{CacheState: "pre_expiryy"}, true},
		// A WITHDRAWN VALUE IS REFUSED, NOT ALIASED. Both once meant "wait until invalidating the
		// cached prefix is free", and mapping either onto a surviving value would change when the
		// component fires without saying so. See removedCacheStates.
		{"the withdrawn `cold` is refused", Trigger{CacheState: "cold"}, true},
		{"the withdrawn `pre_expiry_or_cold` is refused", Trigger{CacheState: "pre_expiry_or_cold"}, true},
		// A WINDOW AT OR ABOVE THE TTL SWALLOWS THE WHOLE LIFETIME: `remaining <= preExpiry` is then
		// true for every request that has an entry at all, so CachePhase returns PreExpiry always and
		// Warm never, and a component gated on pre_expiry rewrites a live prefix on EVERY turn. The
		// settings form accepted 600 silently.
		{"a pre-expiry window wider than the shortest cache lifetime is refused",
			Trigger{PreExpirySeconds: 600}, true},
		{"a pre-expiry window exactly at the shortest cache lifetime is refused",
			Trigger{PreExpirySeconds: 300}, true},
		{"one second inside it is accepted", Trigger{PreExpirySeconds: 299}, false},
		{"a negative window is refused", Trigger{PreExpirySeconds: -1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.tr.Validate("c")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "c:") {
				t.Errorf("error must name the component whose config block is wrong: %v", err)
			}
		})
	}
}

// A 600-second window really does make every warm request pre-expiry, which is the behaviour
// Validate exists to prevent. Asserted directly so the constant's justification is not merely a
// comment.
func TestAWindowWiderThanTheTTLWouldSwallowEveryWarmTurn(t *testing.T) {
	// A maximally warm turn: the entry was written one second ago on a 5-minute TTL.
	warm := &Ctx{CacheTTLMs: 5 * 60 * 1000, IdleMs: 1_000}
	if got := warm.CachePhase(DefaultPreExpiry); got != CachePhaseWarm {
		t.Fatalf("precondition: phase = %s, want %s", got, CachePhaseWarm)
	}
	if got := warm.CachePhase(600 * time.Second); got != CachePhasePreExpiry {
		t.Fatalf("with a 600s window a one-second-old entry classified %s; the point of the "+
			"validation is that it classifies %s, so the component would rewrite a live prefix "+
			"every turn", got, CachePhasePreExpiry)
	}
	if err := (Trigger{PreExpirySeconds: 600}).Validate("c"); err == nil {
		t.Error("...and that configuration must therefore be refused at config time")
	}
}

// THE TWO SIZE THRESHOLDS ARE ANDed, NOT max()ed, and until now nothing asserted it. No shipped
// config sets both, so the change was invisible — which is exactly the kind of semantic change that
// wants a test before someone relies on the old behaviour.
//
// Under max() a request passing EITHER threshold fired. Under AND it must pass BOTH, each measured
// on its own ruler: min_request_tokens against our own message-text count, min_request_frac against
// the provider's billed input. max() could not be salvaged, because taking the larger of two numbers
// on two different rulers is meaningless.
func TestTheTwoSizeThresholdsAreAndedNotMaxed(t *testing.T) {
	// frac 0.9 of a 200k window = 180,000 billed tokens.
	tr := Trigger{MinRequestTokens: 1_000, MinRequestFrac: 0.9}
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{mkMsg(strings.Repeat("word ", 4000))}}

	full := &Ctx{CtxWindow: 200_000, CtxWindowExact: true, PrevBilledInput: 190_000}
	if !tr.Fires(req, full) {
		t.Error("both thresholds met, so it must fire")
	}
	// Passing the token threshold but NOT the fraction: under max() this fired, because 5,000
	// exceeded... nothing comparable. Under AND it must not.
	notFull := &Ctx{CtxWindow: 200_000, CtxWindowExact: true, PrevBilledInput: 50_000}
	if tr.Fires(req, notFull) {
		t.Error("the request is over min_request_tokens but the context is only a quarter full; " +
			"ANDed thresholds must both hold")
	}
	// And the other way: a full context but a tiny request.
	tiny := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{mkMsg("a")}}
	if tr.Fires(tiny, full) {
		t.Error("the context is full but the request is below min_request_tokens; ANDed thresholds " +
			"must both hold")
	}
	// A trigger carrying only ONE threshold — every shipped default — is unaffected, which is why
	// no operator is affected today.
	only := Trigger{MinRequestTokens: 1_000}
	if !only.Fires(req, notFull) {
		t.Error("a trigger with no fraction configured must not be constrained by one")
	}
}

// THE FILL FRACTION IS A FRACTION OF C, NOT OF THE MODEL WINDOW, and this is the case that says why.
//
// "90% full" has to mean 90% of the way to the point where the conversation's OWN compaction
// mechanism acts. That is the argument for the whole component: compacting just before the client
// would have acted captures the saving of a large prefix going cold and costs no accuracy that was not
// already going to be lost, because a compaction was going to happen there anyway.
//
// THE DIRECTION OF THE OLD ERROR IS WHAT MATTERS. A client that compacts EARLY resets the transcript
// before billed input ever reaches `frac x window` — so against the window the component NEVER FIRES
// on that deployment, silently, looking exactly like a gate that is working. Measured on haiku behind
// the current Claude Code, C is 0.996 of the window, so the two denominators are nearly the same
// there and this branch's acceptance runs were unaffected. That is luck, not design.
func TestTheFillFractionIsAFractionOfTheCompactionPointNotTheWindow(t *testing.T) {
	tr := Trigger{MinRequestFrac: 0.9}
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{mkMsg("hello")}}

	// A client that caps its own context at 120,000 billed on a 200,000 model. Against the window the
	// gate would want 180,000, which this conversation can never reach — the client resets first.
	early := &Ctx{
		CtxWindow: 200_000, CtxWindowExact: true,
		CompactionPoint: 120_000, CompactionPointSource: "measured",
		PrevBilledInput: 110_000, // 0.92 of C, and only 0.55 of the window
	}
	if !tr.Fires(req, early) {
		t.Error("the conversation is 92% of the way to where the CLIENT will compact, which is the " +
			"only moment this component can act usefully — but only 55% of the model window. " +
			"Measuring the fraction against the window means never firing on this deployment")
	}
	// And it must still decline when the conversation is genuinely early against C.
	tooEarly := &Ctx{
		CtxWindow: 200_000, CtxWindowExact: true,
		CompactionPoint: 120_000, CompactionPointSource: "measured",
		PrevBilledInput: 60_000, // half of C
	}
	if tr.Fires(req, tooEarly) {
		t.Error("half way to the client's compaction point is not 90% full; the gate must still decline")
	}

	// THE FALLBACK: with no compaction point known, the window is the denominator. That is correct
	// where nothing compacts (a raw API client grows until the provider rejects it) and a guess where
	// a client compacts unobserved — which is why the source is carried alongside.
	noC := &Ctx{CtxWindow: 200_000, CtxWindowExact: true, PrevBilledInput: 185_000}
	if !tr.Fires(req, noC) {
		t.Error("with no compaction point known the window is the denominator, and 185,000 of " +
			"200,000 is over 0.9")
	}
	if got := noC.FillDenominator(); got != 200_000 {
		t.Errorf("FillDenominator = %d with no compaction point, want the window 200000", got)
	}
	if got := early.FillDenominator(); got != 120_000 {
		t.Errorf("FillDenominator = %d, want the compaction point 120000 — it takes precedence over "+
			"the window whenever it is known", got)
	}
}

// A client that compacts LATE than our gate would not be helped by the old denominator either, and
// this is the mirror case: C above the window is clamped by nothing here, so the gate simply asks for
// more fill. It must not overflow into never-firing by arithmetic accident.
func TestACompactionPointAtOrAboveTheWindowStillFires(t *testing.T) {
	tr := Trigger{MinRequestFrac: 0.9}
	req := &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{mkMsg("hello")}}
	// The shipped haiku figure: C is 0.996 of the window, so the gate wants 179,280.
	c := &Ctx{
		CtxWindow: 200_000, CtxWindowExact: true,
		CompactionPoint: 199_200, CompactionPointSource: "measured",
		PrevBilledInput: 180_000,
	}
	if !tr.Fires(req, c) {
		t.Error("180,000 billed against a 199,200 compaction point is 0.904 — over the 0.9 gate")
	}
	c.PrevBilledInput = 170_000 // 0.853 of C
	if tr.Fires(req, c) {
		t.Error("170,000 of 199,200 is 0.853, under the gate")
	}
}
