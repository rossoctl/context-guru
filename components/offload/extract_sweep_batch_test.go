package offload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// Tests for BATCHING (#132): a long list of outputs is asked in balanced batches, in sequence, merged into
// ONE held answer. n <= 12 is one ask, exactly as before.

// batchTestBudget is the sweep's outer timeout in these tests. It must fit k questions even under -race: a
// batch never STARTS after the sweep's context ended (extract_sweep_batch.go), so a budget sized for one
// question (the old 1 s) correctly skips the second batch on a slow machine.
const batchTestBudget = 60 * time.Second

func candsOfSizes(n int) []sweepCand {
	out := make([]sweepCand, n)
	for i := range out {
		out[i] = sweepCand{i: i, toolID: fmt.Sprintf("toolu_b%d", i),
			content: strings.Repeat(fmt.Sprintf("line %d\n", i), 400+10*i)}
	}
	return out
}

func TestMaxAskBatchesDefaultsToFourAndIsClamped(t *testing.T) {
	for cfg, want := range map[string]int{"": 4, "max_ask_batches: 0\n": 4, "max_ask_batches: -2\n": 4,
		"max_ask_batches: 6\n": 6, "max_ask_batches: 8\n": 8, "max_ask_batches: 50\n": 8} {
		e := shippedSweep(t, cfg)
		if got := e.askBatchesLimit(); got != want || e.maxAskBatches != want {
			t.Errorf("max_ask_batches %q reads as %d (stored %d), want %d", cfg, got, e.maxAskBatches, want)
		}
	}
}

func TestTwelveOrFewerCandidatesAreOneBatchInTheirOriginalOrder(t *testing.T) {
	for _, n := range []int{1, 2, 11, 12} {
		b := splitAskBatches(candsOfSizes(n), maxAskItems)
		if len(b) != 1 || len(b[0].cands) != n {
			t.Fatalf("%d candidates split into %d batches, want exactly 1 (as today)", n, len(b))
		}
		for i, cd := range b[0].cands {
			if cd.i != i || b[0].idx[i] != i {
				t.Fatalf("candidate %d moved to position %d: a single batch must keep the original order", cd.i, i)
			}
		}
	}
	if splitAskBatches(nil, maxAskItems) != nil {
		t.Error("no candidates must give no batches")
	}
}

func TestTheBatchesAreBalancedAndMixSizes(t *testing.T) {
	for n, want := range map[int][]int{13: {7, 6}, 16: {8, 8}, 23: {12, 11}, 24: {12, 12}, 25: {9, 8, 8}, 48: {12, 12, 12, 12}} {
		b := splitAskBatches(candsOfSizes(n), maxAskItems)
		var got []int
		for _, x := range b {
			got = append(got, len(x.cands))
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%d candidates split as %v, want %v", n, got, want)
		}
	}
	b := splitAskBatches(candsOfSizes(16), maxAskItems)
	if len(b) != 2 {
		t.Fatalf("16 candidates split into %d batches, want 2", len(b))
	}
	big := func(x askBatch) int {
		n := 0
		for _, i := range x.idx {
			if i >= 8 { // the 8 largest of 16 (size grows with the index)
				n++
			}
		}
		return n
	}
	if big(b[0]) != 4 || big(b[1]) != 4 {
		t.Errorf("the 8 largest are split %d/%d between the 2 batches, want 4/4", big(b[0]), big(b[1]))
	}
	// Largest first: the two LARGEST candidates (15 and 14) lead the two batches.
	if b[0].idx[0] != 15 || b[1].idx[0] != 14 {
		t.Errorf("batches lead with candidates %d and %d, want the two largest (15 and 14)", b[0].idx[0], b[1].idx[0])
	}
}

func TestEveryCandidateIsInExactlyOneBatch(t *testing.T) {
	for _, n := range []int{1, 7, 12, 13, 23, 40, 48} {
		seen := map[int]int{}
		for _, b := range splitAskBatches(candsOfSizes(n), maxAskItems) {
			for _, i := range b.idx {
				seen[i]++
			}
		}
		if len(seen) != n {
			t.Errorf("%d candidates: %d placed", n, len(seen))
		}
		for i, c := range seen {
			if c != 1 {
				t.Errorf("%d candidates: candidate %d is in %d batches", n, i, c)
			}
		}
	}
}

// candidateFor registers a conversation of n outputs through the real registration path (Offload, as the
// request path runs it) and returns the registered candidate, as the ping would find it.
func candidateFor(t *testing.T, cfg, session string, asker components.PrefixAsker, st store.Store, n int) *sweepPingCandidate {
	t.Helper()
	e := shippedSweep(t, cfg)
	key, _ := registered(t, e, session, asker, st, deepCandidates(n))
	sweepPingMu.Lock()
	cand := sweepPingCand[key]
	sweepPingMu.Unlock()
	if cand == nil {
		t.Fatalf("%d outputs registered no candidate", n)
	}
	return cand
}

func collectN(t *testing.T, cfg string, n int) ([]sweepCand, *components.Report) {
	t.Helper()
	cand := candidateFor(t, cfg, "batch-collect", pingAsker(), newMemStore(), n)
	rep := &components.Report{Component: "extract_llm_sweep"}
	return cand.collect(rep), rep
}

func TestOnlyBatchTimesBatchesCandidatesAreOfferedAndTheSmallestAreCounted(t *testing.T) {
	cands, rep := collectN(t, "", 60) // 12 x 4 = 48
	if len(cands) != 48 || rep.Gates["sweep_over_ask_cap"] != 12 {
		t.Fatalf("offered %d (over cap %d), want 48 and 12 over", len(cands), rep.Gates["sweep_over_ask_cap"])
	}
	cands, rep = collectN(t, "max_ask_batches: 2\n", 60) // 12 x 2 = 24
	if len(cands) != 24 || rep.Gates["sweep_over_ask_cap"] != 36 {
		t.Fatalf("with 2 batches offered %d (over cap %d), want 24 and 36 over", len(cands), rep.Gates["sweep_over_ask_cap"])
	}
	cands, rep = collectN(t, "", 20)
	if len(cands) != 20 || rep.Gates["sweep_over_ask_cap"] != 0 {
		t.Fatalf("offered %d (over cap %d), want all 20 and none over", len(cands), rep.Gates["sweep_over_ask_cap"])
	}
}

// ---------------------------------------------------------------------------------------------------

// dynAsker answers each question with a function of its text, so a script need not know the labels.
type dynAsker struct {
	mu    sync.Mutex
	fn    func(n int, ask string) (string, components.PrefixUsage, error)
	asks  []string
	calls int
}

func (a *dynAsker) Ask(_ context.Context, _, ask string) (string, components.PrefixUsage, error) {
	a.mu.Lock()
	n := a.calls
	a.calls++
	a.asks = append(a.asks, ask)
	a.mu.Unlock()
	return a.fn(n, ask)
}

var okUse = components.PrefixUsage{CacheRead: 19595, Fresh: 40, Output: 90}

// dropAll answers a question by dropping every label it names.
func dropAll(ask string) string {
	var parts []string
	for _, l := range askLabelRe.FindAllStringSubmatch(ask, -1) {
		parts = append(parts, `{"i":`+l[1]+`,"needed_by":"none","quote":"","verdict":"drop"}`)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func askN(t *testing.T, asker components.PrefixAsker, cfg string, n int, wait SweepPingWait) (SweepPingResult, store.Store, string) {
	t.Helper()
	resetSweepBatchForTest()
	st := newMemStore()
	cand := candidateFor(t, cfg, "batch-ask", asker, st, n)
	rep := &components.Report{Component: "extract_llm_sweep"}
	cands := cand.collect(rep)
	res := cand.ask(cands, rep, 0, wait)
	return res, st, thread.Key("batch-ask", "")
}

func TestTwelveOrFewerCandidatesMakeExactlyOneAsk(t *testing.T) {
	for _, n := range []int{1, 5, 12} {
		a := &dynAsker{fn: func(i int, ask string) (string, components.PrefixUsage, error) { return dropAll(ask), okUse, nil }}
		res, _, _ := askN(t, a, "", n, SweepPingWait{})
		if a.calls != 1 {
			t.Fatalf("%d candidates made %d asks, want exactly 1 (as today)", n, a.calls)
		}
		if SweepBatchCounters().Batches != 1 || res.Events["sweep_ask_batches"] != 1 || !res.Held {
			t.Fatalf("%d candidates: batches %d, result %+v", n, SweepBatchCounters().Batches, res)
		}
	}
}

func TestTheBatchesAreAskedInSequenceAndMergedIntoOneHeldAnswer(t *testing.T) {
	a := &dynAsker{fn: func(i int, ask string) (string, components.PrefixUsage, error) { return dropAll(ask), okUse, nil }}
	res, st, key := askN(t, a, "", 20, SweepPingWait{})
	if a.calls != 2 {
		t.Fatalf("%d asks, want 2 batches", a.calls)
	}
	if !res.Held || res.Offered != 20 || res.Proposed != 20 {
		t.Fatalf("result = %+v, want one held answer proposing all 20", res)
	}
	h, ok := getHeld(st, key)
	if !ok || len(h.Outputs) != 20 {
		t.Fatalf("held = %d outputs, want ONE merged answer of 20", len(h.Outputs))
	}
	seen := map[string]int{}
	for _, o := range h.Outputs {
		seen[o.ToolUseID]++
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("%s is judged %d times in the merged answer", id, c)
		}
	}
	// No candidate is named in both questions.
	for i := 0; i < 20; i++ {
		named := 0
		for _, ask := range a.asks {
			if strings.Contains(ask, "toolu_wire_"+fmt.Sprint(i)+",") || strings.Contains(ask, "toolu_wire_"+fmt.Sprint(i)+" ") {
				named++
			}
		}
		if named != 1 {
			t.Errorf("candidate %d is named in %d questions, want exactly 1", i, named)
		}
	}
	// The answer cost is the SUM of the asks' usage.
	if res.CacheRead != 2*okUse.CacheRead || res.Output != 2*okUse.Output {
		t.Errorf("usage = read %d out %d, want the sum of two asks (%d, %d)", res.CacheRead, res.Output, 2*okUse.CacheRead, 2*okUse.Output)
	}
	if SweepBatchCounters().Batches != 2 {
		t.Errorf("sweep_ask_batches = %d, want 2", SweepBatchCounters().Batches)
	}
}

// ONE CUT: the merged answer is a single held answer, so a cold request applies it once. One key holds all
// 20 verdicts; there is no per-batch record.
func TestOneMergedAnswerHoldsEveryVerdictUnderOneKey(t *testing.T) {
	a := &dynAsker{fn: func(i int, ask string) (string, components.PrefixUsage, error) { return dropAll(ask), okUse, nil }}
	_, st, key := askN(t, a, "", 20, SweepPingWait{})
	h, ok := getHeld(st, key)
	if !ok {
		t.Fatal("nothing held")
	}
	if n, _ := h.proposed(); n != 20 {
		t.Fatalf("the one held answer proposes %d drops, want all 20", n)
	}
	if _, ok := getHeld(st, key+"/batch2"); ok {
		t.Fatal("a second, per-batch held answer exists")
	}
}

func TestABatchThatFailsFailsOpenAndTheOthersStillCount(t *testing.T) {
	for name, tc := range map[string]struct {
		fn     func(n int, ask string) (string, components.PrefixUsage, error)
		reason string
	}{
		"an error": {func(n int, ask string) (string, components.PrefixUsage, error) {
			if n == 0 {
				return "", components.PrefixUsage{}, errors.New("boom")
			}
			return dropAll(ask), okUse, nil
		}, "ask_failed"},
		"a refused cache read": {func(n int, ask string) (string, components.PrefixUsage, error) {
			if n == 0 {
				return dropAll(ask), components.PrefixUsage{CacheRead: 0, Fresh: 40, Output: 90}, nil
			}
			return dropAll(ask), okUse, nil
		}, "prefix_cache_read_ZERO"},
		"a truncated reply": {func(n int, ask string) (string, components.PrefixUsage, error) {
			if n == 0 {
				return `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"},{"i":1,"needed_by":"no`, okUse, nil
			}
			return dropAll(ask), okUse, nil
		}, "reply_truncated"},
		"an unparseable reply": {func(n int, ask string) (string, components.PrefixUsage, error) {
			if n == 0 {
				return "I cannot do that", okUse, nil
			}
			return dropAll(ask), okUse, nil
		}, "unparseable"},
	} {
		t.Run(name, func(t *testing.T) {
			a := &dynAsker{fn: tc.fn}
			res, st, key := askN(t, a, "", 20, SweepPingWait{})
			if a.calls != 2 {
				t.Fatalf("%d asks, want 2", a.calls)
			}
			h, ok := getHeld(st, key)
			if !ok || !res.Held {
				t.Fatal("nothing held although one batch answered")
			}
			// The failed batch's outputs are UNASKED, not "kept": absent from the held answer, so the next
			// question may offer them again. The batch that answered holds its 10.
			if len(h.Outputs) != 10 {
				t.Fatalf("the held answer carries %d outputs, want only the 10 of the batch that answered", len(h.Outputs))
			}
			for _, o := range h.Outputs {
				if !o.Drop {
					t.Errorf("an answered item is not a drop: %+v", o)
				}
			}
			st2 := SweepBatchCounters()
			if st2.Failed != 1 || st2.FailedBy[tc.reason] != 1 {
				t.Errorf("batch counters = %+v, want one failure for %q", st2, tc.reason)
			}
			ev := "sweep_batch_failed_" + trimReason(tc.reason) // event names are lower-case
			if res.Events[ev] != 1 || res.Events["sweep_ask_batches"] != 2 {
				t.Errorf("events = %v, want %s once and sweep_ask_batches 2", res.Events, ev)
			}
		})
	}
}

func TestIfEveryBatchFailsNothingIsHeldAndItIsAFailedAsk(t *testing.T) {
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		return "", components.PrefixUsage{}, errors.New("boom")
	}}
	res, st, key := askN(t, a, "", 20, SweepPingWait{})
	if res.Held {
		t.Fatal("an answer is held although every batch failed")
	}
	if _, ok := getHeld(st, key); ok {
		t.Fatal("the Store holds an answer")
	}
	if SweepBatchCounters().Failed != 2 {
		t.Errorf("failed batches = %d, want 2", SweepBatchCounters().Failed)
	}
}

// Each batch is judged by prefixAskCacheOK on its own: a read below the turn's read in the SECOND batch
// loses that batch only.
func TestEachBatchIsJudgedByThePrefixAskCacheRuleOnItsOwn(t *testing.T) {
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		use := okUse
		if n == 1 {
			use.CacheRead = 5000 // below the turn's read
		}
		return dropAll(ask), use, nil
	}}
	res, st, key := askN(t, a, "", 20, SweepPingWait{LastTurnRead: 19000})
	h, ok := getHeld(st, key)
	if !ok || len(h.Outputs) != 10 || !res.Held {
		t.Fatalf("held = %d outputs, want only the first batch's 10", len(h.Outputs))
	}
	if SweepBatchCounters().FailedBy["prefix_cache_read_below_turn"] != 1 {
		t.Errorf("counters = %+v, want one below-turn failure", SweepBatchCounters())
	}
}

// A thread claim and ONE question slot cover the whole sweep: the batches never hold more.
func TestABatchedSweepHoldsOneQuestionSlot(t *testing.T) {
	var peak int
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		if got := SweepSlotsInUseForTest(); got > peak {
			peak = got
		}
		return dropAll(ask), okUse, nil
	}}
	askN(t, a, "", 20, SweepPingWait{})
	if peak != 1 {
		t.Fatalf("the sweep held %d question slots at its peak, want 1", peak)
	}
	if SweepSlotsInUseForTest() != 0 {
		t.Fatalf("%d slots still held after the sweep", SweepSlotsInUseForTest())
	}
}

// ONE CUT, end to end. 20 outputs are asked in two batches and merged into one held answer. The next COLD
// request rechecks it once and applies every drop in one pass: all 20 outputs are replaced by a marker
// and each expands back byte for byte. The cold apply is the one on main, unchanged.
func TestTheMergedAnswerIsAppliedAsOneCutOnTheNextColdRequest(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	a := &dynAsker{fn: func(i int, ask string) (string, components.PrefixUsage, error) { return dropAll(ask), okUse, nil }}
	e := shippedSweep(t, "")
	st := newMemStore()
	key, _ := registered(t, e, "batch-cold", a, st, applyConv(20, 0))
	dispatch, _, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered: %s", reason)
	}
	if res := dispatch(batchTestBudget, SweepPingWait{}); !res.Held || res.Proposed != 20 || a.calls != 2 {
		t.Fatalf("fixture: want one held answer of 20 from 2 asks, got %+v after %d asks", res, a.calls)
	}
	req := applyConv(20, 1)
	want := texts(req)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, coldCtx("batch-cold", st)); err != nil {
		t.Fatal(err)
	}
	if rep.Events[eventAppliedCold] != 20 {
		t.Fatalf("events = %v, want all 20 drops applied in ONE cold request", rep.Events)
	}
	for _, i := range toolIndices(req) {
		now := schema.MessageText(req.Input[i])
		if !expand.HasPlaceholder(now) {
			t.Fatalf("output %d is not dropped", i)
		}
		got, ok := expand.Resolve(st, firstMarkerID(t, now))
		if !ok || got != want[i] {
			t.Fatalf("output %d: expand is not byte-identical to the original", i)
		}
	}
	// The held answer is spent: a second cold request applies nothing new from it.
	if q := SweepDropCounters().Overall; q.Applied != 20 {
		t.Fatalf("ledger applied = %d, want 20", q.Applied)
	}
}

// A failed batch's outputs stay in FULL on the cold request, and the answered batch's drops apply.
func TestAFailedBatchKeepsItsOutputsOnTheColdRequestAndTheOtherBatchApplies(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		if n == 0 {
			return "", components.PrefixUsage{}, errors.New("boom")
		}
		return dropAll(ask), okUse, nil
	}}
	e := shippedSweep(t, "")
	st := newMemStore()
	key, _ := registered(t, e, "batch-cold-fail", a, st, applyConv(20, 0))
	dispatch, _, _, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatal("no question offered")
	}
	if res := dispatch(batchTestBudget, SweepPingWait{}); !res.Held || res.Proposed != 10 {
		t.Fatalf("fixture: want a held answer of the 10 that answered, got %+v", res)
	}
	req := applyConv(20, 1)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, coldCtx("batch-cold-fail", st)); err != nil {
		t.Fatal(err)
	}
	if rep.Events[eventAppliedCold] != 10 {
		t.Fatalf("events = %v, want exactly the 10 drops of the batch that answered", rep.Events)
	}
	dropped, full := 0, 0
	for _, i := range toolIndices(req) {
		if expand.HasPlaceholder(schema.MessageText(req.Input[i])) {
			dropped++
		} else {
			full++
		}
	}
	if dropped != 10 || full != 10 {
		t.Fatalf("%d outputs dropped and %d kept in full, want 10 and 10", dropped, full)
	}
}

// The answer cost is the SUM of every batch's cost, and it is carried on the held answer. Priced at known
// rates, two asks of fresh 40 + output 90 + read 19,595 cost twice one ask.
func TestTheHeldAnswerCarriesTheSumOfEveryBatchsCost(t *testing.T) {
	rates := components.TokenRates{Input: 3e-6, Output: 15e-6, CacheRead: 0.3e-6, CacheWrite: 3.75e-6}
	oneAsk := 40*rates.Input + 90*rates.Output + 19595*rates.CacheRead
	a := &dynAsker{fn: func(i int, ask string) (string, components.PrefixUsage, error) { return dropAll(ask), okUse, nil }}
	resetSweepBatchForTest()
	st := newMemStore()
	cand := candidateFor(t, "", "batch-cost", a, st, 20)
	cand.ctx.SelfRates = rates
	rep := &components.Report{Component: "extract_llm_sweep"}
	cands := cand.collect(rep)
	cand.ask(cands, rep, 0, SweepPingWait{})
	h, ok := getHeld(st, thread.Key("batch-cost", ""))
	if !ok {
		t.Fatal("nothing held")
	}
	if h.CostUSD <= 0 || h.CostUSD < 1.99*oneAsk || h.CostUSD > 2.01*oneAsk {
		t.Fatalf("held cost = %.8f, want the sum of two asks = %.8f", h.CostUSD, 2*oneAsk)
	}
}

// ---------------------------------------------------------------------------------------------------
// Round 1 (#747)

// REVIEW 2, FINDING 2. The cg.sweep.held line reported len(judged) as "offered" after a failed batch: 20
// candidates, one batch of 10 failed, and the log said offered=10.
func TestTheHeldLogReportsTheOfferedCountEvenWhenABatchFailed(t *testing.T) {
	var buf bytes.Buffer
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		if n == 0 {
			return "", components.PrefixUsage{}, errors.New("boom")
		}
		return dropAll(ask), okUse, nil
	}}
	resetSweepBatchForTest()
	st := newMemStore()
	cand := candidateFor(t, "", "batch-log", a, st, 20)
	cand.ctx.Ctx = logging.With(cand.ctx.Ctx, slog.New(logging.New(&buf, slog.LevelInfo, false)))
	rep := &components.Report{Component: "extract_llm_sweep"}
	cands := cand.collect(rep)
	res := cand.ask(cands, rep, 0, SweepPingWait{})
	if !res.Held || res.Offered != 20 {
		t.Fatalf("fixture: result = %+v, want a held answer with 20 offered", res)
	}
	var line string
	for _, ln := range strings.Split(buf.String(), "\n") {
		if strings.Contains(ln, "cg.sweep.held") {
			line = ln
		}
	}
	if line == "" {
		t.Fatalf("no cg.sweep.held line in %q", buf.String())
	}
	if !strings.Contains(line, "offered=20") || !strings.Contains(line, "judged=10") {
		t.Fatalf("held line = %q, want offered=20 (what was offered) and judged=10 (what answered)", line)
	}
}

// A panic in batch k loses batch k only: the answers already paid for are kept.
func TestAPanicInOneBatchKeepsTheOtherBatches(t *testing.T) {
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		if n == 1 {
			panic("boom in batch 2")
		}
		return dropAll(ask), okUse, nil
	}}
	res, st, key := askN(t, a, "", 30, SweepPingWait{}) // 3 batches of 10
	if a.calls != 3 {
		t.Fatalf("%d asks, want 3: a panic in batch 2 must not stop batch 3", a.calls)
	}
	h, ok := getHeld(st, key)
	if !ok || !res.Held || len(h.Outputs) != 20 {
		t.Fatalf("held = %d outputs, want the 20 of the two batches that answered", len(h.Outputs))
	}
	if SweepBatchCounters().FailedBy["batch_panic"] != 1 {
		t.Errorf("counters = %+v, want one batch_panic failure", SweepBatchCounters())
	}
}

// directBatch runs askInBatches directly, with candidates taken from a registered conversation and a real
// request, so the asker's questions name real outputs. n candidates, no pre-cap.
func directBatch(t *testing.T, cfg string, a components.PrefixAsker, n, maxBatches int, mut func(*components.Ctx)) (batchRun, *ExtractSweep) {
	t.Helper()
	resetSweepBatchForTest()
	st := newMemStore()
	cand := candidateFor(t, "max_ask_batches: 8\n", "batch-direct", a, st, n)
	e := shippedSweep(t, cfg)
	cand.e = e
	rep := &components.Report{Component: "extract_llm_sweep"}
	// Collect with a wide cap so the full list reaches askInBatches un-pre-capped.
	wide := shippedSweep(t, "max_ask_batches: 8\n")
	cand.e = wide
	cands := cand.collect(rep)
	cand.e = e
	c := *cand.ctx
	if mut != nil {
		mut(&c)
	}
	req := &bschemas.BifrostChatRequest{Input: cand.input}
	return e.askInBatches(req, &c, cands, askOpts{}, maxBatches), e
}

// askInBatches enforces max_ask_batches ITSELF: a caller that did not pre-cap cannot ignore the bound.
func TestAskInBatchesEnforcesTheOperatorsBatchLimitItself(t *testing.T) {
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) { return dropAll(ask), okUse, nil }}
	br, _ := directBatch(t, "max_ask_batches: 2\n", a, 60, 0, nil) // 60 candidates, NO pre-cap
	if a.calls != 2 {
		t.Fatalf("%d asks, want 2: max_ask_batches is the bound", a.calls)
	}
	if got := br.gates["sweep_over_ask_cap"]; got != 36 {
		t.Errorf("sweep_over_ask_cap = %d, want 36 (60 - 24)", got)
	}
	if len(br.judged) != 24 || SweepBatchCounters().OverCap != 36 {
		t.Errorf("judged %d, ledger over-cap %d, want 24 and 36", len(br.judged), SweepBatchCounters().OverCap)
	}
}

// The keeper may lower the limit below max_ask_batches; it can never raise it.
func TestACallerCanLowerTheBatchLimitButNotRaiseIt(t *testing.T) {
	for _, tc := range []struct {
		cfg                  string
		maxBatches, wantAsks int
	}{
		{"", 1, 1}, {"", 2, 2}, {"", 4, 4}, {"", 8, 4}, {"", 0, 4},
		{"max_ask_batches: 2\n", 8, 2},
	} {
		a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) { return dropAll(ask), okUse, nil }}
		directBatch(t, tc.cfg, a, 96, tc.maxBatches, nil)
		if int(a.calls) != tc.wantAsks {
			t.Errorf("cfg %q maxBatches %d: %d asks, want %d", tc.cfg, tc.maxBatches, a.calls, tc.wantAsks)
		}
	}
}

// A batch never STARTS after the sweep's context ended: k slow batches cannot run past the outer timeout.
func TestABatchNeverStartsAfterTheSweepsDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		if n == 0 {
			cancel() // the deadline passes during batch 1
		}
		return dropAll(ask), okUse, nil
	}}
	br, _ := directBatch(t, "", a, 36, 0, func(c *components.Ctx) { c.Ctx = ctx }) // 3 batches
	if a.calls != 1 {
		t.Fatalf("%d asks, want 1: batches 2 and 3 must not start after the deadline", a.calls)
	}
	if br.skipped != 2 || SweepBatchCounters().Skipped != 2 {
		t.Errorf("skipped %d (ledger %d), want 2", br.skipped, SweepBatchCounters().Skipped)
	}
}

// The planned batch count the keeper prices: ceil(offered / 12).
func TestThePlannedBatchCountIsCeilOfOfferedOverTwelve(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 1, 12: 1, 13: 2, 24: 2, 25: 3, 48: 4, 96: 8} {
		if got := plannedAskBatches(n); got != want {
			t.Errorf("%d offered plans %d batches, want %d", n, got, want)
		}
	}
	// And the registered candidate carries it on SweepPingInfo.
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "batch-info", pingAsker(), newMemStore(), deepCandidates(30))
	_, info, _, ok := SweepPingSubstitute(key)
	if !ok || info.Offered != 30 || info.Batches != 3 {
		t.Fatalf("info = %+v ok=%v, want Offered 30 and Batches 3", info, ok)
	}
}
