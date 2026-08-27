package offload

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// #119: extract_llm raised two gates from inside its per-call goroutines.
//
// components.Report is copied by value throughout this codebase and its Gates map therefore
// carries no lock — the file says so itself, at the declaration of the per-slot ModelCall records
// that exist for exactly this reason. Two concurrent raises are a data race on a Go map, and the
// runtime's response is not a wrong counter and not a recoverable panic: it is
// `fatal error: concurrent map writes`, which aborts the PROCESS. In a proxy that means every
// in-flight request of every session dies, rather than one component failing open — the severity
// that makes this worth its own fix rather than a note.
//
// The reachable path is the single-flight follower. extractInflight collapses identical content
// into one call and releases every waiter at the same instant, so N byte-identical candidates in
// one request produce N-1 simultaneous `deduped_inflight_extraction` raises.
//
// Under `-race` the reversion is caught deterministically. Without it the abort is
// timing-dependent, so the count assertion is what holds in the plain suite: a lost gate is the
// quiet form of the same defect, and `deduped_inflight_extraction` is the one counter that says a
// call was avoided rather than made.
func TestConcurrentCallsDoNotRaceOnTheGateHistogram(t *testing.T) {
	// Byte-identical bodies, so all four share one extraction key and three become followers.
	// Distinct enough from every other fixture in this package that the process-wide
	// extractInflight group cannot collide with another test.
	body := strings.Repeat("gaterace fixture line for issue 119, identical across candidates\n", 400)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Summarise what these four identical outputs say."),
	}}
	const identical = 4
	for i := 0; i < identical; i++ {
		req.Input = append(req.Input, toolResultMsg(body))
	}

	model := &silentModel{}
	e := newCtxGuardComponent(t, model, "")
	rep := &components.Report{}
	c := &components.Ctx{
		Session: "gaterace", Ctx: context.Background(),
		Store: store.NewMemory(store.Options{}), CtxWindow: 1_000_000,
		// Caching off, so the tail gate lets every candidate through and all four reach the
		// concurrent phase.
		CacheAware: false, MaxCachedIdx: -1,
	}
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}

	// PRECONDITION: the concurrent phase ran with more than one goroutine in it. If the
	// candidates never reached phase 2 — a floor, the trigger, the economic gate — then no two
	// raises were ever concurrent and this test proves nothing about the race. The dedup gate
	// firing is the proof, because only a follower can raise it.
	got := rep.Gates["deduped_inflight_extraction"]
	if got == 0 {
		t.Fatalf("no single-flight follower ran, so no two gate raises were concurrent "+
			"(gates: %v) — the race was never exercised", rep.Gates)
	}
	if want := identical - 1; got != want {
		t.Errorf("deduped_inflight_extraction = %d, want %d: a follower's gate was lost, which is "+
			"the quiet form of the same unsynchronised write (gates: %v)", got, want, rep.Gates)
	}
}
