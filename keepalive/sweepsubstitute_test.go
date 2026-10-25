package keepalive

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/components/offload"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// sweepAsker stands in for the host's PrefixAsker: it answers "drop" for every label it is shown, and
// reports the cache read the caller gates on.
type sweepAsker struct {
	calls     atomic.Int64
	lastAsk   atomic.Value
	cacheRead int
	err       error
}

func (a *sweepAsker) Ask(_ context.Context, _ string, ask string) (string, components.PrefixUsage, error) {
	a.calls.Add(1)
	a.lastAsk.Store(ask)
	if a.err != nil {
		return "", components.PrefixUsage{}, a.err
	}
	var parts []string
	for i := 0; strings.Contains(ask, fmt.Sprintf("[%d] ", i)); i++ {
		parts = append(parts, fmt.Sprintf(`{"i":%d,"needed_by":"none","quote":"","verdict":"drop"}`, i))
	}
	return "[" + strings.Join(parts, ",") + "]", components.PrefixUsage{CacheRead: a.cacheRead, Fresh: 600, Output: 300}, nil
}

func sweepToolMsg(id, text string) bschemas.ChatMessage {
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool}
	schema.SetMessageText(&m, text)
	call := id
	m.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &call}
	return m
}

// sweepConversation is a user turn and n large tool outputs with wire ids.
func sweepConversation(n int) []bschemas.ChatMessage {
	user := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&user, "fix the failing tests")
	msgs := []bschemas.ChatMessage{user}
	for i := 0; i < n; i++ {
		msgs = append(msgs, sweepToolMsg(fmt.Sprintf("toolu_k%d", i),
			strings.Repeat(fmt.Sprintf("candidate %d distinct line\n", i), 900)))
	}
	return msgs
}

// registerSweep puts the sweep's registration for session "sess-1" in place, through the sweep's own
// Offload, and arranges for it to be removed.
func registerSweep(t *testing.T, st store.Store, asker components.PrefixAsker, model string, msgs []bschemas.ChatMessage) {
	t.Helper()
	if err := offload.RegisterSweepPingForTest("sess-1", "", st, asker, model, msgs, "min_tokens: 2000\n"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(thread.Key("sess-1", "")) })
}

func waitSweepOutcome(t *testing.T, k *Keeper, fs *fakeDispatcher) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.sweepSubstituted.Load()+k.sweepAskFailed.Load()+k.sweepOverBudget.Load()+
		int64(fs.n()) < 1 {
		time.Sleep(time.Millisecond)
	}
}

// ⭐ POINT 1 (A1): the question takes the place of the ping. It is counted once as THE ping for the
// span, no ordinary ping is sent, and the answer is held for the thread with its wait cause (point 12).
func TestKeepAliveSendsTheSweepQuestionInPlaceOfTheNextPing(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))

	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	if n := k.Sweep(clock.advance(281 * time.Second)); n != 1 {
		t.Fatalf("the due ping did not fire (%d)", n)
	}
	waitSweepOutcome(t, k, fs)
	if k.sweepSubstituted.Load() != 1 {
		t.Fatalf("sweepSubstituted = %d, want 1 (ask failed %d, over budget %d, ordinary pings %d)",
			k.sweepSubstituted.Load(), k.sweepAskFailed.Load(), k.sweepOverBudget.Load(), fs.n())
	}
	if fs.n() != 0 {
		t.Errorf("sent %d ordinary pings: the question must replace the ping, not add to it", fs.n())
	}
	if got := k.pings.Load(); got != 1 {
		t.Errorf("pings = %d, want 1: a substituted ping counts once", got)
	}
	if asker.calls.Load() != 1 {
		t.Errorf("asked %d times, want 1", asker.calls.Load())
	}
	asked, proposed, waiting, ok := offload.HeldSweepAnswerForTest(st, thread.Key("sess-1", ""))
	if !ok || asked != 3 || proposed != 3 {
		t.Fatalf("held answer = (%d asked, %d proposed, ok=%v), want 3 and 3", asked, proposed, ok)
	}
	// rec's response carried no tool_use and parsed usage, so the thread waits on the human.
	if waiting != "human" {
		t.Errorf("wait cause = %q, want human", waiting)
	}
	s := k.Stats()
	if s.SweepSubstituted != 1 || s.SweepHeld < 1 || s.SweepAsked < 1 {
		t.Errorf("stats = substituted %d, held %d, asked %d", s.SweepSubstituted, s.SweepHeld, s.SweepAsked)
	}
}

// The wait cause is the establishing response's own: a response that called a tool waits on its tool.
func TestTheWaitCauseComesFromTheEstablishingResponse(t *testing.T) {
	k, fs, clock := testKeeperWith(t, func(o *Options) {
		o.OpenTasks = func(string, string, string) (int, int) { return 0, 0 }
	})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	registerSweep(t, st, &sweepAsker{cacheRead: 48576}, "claude-sonnet-5", sweepConversation(2))
	at := clock.now()
	for _, d := range []time.Duration{-time.Second, 0} {
		k.Record(Record{Key: sess1, StartedAt: at.Add(d), Body: []byte(kaBody), Provider: bschemas.Anthropic,
			Route: "/v1/messages", Status: http.StatusOK, Usage: Usage{CacheRead: 48576, CacheWrite: 1200},
			UsageOK: true, ToolUseNames: []string{"Read", "Bash"}, Policy: kaPolicy(), Header: kaHeader(),
			Hold: fakeOf(k).hold(sess1)})
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitSweepOutcome(t, k, fs)
	_, _, waiting, ok := offload.HeldSweepAnswerForTest(st, thread.Key("sess-1", ""))
	if !ok || waiting != "own_tool" {
		t.Fatalf("wait cause = %q (held %v), want own_tool", waiting, ok)
	}
}

// A second ping in the same idle span is an ordinary ping: the conversation was already asked about.
func TestKeepAliveSecondPingOfASpanIsPlain(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(2))

	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 2)
	if asker.calls.Load() != 1 {
		t.Errorf("asked %d times across two pings, want 1", asker.calls.Load())
	}
	if fs.n() != 1 {
		t.Errorf("sent %d ordinary pings, want 1 (the second ping of the span)", fs.n())
	}
}

// FAIL OPEN: a question that fails is replaced by the ordinary ping.
func TestKeepAliveSweepQuestionFallsBackToAPingOnFailure(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576, err: errors.New("upstream 529")}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(2))

	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 {
		t.Fatalf("sent %d ordinary pings after the question failed, want 1", fs.n())
	}
	if k.sweepSubstituted.Load() != 0 || k.sweepAskFailed.Load() != 1 {
		t.Errorf("substituted %d, failed %d, want 0 and 1", k.sweepSubstituted.Load(), k.sweepAskFailed.Load())
	}
	if _, _, _, ok := offload.HeldSweepAnswerForTest(st, thread.Key("sess-1", "")); ok {
		t.Error("an answer that never arrived was held")
	}
}

// ⭐ A4: a plain ping, and the skip is counted as sweep_model_declined.
func TestKeepAliveSendsAPlainPingWhereTheQuestionWouldBreakTheCache(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	msgs := sweepConversation(3)
	// A thinking block before the last message, on a model that keeps only the last turn's thinking.
	think := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{}}
	reasoning := "working through it"
	think.ChatAssistantMessage.Reasoning = &reasoning
	schema.SetMessageText(&think, "looking")
	msgs = append([]bschemas.ChatMessage{msgs[0], think}, msgs[1:]...)
	registerSweep(t, st, asker, "claude-haiku-4-5", msgs)

	before := k.Stats().SweepModelDeclined
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if asker.calls.Load() != 0 {
		t.Fatalf("the question was sent %d times on a model it breaks the cache of", asker.calls.Load())
	}
	if fs.n() != 1 {
		t.Fatalf("sent %d ordinary pings, want 1", fs.n())
	}
	if got := k.Stats().SweepModelDeclined - before; got != 1 {
		t.Errorf("sweep_model_declined moved by %d, want 1", got)
	}
}

// The per-ping ceiling applies to the question's own projected cost.
func TestKeepAliveSweepQuestionRespectsTheCostCap(t *testing.T) {
	k, fs, clock := testKeeper(t)
	fakeOf(k).setPrices(proxyPricer{fixedPrice{modelinfo.Price{CacheRead: 3.8e-7, Output: 19e-6}}})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 20000}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))

	pol := kaPolicy()
	pol.MaxUSDPerPing = 0.02 // ping ≈ $0.0076; the question ≈ $0.0836
	at := clock.now()
	for _, d := range []time.Duration{-time.Second, 0} {
		rec(k, pol, sess1, at.Add(d), kaBody, http.StatusOK, Usage{CacheRead: 20000}, true)
	}
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if asker.calls.Load() != 0 {
		t.Errorf("an over-budget question was dispatched %d times", asker.calls.Load())
	}
	if k.sweepOverBudget.Load() != 1 || fs.n() != 1 {
		t.Errorf("over-budget %d, ordinary pings %d, want 1 and 1", k.sweepOverBudget.Load(), fs.n())
	}
}

// No registration, no question: the ordinary ping goes out and the reason is counted.
func TestKeepAliveWithNoSweepRegistrationSendsAnOrdinaryPing(t *testing.T) {
	k, fs, clock := testKeeper(t)
	before := k.Stats().SweepNoCandidate
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 || k.sweepSubstituted.Load() != 0 {
		t.Fatalf("ordinary pings %d, substituted %d, want 1 and 0", fs.n(), k.sweepSubstituted.Load())
	}
	if k.Stats().SweepNoCandidate-before < 1 {
		t.Error("the missing registration was not counted")
	}
}

// The summary goes first when it has material: it replaces the very candidates the sweep would ask
// about (#404 point 10, which PR D makes explicit).
func TestKeepAliveSummaryTakesThePingBeforeTheSweep(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(2))
	model := &fakeSummaryModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	kaCandidate(t, "sess-1", st, model)

	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.summarySubstituted.Load()+k.sweepSubstituted.Load()+int64(fs.n()) < 1 {
		time.Sleep(time.Millisecond)
	}
	if k.summarySubstituted.Load() != 1 || k.sweepSubstituted.Load() != 0 || asker.calls.Load() != 0 {
		t.Errorf("summary %d, sweep %d, sweep asks %d: the summary must take the ping",
			k.summarySubstituted.Load(), k.sweepSubstituted.Load(), asker.calls.Load())
	}
}

// ⭐ A2. Keep-alive off, or a prompt under the keep-alive floor (80k): the keeper never pings, so the
// sweep is never asked. No keep-alive means no question.
func TestNoKeepAliveNoQuestion(t *testing.T) {
	for _, tc := range []struct {
		name string
		pol  func() Policy
	}{
		{"keep-alive off", func() Policy { p := kaPolicy(); p.KeepAlive = false; return p }},
		{"prompt under the 80k floor", func() Policy { p := kaPolicy(); p.MinPrefixTokens = 80000; return p }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, fs, clock := testKeeper(t)
			st := store.NewMemory(store.Options{MaxEntries: 400})
			asker := &sweepAsker{cacheRead: 48576}
			registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
			recordOne(t, k, tc.pol(), kaBody, clock.now())
			if n := k.Sweep(clock.advance(281 * time.Second)); n != 0 {
				t.Fatalf("a ping fired (%d) where the keeper should track nothing", n)
			}
			time.Sleep(20 * time.Millisecond)
			if asker.calls.Load() != 0 || fs.n() != 0 {
				t.Fatalf("question asks %d, pings %d, want none", asker.calls.Load(), fs.n())
			}
		})
	}
}

// ⭐ A3: one candidate gives a question (the offload tests use one); zero candidates give a plain ping.
func TestKeepAliveWithZeroCandidatesSendsAPlainPing(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	small := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&small, "hello")
	// A big output that is already a marker: it registers, and offers nothing.
	registerSweep(t, st, asker, "claude-sonnet-5", []bschemas.ChatMessage{small,
		sweepToolMsg("toolu_s", strings.Repeat("a spent line\n", 900)+expand.Marker("abcdef0"))})
	before := k.Stats().SweepNoInventory
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if asker.calls.Load() != 0 || fs.n() != 1 {
		t.Fatalf("question asks %d, ordinary pings %d, want 0 and 1", asker.calls.Load(), fs.n())
	}
	if k.Stats().SweepNoInventory-before != 1 {
		t.Error("the empty inventory was not counted")
	}
}

type panicAskerKA struct{}

func (panicAskerKA) Ask(context.Context, string, string) (string, components.PrefixUsage, error) {
	panic("boom in Ask")
}

// FAIL OPEN: a panic in the question leaves the ordinary ping, the keeper keeps running, and the panic
// is counted.
func TestKeepAliveSurvivesAPanicInTheQuestion(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	registerSweep(t, st, panicAskerKA{}, "claude-sonnet-5", sweepConversation(3))
	before := k.Stats().SweepPanics
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 {
		t.Fatalf("sent %d ordinary pings after the panic, want 1", fs.n())
	}
	if k.Stats().SweepPanics-before != 1 {
		t.Errorf("the panic was not counted (%d)", k.Stats().SweepPanics-before)
	}
	if k.sweepSubstituted.Load() != 0 {
		t.Error("a panicked question was booked as the ping")
	}
}

// The keeper's own recover is the last line: a panic OUTSIDE the offload package's recovers (here the
// host's open-task hook, called while the wait cause is built) must still leave the ordinary ping.
func TestKeepAliveSurvivesAPanicInTheKeepersOwnQuestionCode(t *testing.T) {
	k, fs, clock := testKeeperWith(t, func(o *Options) {
		o.OpenTasks = func(string, string, string) (int, int) { panic("boom in OpenTasks") }
	})
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(3))
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 {
		t.Fatalf("sent %d ordinary pings after the keeper-level panic, want 1", fs.n())
	}
	if k.sweepAskFailed.Load() != 1 || k.sweepSubstituted.Load() != 0 {
		t.Errorf("ask failed %d, substituted %d, want 1 and 0", k.sweepAskFailed.Load(), k.sweepSubstituted.Load())
	}
}

// registerSweepThread is registerSweep for one thread of session "sess-1".
func registerSweepThread(t *testing.T, th string, st store.Store, asker components.PrefixAsker, msgs []bschemas.ChatMessage) {
	t.Helper()
	if err := offload.RegisterSweepPingForTest("sess-1", th, st, asker, "claude-sonnet-5", msgs, "min_tokens: 2000\n"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(thread.Key("sess-1", th)) })
}

// kaCandidateThread registers a summary candidate for one thread of session "sess-1".
func kaCandidateThread(t *testing.T, th string, st store.Store, m *fakeSummaryModel) {
	t.Helper()
	msg := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&msg, "conversation of thread "+th)
	span := []bschemas.ChatMessage{msg}
	call := func(ctx context.Context) (string, error) { return m.CompleteMessages(ctx, "", span) }
	offload.RegisterThreadKeepAliveCandidateForTest("sess-1", th, st, call, "messages", span, 1, "", 0)
	t.Cleanup(func() { offload.ClearKeepAliveCandidate(thread.Key("sess-1", th)) })
}

// ⭐ S1, trigger fired. A thread the summarizer registered for (its trigger fired) has the summary
// take the ping. The sweep does not ask, and the skip is counted.
func TestWhenTheSummarizerTriggerFiresTheSummaryTakesThePingAndTheSweepDoesNotAsk(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(2))
	model := &fakeSummaryModel{out: "<summary>explored the handler, 3 tests fail.</summary>"}
	kaCandidate(t, "sess-1", st, model)
	before := k.Stats().SweepSummaryOwnedPing

	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && k.summarySubstituted.Load()+k.sweepSubstituted.Load()+int64(fs.n()) < 1 {
		time.Sleep(time.Millisecond)
	}
	if k.summarySubstituted.Load() != 1 || model.calls.Load() != 1 {
		t.Errorf("summary substituted %d, summary calls %d, want 1 and 1", k.summarySubstituted.Load(), model.calls.Load())
	}
	if asker.calls.Load() != 0 || k.sweepSubstituted.Load() != 0 {
		t.Errorf("the sweep asked (%d asks, %d substituted) on a ping the summary owns",
			asker.calls.Load(), k.sweepSubstituted.Load())
	}
	if got := k.Stats().SweepSummaryOwnedPing - before; got != 1 {
		t.Errorf("sweep_summary_owned_ping rose by %d, want 1", got)
	}
}

// ⭐ S1, trigger did not fire. No summary candidate on the thread: the sweep asks and nothing is counted
// as a skip.
func TestWhenTheSummarizerTriggerDoesNotFireTheSweepAsks(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(2))
	before := k.Stats().SweepSummaryOwnedPing

	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitSweepOutcome(t, k, fs)
	if k.sweepSubstituted.Load() != 1 || asker.calls.Load() != 1 || fs.n() != 0 {
		t.Errorf("substituted %d, asks %d, ordinary pings %d, want 1, 1, 0",
			k.sweepSubstituted.Load(), asker.calls.Load(), fs.n())
	}
	if got := k.Stats().SweepSummaryOwnedPing - before; got != 0 {
		t.Errorf("sweep_summary_owned_ping rose by %d with no summary candidate", got)
	}
}

// ⭐ One ping never runs both calls, with 2 threads due at the same time. Thread A has both a summary
// and a sweep candidate (summary wins). Thread B has only a sweep candidate (sweep asks). Each ping runs
// exactly one call, and the sweep's ask on B is its only ask.
func TestOnePingNeverRunsBothCallsWithTwoThreadsDue(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	sumA := &fakeSummaryModel{out: "<summary>thread A so far.</summary>"}
	registerSweepThread(t, "", st, asker, sweepConversation(2))
	kaCandidateThread(t, "", st, sumA)
	registerSweepThread(t, "a:B", st, asker, sweepConversation(2))

	t0 := clock.now()
	for _, th := range []string{"", "a:B"} {
		for i := 0; i < 2; i++ {
			rec(k, kaPolicy(), Key{Tenant: "t1", Session: "sess-1", Thread: th},
				t0.Add(time.Duration(i)*time.Second), kaBody, http.StatusOK, Usage{CacheRead: 48576}, true)
		}
	}
	if n := k.Sweep(clock.advance(283 * time.Second)); n != 2 {
		t.Fatalf("fired %d pings, want 2 (both threads due at once)", n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && k.pings.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	if got := k.pings.Load(); got != 2 {
		t.Fatalf("pings = %d, want 2: one per thread, never 2 on one thread", got)
	}
	if sumA.calls.Load() != 1 {
		t.Errorf("summary calls on thread A = %d, want 1", sumA.calls.Load())
	}
	if asker.calls.Load() != 1 {
		t.Errorf("sweep asks = %d, want 1 (thread B only)", asker.calls.Load())
	}
	if k.summarySubstituted.Load() != 1 || k.sweepSubstituted.Load() != 1 || fs.n() != 0 {
		t.Errorf("summary %d, sweep %d, ordinary %d, want 1, 1, 0",
			k.summarySubstituted.Load(), k.sweepSubstituted.Load(), fs.n())
	}
}

// A claimed ping frees the thread and the sweep's collection slot on every exit, a declined question
// included: the next ping on the thread can claim and ask again.
func TestAClaimedPingFreesTheThreadAndTheSlotOnEveryExit(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	small := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser}
	schema.SetMessageText(&small, "hello")
	registerSweep(t, st, asker, "claude-sonnet-5", []bschemas.ChatMessage{small,
		sweepToolMsg("toolu_s", strings.Repeat("a spent line\n", 900)+expand.Marker("abcdef0"))})
	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second)) // declines: no inventory, ordinary ping
	waitPings(t, k, 1)
	_ = fs
	c := offload.ClaimPing(thread.Key("sess-1", ""))
	defer c.Release()
	if c.Choice != offload.PingSweep {
		t.Errorf("claim after a declined ping = %q, want sweep: the thread was not freed", c.Choice)
	}
	if offload.SweepSlotsInUseForTest() != 0 {
		t.Errorf("%d sweep slots still held after the ping", offload.SweepSlotsInUseForTest())
	}
}

// R1-1. The summary owns the ping even when it declines: the sweep does not ask, the ordinary ping goes
// out, and the owned-ping counter rises. The ping is never lost, and never turned into a sweep question.
func TestWhenTheSummaryDeclinesTheSweepStillDoesNotAskAndAPlainPingGoesOut(t *testing.T) {
	k, fs, clock := testKeeper(t)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	asker := &sweepAsker{cacheRead: 48576}
	registerSweep(t, st, asker, "claude-sonnet-5", sweepConversation(2))
	model := &fakeSummaryModel{err: errors.New("upstream refused")}
	kaCandidate(t, "sess-1", st, model)
	before := k.Stats().SweepSummaryOwnedPing

	recordOne(t, k, kaPolicy(), kaBody, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if model.calls.Load() != 1 || k.summarySubstituted.Load() != 0 {
		t.Errorf("summary calls %d, substituted %d, want 1 and 0 (it must have declined)",
			model.calls.Load(), k.summarySubstituted.Load())
	}
	if fs.n() != 1 {
		t.Errorf("ordinary pings = %d, want 1", fs.n())
	}
	if asker.calls.Load() != 0 || k.sweepSubstituted.Load() != 0 {
		t.Errorf("the sweep asked (%d asks, %d substituted) after the summary declined",
			asker.calls.Load(), k.sweepSubstituted.Load())
	}
	if got := k.Stats().SweepSummaryOwnedPing - before; got != 1 {
		t.Errorf("sweep_summary_owned_ping rose by %d, want 1", got)
	}
}

// ⭐ C5 (#404 point 15). After a cold apply, the next ping carries the FORWARDED body.
//
// The keeper records the bytes that went upstream, so when a cold request is forwarded with its drops
// applied, the entry it holds is the dropped form, and the ping that follows reads THAT prefix. The wasted
// ping of the issue's finding 6 read the conversation the sweep had just abandoned ($0.08). The test
// feeds the keeper two requests: one with the original output, then a cold one whose forwarded body is
// the dropped form. The ping body must be the second, byte for byte, and must not carry the abandoned
// output.
func TestC5AfterAColdApplyTheNextPingCarriesTheForwardedBody(t *testing.T) {
	k, fs, clock := testKeeper(t)
	const big = "ORIGINAL-OUTPUT-THE-SWEEP-DROPPED"
	before := `{"model":"aws/claude-sonnet-5","max_tokens":32000,"stream":true,` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":"fix it"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"` + big + `"}]},` +
		`{"role":"assistant","content":"done","cache_control":{"type":"ephemeral"}}]}`
	forwarded := `{"model":"aws/claude-sonnet-5","max_tokens":32000,"stream":true,` +
		`"system":[{"type":"text","text":"you are claude code","cache_control":{"type":"ephemeral"}}],` +
		`"messages":[{"role":"user","content":"fix it"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"[context-guru removed a spent tool output]\n<<cg:abc123>>"}]},` +
		`{"role":"assistant","content":"done","cache_control":{"type":"ephemeral"}}]}`
	pol := kaPolicy()
	pol.MinPrefixTokens = 0
	at := clock.now()
	rec(k, pol, sess1, at.Add(-time.Second), before, http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	rec(k, pol, sess1, at, before, http.StatusOK, Usage{CacheRead: 48576, CacheWrite: 1200}, true)
	// The cold request: forwarded with the drops applied. The keeper is handed the forwarded bytes.
	at2 := at.Add(10 * time.Minute)
	clock.advance(10 * time.Minute)
	rec(k, pol, sess1, at2, forwarded, http.StatusOK, Usage{CacheWrite: 30000}, true)

	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	if fs.n() != 1 {
		t.Fatalf("sent %d pings, want 1", fs.n())
	}
	fs.mu.Lock()
	sent := append([]byte(nil), fs.sent[0].Body...)
	fs.mu.Unlock()
	if strings.Contains(string(sent), big) {
		t.Fatal("the ping carries the output the sweep dropped: it re-read the prefix the cold request abandoned")
	}
	if !strings.Contains(string(sent), "<<cg:abc123>>") {
		t.Fatalf("the ping does not carry the forwarded (dropped) form:\n%s", sent)
	}
	// Byte for byte the forwarded messages: only max_tokens and stream differ, as for any ping.
	if got, want := gjson.GetBytes(sent, "messages").Raw, gjson.Get(forwarded, "messages").Raw; got != want {
		t.Errorf("ping messages differ from the forwarded body:\n got %s\nwant %s", got, want)
	}
}

// ⭐ M1 (keeper half, #404 point 9). The answer cost is booked ONCE, on the ping row the question
// replaced, and it is the OUTPUT (thinking included) plus the question's own fresh input: never the
// cache read, which the plain ping would have made anyway. An ordinary ping carries none.
func TestTheAnswerCostIsBookedOnceOnThePingRowTheQuestionReplaced(t *testing.T) {
	k, fd, clock := testKeeper(t)
	price := modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}
	fd.setPrices(proxyPricer{fixedPrice{price}})
	sink := &reportSink{}
	st := store.NewMemory(store.Options{MaxEntries: 400})
	registerSweep(t, st, &sweepAsker{cacheRead: 48576}, "claude-sonnet-5", sweepConversation(3))
	recordWithSink(k, sink, sess1, clock.now())
	// The registration is for "sess-1" with no thread: sess1's session id is the same.
	k.Sweep(clock.advance(281 * time.Second))
	waitSweepOutcome(t, k, fd)
	if k.sweepSubstituted.Load() != 1 {
		t.Fatalf("the question did not replace the ping (ordinary pings %d)", fd.n())
	}
	reps := sink.all()
	var onPing []components.KeepAliveReport
	for _, r := range reps {
		if r.Pings > 0 || r.SweepAnswerUSD > 0 {
			onPing = append(onPing, r)
		}
	}
	var withAnswer int
	var r components.KeepAliveReport
	for _, x := range onPing {
		if x.SweepAnswerUSD > 0 {
			withAnswer++
			r = x
		}
	}
	if withAnswer != 1 {
		t.Fatalf("%d ping rows carry the answer cost, want exactly 1 (reports: %+v)", withAnswer, onPing)
	}
	// sweepAsker's usage: Fresh 600, Output 300, CacheRead 48576.
	wantAnswer := 600*price.Input + 300*price.Output
	if math.Abs(r.SweepAnswerUSD-wantAnswer) > 1e-12 {
		t.Errorf("answer cost = %v, want output + fresh = %v (NOT including the cache read)", r.SweepAnswerUSD, wantAnswer)
	}
	// The row's own cost is the whole call: the read PLUS the answer. The answer is part of it.
	wantRow := wantAnswer + 48576*price.CacheRead
	if math.Abs(r.CostUSD-wantRow) > 1e-9 {
		t.Errorf("ping row cost = %v, want the read + the answer = %v", r.CostUSD, wantRow)
	}
	if r.SweepAnswerUSD >= r.CostUSD {
		t.Errorf("the answer (%v) is the whole row (%v): the read the ping pays anyway was lost", r.SweepAnswerUSD, r.CostUSD)
	}
}

// A plain ping, and a summary-substituted ping, carry NO sweep answer cost.
func TestAnOrdinaryPingCarriesNoSweepAnswerCost(t *testing.T) {
	k, fd, clock := testKeeper(t)
	fd.setPrices(proxyPricer{fixedPrice{modelinfo.Price{Input: 3e-6, Output: 15e-6, CacheRead: 3e-7, CacheWrite: 3.75e-6}}})
	sink := &reportSink{}
	recordWithSink(k, sink, sess1, clock.now())
	k.Sweep(clock.advance(281 * time.Second))
	waitPings(t, k, 1)
	for _, r := range sink.all() {
		if r.SweepAnswerUSD != 0 {
			t.Errorf("an ordinary ping carries a sweep answer cost of %v", r.SweepAnswerUSD)
		}
	}
}
