package offload

import (
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
)

func horizonCtx(billed, window int, exact bool) *components.Ctx {
	return &components.Ctx{CtxWindow: window, CtxWindowExact: exact, PrevBilledInputThread: billed, Session: "h", Thread: "t"}
}

// growthOf builds a billedGrowth that grew by `per` each turn over `turns` turns, ending at `last`.
func growthOf(last, per, turns int) billedGrowth {
	return billedGrowth{from: last - per*turns, turns: turns, last: last}
}

// shapedReq builds a transcript of `turns` model turns, `big` of them followed by a large tool output.
func shapedReq(turns, big, bigLines int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{}
	for i := 0; i < turns; i++ {
		req.Input = append(req.Input, assistantMsg("step"))
		body := "ok"
		if i < big {
			body = strings.Repeat("a long listing line\n", bigLines)
		}
		req.Input = append(req.Input, toolResultMsg(body))
	}
	return req
}

// Defect 2's own case: a few large tool outputs move the OLD horizon by orders of magnitude, while the
// billed growth does not move, so the new horizon is the same for both shapes.
func TestHorizonStableWhereLocalEstimateSwings(t *testing.T) {
	small, large := shapedReq(40, 0, 0), shapedReq(40, 3, 3000)
	win := 1_000_000
	oldSmall := estimateTurnsRemaining(schema.MessagesTokens(small), modelTurns(small), win)
	oldLarge := estimateTurnsRemaining(schema.MessagesTokens(large), modelTurns(large), win)
	if oldSmall < 50*oldLarge {
		t.Fatalf("fixture does not show the swing: old horizons %d vs %d", oldSmall, oldLarge)
	}
	c := horizonCtx(200_000, win, true)
	g := growthOf(200_000, 4_000, 10)
	a := cacheRewriteHorizon(small, c, g, 0)
	b := cacheRewriteHorizon(large, c, g, 0)
	if a.Model != horizonModelBilled || b.Model != horizonModelBilled {
		t.Fatalf("models %q %q, want billed", a.Model, b.Model)
	}
	if a.turns() != b.turns() || a.turns() != 200 { // (1,000,000 - 200,000) / 4,000
		t.Fatalf("N = %d and %d, want 200 for both", a.turns(), b.turns())
	}
}

func TestHorizonCutFracIsTheCut(t *testing.T) {
	c := horizonCtx(200_000, 1_000_000, true)
	c.CutFrac = 0.5
	h := cacheRewriteHorizon(shapedReq(10, 0, 0), c, growthOf(200_000, 10_000, 5), 0)
	if h.turns() != 30 { // (500,000 - 200,000) / 10,000
		t.Fatalf("N = %d, want 30", h.turns())
	}
	c.PrevBilledInputThread = 600_000 // past the cut
	if h := cacheRewriteHorizon(shapedReq(10, 0, 0), c, growthOf(600_000, 10_000, 5), 0); h.turns() != 0 || h.Model != horizonModelBilled {
		t.Fatalf("past the cut: N = %d model %q, want 0 billed", h.turns(), h.Model)
	}
}

func TestHorizonFallbacks(t *testing.T) {
	req := shapedReq(10, 0, 0)
	g := growthOf(100_000, 5_000, 4)
	for _, tc := range []struct {
		name string
		c    *components.Ctx
		g    billedGrowth
		why  string
	}{
		{"no billed size", horizonCtx(0, 1_000_000, true), g, horizonWhyNoBilled},
		{"window not exact", horizonCtx(100_000, 1_000_000, false), g, horizonWhyWindowNotExact},
		{"no growth yet", horizonCtx(100_000, 1_000_000, true), billedGrowth{}, horizonWhyGrowthUnknown},
		{"flat growth", horizonCtx(100_000, 1_000_000, true), billedGrowth{from: 100_000, turns: 3, last: 100_000}, horizonWhyGrowthUnknown},
	} {
		h := cacheRewriteHorizon(req, tc.c, tc.g, 0)
		want := estimateTurnsRemaining(schema.MessagesTokens(req), modelTurns(req), tc.c.CtxWindow)
		if h.Model != horizonModelFallback || h.Why != tc.why || h.turns() != want {
			t.Errorf("%s: got model=%q why=%q N=%d, want fallback %q N=%d", tc.name, h.Model, h.Why, h.turns(), tc.why, want)
		}
	}
}

// A shrink restarts the growth; a repeated size is not a turn.
func TestBilledGrowthRestartsOnShrink(t *testing.T) {
	var g billedGrowth
	for _, b := range []int{100, 200, 300} {
		g.note(b)
	}
	if g.perTurn() != 100 {
		t.Fatalf("per turn %v, want 100", g.perTurn())
	}
	g.note(300) // same previous turn read twice
	if g.perTurn() != 100 {
		t.Fatalf("a repeated size changed growth: %v", g.perTurn())
	}
	g.note(150) // shrink
	if g.perTurn() != 0 || g.from != 150 || g.turns != 0 {
		t.Fatalf("after a shrink: %+v, want a restart at 150", g)
	}
	g.note(170)
	if g.perTurn() != 20 {
		t.Fatalf("growth since the shrink %v, want 20", g.perTurn())
	}
}

func TestThreadGrowthIsPerThreadAndAged(t *testing.T) {
	growthMu.Lock()
	growthThreads = map[string]*growthEntry{}
	growthMu.Unlock()
	now := time.Now()
	growthNow = func() time.Time { return now }
	defer func() { growthNow = time.Now }()
	a := &components.Ctx{Session: "s", Thread: "a"}
	b := &components.Ctx{Session: "s", Thread: "b"}
	for _, v := range []int{100, 200} {
		a.PrevBilledInputThread = v
		noteThreadGrowth(a)
	}
	b.PrevBilledInputThread = 5000
	if g := noteThreadGrowth(b); g.perTurn() != 0 {
		t.Fatalf("thread b read thread a's growth: %+v", g)
	}
	a.PrevBilledInputThread = 300
	if g := noteThreadGrowth(a); g.perTurn() != 100 {
		t.Fatalf("thread a growth %v, want 100", g.perTurn())
	}
	now = now.Add(horizonGrowthMaxAge + time.Minute)
	a.PrevBilledInputThread = 400
	if g := noteThreadGrowth(a); g.perTurn() != 0 {
		t.Fatalf("an aged entry was kept: %+v", g)
	}
}

func TestThreadGrowthMapIsBounded(t *testing.T) {
	growthMu.Lock()
	growthThreads = map[string]*growthEntry{}
	growthMu.Unlock()
	for i := 0; i < horizonMaxThreads+50; i++ {
		noteThreadGrowth(&components.Ctx{Session: "s" + string(rune('A'+i%26)) + string(rune('a'+i/26)), PrevBilledInputThread: 10})
	}
	growthMu.Lock()
	n := len(growthThreads)
	growthMu.Unlock()
	if n > horizonMaxThreads {
		t.Fatalf("map grew to %d, cap %d", n, horizonMaxThreads)
	}
}

// Defect 1 in the new model: the horizon is measured after the removal. Removing mass leaves more room,
// so N grows by (billed removed) / growth.
func TestHorizonCreditsTheRemoval(t *testing.T) {
	req := shapedReq(10, 2, 2000)
	local := schema.MessagesTokens(req)
	c := horizonCtx(local*3, 1_000_000, true) // ratio 3
	g := growthOf(local*3, 3_000, 5)
	before := cacheRewriteHorizon(req, c, g, 0)
	removed := local / 2
	after := cacheRewriteHorizon(req, c, g, removed)
	want := before.N + float64(3*removed)/3000
	if after.N < want-0.01 || after.N > want+0.01 {
		t.Fatalf("N after removal %.2f, want %.2f (before %.2f)", after.N, want, before.N)
	}
	// The ratio is bounded: a nonsense billed size must not credit more than horizonMaxRatio per token.
	c2 := horizonCtx(local*100, 100_000_000, true)
	a, b := cacheRewriteHorizon(req, c2, growthOf(local*100, 1000, 5), 0), cacheRewriteHorizon(req, c2, growthOf(local*100, 1000, 5), removed)
	if got := b.N - a.N; got > float64(horizonMaxRatio)*float64(removed)/1000+0.01 {
		t.Fatalf("credit %.1f exceeds the ratio bound", got)
	}
}

func TestCorefCountsHorizonFallback(t *testing.T) {
	req := bigReq(24, 400)
	c := horizonCtx(0, 2_000_000, true)
	var rep components.Report
	cf := &Coref{}
	cf.breakEvenTurns(req, []plannedCut{{idx: 2, saved: 800}}, c, billedGrowth{}, &rep)
	if rep.Events[horizonFallbackEventPrefix+horizonWhyNoBilled] != 1 {
		t.Fatalf("fallback not counted: %v", rep.Events)
	}
	rep = components.Report{}
	c = horizonCtx(100_000, 2_000_000, true)
	cf.breakEvenTurns(req, []plannedCut{{idx: 2, saved: 800}}, c, growthOf(100_000, 2000, 5), &rep)
	if len(rep.Events) != 0 {
		t.Fatalf("billed horizon counted as a fallback: %v", rep.Events)
	}
}

// A side request between the main thread's turns does not reset the main thread's run, because the
// horizon notes PrevBilledInputThread, not the session figure (which the side request overwrites).
func TestSideRequestDoesNotResetMainThreadGrowth(t *testing.T) {
	growthMu.Lock()
	growthThreads = map[string]*growthEntry{}
	growthMu.Unlock()
	main := &components.Ctx{Session: "sx", Thread: ""}
	var g billedGrowth
	for _, b := range []int{100_000, 104_000, 108_000, 112_000} {
		// The session figure at this request is a small side answer; the thread figure is the main one.
		main.PrevBilledInput, main.PrevBilledInputThread = 3_000, b
		g = noteThreadGrowth(main)
	}
	if g.perTurn() != 4_000 || g.turns != 3 {
		t.Fatalf("main thread growth %+v per turn %v, want a steady 4000 over 3 turns", g, g.perTurn())
	}
}
