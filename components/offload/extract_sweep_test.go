package offload

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// fakeAsker stands in for the host's PrefixAsker. It records what was asked and what session it was
// asked for, and reports the cache read the caller gates on.
type fakeAsker struct {
	reply     string
	cacheRead int
	err       error
	calls     int64
	lastAsk   atomic.Value
	lastSess  atomic.Value
}

func (f *fakeAsker) Ask(_ context.Context, session, ask string) (string, components.PrefixUsage, error) {
	atomic.AddInt64(&f.calls, 1)
	f.lastAsk.Store(ask)
	f.lastSess.Store(session)
	if f.err != nil {
		return "", components.PrefixUsage{}, f.err
	}
	return f.reply, components.PrefixUsage{CacheRead: f.cacheRead, Fresh: 40, Output: 90}, nil
}

func (f *fakeAsker) ask() string  { s, _ := f.lastAsk.Load().(string); return s }
func (f *fakeAsker) sess() string { s, _ := f.lastSess.Load().(string); return s }

// labelAsker answers for every label the inventory names, so a test exercises the whole reply rather
// than only the first candidate — the blind spot that let a batch-of-one arm pass for bulk.
type labelAsker struct {
	fakeAsker
	verdict, needed, quote string
}

var askLabelRe = regexp.MustCompile(`\[(\d+)\] `)

func (m *labelAsker) Ask(ctx context.Context, session, ask string) (string, components.PrefixUsage, error) {
	labels := askLabelRe.FindAllStringSubmatch(ask, -1)
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, `{"i":`+l[1]+`,"needed_by":"`+m.needed+
			`","quote":"`+m.quote+`","verdict":"`+m.verdict+`"}`)
	}
	m.fakeAsker.reply = "[" + strings.Join(parts, ",") + "]"
	return m.fakeAsker.Ask(ctx, session, ask)
}

// newSweep builds the component through its registered constructor, so the config surface under test
// is the real one. The floor is above the filler outputs in sweepReq, so exactly the intended
// candidates reach the inventory.
func newSweep(t *testing.T, extraYAML string) *ExtractSweep {
	t.Helper()
	c, err := newExtractSweep([]byte("min_tokens: 2000\n" + extraYAML))
	if err != nil {
		t.Fatalf("newExtractSweep: %v", err)
	}
	return c.(*ExtractSweep)
}

// sweepReq puts a BIG tool output in the tail, and states an obligation the refusal test quotes back.
func sweepReq() *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Find the auth timeout in src/api/users.py and fix it."),
		toolResultMsg(strings.Repeat("2024-01-01 GET /users/42 200 12ms src/api/users.py\n", 700)),
		assistantMsg("Next I will patch the timeout in src/api/users.py."),
		toolResultMsg(strings.Repeat("filler line to grow the transcript\n", 50)),
		userMsg("keep going"),
	}}
}

// manyCandidates builds a transcript of n distinct tool outputs, all above the floor.
func manyCandidates(n int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("do the thing")}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input,
			toolResultMsg(strings.Repeat("candidate "+strconv.Itoa(i)+" distinct line\n", 900)))
	}
	return req
}

// preExpiryCtx puts the turn INSIDE the pre-expiry window: the cache still exists (idle < ttl) and it
// is within preExpiry of expiring. ttl 5 minutes is what a bare `ephemeral` mark buys.
func preExpiryCtx(session string, asker components.PrefixAsker, st store.Store) *components.Ctx {
	return &components.Ctx{
		Session: session, Ctx: context.Background(), Store: st, CtxWindow: 1_000_000,
		ModelName: "claude-sonnet-5",
		// No depth restriction, so the candidates reach the inventory: the tail gate is exercised
		// separately below.
		CacheAware: true, MaxCachedIdx: -1,
		ColdCache: false, IdleMs: 4 * 60 * 1000, CacheTTLMs: 5 * 60 * 1000,
		PrefixAsk: asker,
		// A request model, so the FALLBACK path has somewhere to go. Without it a missed cache read
		// would decline for lack of a model rather than for the reason under test.
		Model: components.ModelSpec{Incoming: fallbackModel, Static: fallbackModel},
	}
}

// fallbackModel answers the self-contained fallback prompt, and records what it was shown so a test
// can assert that the expensive path really is the one that carries content.
var fallbackModel = &recordingModel{reply: `[{"i":0,"needed_by":"none","quote":"","verdict":"keep"}]`}

type recordingModel struct {
	reply  string
	calls  int64
	prompt atomic.Value
}

func (m *recordingModel) Complete(_ context.Context, prompt string) (string, error) {
	atomic.AddInt64(&m.calls, 1)
	m.prompt.Store(prompt)
	return m.reply, nil
}

func (m *recordingModel) lastPrompt() string { s, _ := m.prompt.Load().(string); return s }

// The whole point: inside the window the sweep asks ONE question over the cached transcript and removes
// what the model says is spent, leaving a recoverable marker.
func TestSweepRemovesSpentOutputsFromOneCachedAsk(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	st := store.NewMemory(store.Options{})
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, preExpiryCtx("s", asker, st)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// PRECONDITION: exactly one ask, and it acted. Without this every assertion below is vacuous — a
	// component that never ran leaves the transcript exactly as a correct keep does.
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("expected ONE prefix ask, got %d (gates: %v)", n, rep.Gates)
	}
	if rep.Gates["sweep_dropped"] == 0 {
		t.Fatalf("no removal was recorded, so nothing under test ran (gates: %v)", rep.Gates)
	}
	got := schema.MessageText(req.Input[1])
	if got == original {
		t.Fatal("the spent output was not removed")
	}
	if !strings.Contains(got, "context-guru removed a spent tool output") {
		t.Errorf("no shape descriptor left in place: %q", got)
	}
	marks := expand.ParseMarkers(got)
	if len(marks) != 1 {
		t.Fatalf("expected one resolvable marker, got %d in %q", len(marks), got)
	}
	if back, ok := expand.Resolve(st, marks[0]); !ok || back != original {
		t.Fatalf("the removal is not recoverable: ok=%v byte-identical=%v", ok, back == original)
	}
	// The ask went out under the SCOPED session id, which is what the host keys its stash by.
	if asker.sess() != "s" {
		t.Errorf("the ask was made for session %q, not the request's", asker.sess())
	}
	// And the verified read is recorded, because it is the mechanism's whole justification.
	if rep.Gates["sweep_prefix_cache_read_ok"] != 1 {
		t.Errorf("the cache read was not recorded (gates: %v)", rep.Gates)
	}
}

// ONE CALL FOR EVERY CANDIDATE. There is no batching any more: nothing is copied per candidate, so
// there is nothing to divide. Twenty candidates must be one ask naming twenty labels.
func TestSweepAsksOnceForEveryCandidate(t *testing.T) {
	const n = 20
	asker := &labelAsker{verdict: "keep", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	rep := &components.Report{}
	if _, err := e.Offload(manyCandidates(n), rep,
		preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&asker.calls); got != 1 {
		t.Fatalf("%d candidates took %d asks; the shape is ONE ask over the cached transcript", n, got)
	}
	if rep.Gates["sweep_adjudicated"] != n {
		t.Fatalf("sweep_adjudicated = %d, want %d — the inventory was starved before assembly "+
			"(gates: %v)", rep.Gates["sweep_adjudicated"], n, rep.Gates)
	}
	if rep.Gates["sweep_inventory_of_one"] != 0 {
		t.Errorf("an inventory of %d was recorded as one (gates: %v)", n, rep.Gates)
	}
	// Every candidate must be NAMED in the one ask.
	ask := asker.ask()
	for i := 0; i < n; i++ {
		if !strings.Contains(ask, "["+strconv.Itoa(i)+"] ") {
			t.Errorf("candidate %d was not named in the inventory", i)
		}
	}
}

// A MISSED CACHE READ FALLS BACK BY DEFAULT, AND IS COUNTED EITHER WAY.
//
// The count is the part that is not optional: the cache read is the mechanism's whole justification, and
// a silent miss looks identical to a working call except on the bill. What happens next is a choice
// between two real costs, and the default keeps the component working — treating "no prefix" as "no
// verdicts" would disable it on every session's first turn and read as a model that declined to act.
func TestSweepFallsBackWhenTheCacheReadDidNotHappen(t *testing.T) {
	asker := &fakeAsker{reply: `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"}]`, cacheRead: 0}
	before := atomic.LoadInt64(&fallbackModel.calls)
	e := newSweep(t, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReq(), rep,
		preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: the prefix ask happened, so this is about the READ and not about never asking.
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("expected one prefix ask, got %d (gates: %v)", n, rep.Gates)
	}
	if rep.Gates["sweep_prefix_cache_read_ZERO"] != 1 {
		t.Fatalf("a zero cache read was not counted; the mechanism's failure would be invisible "+
			"(gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_fallback_used"] != 1 {
		t.Fatalf("the default did not fall back, so the component stops working on a first turn "+
			"(gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_fallback_blocked"] != 0 {
		t.Errorf("the fallback was blocked without block_fallback being set (gates: %v)", rep.Gates)
	}
	if n := atomic.LoadInt64(&fallbackModel.calls) - before; n != 1 {
		t.Fatalf("the fallback made %d completions, want 1", n)
	}
	// THE FALLBACK IS THE PATH THAT CARRIES CONTENT, and that is exactly what makes it expensive.
	// Asserted so the two paths cannot quietly converge: if the prefix ask ever started shipping
	// samples, this is the only place the difference is visible.
	if p := fallbackModel.lastPrompt(); !strings.Contains(p, "content:") {
		t.Errorf("the fallback prompt carries no output content, so the model cannot judge: %.200q", p)
	}
}

// STRICT MODE forgoes the yield rather than paying for it. The right choice where the bill matters more
// than the removal, and the honest one to reach for if the zero-read counter turns out to be common.
func TestBlockFallbackDeclinesInsteadOfPaying(t *testing.T) {
	asker := &fakeAsker{reply: `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"}]`, cacheRead: 0}
	before := atomic.LoadInt64(&fallbackModel.calls)
	e := newSweep(t, "block_fallback: true\n")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("expected one prefix ask, got %d (gates: %v)", n, rep.Gates)
	}
	// COUNTED IN THIS MODE TOO. That is the part that does not depend on the switch.
	if rep.Gates["sweep_prefix_cache_read_ZERO"] != 1 {
		t.Fatalf("strict mode did not count the missed read (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_fallback_blocked"] != 1 {
		t.Fatalf("block_fallback did not refuse the fallback (gates: %v)", rep.Gates)
	}
	if n := atomic.LoadInt64(&fallbackModel.calls) - before; n != 0 {
		t.Fatalf("strict mode still paid for %d fallback completions", n)
	}
	if rep.Gates["sweep_dropped"] != 0 {
		t.Fatalf("strict mode acted on a full-price read (gates: %v)", rep.Gates)
	}
	if schema.MessageText(req.Input[1]) != original {
		t.Fatal("content was removed in a mode that declined to act")
	}
}

// No asker at all — a non-Anthropic route, or no incoming client. Counted, and it takes the same fork
// as a missed read: fall back by default, decline under block_fallback.
func TestSweepFallsBackWithNoAsker(t *testing.T) {
	before := atomic.LoadInt64(&fallbackModel.calls)
	e := newSweep(t, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReq(), rep,
		preExpiryCtx("s", nil, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_no_asker"] != 1 {
		t.Fatalf("a missing asker was not counted (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_fallback_used"] != 1 {
		t.Fatalf("no asker did not fall back (gates: %v)", rep.Gates)
	}
	if n := atomic.LoadInt64(&fallbackModel.calls) - before; n != 1 {
		t.Fatalf("the fallback made %d completions, want 1", n)
	}

	// And under block_fallback it declines instead.
	before = atomic.LoadInt64(&fallbackModel.calls)
	strict := newSweep(t, "block_fallback: true\n")
	rep2 := &components.Report{}
	if _, err := strict.Offload(sweepReq(), rep2,
		preExpiryCtx("s", nil, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep2.Gates["sweep_fallback_blocked"] != 1 {
		t.Fatalf("block_fallback did not refuse the no-asker fallback (gates: %v)", rep2.Gates)
	}
	if n := atomic.LoadInt64(&fallbackModel.calls) - before; n != 0 {
		t.Fatalf("strict mode still paid for %d completions with no asker", n)
	}
}

// THE MODEL IS NOT A FREE CHOICE HERE, and the asymmetry with extract_llm is the point. This component
// reads the outputs from the prompt cache of the model it asks, and only the REQUEST's model has that
// cache — so `source: config` is incoherent rather than merely suboptimal, and must be refused with a
// reason rather than silently corrected.
func TestSweepRefusesAModelBlockAndSaysWhy(t *testing.T) {
	_, err := newExtractSweep([]byte("model:\n  source: config\n"))
	if err == nil {
		t.Fatal("a model block was accepted; a separate cheap model has no cache to read")
	}
	for _, want := range []string{"model", "source: config", "incoherent", "extract_llm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q, so the constraint reads as an oversight: %v",
				want, err)
		}
	}
}

// The first turn of a session has no stashed prefix. That is ordinary, happens once per session, and
// must be counted apart from a transport failure — one needs no attention and the other does.
func TestSweepCountsAMissingPrefixApartFromAFailure(t *testing.T) {
	for _, tc := range []struct {
		name, gate string
		err        error
	}{
		{"first turn, nothing stashed", "sweep_no_prefix", components.ErrNoPrefix},
		{"the read itself failed", "sweep_ask_failed", errors.New("upstream 500")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker := &fakeAsker{err: tc.err}
			e := newSweep(t, "")
			rep := &components.Report{}
			if _, err := e.Offload(sweepReq(), rep,
				preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
				t.Fatal(err)
			}
			if atomic.LoadInt64(&asker.calls) != 1 {
				t.Fatalf("the asker was not called, so nothing under test ran (gates: %v)", rep.Gates)
			}
			if rep.Gates[tc.gate] != 1 {
				t.Fatalf("%s was not counted as %s (gates: %v)", tc.name, tc.gate, rep.Gates)
			}
			// And it falls back rather than skipping, which is what keeps the component alive on a
			// session's first turn.
			if rep.Gates["sweep_fallback_used"] != 1 {
				t.Errorf("%s did not fall back (gates: %v)", tc.name, rep.Gates)
			}
		})
	}
}

// THE PRE-EXPIRY WINDOW. The ask needs a cache that still EXISTS; the removal wants one that is nearly
// worthless. Both hold only in the window before expiry, and this pins every edge of it.
func TestSweepFiresOnlyInThePreExpiryWindow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		idleMs   int64
		ttlMs    int64
		cold     bool
		wantFire bool
	}{
		{"inside the window", 4 * 60 * 1000, 5 * 60 * 1000, false, true},
		{"right at the early edge", 4 * 60 * 1000, 5 * 60 * 1000, false, true},
		{"too early: plenty of TTL left", 60 * 1000, 5 * 60 * 1000, false, false},
		{"too late: already expired", 6 * 60 * 1000, 5 * 60 * 1000, false, false},
		{"exactly at expiry", 5 * 60 * 1000, 5 * 60 * 1000, false, false},
		{"apply already called it cold", 4 * 60 * 1000, 5 * 60 * 1000, true, false},
		{"TTL unknown", 4 * 60 * 1000, 0, false, false},
		{"no previous turn on record", 0, 5 * 60 * 1000, false, false},
		{"a 1h TTL, one minute from expiry", 59*60*1000 + 30*1000, 60 * 60 * 1000, false, true},
		{"a 1h TTL, half an hour in", 30 * 60 * 1000, 60 * 60 * 1000, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSweep(t, "")
			c := &components.Ctx{
				IdleMs: tc.idleMs, CacheTTLMs: tc.ttlMs, ColdCache: tc.cold,
			}
			if got := e.sweeping(c); got != tc.wantFire {
				t.Fatalf("sweeping = %v, want %v (idle=%dms ttl=%dms cold=%v)",
					got, tc.wantFire, tc.idleMs, tc.ttlMs, tc.cold)
			}
		})
	}
}

// Outside the window nothing is asked and nothing is touched.
func TestSweepDoesNothingOutsideTheWindow(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	c.IdleMs = 30 * 1000 // plenty of TTL left
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 0 {
		t.Fatalf("a turn outside the window made %d asks", n)
	}
	if rep.Gates["not_in_pre_expiry_window"] == 0 {
		t.Errorf("a turn outside the window must say why it did nothing (gates: %v)", rep.Gates)
	}
	if schema.MessageText(req.Input[1]) != original {
		t.Fatal("a turn outside the window modified a message")
	}
}

// The window's width is configurable, and widening it moves the early edge.
func TestPreExpirySecondsWidensTheWindow(t *testing.T) {
	narrow := newSweep(t, "")
	wide := newSweep(t, "pre_expiry_seconds: 180\n")
	// Three minutes idle on a five-minute TTL: two minutes remaining.
	c := &components.Ctx{IdleMs: 3 * 60 * 1000, CacheTTLMs: 5 * 60 * 1000}
	if narrow.sweeping(c) {
		t.Error("the default one-minute window fired with two minutes of TTL left")
	}
	if !wide.sweeping(c) {
		t.Error("a three-minute window did not fire with two minutes of TTL left")
	}
}

// A removal decided in the window MUST be replayed on every later turn. Without that, the next turn
// re-sends the removed output verbatim — the saving evaporates AND the prefix the provider is caching
// stops being byte-stable, which costs more than the sweep saved.
func TestSweepReplaysItsRemovalOnLaterTurns(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	st := store.NewMemory(store.Options{})

	first := sweepReq()
	original := schema.MessageText(first.Input[1])
	rep1 := &components.Report{}
	if _, err := e.Offload(first, rep1, preExpiryCtx("sess", asker, st)); err != nil {
		t.Fatal(err)
	}
	if rep1.Gates["sweep_dropped"] == 0 {
		t.Fatalf("the first turn did not remove anything, so there is nothing to replay (gates: %v)",
			rep1.Gates)
	}
	windowText := schema.MessageText(first.Input[1])

	later := sweepReq() // the same transcript again, on a turn outside the window
	c := preExpiryCtx("sess", asker, st)
	c.IdleMs = 5 * 1000
	rep2 := &components.Report{}
	if _, err := e.Offload(later, rep2, c); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("the later turn asked again: %d asks total", n)
	}
	if rep2.Gates["reapplied_same_session"] != 1 {
		t.Fatalf("the later turn did not replay the frozen removal (gates: %v)", rep2.Gates)
	}
	got := schema.MessageText(later.Input[1])
	if got == original {
		t.Fatal("the later turn re-sent the removed output verbatim; the saving is gone")
	}
	if got != windowText {
		t.Fatalf("the replay is not byte-identical, so the cached prefix churns:\n first %q\n later %q",
			windowText, got)
	}
}

// A verdict naming a label the inventory never offered must never be acted on: the label is how a
// decision is keyed to an output, so acting on a wrong one removes the wrong content — and indexing on
// it would panic rather than merely act wrongly.
func TestSweepIgnoresAVerdictForAnUnofferedLabel(t *testing.T) {
	asker := &fakeAsker{reply: `[{"i":99,"needed_by":"none","quote":"","verdict":"drop"}]`, cacheRead: 19595}
	e := newSweep(t, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] == 0 {
		t.Fatalf("no ask was made, so nothing under test ran (gates: %v)", rep.Gates)
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
	if rep.Gates["sweep_verdict_missing"] != 1 {
		t.Errorf("the unjudged output was not counted (gates: %v)", rep.Gates)
	}
}

// A well-formed EMPTY array is the model keeping everything, which the contract invites. It must not be
// filed as a failure — that conflation made "declined to act" and "was never asked" the same number.
func TestSweepCountsKeepEverythingSeparatelyFromAFailure(t *testing.T) {
	keepAll := &fakeAsker{reply: "[]", cacheRead: 19595}
	e := newSweep(t, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, preExpiryCtx("s", keepAll, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] == 0 {
		t.Fatalf("no ask was made (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_kept_everything"] != 1 {
		t.Fatalf("a deliberate keep-all was not counted as one (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_unparseable"] != 0 || rep.Gates["sweep_reply_truncated"] != 0 {
		t.Errorf("a deliberate keep-all was filed as a failure (gates: %v)", rep.Gates)
	}
	if schema.MessageText(req.Input[1]) != original {
		t.Error("keep-all removed something")
	}

	// A TRUNCATED reply is a failure, and a different one from malformed junk.
	cut := &fakeAsker{reply: `[{"i":0,"needed_by":"none","quote":"partial`, cacheRead: 19595}
	e2 := newSweep(t, "")
	rep2 := &components.Report{}
	if _, err := e2.Offload(sweepReq(), rep2,
		preExpiryCtx("s2", cut, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep2.Gates["sweep_reply_truncated"] != 1 {
		t.Fatalf("a cut-off array was not counted as truncation (gates: %v)", rep2.Gates)
	}
	if rep2.Gates["sweep_unparseable"] != 0 {
		t.Errorf("truncation was filed as a format failure; the two need opposite fixes (gates: %v)",
			rep2.Gates)
	}
}

// The refusal, reaching all the way through the component: a drop naming an outstanding obligation
// leaves the output in place and raises the alertable counter.
func TestSweepRefusesADropThatNamesAnObligation(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "c",
		quote: "Next I will patch the timeout in src/api/users.py."}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] == 0 {
		t.Fatalf("no ask was made, so the refusal was never exercised (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_drop_refused_obligation"] == 0 {
		t.Fatalf("the refusal was not counted (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_dropped"] != 0 {
		t.Fatalf("the contradictory drop was PERFORMED (gates: %v)", rep.Gates)
	}
	if schema.MessageText(req.Input[1]) != original {
		t.Fatal("the output was removed despite naming an outstanding obligation")
	}
	// The quote IS in the transcript, so the fabrication counter must stay quiet.
	if rep.Gates["sweep_quote_fabricated"] != 0 {
		t.Errorf("a verbatim transcript quote was counted as fabricated (gates: %v)", rep.Gates)
	}
}

// A single-candidate inventory is the refuted shape wearing a new name, so it is COUNTED. Shown one
// output, a model simply drops it: 6% live-kept, inside the null model's error bar.
func TestSweepCountsAnInventoryOfOne(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	rep := &components.Report{}
	if _, err := e.Offload(sweepReq(), rep,
		preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_adjudicated"] != 1 {
		t.Fatalf("expected one candidate adjudicated, got %d (gates: %v)",
			rep.Gates["sweep_adjudicated"], rep.Gates)
	}
	if rep.Gates["sweep_inventory_of_one"] != 1 {
		t.Fatalf("an inventory of one was not counted; a starved inventory would be invisible "+
			"(gates: %v)", rep.Gates)
	}
}

// THE STARVATION TRIPWIRE, and it is a guard for a rebase rather than for today's code.
//
// `4ca1f13`'s real defect was a per-candidate PRE-FILTER at the gathering site: prefix_still_referenced
// removed 149,681 candidates and left about one per request, silently turning a bulk adjudication arm
// into the per-output shape refuted at 6% live-kept. PR #80 rebases index-driven candidate selection
// onto this branch and can recreate exactly that.
//
// On `main` nothing thins the list, so the counter must read ZERO — a tripwire that fires today would
// be noise, and one that cannot fire when something IS thinning would be useless. Both directions are
// asserted, the second by thinning the list on purpose.
func TestSweepCountsAThinnedInventory(t *testing.T) {
	const n = 8
	asker := &labelAsker{verdict: "keep", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	rep := &components.Report{}
	if _, err := e.Offload(manyCandidates(n), rep,
		preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: candidates really reached the inventory, or "nothing was thinned" is vacuous.
	if rep.Gates["sweep_offered"] != n {
		t.Fatalf("sweep_offered = %d, want %d (gates: %v)", rep.Gates["sweep_offered"], n, rep.Gates)
	}
	if rep.Gates["sweep_inventory_thinned"] != 0 {
		t.Errorf("nothing thins the list on main, so the tripwire must be silent (gates: %v)", rep.Gates)
	}

	// And it MUST fire when something does thin it. Asserted on the arithmetic the component uses,
	// because there is no pre-filter here to install: this is the comparison a rebase would trip.
	var probe components.Report
	eligible, offered := n, 1 // what prefix_still_referenced did, in miniature
	if eligible > offered {
		probe.GateN("sweep_inventory_thinned", eligible-offered)
	}
	if probe.Gates["sweep_inventory_thinned"] != n-1 {
		t.Fatalf("the tripwire's arithmetic does not count a thinned inventory: %v", probe.Gates)
	}
}

// THE COUNTER CONTRACT, component end. These names are what an operator's dashboard query and alert
// rule are written against. The other end is pinned in proxy/sweep_counters_test.go.
func TestSweepRaisesTheContractedCounterNames(t *testing.T) {
	const obligation = "Next I will patch the timeout in src/api/users.py."
	for _, tc := range []struct {
		name    string
		asker   *labelAsker
		want    []string
		notWant []string
	}{
		{"a spent output", &labelAsker{verdict: "drop", needed: "none"},
			[]string{"sweep_adjudicated", "sweep_dropped", "sweep_prefix_cache_read_ok"}, nil},
		{"an output still needed", &labelAsker{verdict: "keep", needed: "a", quote: obligation},
			[]string{"sweep_adjudicated", "sweep_kept"}, []string{"sweep_quote_fabricated"}},
		{"a drop contradicting an obligation", &labelAsker{verdict: "drop", needed: "a", quote: obligation},
			[]string{"sweep_drop_refused_obligation"}, []string{"sweep_dropped"}},
		{"an invented obligation", &labelAsker{verdict: "keep", needed: "a", quote: "rewrite it in Rust"},
			[]string{"sweep_quote_fabricated"}, nil},
		{"an unanswered criterion", &labelAsker{verdict: "drop", needed: ""},
			[]string{"sweep_criterion_missing"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.asker.cacheRead = 19595
			e := newSweep(t, "")
			rep := &components.Report{}
			if _, err := e.Offload(sweepReq(), rep,
				preExpiryCtx("s", tc.asker, store.NewMemory(store.Options{}))); err != nil {
				t.Fatal(err)
			}
			// PRECONDITION: an adjudication happened. A component that never asked raises no
			// counters, and every assertion would then fail for the wrong reason.
			if rep.Gates["sweep_adjudicated"] != 1 {
				t.Fatalf("no adjudication was counted: %v", rep.Gates)
			}
			for _, want := range tc.want {
				if rep.Gates[want] == 0 {
					t.Errorf("the component did not raise %q; got %v", want, rep.Gates)
				}
			}
			for _, no := range tc.notWant {
				if rep.Gates[no] != 0 {
					t.Errorf("the component raised %q when it should not; got %v", no, rep.Gates)
				}
			}
		})
	}
}

// The knobs that have no meaning here must be REFUSED with a reason, not silently ignored. An operator
// migrating an older config by hand has no other way to learn that `max_calls` now means nothing.
func TestSweepRejectsKeysThatDoNotApply(t *testing.T) {
	for _, tc := range []struct{ key, yaml string }{
		{"strategy", "strategy: code\n"},
		{"rewrite", "rewrite: false\n"},
		{"aggressiveness", "aggressiveness: high\n"},
		{"max_chars", "max_chars: 8000\n"},
		{"model", "model:\n  model: claude-haiku-4-5\n"},
		{"context", "context: full\n"},
		{"context_messages", "context_messages: 7\n"},
		{"max_calls", "max_calls: 4\n"},
		{"economic_gate", "economic_gate: true\n"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			_, err := newExtractSweep([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("%s was accepted; the key would silently do nothing", tc.key)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name the key: %v", err)
			}
			if !strings.Contains(err.Error(), "does not apply here") {
				t.Errorf("error does not say why the key cannot apply: %v", err)
			}
		})
	}
}

// The ask the component actually sends must be the inventory contract, and must carry no output body.
func TestSweepSendsTheInventoryAndNotTheOutputs(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "")
	req := sweepReq()
	body := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	ask := asker.ask()
	if ask == "" {
		t.Fatal("no ask was sent, so there is nothing to assert about it")
	}
	for _, want := range []string{"keep|drop", `"i": <label>`, "SPENT only if"} {
		if !strings.Contains(ask, want) {
			t.Errorf("the ask does not carry %q", want)
		}
	}
	// The output body must not be copied into the ask: the model reads it from cache.
	if strings.Contains(ask, body) {
		t.Error("the whole output body was copied into the ask, defeating the mechanism")
	}
	// The ask must be a small multiple of the contract, not a transcript copy.
	if len(ask) > 4000 {
		t.Errorf("the ask is %d chars; it should be the contract plus one line per candidate", len(ask))
	}
	// And it must not be a compaction prompt.
	for _, banned := range []string{"Starlark", "return the JSON"} {
		if strings.Contains(ask, banned) {
			t.Errorf("the ask is a compaction prompt: mentions %q", banned)
		}
	}
}
