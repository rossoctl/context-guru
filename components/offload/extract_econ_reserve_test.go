package offload

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"

	bschemas "github.com/maximhq/bifrost/core/schemas"
)

// #214: while the stash reserve refuses payloads, a trial call is wasted, so exploring()
// must grant no slot. These tests pin the tracker, then the component end to end.

func fixedClock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func TestExploringWithholdsASlotWhileTheReserveRefuses(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	r := &ratioTracker{now: fixedClock(&now)}
	r.noteRefused()
	for _, s := range []string{"sessA", "sessB"} {
		ok, skipped := r.exploringWhy(s)
		if ok || !skipped {
			t.Fatalf("%s: explore=%v skipped=%v, want false/true while the reserve refuses", s, ok, skipped)
		}
	}
	if r.explored["sessA"] != 0 || r.explored["sessB"] != 0 {
		t.Fatalf("a withheld slot was consumed: %v", r.explored)
	}
}

func TestExploringResumesWhenTheReserveFrees(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	r := &ratioTracker{now: fixedClock(&now)}

	// Freed by a successful splice.
	r.noteRefused()
	r.noteSpliced()
	if !r.exploring("sessA") {
		t.Fatal("exploration did not resume after a splice")
	}

	// Freed by the window passing. The slot withheld earlier was not consumed.
	r.noteRefused()
	if r.exploring("sessB") {
		t.Fatal("explored inside the refusal window")
	}
	now = now.Add(reserveRefuseWindow + time.Second)
	if !r.exploring("sessB") {
		t.Fatal("exploration did not resume after the window passed")
	}
	if !r.exploring("sessB") || r.exploring("sessB") {
		t.Fatal("sessB must still get exactly maxExploreCalls slots after a withheld one")
	}
}

func TestExploringIsUnchangedWithoutARefusal(t *testing.T) {
	r := &ratioTracker{}
	n := 0
	for i := 0; i < 10; i++ {
		ok, skipped := r.exploringWhy("s")
		if skipped {
			t.Fatal("reported a reserve skip with no refusal ever recorded")
		}
		if ok {
			n++
		}
	}
	if n != maxExploreCalls {
		t.Fatalf("granted %d slots, want %d", n, maxExploreCalls)
	}
}

// End to end: with a reserve that refuses everything, session A pays its trial calls and
// learns of the refusal; session B then makes NO model call and the gate is counted.
func TestExtractLLMMakesNoTrialCallUnderAFullReserve(t *testing.T) {
	original := strings.Repeat("2026-08-31T10:00:00Z INFO  worker: processed batch\n", 400)
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model)
	e.gate = true
	e.allowCached = false
	run := func(session string) (*components.Report, int) {
		st := &refusingStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
		c := &components.Ctx{Session: session, Store: st, Ctx: context.Background(),
			CacheAware: true, Model: components.ModelSpec{Static: model, Incoming: model}}
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			{Role: bschemas.ChatMessageRoleUser}, {Role: bschemas.ChatMessageRoleTool},
			{Role: bschemas.ChatMessageRoleUser},
		}}
		schema.SetMessageText(&req.Input[0], "Summarize the worker log.")
		schema.SetMessageText(&req.Input[1], original)
		schema.SetMessageText(&req.Input[2], "ok")
		rep := &components.Report{Component: "extract_llm"}
		before := model.calls
		if _, err := e.Offload(req, rep, c); err != nil {
			t.Fatal(err)
		}
		return rep, model.calls - before
	}
	repA, callsA := run("sessA")
	if callsA == 0 || repA.Gates["stash_reserve_exhausted"] == 0 {
		t.Fatalf("precondition: session A must explore and be refused (calls %d, gates %v)", callsA, repA.Gates)
	}
	repB, callsB := run("sessB")
	if callsB != 0 {
		t.Fatalf("session B made %d trial calls under a full reserve, want 0", callsB)
	}
	if repB.Gates["explore_skipped_reserve_full"] == 0 {
		t.Fatalf("the skip is not counted: gates %v", repB.Gates)
	}
}

// A splice that lands through the real component clears the refusal. The splice here is a
// cross-session cache hit (no model call), so only apply() can have cleared the state.
func TestExtractLLMSpliceClearsTheReserveRefusal(t *testing.T) {
	original := strings.Repeat("2026-08-31T10:00:00Z INFO  worker: processed batch\n", 400)
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model)
	run := func(session string) {
		st := &gatedStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
		c := &components.Ctx{Session: session, Store: st, Ctx: context.Background(),
			Model: components.ModelSpec{Static: model, Incoming: model}}
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			{Role: bschemas.ChatMessageRoleUser}, {Role: bschemas.ChatMessageRoleTool},
		}}
		schema.SetMessageText(&req.Input[0], "Summarize the worker log.")
		schema.SetMessageText(&req.Input[1], original)
		if _, err := e.Offload(req, &components.Report{Component: "extract_llm"}, c); err != nil {
			t.Fatal(err)
		}
		if schema.MessageText(req.Input[1]) == original {
			t.Fatalf("%s: no splice, so the test proves nothing", session)
		}
	}
	e.ratios.noteRefused()
	run("sessA")
	e.ratios.mu.Lock()
	refusing := e.ratios.reserveRefusingLocked()
	e.ratios.mu.Unlock()
	if refusing {
		t.Fatal("a successful splice left the reserve marked as refusing")
	}
}
