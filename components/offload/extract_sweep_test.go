package offload

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// verdictModel answers every adjudication with the same canned reply, and counts the calls.
type verdictModel struct {
	reply  string
	calls  int64
	prompt atomic.Value // the last prompt, for asserting what the model was shown
}

func (m *verdictModel) Complete(_ context.Context, prompt string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	m.prompt.Store(prompt)
	return m.reply, nil
}

func (m *verdictModel) lastPrompt() string {
	s, _ := m.prompt.Load().(string)
	return s
}

// newSweep builds the component through its registered constructor, so the config surface under
// test is the real one. economic_gate off by default here: the gate's break-even for a DROP is
// unmeasured (proposal open question 3), and a test that let it decide would be measuring the gate.
// The floor is above the filler outputs in sweepReq, so exactly ONE candidate reaches the model and
// a call count is an unambiguous assertion about it.
func newSweep(t *testing.T, model components.Model, extraYAML string) *ExtractSweep {
	t.Helper()
	c, err := newExtractSweep([]byte("min_tokens: 2000\neconomic_gate: false\n" + extraYAML))
	if err != nil {
		t.Fatalf("newExtractSweep: %v", err)
	}
	e := c.(*ExtractSweep)
	e.modelClient = model
	return e
}

// sweepReq puts a BIG tool output at depth (index 1), inside the cached prefix, so only a cold sweep
// can reach it. The transcript also states an obligation the refusal test quotes back.
func sweepReq() *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Find the auth timeout in src/api/users.py and fix it."),
		toolResultMsg(strings.Repeat("2024-01-01 GET /users/42 200 12ms src/api/users.py\n", 700)),
		assistantMsg("Next I will patch the timeout in src/api/users.py."),
		toolResultMsg(strings.Repeat("filler line to grow the transcript\n", 50)),
		userMsg("keep going"),
	}}
}

func sweepCtx(session string, cold bool, idleMs int64, st store.Store) *components.Ctx {
	return &components.Ctx{
		Session: session, Ctx: context.Background(),
		Store: st, CtxWindow: 1_000_000,
		// The cached boundary sits AFTER the big output, so index 1 is inside the prefix and a
		// warm turn must not touch it.
		CacheAware: true, MaxCachedIdx: 3,
		ColdCache: cold, IdleMs: idleMs,
	}
}

// The whole point of the component: on a cold turn it removes a spent output at DEPTH, leaves a
// shape descriptor plus a recoverable marker, and recovers the original byte-for-byte.
func TestSweepDropsASpentOutputAtDepthAndKeepsItRecoverable(t *testing.T) {
	model := &verdictModel{reply: `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"}]`}
	e := newSweep(t, model, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	st := store.NewMemory(store.Options{})
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, st)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// PRECONDITION: the component acted. Without it every assertion below is vacuous — a
	// component that never ran leaves the transcript in exactly the state a "correct keep" does.
	if n := atomic.LoadInt64(&model.calls); n != 1 {
		t.Fatalf("expected exactly one adjudication call, got %d (gates: %v)", n, rep.Gates)
	}
	if rep.Gates["sweep_dropped"] != 1 {
		t.Fatalf("no drop was recorded, so nothing under test ran (gates: %v)", rep.Gates)
	}
	got := schema.MessageText(req.Input[1])
	if got == original {
		t.Fatal("the output at depth was not removed")
	}
	if !strings.Contains(got, "context-guru removed a spent tool output") {
		t.Errorf("no shape descriptor left in place: %q", got)
	}
	marks := expand.ParseMarkers(got)
	if len(marks) != 1 {
		t.Fatalf("expected one resolvable marker, got %d in %q", len(marks), got)
	}
	if back, ok := expand.Resolve(st, marks[0]); !ok || back != original {
		t.Fatalf("the drop is not recoverable: ok=%v byte-identical=%v", ok, back == original)
	}
}

// A keep leaves the output VERBATIM. Not "smaller" — verbatim, because there is no rewriting on this
// path and a keep that changed a byte would be the very failure the split exists to remove.
func TestSweepKeepLeavesTheOutputVerbatim(t *testing.T) {
	model := &verdictModel{reply: `[{"i":0,"needed_by":"c","quote":"Next I will patch the timeout in src/api/users.py.","verdict":"keep"}]`}
	e := newSweep(t, model, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&model.calls) != 1 {
		t.Fatalf("the model was never asked, so a verbatim output proves nothing (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_kept"] != 1 {
		t.Fatalf("no keep was recorded (gates: %v)", rep.Gates)
	}
	if got := schema.MessageText(req.Input[1]); got != original {
		t.Fatalf("a kept output was modified:\n want %q\n  got %q", original[:80], got[:80])
	}
	if !rep.Skipped {
		t.Error("a sweep that changed nothing must report Skipped")
	}
	// And the fabrication counter must be quiet: the quote IS in the transcript.
	if rep.Gates["sweep_quote_fabricated"] != 0 {
		t.Errorf("a verbatim transcript quote was counted as fabricated (gates: %v)", rep.Gates)
	}
}

// The refusal, reaching all the way through the component: a drop naming an outstanding obligation
// leaves the output in place and raises the alertable counter.
func TestSweepRefusesADropThatNamesAnObligation(t *testing.T) {
	model := &verdictModel{reply: `[{"i":0,"needed_by":"c","quote":"Next I will patch the timeout in src/api/users.py.","verdict":"drop"}]`}
	e := newSweep(t, model, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&model.calls) != 1 {
		t.Fatalf("the model was never asked, so the refusal was never exercised (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_drop_refused_obligation"] != 1 {
		t.Fatalf("the refusal was not counted (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_dropped"] != 0 {
		t.Fatalf("the contradictory drop was PERFORMED (gates: %v)", rep.Gates)
	}
	if got := schema.MessageText(req.Input[1]); got != original {
		t.Fatal("the output was removed despite naming an outstanding obligation")
	}
}

// A warm turn makes no call and touches nothing, however large the candidate. This is the condition
// the whole component rests on: acting at depth is only free because the provider's entry is gone.
func TestSweepDoesNothingOnAWarmTurn(t *testing.T) {
	model := &verdictModel{reply: `[{"i":0,"needed_by":"none","verdict":"drop"}]`}
	e := newSweep(t, model, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", false, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&model.calls); n != 0 {
		t.Fatalf("a warm turn made %d adjudication calls", n)
	}
	if rep.Gates["not_a_cold_sweep"] == 0 {
		t.Errorf("a warm turn must say why it did nothing (gates: %v)", rep.Gates)
	}
	if got := schema.MessageText(req.Input[1]); got != original {
		t.Fatal("a warm turn modified a message inside the cached prefix")
	}
}

// min_idle_seconds may only RAISE the bar: the TTL check is the correctness condition, this is extra
// caution on top of it.
func TestSweepMinIdleRaisesTheBar(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cold   bool
		idleMs int64
		want   bool
	}{
		{"cold but only ten minutes idle", true, 600_000, false},
		{"cold and an hour idle", true, 3_600_000, true},
		{"warm, however long idle", false, 7_200_000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSweep(t, &verdictModel{}, "min_idle_seconds: 1800\n")
			if got := e.sweeping(sweepCtx("s", tc.cold, tc.idleMs, store.Nop{})); got != tc.want {
				t.Fatalf("sweeping=%v, want %v", got, tc.want)
			}
		})
	}
}

// A DROP DECIDED ON A COLD TURN MUST BE REPLAYED ON EVERY LATER TURN, warm ones included. Without
// that, the next warm turn re-sends the removed output verbatim — the saving evaporates AND the
// prefix the provider is caching stops being byte-stable, which costs more than the sweep saved.
func TestSweepReplaysItsDropOnTheNextWarmTurn(t *testing.T) {
	model := &verdictModel{reply: `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"}]`}
	e := newSweep(t, model, "")
	st := store.NewMemory(store.Options{})

	cold := sweepReq()
	original := schema.MessageText(cold.Input[1])
	rep1 := &components.Report{}
	if _, err := e.Offload(cold, rep1, sweepCtx("sess", true, 3_600_000, st)); err != nil {
		t.Fatal(err)
	}
	if rep1.Gates["sweep_dropped"] != 1 {
		t.Fatalf("the cold turn did not drop, so there is nothing to replay (gates: %v)", rep1.Gates)
	}
	coldText := schema.MessageText(cold.Input[1])

	warm := sweepReq() // the same transcript again, on a warm turn
	rep2 := &components.Report{}
	if _, err := e.Offload(warm, rep2, sweepCtx("sess", false, 0, st)); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&model.calls); n != 1 {
		t.Fatalf("the warm turn made a model call: %d total", n)
	}
	if rep2.Gates["reapplied_same_session"] != 1 {
		t.Fatalf("the warm turn did not replay the frozen drop (gates: %v)", rep2.Gates)
	}
	got := schema.MessageText(warm.Input[1])
	if got == original {
		t.Fatal("the warm turn re-sent the dropped output verbatim; the saving is gone")
	}
	if got != coldText {
		t.Fatalf("the replay is not byte-identical, so the cached prefix churns:\n cold %q\n warm %q",
			coldText, got)
	}
}

// THE BATCH IS ASSEMBLED AS A BATCH. This is the assertion `4ca1f13` says was missing: one call
// carrying one item is the per-output design refuted at 6% live-kept, so asserting a single call is not
// enough — the call must be shown MORE THAN ONE output. Twelve candidates must be one call of twelve.
func TestSweepOffersTheWholeBatchInOneCall(t *testing.T) {
	const n = 12
	model := &labelModel{verdict: "drop", needed: "none"}
	e := newSweep(t, model, "")
	req := manyCandidates(n)
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&model.calls); got != 1 {
		t.Fatalf("%d candidates took %d calls; the measured shape is ONE call over the batch", n, got)
	}
	// PRECONDITION and the point of the test: every candidate was OFFERED in that one call.
	if rep.Gates["sweep_offered"] != n {
		t.Fatalf("sweep_offered = %d, want %d — the batch was starved before assembly (gates: %v)",
			rep.Gates["sweep_offered"], n, rep.Gates)
	}
	if rep.Gates["sweep_adjudicated"] != n {
		t.Errorf("sweep_adjudicated = %d, want %d", rep.Gates["sweep_adjudicated"], n)
	}
	if rep.Gates["sweep_batch_of_one"] != 0 {
		t.Errorf("a batch of %d was recorded as a batch of one (gates: %v)", n, rep.Gates)
	}
	// And the prompt must actually show all twelve, labelled.
	p := e2ePrompt(t, model.prompt())
	for i := 0; i < n; i++ {
		if !strings.Contains(p, "=== OUTPUT "+strconv.Itoa(i)) {
			t.Errorf("output %d was not offered in the prompt", i)
		}
	}
}

// A single-item batch is the refuted design wearing the new name, so it is COUNTED. It is legitimate
// — a transcript can have one candidate above the floor — but a workload where it fires routinely has
// an upstream filter starving the batch, which is the failure that cost three iterations.
func TestSweepCountsABatchOfOne(t *testing.T) {
	model := &labelModel{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	e := newSweep(t, model, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReq(), rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] != 1 {
		t.Fatalf("the component did not adjudicate, so nothing under test ran (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_batch_of_one"] != 1 {
		t.Fatalf("a batch of one was not counted; a starved batch would be invisible (gates: %v)",
			rep.Gates)
	}
}

// Past the item cap the sweep uses more batches, and max_calls bounds how many. Neither bound may be
// silent: a truncated sweep is a bounded-coverage decision, and "we judged everything" must not read
// the same as "we judged the first twelve".
func TestSweepBatchesPastTheItemCapAndCountsWhatItTruncated(t *testing.T) {
	const n = 30 // three batches of 12/12/6
	model := &labelModel{verdict: "keep", needed: "none"}
	e := newSweep(t, model, "max_calls: 2\n")
	rep := &components.Report{}
	if _, err := e.Offload(manyCandidates(n), rep,
		sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&model.calls); got != 2 {
		t.Fatalf("max_calls: 2 allowed %d calls (gates: %v)", got, rep.Gates)
	}
	// Two batches of twelve were judged; the remaining six were truncated and counted.
	if rep.Gates["sweep_adjudicated"] != 24 {
		t.Errorf("sweep_adjudicated = %d, want 24 (two full batches)", rep.Gates["sweep_adjudicated"])
	}
	if rep.Gates["sweep_batch_truncated"] != n-24 {
		t.Errorf("sweep_batch_truncated = %d, want %d — bounded coverage must be visible",
			rep.Gates["sweep_batch_truncated"], n-24)
	}
	// No batch may exceed the measured quote-fidelity ceiling.
	if got := model.maxItems(); got > extract.MaxAdjudicationItems {
		t.Errorf("a batch offered %d items, above the measured ceiling of %d",
			got, extract.MaxAdjudicationItems)
	}
}

// A well-formed EMPTY array is the model saying "keep everything", which the contract invites. It must
// not be filed as a failure — that conflation made "the model declined to act" and "the model was
// never successfully asked" the same number for three iterations.
func TestSweepCountsADeliberateKeepAllSeparatelyFromAFailure(t *testing.T) {
	keepAll := &verdictModel{reply: "[]"}
	e := newSweep(t, keepAll, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] == 0 {
		t.Fatalf("no call was made, so nothing under test ran (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_kept_whole_batch"] != 1 {
		t.Fatalf("a deliberate keep-all was not counted as one (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_unparseable"] != 0 || rep.Gates["sweep_reply_truncated"] != 0 {
		t.Errorf("a deliberate keep-all was filed as a failure (gates: %v)", rep.Gates)
	}
	if schema.MessageText(req.Input[1]) != original {
		t.Error("keep-all removed something")
	}

	// A TRUNCATED reply, by contrast, is a failure — and a different one from malformed junk.
	cut := &verdictModel{reply: `[{"i":0,"needed_by":"none","quote":"partial`}
	e2 := newSweep(t, cut, "")
	rep2 := &components.Report{}
	if _, err := e2.Offload(sweepReq(), rep2, sweepCtx("s2", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep2.Gates["sweep_reply_truncated"] != 1 {
		t.Fatalf("a cut-off array was not counted as truncation (gates: %v)", rep2.Gates)
	}
	if rep2.Gates["sweep_unparseable"] != 0 {
		t.Errorf("truncation was filed as a format failure; the two need opposite fixes (gates: %v)",
			rep2.Gates)
	}
	if rep2.Gates["sweep_kept_whole_batch"] != 0 {
		t.Errorf("a truncated reply was filed as a deliberate keep-all (gates: %v)", rep2.Gates)
	}
}

// A verdict naming a label the batch never offered must never be acted on: the label is how a decision
// is keyed to an output, so acting on a wrong one removes the wrong content.
func TestSweepIgnoresAVerdictForAnUnofferedLabel(t *testing.T) {
	model := &verdictModel{reply: `[{"i":99,"needed_by":"none","quote":"","verdict":"drop"}]`}
	e := newSweep(t, model, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] == 0 {
		t.Fatalf("no call was made, so nothing under test ran (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_verdict_unknown_label"] != 1 {
		t.Fatalf("a verdict for an unoffered label was not counted (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_dropped"] != 0 {
		t.Fatalf("a verdict for an unoffered label was ACTED ON (gates: %v)", rep.Gates)
	}
	if schema.MessageText(req.Input[1]) != original {
		t.Fatal("content was removed on a verdict that named no offered output")
	}
	// The offered output got no answer, and that must not look like a keep.
	if rep.Gates["sweep_verdict_missing"] != 1 {
		t.Errorf("an unjudged output was not counted (gates: %v)", rep.Gates)
	}
}

// The reply budget must be RAISED for a batched reply. `659e7a6`: 24 of 34 replies were cut off at the
// client's default, which parses as nothing and is indistinguishable from a model declining to act.
func TestSweepRaisesTheReplyBudget(t *testing.T) {
	model := &budgetModel{verdictModel: verdictModel{reply: "[]"}}
	e := newSweep(t, model, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReq(), rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] == 0 {
		t.Fatalf("no call was made, so the budget was never requested (gates: %v)", rep.Gates)
	}
	if got := model.granted(); got != extract.AdjudicationReplyTokens {
		t.Fatalf("the sweep asked for a %d-token reply budget, want %d", got,
			extract.AdjudicationReplyTokens)
	}
	if rep.Gates["sweep_reply_budget_not_raised"] != 0 {
		t.Errorf("a Budgeter client was recorded as unable to raise its budget (gates: %v)", rep.Gates)
	}
}

// A client that cannot raise its budget still works, and says so — otherwise the truncation regime
// returns silently on whatever client shape that is.
func TestSweepCountsAClientThatCannotRaiseItsBudget(t *testing.T) {
	model := &verdictModel{reply: "[]"} // no WithMaxTokens
	e := newSweep(t, model, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReq(), rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] == 0 {
		t.Fatalf("no call was made (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_reply_budget_not_raised"] != 1 {
		t.Fatalf("a client without a budget knob was not counted (gates: %v)", rep.Gates)
	}
}

// EVERY CANDIDATE MUST BE ACCOUNTED FOR, and the accounting must survive concurrency.
//
// This is not a style test. The gates were originally raised from inside the per-call goroutines, and
// components.Report's Gates map carries no lock — a Report is copied by value across this codebase and
// cannot hold one — so Go's map implementation turned it into `fatal error: concurrent map writes` and
// killed the test binary rather than producing a wrong count. Batching did not remove the hazard: the
// batches still fan out, so five concurrent batch calls reach it just as five per-output calls did.
//
// Under `-race` the reversion is caught deterministically (a DATA RACE on components.Report.Gate).
// Without it the crash is timing-dependent, so the count assertions below are the part that holds in
// the plain suite: a lost gate is the quiet form of the same defect.
func TestSweepAccountsForEveryCandidateUnderConcurrency(t *testing.T) {
	const n = 60 // five batches of twelve
	model := &labelModel{verdict: "drop", needed: "none"}
	e := newSweep(t, model, "max_calls: -1\n")
	rep := &components.Report{}
	if _, err := e.Offload(manyCandidates(n), rep,
		sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: more than llmConcurrency batches really went to the model, so several were in
	// flight at once. With the cap binding this would be fewer and the test would prove nothing.
	if got := atomic.LoadInt64(&model.calls); got != n/extract.MaxAdjudicationItems {
		t.Fatalf("expected %d concurrent batch calls, got %d (gates: %v)",
			n/extract.MaxAdjudicationItems, got, rep.Gates)
	}
	if rep.Gates["sweep_offered"] != n {
		t.Errorf("sweep_offered = %d, want %d — a gate was lost", rep.Gates["sweep_offered"], n)
	}
	if rep.Gates["sweep_adjudicated"] != n {
		t.Errorf("sweep_adjudicated = %d, want %d — a gate was lost", rep.Gates["sweep_adjudicated"], n)
	}
	if rep.Gates["sweep_dropped"] != n {
		t.Errorf("sweep_dropped = %d, want %d — a gate was lost", rep.Gates["sweep_dropped"], n)
	}
}

// The compaction knobs must be REFUSED with a reason, not silently ignored. An operator migrating a
// cold_cache block by hand has no other way to learn that `rewrite: false` now means nothing.
func TestSweepRejectsCompactionOnlyKeys(t *testing.T) {
	for _, tc := range []struct{ key, yaml string }{
		{"strategy", "strategy: code\n"},
		{"rewrite", "rewrite: false\n"},
		{"aggressiveness", "aggressiveness: high\n"},
		{"max_chars", "max_chars: 8000\n"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_, err := newExtractSweep([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("%s was accepted; the key would silently do nothing", tc.key)
			}
			// The reason must be NAMED. A generic yaml "field not found" is what this test exists
			// to rule out: it says the key is unknown, not why it cannot apply here.
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name the key: %v", err)
			}
			if !strings.Contains(err.Error(), "does not apply here") {
				t.Errorf("error does not say why the key cannot apply: %v", err)
			}
		})
	}
}

// THE ECONOMIC GATE MUST NOT THIN THE BATCH. This is the failure `4ca1f13` traced: the merged arm's
// real defect was an upstream PER-CANDIDATE filter (prefix_still_referenced removed 149,681
// candidates), which left about one candidate per request and silently ran the per-output design
// refuted at 6% live-kept while reporting itself as bulk. A per-candidate economic gate is the same
// mechanism, so the gate is evaluated ONCE for the batch.
//
// Run with the gate ON, which every other test in this file turns off — without this, the whole gated
// path is untested and a reversion to per-candidate gating passes the suite.
func TestSweepEconomicGateDoesNotThinTheBatch(t *testing.T) {
	const n = 12
	model := &labelModel{verdict: "drop", needed: "none"}
	c, err := newExtractSweep([]byte("min_tokens: 2000\n")) // gate left at its default: ON
	if err != nil {
		t.Fatal(err)
	}
	e := c.(*ExtractSweep)
	e.modelClient = model
	if !e.gate {
		t.Fatal("this test is meaningless with the gate off")
	}
	rep := &components.Report{}
	if _, err := e.Offload(manyCandidates(n), rep,
		sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: the gate ALLOWED this batch. If it suppressed, the assertions below would pass
	// vacuously — a batch of zero is not a thinned batch, it is no batch.
	if atomic.LoadInt64(&model.calls) == 0 {
		t.Fatalf("the gate suppressed a batch of %d cold-turn candidates, so thinning was never "+
			"exercised (gates: %v)", n, rep.Gates)
	}
	if rep.Gates["sweep_offered"] != n {
		t.Fatalf("sweep_offered = %d, want %d: the gate thinned the batch one candidate at a time, "+
			"which is how a bulk arm silently becomes the refuted per-output shape (gates: %v)",
			rep.Gates["sweep_offered"], n, rep.Gates)
	}
	if rep.Gates["sweep_batch_of_one"] != 0 {
		t.Errorf("the gate reduced the batch to one (gates: %v)", rep.Gates)
	}
	// And it must be an all-or-nothing decision: a partial refusal is per-candidate gating by
	// another name.
	if g := rep.Gates["economic_gate"]; g != 0 && g != n {
		t.Errorf("economic_gate refused %d of %d candidates; the decision must cover the whole batch",
			g, n)
	}
}

// The other direction, so the gate is not simply inert: a batch whose total cannot pay for one call is
// refused as a whole, and every candidate it would have covered is counted so the refusal is
// comparable with the other gates rather than reading as one.
func TestSweepEconomicGateRefusesABatchThatCannotPay(t *testing.T) {
	model := &labelModel{verdict: "drop", needed: "none"}
	// A floor low enough to admit tiny candidates, so the batch total is far below break-even.
	c, err := newExtractSweep([]byte("min_tokens: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := c.(*ExtractSweep)
	e.modelClient = model
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("tiny task")}}
	const n = 3
	for i := 0; i < n; i++ {
		req.Input = append(req.Input, toolResultMsg("small output "+strconv.Itoa(i)+"\n"))
	}
	// Exhaust the exploration budget first: exploration deliberately allows a bounded number of
	// unprofitable calls so a pessimistic prior cannot justify itself forever, and it would otherwise
	// mask the gate's arithmetic here.
	for i := 0; i < 8; i++ {
		e.ratios.observe(0, 4000)
	}
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: the candidates cleared every earlier gate and reached the economic one. If they
	// were stopped by the floor or the depth gate, this proves nothing about the economics.
	if rep.Gates["below_output_floor"] != 0 {
		t.Fatalf("candidates were stopped by the floor, not the gate (gates: %v)", rep.Gates)
	}
	if rep.Gates["economic_gate"] != n {
		t.Fatalf("economic_gate = %d, want %d: an unprofitable batch must be refused as a whole and "+
			"counted per candidate it would have covered (gates: %v)",
			rep.Gates["economic_gate"], n, rep.Gates)
	}
	if got := atomic.LoadInt64(&model.calls); got != 0 {
		t.Errorf("a refused batch still made %d calls", got)
	}
}

// THE COUNTER CONTRACT, component end. These six names are what an operator's dashboard query and
// alert rule are written against, so a rename breaks monitoring silently rather than loudly. The
// other end is pinned in proxy/sweep_counters_test.go, which asserts the same literal strings survive
// to /stats and to the Prometheus gate series.
//
// Two of them are the ones that must be ALERTABLE, and each names a distinct misbehaviour:
// sweep_drop_refused_obligation means the model tried to remove an output it had just said was still
// needed, and sweep_quote_fabricated means it is inventing evidence — the only such signal left on
// this design, because nothing else it returns is content.
func TestSweepRaisesTheContractedCounterNames(t *testing.T) {
	const obligation = "Next I will patch the timeout in src/api/users.py."
	for _, tc := range []struct {
		name, reply string
		want        []string
	}{
		{"a spent output", `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"}]`,
			[]string{"sweep_adjudicated", "sweep_dropped"}},
		{"an output still needed", `[{"i":0,"needed_by":"a","quote":"` + obligation + `","verdict":"keep"}]`,
			[]string{"sweep_adjudicated", "sweep_kept"}},
		{"a drop contradicting an obligation", `[{"i":0,"needed_by":"a","quote":"` + obligation + `","verdict":"drop"}]`,
			[]string{"sweep_adjudicated", "sweep_drop_refused_obligation"}},
		{"an invented obligation", `[{"i":0,"needed_by":"a","quote":"rewrite the parser in Rust","verdict":"keep"}]`,
			[]string{"sweep_quote_fabricated"}},
		{"an unanswered criterion", `[{"i":0,"verdict":"drop"}]`,
			[]string{"sweep_criterion_missing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := &verdictModel{reply: tc.reply}
			e := newSweep(t, model, "")
			rep := &components.Report{}
			if _, err := e.Offload(sweepReq(), rep,
				sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
				t.Fatal(err)
			}
			// PRECONDITION: an adjudication happened. A component that never called the model
			// raises no counters at all, and every assertion below would then fail for the wrong
			// reason — or, for a subset check, pass while proving nothing.
			if rep.Gates["sweep_adjudicated"] != 1 {
				t.Fatalf("no adjudication was counted, so no counter was exercised: %v", rep.Gates)
			}
			for _, want := range tc.want {
				if rep.Gates[want] == 0 {
					t.Errorf("the component did not raise %q; got %v", want, rep.Gates)
				}
			}
		})
	}
}

// The prompt the component actually sends must be the adjudication contract — not a compaction
// prompt, and never one inviting the model to return content.
func TestSweepSendsTheAdjudicationContract(t *testing.T) {
	model := &verdictModel{reply: `[{"i":0,"needed_by":"none","verdict":"drop"}]`}
	e := newSweep(t, model, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReq(), rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	p := e2ePrompt(t, model.lastPrompt())
	for _, want := range []string{"keep|drop", `"needed_by"`, "SPENT only if"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt does not carry %q", want)
		}
	}
	for _, banned := range []string{"Starlark", "SUMMARY", "return the JSON"} {
		if strings.Contains(p, banned) {
			t.Errorf("the prompt is a compaction prompt: mentions %q", banned)
		}
	}
}

func e2ePrompt(t *testing.T, p string) string {
	t.Helper()
	if p == "" {
		t.Fatal("no prompt was sent, so there is nothing to assert about it")
	}
	return p
}

// manyCandidates builds a transcript of n tool outputs, each distinct and each above the sweep's
// floor, so batch assembly and the caps are exercised on real candidate counts.
func manyCandidates(n int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("do the thing")}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input,
			toolResultMsg(strings.Repeat("candidate "+strconv.Itoa(i)+" distinct line\n", 900)))
	}
	return req
}

// labelModel answers a whole batch: it reads the labels out of the prompt and returns one verdict per
// label. Without it a batch test could only ever exercise the first candidate, which is precisely the
// blind spot that let a batch-of-one arm pass for bulk.
type labelModel struct {
	verdict, needed, quote string
	calls                  int64
	mu                     sync.Mutex
	seenMax                int
	last                   string
}

var labelRe = regexp.MustCompile(`=== OUTPUT (\d+) `)

func (m *labelModel) Complete(_ context.Context, prompt string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	labels := labelRe.FindAllStringSubmatch(prompt, -1)
	m.mu.Lock()
	m.last = prompt
	if len(labels) > m.seenMax {
		m.seenMax = len(labels)
	}
	m.mu.Unlock()
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, `{"i":`+l[1]+`,"needed_by":"`+m.needed+
			`","quote":"`+m.quote+`","verdict":"`+m.verdict+`"}`)
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

func (m *labelModel) maxItems() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seenMax
}

func (m *labelModel) prompt() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

// budgetModel records the reply budget the caller asked for, so the raise is observable.
type budgetModel struct {
	verdictModel
	got atomic.Int64
}

func (m *budgetModel) WithMaxTokens(n int) components.Model {
	m.got.Store(int64(n))
	return m
}

func (m *budgetModel) granted() int { return int(m.got.Load()) }
