package offload

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// `fire_on: size` (issue: "run it whenever a message is big enough, bound it by the caps,
// not by context pressure"). The contract has three halves and each is a separate way to
// get this wrong:
//
//  1. size fires where pressure would not,
//  2. the per-session cap is the outer bound on spend,
//  3. the caching-backend guard becomes ADVISORY rather than silently still blocking —
//     which would make the whole option look like it did nothing.

// newSizeComponent builds through the registered constructor, leaving the economic gate at
// its DEFAULT (on). newCtxGuardComponent cannot be reused here: it sets
// economic_gate: false, which also implies allow_on_caching_backend, so it would hide
// exactly the interaction tested below.
func newSizeComponent(t *testing.T, model components.Model, yaml string) *ExtractLLM {
	t.Helper()
	c, err := newExtractLLM([]byte(yaml))
	if err != nil {
		t.Fatalf("newExtractLLM(%q): %v", yaml, err)
	}
	e := c.(*ExtractLLM)
	e.modelClient = model
	e.mode = markerFull
	return e
}

// sizeBigOutput is ~8k tokens: above the 0.6%-of-window pressure floor a 1M-window model
// derives at low pressure, so a declined call can only be the TRIGGER's doing and not the
// per-output floor's.
func sizeBigOutput() string {
	return strings.Repeat("2024-01-01 GET /users/42 200 12ms handler=src/api/users.py\n", 700)
}

func sizeReq() *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Find the auth timeout in src/api/users.py and fix it."),
		toolResultMsg(sizeBigOutput()),
		userMsg("keep going"),
	}}
}

// A tiny request against a 1M window is ~0% context pressure — the case the derived
// trigger exists to decline and the case Osher wants to fire on.
func TestFireOnSizeFiresWherePressureDeclines(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yaml     string
		wantCall bool
		wantGate string
	}{
		{"pressure (default) declines a low-pressure turn", "strategy: code\neconomic_gate: false\n", false, "no_model_this_request"},
		{"size fires on the same turn", "fire_on: size\nmin_tokens: 500\nstrategy: code\neconomic_gate: false\n", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &silentModel{}
			e := newSizeComponent(t, model, tc.yaml)
			rep := &components.Report{}
			c := &components.Ctx{
				Session: "size-" + tc.name, Ctx: context.Background(),
				Store: store.NewMemory(store.Options{}), CtxWindow: 1_000_000,
				Model: components.ModelSpec{Static: model, Incoming: model},
			}
			if _, err := e.Offload(sizeReq(), rep, c); err != nil {
				t.Fatalf("Offload must fail open: %v", err)
			}
			calls := atomic.LoadInt64(&model.calls)
			if tc.wantCall && calls == 0 {
				t.Fatalf("fire_on: size made no model call at ~0%% context pressure — the "+
					"size trigger is not reaching the candidate loop (gates: %v)", rep.Gates)
			}
			if !tc.wantCall {
				if calls != 0 {
					t.Fatalf("the default pressure trigger made %d call(s) on a ~0%% "+
						"pressure turn; that is the 271-call behaviour returning", calls)
				}
				if rep.Gates[tc.wantGate] == 0 {
					t.Fatalf("no %s gate recorded, so the refusal is undiagnosable (gates: %v)",
						tc.wantGate, rep.Gates)
				}
			}
		})
	}
}

// llm_max_per_session is the only thing bounding a long session's spend once the economic
// gate is advisory: llm_every_n_requests throttles REQUESTS and llm_max_per_request caps
// one turn, so neither can stop 2 calls x 300 turns.
func TestSessionCapIsTheOuterBoundOnCalls(t *testing.T) {
	const cap = 2
	model := &silentModel{}
	e := newSizeComponent(t, model, "fire_on: size\nmin_tokens: 500\nstrategy: code\n"+
		"economic_gate: false\nllm_max_per_session: "+strconv.Itoa(cap)+"\n")
	var capped int
	for turn := 0; turn < 5; turn++ {
		rep := &components.Report{}
		c := &components.Ctx{
			Session: "one-session", Ctx: context.Background(),
			Store: store.NewMemory(store.Options{}), CtxWindow: 1_000_000,
			Model: components.ModelSpec{Static: model, Incoming: model},
		}
		// A distinct body per turn, or the frozen/global result cache answers without a call
		// and the test would pass with the cap doing nothing.
		req := sizeReq()
		req.Input[1] = toolResultMsg(sizeBigOutput() + strconv.Itoa(turn))
		if _, err := e.Offload(req, rep, c); err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		capped += rep.Gates["over_per_session_cap"]
	}
	if got := atomic.LoadInt64(&model.calls); got != cap {
		t.Fatalf("made %d model calls across 5 turns with llm_max_per_session: %d — the "+
			"session cap is not bounding spend", got, cap)
	}
	if capped == 0 {
		t.Fatal("the cap refused calls but recorded no over_per_session_cap gate, so an " +
			"operator cannot tell a quiet session from an exhausted budget")
	}
}

// On a caching backend the gate HARD-declines by default (measured net-negative). With
// `fire_on: size` the operator has taken that decision, so the call must happen AND the
// counterfactual must be recorded — a silent override is how a $3 surprise happens.
func TestFireOnSizeDemotesTheCachingGuardToAdvisory(t *testing.T) {
	for _, tc := range []struct {
		name     string
		yaml     string
		wantCall bool
		gate     string
	}{
		{"default blocks on a caching backend", "min_tokens: 500\nstrategy: code\n", false, "economic_gate"},
		{"size makes it advisory", "fire_on: size\nmin_tokens: 500\nstrategy: code\n", true, "economic_gate_advisory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &silentModel{}
			e := newSizeComponent(t, model, tc.yaml)
			rep := &components.Report{}
			c := &components.Ctx{
				Session: "advisory-" + tc.name, Ctx: context.Background(),
				Store: store.NewMemory(store.Options{}), CtxWindow: 1_000_000,
				CacheAware: true, MaxCachedIdx: 0, // the tool output at index 1 is in the tail
				Model: components.ModelSpec{Static: model, Incoming: model},
			}
			if _, err := e.Offload(sizeReq(), rep, c); err != nil {
				t.Fatalf("Offload must fail open: %v", err)
			}
			calls := atomic.LoadInt64(&model.calls)
			if tc.wantCall != (calls > 0) {
				t.Fatalf("calls=%d, wantCall=%v (gates: %v)", calls, tc.wantCall, rep.Gates)
			}
			if rep.Gates[tc.gate] == 0 {
				t.Fatalf("expected the %s gate; got %v", tc.gate, rep.Gates)
			}
		})
	}
}

// A size trigger with no size must not inherit the legacy 300-token default: nearly every
// tool output clears 300, so it would fire on every turn of every session.
func TestFireOnSizeWithoutAThresholdUsesTheConservativeDefault(t *testing.T) {
	e := newSizeComponent(t, &silentModel{}, "fire_on: size\n")
	if e.minTokens != defaultSizeThreshold {
		t.Fatalf("min_tokens = %d, want the conservative %d", e.minTokens, defaultSizeThreshold)
	}
	if !e.minTokensSet {
		t.Fatal("the derived pressure floor can still override the size threshold")
	}
	if got := e.outputFloor(1_000_000); got != defaultSizeThreshold {
		t.Fatalf("outputFloor = %d on a 1M window, want %d", got, defaultSizeThreshold)
	}
}

func TestFireOnRejectsAnUnknownValue(t *testing.T) {
	if _, err := newExtractLLM([]byte("fire_on: whenever\n")); err == nil {
		t.Fatal("an unknown fire_on was accepted; a typo must be a save-time 400, not a " +
			"component that silently keeps its old trigger")
	}
}
