package offload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/logging"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// fakeAsker stands in for the host's PrefixAsker. It records what was asked and what session it was
// asked for, and reports the cache read the caller gates on.
type fakeAsker struct {
	reply     string
	cacheRead int
	// cacheWrite is what the provider says it WROTE for this ask. Non-zero means the agent's own
	// prefix was re-created at the agent model's write rate, which is the harm the component's
	// counters exist to catch -- see TestSweepPrefixWriteTripsTheCounterEvenOnAPartialHit.
	cacheWrite int
	err        error
	// viaTool reports the reply as having arrived through the proxy's structured-answer tool rather
	// than as text, which is the one thing about a reply the parser CANNOT infer: it reads a
	// tool_use input and a JSON array in prose identically.
	viaTool  bool
	calls    int64
	lastAsk  atomic.Value
	lastSess atomic.Value
}

func (f *fakeAsker) Ask(_ context.Context, session, ask string) (string, components.PrefixUsage, error) {
	atomic.AddInt64(&f.calls, 1)
	f.lastAsk.Store(ask)
	f.lastSess.Store(session)
	if f.err != nil {
		return "", components.PrefixUsage{}, f.err
	}
	return f.reply, components.PrefixUsage{CacheRead: f.cacheRead, CacheWrite: f.cacheWrite,
		Fresh: 40, Output: 90, ViaTool: f.viaTool}, nil
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
//
// IT PREPENDS ITS OWN min_tokens, so passing one in extraYAML does not override it — YAML rejects
// the duplicate key and newExtractSweep fails with
//
//	yaml: unmarshal errors: line 3: mapping key "min_tokens" already defined at line 1
//
// which reads as a broken component rather than as a fixture mistake. A test that needs a different
// floor — e.g. one BELOW the shape descriptor's own size, to reach the never-worse pre-check —
// should call newExtractSweep directly rather than routing through here.
func newSweep(t *testing.T, extraYAML string) *ExtractSweep {
	t.Helper()
	c, err := newExtractSweep([]byte("min_tokens: 2000\n" + extraYAML))
	if err != nil {
		t.Fatalf("newExtractSweep: %v", err)
	}
	e := c.(*ExtractSweep)
	// THE TWO SEAMS THESE FIXTURES NEED, because the production defaults moved (#404):
	//
	// forceAsk: the sweep no longer asks inside a request on a time window. It asks at the keep-alive
	// ping, and the in-request ask that remains is the econ trigger's. These tests drive the
	// in-request ask (the adjudication and the splice), so they force it, except where the subject is
	// the econ trigger itself.
	//
	// minInventory: the shipped default is now 1. These fixtures were written against the old floor
	// of 10 and several assert exactly what it declines, so they keep it unless they set their own.
	if !strings.Contains(extraYAML, "min_inventory") {
		e.minInventory = fixtureInventory
	}
	return e
}

// askOffload is e.Offload followed by the ping-time question, with the question's gates and events
// merged into rep. The in-request ask is gone (#404); the adjudication these tests exercise now runs
// at the keep-alive ping, and this lets a test read its counters the way it read them from a request's
// Report. It asks only when the ping would be offered the question. A removal is NOT applied here: that
// is the cold apply (extract_sweep_apply.go).
func askOffload(e *ExtractSweep, req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	keys, err := e.Offload(req, rep, c)
	key := thread.Key(c.Session, c.Thread)
	defer clearSweepPing(key)
	dispatch, _, reason, ok := SweepPingSubstitute(key)
	if !ok {
		rep.Gate("ping_not_offered_" + string(reason))
		return keys, err
	}
	res := dispatch(batchTestBudget, SweepPingWait{})
	for g, n := range res.Gates {
		rep.GateN(g, n)
	}
	for ev, n := range res.Events {
		rep.EventN(ev, n)
	}
	return keys, err
}

// fixtureInventory is the inventory floor the pre-#404 fixtures assume. It is NOT the production
// default (defaultMinInventory is 1); the stocked fixtures below are sized to it.
const fixtureInventory = 10

// newSweepSmall is newSweep with the inventory floor lowered to 1.
//
// For the tests whose subject is PER-CANDIDATE accounting — this counter fired once, that verdict was
// ignored, this removal replayed — and which therefore assert exact counts against sweepReq's single
// above-floor output. Under the default floor of 10 those inventories decline before any ask, so the
// tests would assert an impossibility.
//
// It is a deliberate opt-in rather than the default for these, because the config it selects is one
// the measurements refute: at an inventory of one a model scored 6% live-kept, inside the
// drop-everything null model's error bar. A test may run there to count something precisely; a
// deployment should not. The floor's own behaviour is covered against the real default in
// TestSweepDeclinesBelowTheInventoryFloor, and the default is exercised end to end in
// TestSweepOffersCandidatesInsideTheCachedPrefix.
func newSweepSmall(t *testing.T, extraYAML string) *ExtractSweep {
	t.Helper()
	return newSweep(t, "min_inventory: 1\n"+extraYAML)
}

// sweepReq puts a BIG tool output in the tail, and states an obligation the refusal test quotes back.
func sweepReq() *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Find the auth timeout in src/api/users.py and fix it."),
		toolResultMsg(strings.Repeat("2024-01-01 GET /users/42 200 12ms src/api/users.py\n", 700)),
		assistantMsg("Next I will patch the timeout in src/api/users.py."),
		toolResultMsg(strings.Repeat("filler line to grow the transcript\n", 50)),
		userMsg("keep going"),
	}}
	return req
}

// sweepReqStocked is sweepReq with enough further candidates to clear the inventory floor, for the
// tests whose subject is that the component ACTS — an ask happening, a removal applying, the fallback
// firing. Those cannot run on sweepReq alone any more: it carries a single above-floor output, and an
// inventory of one is the per-output shape `4ca1f13` records as refuted at 6% live-kept, which the
// floor now declines by design.
//
// Kept SEPARATE from sweepReq rather than folded into it, because several tests assert exact
// per-candidate counts against that fixture's precise shape and stocking it shifts every one of them.
// A test asserting "one candidate was adjudicated" and a test asserting "the component acted at all"
// want different fixtures, and conflating them is how a fixture change becomes a four-test cascade.
//
// Appended AFTER the original five, so index 1 remains the big output and index 0's obligation text is
// unchanged — the refusal test quotes both back.
func sweepReqStocked() *bschemas.BifrostChatRequest {
	req := sweepReq()
	for i := 0; i < fixtureInventory; i++ {
		req.Input = append(req.Input, toolResultMsgWithID("toolu_fixture_"+strconv.Itoa(i),
			strings.Repeat("record "+strconv.Itoa(i)+" of the audit log\n", 900)))
	}
	return req
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
		// The host's keep-alive can ping this thread. A test about a thread it cannot ping clears this.
		CanKeepAlive: func() bool { return true },
		Model:        components.ModelSpec{Incoming: fallbackModel, Static: fallbackModel},
	}
}

// debugLogCtx attaches a logger at DEBUG level to the context, writing to buf, so a test can assert on
// what was actually logged. Built on logging.New/logging.With rather than a hand-rolled handler, so the
// level check this test depends on is the SAME code path Setup() wires up in production.
func debugLogCtx(buf *bytes.Buffer, session string, asker components.PrefixAsker, st store.Store) *components.Ctx {
	c := preExpiryCtx(session, asker, st)
	c.Ctx = logging.With(c.Ctx, slog.New(logging.New(buf, slog.LevelDebug, false)))
	return c
}

// fallbackModel is a request model the fixtures put on the Ctx, and records what it was shown. The
// sweep no longer falls back to it; tests use it to assert that nothing ever calls it.
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

// BATCHED, NOT CAPPED (#132). Twenty candidates are two balanced questions of 10 (the batch size is 12, so
// k = ceil(20/12) = 2), asked in sequence over the same cached transcript, and every candidate is judged
// exactly once. They used to be one ask of the 12 largest with the other 8 left out.
func TestSweepAsksTwentyCandidatesAsTwoBalancedBatches(t *testing.T) {
	const n = 20
	asker := &labelAsker{verdict: "keep", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	rep := &components.Report{}
	if _, err := askOffload(e, manyCandidates(n), rep,
		preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&asker.calls); got != 2 {
		t.Fatalf("%d candidates took %d asks, want 2 balanced batches", n, got)
	}
	// Every candidate was judged, and none twice.
	if got := rep.Events["sweep_adjudicated"]; got != n {
		t.Fatalf("sweep_adjudicated = %d, want %d: every candidate is asked about once (gates: %v)", got, n, rep.Gates)
	}
	if got := rep.Gates["sweep_over_ask_cap"]; got != 0 {
		t.Errorf("sweep_over_ask_cap = %d, want 0: 20 is under 12 x 4", got)
	}
	if rep.Events["sweep_inventory_of_one"] != 0 {
		t.Errorf("an inventory of %d was recorded as one (gates: %v)", n, rep.Gates)
	}
	// Batching must NOT trip the starvation tripwire: that alarm is for a pre-filter quietly thinning the
	// comparison.
	if got := rep.Events["sweep_inventory_thinned"]; got != 0 {
		t.Errorf("batching tripped sweep_inventory_thinned (%d); that counter is for an upstream filter", got)
	}
}

// THE FAILURE IS LOGGED AT WARN, so the cause is never lost: the gate and session must appear on the
// line.
func TestSweepAskFailureIsLoggedAtWarn(t *testing.T) {
	askFailed := errors.New("upstream 500: context deadline-ish failure")
	for _, tc := range []struct {
		name, extraYAML string
	}{
		{"failed ask", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			asker := &fakeAsker{err: askFailed}
			e := newSweepSmall(t, tc.extraYAML)
			rep := &components.Report{}
			if _, err := askOffload(e, sweepReq(), rep,
				debugLogCtx(&buf, "sess-warn-1", asker, store.NewMemory(store.Options{}))); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			if !strings.Contains(out, "WARN") {
				t.Errorf("the ask failure was not logged at WARN: %s", out)
			}
			if !strings.Contains(out, "sweep_ask_failed") {
				t.Errorf("the WARN line does not name the gate it raised: %s", out)
			}
			if !strings.Contains(out, "sess-warn-1") {
				t.Errorf("the WARN line does not carry the session: %s", out)
			}
		})
	}
}

// fakeCoverageAsker adds components.PrefixCoverage to fakeAsker, so a test can control what the
// sweep's own stale-prefix guard sees without going through proxy's real sentStash.
type fakeCoverageAsker struct {
	fakeAsker
	covers bool
	// gotCount/gotHash record what the guard actually asked for, so a test can tell "the guard
	// never ran" from "it ran and was satisfied" — both look like covers==true from the outside.
	gotCount atomic.Int64
	gotHash  atomic.Value
}

func (f *fakeCoverageAsker) CoversSpan(_ string, count int, hash string) bool {
	f.gotCount.Store(int64(count))
	f.gotHash.Store(hash)
	return f.covers
}

// The sweep's own version of #402's stale-prefix guard: a candidate the component believes is
// already inside the cached prefix (i <= c.MaxCachedIdx, the same test sweep_candidate_at_depth
// uses) must be verified against the asker's actual coverage before the ask runs — the ask reads
// proxy's sentStash through the identical PrefixAsker.Ask construction cache_aware_summarizer
// does, and so inherits the identical risk of answering from a transcript that does not cover
// what this turn is about to rely on it having read.
func TestSweepDeclinesOnAStalePrefix(t *testing.T) {
	asker := &fakeCoverageAsker{covers: false}
	e := newSweepSmall(t, "")
	req := sweepReqStocked()
	ctx := preExpiryCtx("s-stale", asker, store.NewMemory(store.Options{}))
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, ctx); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_stale_prefix"] != 1 {
		t.Fatalf("a stale stash was not counted as sweep_stale_prefix (gates: %v)", rep.Gates)
	}
	if atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatalf("Ask was called %d times; a stale prefix must be caught BEFORE paying for the "+
			"call, the same discipline cache_aware_summarizer's guard uses", asker.calls)
	}
	// At the ping the stash holds the registering request's whole body, so every message is inside the
	// cached prefix and the deepest candidate decides how much coverage is required.
	if got := asker.gotCount.Load(); got != int64(len(req.Input)) {
		t.Errorf("the guard asked coverage for %d messages, want %d (the deepest candidate is the last "+
			"message)", got, len(req.Input))
	}
}

// A count-changing component BEFORE the sweep is a third decline, distinct from a stale stash
// (#452): the raw and pipeline coordinates name different messages, so rawSpanForHash cannot
// locate a span at all and the guard declines WITHOUT judging the stash — which can be perfectly
// current. Counted as sweep_coords_shifted, never as sweep_stale_prefix, so a `high`/`xhigh`
// pipeline (summarize first, then this) does not report a stash defect on every compacted turn.
func TestSweepCountsShiftedCoordinatesApartFromAStalePrefix(t *testing.T) {
	// covers: true, so only the coordinate shift can decline — if the stash were judged, it
	// would pass.
	asker := &fakeCoverageAsker{covers: true}
	e := newSweepSmall(t, "")
	req := sweepReqStocked()
	ctx := preExpiryCtx("s-shifted", asker, store.NewMemory(store.Options{}))
	// What apply.BodyOpts would have set had a component ahead of the sweep changed the message
	// count: the pre-pipeline body holds one message more than req.Input does at this position.
	raw := make([]bschemas.ChatMessage, len(req.Input), len(req.Input)+1)
	copy(raw, req.Input)
	ctx.RawInput = append(raw, req.Input[0])
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, ctx); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_coords_shifted"] != 1 {
		t.Fatalf("a shifted coordinate system was not counted as sweep_coords_shifted (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_stale_prefix"] != 0 {
		t.Fatalf("a shifted coordinate system was counted as a stale stash (gates: %v)", rep.Gates)
	}
	if got := asker.gotCount.Load(); got != 0 {
		t.Errorf("CoversSpan was consulted for %d messages; with the coordinates shifted there is "+
			"no span safe to compare, so the stash must not be judged at all", got)
	}
	if atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatalf("Ask was called %d times; a guard that cannot locate the span must decline before "+
			"paying for the call", asker.calls)
	}
}

// The mirror of the test above: the asker reports full coverage, so the ask proceeds exactly as
// it did before this guard existed.
func TestSweepAsksNormallyWhenPrefixCovers(t *testing.T) {
	asker := &fakeCoverageAsker{covers: true, fakeAsker: fakeAsker{reply: "[]"}}
	e := newSweepSmall(t, "")
	req := sweepReqStocked()
	ctx := preExpiryCtx("s-covers", asker, store.NewMemory(store.Options{}))
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, ctx); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["sweep_stale_prefix"] != 0 {
		t.Fatalf("a covering prefix was wrongly declined as stale (gates: %v)", rep.Gates)
	}
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Fatalf("the ask did not run (calls=%d)", asker.calls)
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
			e := newSweepSmall(t, "")
			rep := &components.Report{}
			if _, err := askOffload(e, sweepReq(), rep,
				preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
				t.Fatal(err)
			}
			if atomic.LoadInt64(&asker.calls) != 1 {
				t.Fatalf("the asker was not called, so nothing under test ran (gates: %v)", rep.Gates)
			}
			if rep.Gates[tc.gate] != 1 {
				t.Fatalf("%s was not counted as %s (gates: %v)", tc.name, tc.gate, rep.Gates)
			}
			// There is no fallback: the ordinary ping is sent instead.
			if rep.Events["sweep_fallback_used"] != 0 || tc.name == "" {
				t.Errorf("%s fell back (gates: %v)", tc.name, rep.Gates)
			}
		})
	}
}

// THE TIME WINDOW IS GONE (#404). A sweep configured the shipped way asks NOTHING inside a request, on
// any cache state, including the state that used to be its window (a cache a minute from expiry). It
// asks at the keep-alive ping instead (extract_sweep_ping_test.go).
func TestSweepAsksNothingInsideARequestByDefault(t *testing.T) {
	for _, tc := range []struct {
		name   string
		idleMs int64
		cold   bool
	}{
		{"where the old window was: a minute from expiry", 4 * 60 * 1000, false},
		{"plenty of TTL left", 30 * 1000, false},
		{"a cold request", 10 * 60 * 1000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker := &labelAsker{verdict: "drop", needed: "none"}
			asker.cacheRead = 19595
			c0, err := newExtractSweep([]byte("min_tokens: 2000\nmin_inventory: 1\nshadow: true\n"))
			if err != nil {
				t.Fatal(err)
			}
			e := c0.(*ExtractSweep) // as shipped, shadow aside
			req := sweepReqStocked()
			original := schema.MessageText(req.Input[1])
			c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
			c.IdleMs, c.ColdCache = tc.idleMs, tc.cold
			rep := &components.Report{}
			if _, err := e.Offload(req, rep, c); err != nil {
				t.Fatal(err)
			}
			if n := atomic.LoadInt64(&asker.calls); n != 0 {
				t.Fatalf("a request asked %d times inside the pipeline; the question belongs to the ping", n)
			}
			if schema.MessageText(req.Input[1]) != original {
				t.Fatal("a request modified a message with no held answer to apply")
			}
			if rep.Gates["not_in_pre_expiry_window"] != 0 {
				t.Errorf("the retired window gate is still being raised: %v", rep.Gates)
			}
		})
	}
}

// The retired keys must not stop a config from loading: a production config may still hold them, and a
// proxy that will not start breaks fail open. Each is ignored with one warning and a count, and the
// sweep is then exactly the sweep built without the key.
func TestRetiredKeysLoadWarnAndChangeNothing(t *testing.T) {
	want := map[string]string{
		"pre_expiry_seconds": "180", "econ_trigger": "true", "econ_ignore_ask_cost": "true",
		"reward_premium": "20", "min_pressure": "0.2", "block_fallback": "true",
	}
	base, err := newExtractSweep([]byte("min_tokens: 1500\nshadow: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	for key, val := range want {
		before := SweepPingCounters().RetiredKeys[key]
		var buf bytes.Buffer
		old := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		got, err := newExtractSweep([]byte("min_tokens: 1500\nshadow: true\n" + key + ": " + val + "\n"))
		slog.SetDefault(old)
		if err != nil {
			t.Errorf("%s stopped the config from loading: %v", key, err)
			continue
		}
		if !reflect.DeepEqual(got, base) {
			t.Errorf("%s changed the sweep: %+v vs %+v", key, got, base)
		}
		if n := strings.Count(buf.String(), "retired setting"); n != 1 || !strings.Contains(buf.String(), key) {
			t.Errorf("%s: want exactly one warning naming it, got %d: %s", key, n, buf.String())
		}
		if SweepPingCounters().RetiredKeys[key]-before != 1 {
			t.Errorf("%s was not counted", key)
		}
	}
	// All six at once, and in a block that is otherwise empty.
	all := ""
	for k, v := range want {
		all += k + ": " + v + "\n"
	}
	if _, err := newExtractSweep([]byte(all)); err != nil {
		t.Errorf("a block of only retired keys did not load: %v", err)
	}
}

// A verdict naming a label the inventory never offered must never be acted on: the label is how a
// decision is keyed to an output, so acting on a wrong one removes the wrong content — and indexing on
// it would panic rather than merely act wrongly.
func TestSweepIgnoresAVerdictForAnUnofferedLabel(t *testing.T) {
	asker := &fakeAsker{reply: `[{"i":99,"needed_by":"none","quote":"","verdict":"drop"}]`, cacheRead: 19595}
	e := newSweepSmall(t, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Events["sweep_adjudicated"] == 0 {
		t.Fatalf("no ask was made, so nothing under test ran (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_verdict_unknown_label"] != 1 {
		t.Fatalf("a verdict for an unoffered label was not counted (gates: %v)", rep.Gates)
	}
	if rep.Events["sweep_dropped"] != 0 {
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
	e := newSweepSmall(t, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, preExpiryCtx("s", keepAll, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Events["sweep_adjudicated"] == 0 {
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
	e2 := newSweepSmall(t, "")
	rep2 := &components.Report{}
	if _, err := askOffload(e2, sweepReq(), rep2,
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
	e := newSweepSmall(t, "")
	req := sweepReq()
	original := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Events["sweep_adjudicated"] == 0 {
		t.Fatalf("no ask was made, so the refusal was never exercised (gates: %v)", rep.Gates)
	}
	if rep.Gates["sweep_drop_refused_obligation"] == 0 {
		t.Fatalf("the refusal was not counted (gates: %v)", rep.Gates)
	}
	if rep.Events["sweep_dropped"] != 0 {
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
//
// UNREACHABLE UNDER THE DEFAULT CONFIG, which is why this test lowers the floor explicitly. With
// min_inventory at its default of 10 an inventory of one declines before any ask, so this counter can
// only fire where an operator has deliberately lowered the floor into the range the measurements
// refute. That is the counter's remaining job: not "this happens sometimes" but "you asked for this,
// and here is how often it is costing you". Keeping the test on the default would assert an
// impossibility; keeping the counter without the floor was what let the shape ship.
func TestSweepCountsAnInventoryOfOne(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	rep := &components.Report{}
	if _, err := askOffload(e, sweepReq(), rep,
		preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Events["sweep_adjudicated"] != 1 {
		t.Fatalf("expected one candidate adjudicated, got %d (gates: %v)",
			rep.Events["sweep_adjudicated"], rep.Gates)
	}
	if rep.Events["sweep_inventory_of_one"] != 1 {
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
	e := newSweepSmall(t, "")
	rep := &components.Report{}
	if _, err := askOffload(e, manyCandidates(n), rep,
		preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: candidates really reached the inventory, or "nothing was thinned" is vacuous.
	if rep.Events["sweep_offered"] != n {
		t.Fatalf("sweep_offered = %d, want %d (gates: %v)", rep.Events["sweep_offered"], n, rep.Gates)
	}
	if rep.Events["sweep_inventory_thinned"] != 0 {
		t.Errorf("nothing thins the list on main, so the tripwire must be silent (gates: %v)", rep.Gates)
	}

	// And it MUST fire when something does thin it. Asserted on the arithmetic the component uses,
	// because there is no pre-filter here to install: this is the comparison a rebase would trip.
	var probe components.Report
	eligible, offered := n, 1 // what prefix_still_referenced did, in miniature
	if eligible > offered {
		probe.EventN("sweep_inventory_thinned", eligible-offered)
	}
	if probe.Events["sweep_inventory_thinned"] != n-1 {
		t.Fatalf("the tripwire's arithmetic does not count a thinned inventory: %v", probe.Events)
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
			e := newSweepSmall(t, "")
			rep := &components.Report{}
			if _, err := askOffload(e, sweepReq(), rep,
				preExpiryCtx("s", tc.asker, store.NewMemory(store.Options{}))); err != nil {
				t.Fatal(err)
			}
			// PRECONDITION: an adjudication happened. A component that never asked raises no
			// counters, and every assertion would then fail for the wrong reason.
			if rep.Events["sweep_adjudicated"] != 1 {
				t.Fatalf("no adjudication was counted: %v", rep.Gates)
			}
			// Looked up across BOTH histograms on purpose. What this test pins is that the
			// component raises the contracted NAME; which of the two series it lands in is a
			// different contract, pinned at the exporter end in proxy/sweep_counters_test.go, and
			// the no-name-in-both invariant is pinned by components.TestGatesAndEventsAreDisjoint.
			// Asserting the series here as well would make one fixture fail for two unrelated
			// reasons.
			raised := func(name string) int { return rep.Gates[name] + rep.Events[name] }
			both := func() string { return fmt.Sprintf("gates=%v events=%v", rep.Gates, rep.Events) }
			for _, want := range tc.want {
				if raised(want) == 0 {
					t.Errorf("the component did not raise %q; got %s", want, both())
				}
			}
			for _, no := range tc.notWant {
				if raised(no) != 0 {
					t.Errorf("the component raised %q when it should not; got %s", no, both())
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
	e := newSweepSmall(t, "")
	req := sweepReq()
	body := schema.MessageText(req.Input[1])
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, preExpiryCtx("s", asker, store.NewMemory(store.Options{}))); err != nil {
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
	// The ask must be a small multiple of the contract, not a transcript copy. The bound includes the
	// evidence paragraph, which is on by default (#404).
	if len(ask) > 6500 {
		t.Errorf("the ask is %d chars; it should be the contract plus one line per candidate", len(ask))
	}
	// And it must not be a compaction prompt.
	for _, banned := range []string{"Starlark", "return the JSON"} {
		if strings.Contains(ask, banned) {
			t.Errorf("the ask is a compaction prompt: mentions %q", banned)
		}
	}
}

// The verdict tool's whole purpose is that the model ANSWERS with it, and until this counter existed
// nothing could say whether it did. extract.ParseVerdicts reads a tool_use `input` and a JSON array in
// reply text identically -- deliberately, so that declaring the tool is additive -- with the
// consequence that a run where the declared tool is never touched produces the same verdicts, the same
// savings and the same gates as one where it is used every time. That is not hypothetical: a review of
// PR #137 measured 0 of 5 live asks using the tool while the PR claimed 6 of 6, and no published
// figure in this repo could adjudicate between the two readings.
func TestSweepCountsWhetherTheAnswerCameViaTheToolOrProse(t *testing.T) {
	verdicts := `[{"i":0,"needed_by":"none","quote":"","verdict":"keep"}]`
	for _, tc := range []struct {
		name    string
		viaTool bool
		want    string
		notWant string
	}{
		{"tool_use", true, "sweep_answered_via_tool", "sweep_answered_via_prose"},
		{"prose", false, "sweep_answered_via_prose", "sweep_answered_via_tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker := &fakeAsker{reply: verdicts, cacheRead: 19595, viaTool: tc.viaTool}
			e := newSweepSmall(t, "")
			rep := &components.Report{}
			if _, err := askOffload(e, sweepReq(), rep,
				preExpiryCtx("s-"+tc.name, asker, store.NewMemory(store.Options{}))); err != nil {
				t.Fatal(err)
			}
			// PRECONDITION. Without it a turn that never reached the ask at all would pass both
			// assertions below by producing neither event, which is exactly the vacuous shape this
			// repo's convention exists to catch.
			if rep.Events["sweep_prefix_cache_read_ok"] != 1 {
				t.Fatalf("the prefix ask never happened, so no reply shape was observed "+
					"(gates: %v events: %v)", rep.Gates, rep.Events)
			}
			if rep.Events[tc.want] != 1 {
				t.Errorf("%s: %s = %d, want 1 (events: %v)", tc.name, tc.want,
					rep.Events[tc.want], rep.Events)
			}
			if rep.Events[tc.notWant] != 0 {
				t.Errorf("%s: %s = %d, want 0 -- the two shapes must not both be counted "+
					"(events: %v)", tc.name, tc.notWant, rep.Events[tc.notWant], rep.Events)
			}
		})
	}
}

// A component that DECLINED must not report itself as having mutated the request, and the inventory
// floor is a decline. It returns early, so the tail's `changed == 0` check never saw it: 29,321 of
// 29,372 production rows read `mutated=1, acted=0, skipped=0`, which dash/event.go's
// `Mutated = !Reverted && !Skipped` makes impossible for a component that actually skipped. Both
// exits are asserted, because the two reach the floor with different candidate counts and only one
// of them records a gate at all (GateN is a no-op at n<=0).
func TestSweepDeclinedAtInventoryFloorReportsSkipped(t *testing.T) {
	// A request asks nothing, collects nothing and changes nothing, so it records no gate and must
	// still report Skipped. The inventory floor is the ping's: a thread below it is offered no question.
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweep(t, "") // the fixture floor of 10, NOT newSweepSmall's floor of 1
	req := sweepReq()
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, c); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 0 {
		t.Fatalf("a declining turn made %d asks", n)
	}
	if !rep.Skipped {
		t.Errorf("a component that declined reports Skipped=false, so dash reads mutated=1 "+
			"for a turn that changed nothing (gates: %v)", rep.Gates)
	}
	if rep.Gates["ping_not_offered_below_min_inventory"] == 0 {
		t.Errorf("the ping was offered a question below the floor (gates: %v)", rep.Gates)
	}
}

// The prefix ask's whole justification is that it READS the agent's cached prefix. When the entry has
// gone it WRITES it instead, at 1.25x fresh on the agent's own model -- 704,925 opus tokens and $3.42
// across three production calls, 73% of this component's entire spend. Testing `CacheRead == 0` alone
// missed two of those three: they read 39,805 tokens of a stable sub-prefix and wrote a quarter of a
// million, so the counter stayed silent on $2.26 of the harm. A partial hit is not evidence of safety.
func TestSweepPrefixWriteTripsTheCounterEvenOnAPartialHit(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "none"}
	asker.cacheRead = 39805 // a real partial hit, so CacheRead == 0 is FALSE
	asker.cacheWrite = 260604
	e := newSweepSmall(t, "")
	c := preExpiryCtx("s", asker, store.NewMemory(store.Options{}))
	if _, err := e.Offload(sweepReq(), &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	key := thread.Key(c.Session, c.Thread)
	defer clearSweepPing(key)
	dispatch, _, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered (reason %q)", reason)
	}
	// Since #721 the write alone decides nothing: the partial hit is caught because it read far less
	// than the turn it followed (here 300,000), which is what a rebuilt prefix looks like.
	res := dispatch(time.Second, SweepPingWait{LastTurnRead: 300000})
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("expected ONE ask, got %d (gates: %v)", n, res.Gates)
	}
	if res.Gates["sweep_prefix_cache_read_below_turn"] == 0 {
		t.Errorf("an ask that read 39805 of a 300000 turn recorded nothing (gates: %v)", res.Gates)
	}
	// And the zero-read counter must NOT fire, because the read did happen. The two name different
	// failures and collapsing them would lose the distinction the fix is for.
	if res.Gates["sweep_prefix_cache_read_ZERO"] != 0 {
		t.Errorf("a partial HIT was counted as a zero read (gates: %v)", res.Gates)
	}
}

// fakeCoveredAsker adds components.PrefixCoveredAsker to fakeCoverageAsker: the single-snapshot ask
// (#571), recording the span it was handed.
type fakeCoveredAsker struct {
	fakeCoverageAsker
	snapshotErr   error
	askCoveredHit atomic.Int64
	gotCoverCount atomic.Int64
	gotCoverHash  atomic.Value
}

func (f *fakeCoveredAsker) AskCovered(_ context.Context, _ string, count int, hash, _ string) (string, components.PrefixUsage, error) {
	f.askCoveredHit.Add(1)
	f.gotCoverCount.Store(int64(count))
	f.gotCoverHash.Store(hash)
	if f.snapshotErr != nil {
		return "", components.PrefixUsage{}, f.snapshotErr
	}
	return f.reply, components.PrefixUsage{CacheRead: f.cacheRead}, nil
}

// The ask goes out through AskCovered with the span the guard verified, so the stash cannot change
// between the check and the ask; a snapshot AskCovered refuses is counted as a stale prefix, not as
// a failed call, and nothing was sent.
func TestSweepAsksOnTheSnapshotItChecked(t *testing.T) {
	asker := &fakeCoveredAsker{fakeCoverageAsker: fakeCoverageAsker{covers: true, fakeAsker: fakeAsker{reply: "[]", cacheRead: 19595}}}
	e := newSweepSmall(t, "")
	req := sweepReqStocked()
	ctx := preExpiryCtx("s-covered", asker, store.NewMemory(store.Options{}))
	rep := &components.Report{}
	if _, err := askOffload(e, req, rep, ctx); err != nil {
		t.Fatal(err)
	}
	if asker.askCoveredHit.Load() != 1 || atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatalf("AskCovered calls=%d, plain Ask calls=%d; the ask must be the single-snapshot one",
			asker.askCoveredHit.Load(), asker.calls)
	}
	// At the ping every message is inside the cached prefix, so the span the guard verified reaches
	// the deepest candidate: the last message.
	if got := asker.gotCoverCount.Load(); got != int64(len(req.Input)) {
		t.Errorf("AskCovered was given count %d, want the guard's %d", got, len(req.Input))
	}
	if h, _ := asker.gotCoverHash.Load().(string); h == "" || h != asker.gotHash.Load() {
		t.Errorf("AskCovered was given hash %q, want the one the guard verified (%v)", h, asker.gotHash.Load())
	}

	// Now the snapshot disagrees with the early read.
	stale := &fakeCoveredAsker{fakeCoverageAsker: fakeCoverageAsker{covers: true, fakeAsker: fakeAsker{reply: "[]"}},
		snapshotErr: components.ErrStalePrefix}
	ctx2 := preExpiryCtx("s-covered-stale", stale, store.NewMemory(store.Options{}))
	rep2 := &components.Report{}
	if _, err := askOffload(e, sweepReqStocked(), rep2, ctx2); err != nil {
		t.Fatal(err)
	}
	if rep2.Gates["sweep_stale_prefix"] != 1 || rep2.Gates["sweep_ask_failed"] != 0 {
		t.Fatalf("a refused snapshot must count as sweep_stale_prefix, not a failed ask (gates: %v)", rep2.Gates)
	}
}

// readLabelAsker is a labelAsker that also reports the text the model read, as a PrefixReadAsker does.
type readLabelAsker struct {
	labelAsker
	read string
}

func (m *readLabelAsker) AskRead(ctx context.Context, session string, _ int, _, ask string) (string, components.PrefixUsage, string, error) {
	reply, u, err := m.labelAsker.Ask(ctx, session, ask)
	return reply, u, m.read, err
}

// The quote check uses the text the model READ (fix 6). A quote taken from a message newer than the
// request's transcript is real, and must not be counted as fabricated. With no read text the old check
// stays, so the same quote IS counted: that is the control.
func TestAQuoteFromTheBodyTheModelReadIsNotCountedAsFabricated(t *testing.T) {
	const quote = "a line that only the newest message carries"
	run := func(read string) *components.Report {
		a := &readLabelAsker{labelAsker: labelAsker{verdict: "keep", needed: "c", quote: quote}, read: read}
		a.cacheRead = 19595
		rep := &components.Report{}
		if _, err := askOffload(newSweepSmall(t, ""), sweepReq(), rep,
			preExpiryCtx("s", a, store.NewMemory(store.Options{}))); err != nil {
			t.Fatal(err)
		}
		if rep.Events["sweep_adjudicated"] == 0 {
			t.Fatalf("no ask was made (gates: %v)", rep.Gates)
		}
		return rep
	}
	if rep := run("the body the model read. " + quote); rep.Gates["sweep_quote_fabricated"] != 0 {
		t.Errorf("a quote found in the body the model read was counted as fabricated (gates: %v)", rep.Gates)
	}
	if rep := run(""); rep.Gates["sweep_quote_fabricated"] == 0 {
		t.Errorf("control: the same quote with no read text should be counted as fabricated (gates: %v)", rep.Gates)
	}
	if rep := run("a body without it"); rep.Gates["sweep_quote_fabricated"] == 0 {
		t.Errorf("a quote absent from the body the model read was not counted (gates: %v)", rep.Gates)
	}
}
