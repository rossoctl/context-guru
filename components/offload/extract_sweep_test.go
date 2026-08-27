package offload

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
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
	model := &verdictModel{reply: `{"needed_by":"none","quote":"","verdict":"drop"}`}
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
	model := &verdictModel{reply: `{"needed_by":"c","quote":"Next I will patch the timeout in src/api/users.py.","verdict":"keep"}`}
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
	model := &verdictModel{reply: `{"needed_by":"c","quote":"Next I will patch the timeout in src/api/users.py.","verdict":"drop"}`}
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
	model := &verdictModel{reply: `{"needed_by":"none","verdict":"drop"}`}
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
	model := &verdictModel{reply: `{"needed_by":"none","quote":"","verdict":"drop"}`}
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

// The cap is the sweep's ONLY brake, so it must bind. Unbounded was measured at 27 calls, $0.229 and
// 76.6 s added to a turn whose upstream took 33.5 s.
func TestSweepCapBinds(t *testing.T) {
	model := &verdictModel{reply: `{"needed_by":"none","quote":"","verdict":"drop"}`}
	e := newSweep(t, model, "max_calls: 2\n")
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("do the thing"),
	}}
	for i := 0; i < 5; i++ {
		req.Input = append(req.Input,
			toolResultMsg(strings.Repeat("distinct line "+string(rune('a'+i))+"\n", 900)))
	}
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&model.calls); n != 2 {
		t.Fatalf("max_calls: 2 allowed %d calls (gates: %v)", n, rep.Gates)
	}
	if rep.Gates["over_sweep_cap"] != 3 {
		t.Errorf("the three refused candidates were not counted (gates: %v)", rep.Gates)
	}
}

// EVERY CANDIDATE MUST BE ACCOUNTED FOR, and the accounting must survive concurrency.
//
// This is not a style test. The gates were originally raised from inside the per-call goroutines,
// and components.Report's Gates map carries no lock — a Report is copied by value across this
// codebase and cannot hold one — so Go's map implementation turned it into
// `fatal error: concurrent map writes` and killed the test binary rather than producing a wrong
// count. Sixteen candidates with the cap lifted puts more than llmConcurrency calls in flight, which
// is what it takes to reach it.
//
// Under `-race` the reversion is caught deterministically (a DATA RACE on components.Report.Gate).
// Without it the crash is timing-dependent, so the count assertions below are the part that holds in
// the plain suite: a lost gate is the quiet form of the same defect.
func TestSweepAccountsForEveryCandidateUnderConcurrency(t *testing.T) {
	const n = 16
	model := &verdictModel{reply: `{"needed_by":"none","quote":"","verdict":"drop"}`}
	e := newSweep(t, model, "max_calls: -1\n")
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("do the thing")}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input,
			toolResultMsg(strings.Repeat("candidate "+string(rune('a'+i))+" line\n", 900)))
	}
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: all sixteen really went to the model, so more than llmConcurrency were in
	// flight at once. With the cap binding this would be 4 and the test would prove nothing.
	if got := atomic.LoadInt64(&model.calls); got != n {
		t.Fatalf("expected %d concurrent adjudications, got %d (gates: %v)", n, got, rep.Gates)
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

// The prompt the component actually sends must be the adjudication contract — not a compaction
// prompt, and never one inviting the model to return content.
func TestSweepSendsTheAdjudicationContract(t *testing.T) {
	model := &verdictModel{reply: `{"needed_by":"none","verdict":"drop"}`}
	e := newSweep(t, model, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReq(), rep, sweepCtx("s", true, 3_600_000, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	p := e2ePrompt(t, model)
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

func e2ePrompt(t *testing.T, m *verdictModel) string {
	t.Helper()
	p := m.lastPrompt()
	if p == "" {
		t.Fatal("no prompt was sent, so there is nothing to assert about it")
	}
	return p
}
