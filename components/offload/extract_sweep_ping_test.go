package offload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// shippedSweep builds the sweep as the registry builds it for an operator, with shadow mode on (the
// ping asks only then, until PR C) and the shipped inventory floor. The helpers in extract_sweep_test.go
// keep the old floor of 10; these tests are about the ping-time question and use the shipped one.
func shippedSweep(t *testing.T, extra string) *ExtractSweep {
	t.Helper()
	c, err := newExtractSweep([]byte("min_tokens: 2000\nshadow: true\n" + extra))
	if err != nil {
		t.Fatalf("newExtractSweep: %v", err)
	}
	return c.(*ExtractSweep)
}

func pingAsker() *labelAsker {
	a := &labelAsker{verdict: "drop", needed: "none"}
	a.cacheRead = 19595
	return a
}

// registered runs one request through the sweep so the thread registers, and returns what a test
// needs to ask at the ping.
func registered(t *testing.T, e *ExtractSweep, session string, asker components.PrefixAsker,
	st store.Store, req *bschemas.BifrostChatRequest) (key string, rep *components.Report) {
	t.Helper()
	c := preExpiryCtx(session, asker, st)
	rep = &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	key = thread.Key(session, "")
	t.Cleanup(func() { clearSweepPing(key) })
	return key, rep
}

// ⭐ POINT 1 AND 2: the question is asked at the ping, not in the request, with ONE candidate or more.
func TestTheQuestionIsAskedAtThePingAndNotInTheRequest(t *testing.T) {
	asker := pingAsker()
	e := shippedSweep(t, "")
	st := newMemStore()
	req := deepCandidates(1) // ONE candidate: no minimum
	before := schema.MessageText(req.Input[1])
	key, rep := registered(t, e, "ping-basic", asker, st, req)

	if n := atomic.LoadInt64(&asker.calls); n != 0 {
		t.Fatalf("the request asked %d times; the question belongs to the ping", n)
	}
	if schema.MessageText(req.Input[1]) != before {
		t.Fatal("the registering request was modified")
	}
	if rep.Skipped != true {
		t.Error("a request that changed nothing must report Skipped")
	}

	dispatch, info, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered for a thread with one candidate (reason %q)", reason)
	}
	if info.Offered != 1 || info.Model != "claude-sonnet-5" {
		t.Fatalf("info = %+v, want 1 candidate on claude-sonnet-5", info)
	}
	res := dispatch(time.Second, SweepPingWait{WaitingFor: "own_tool", WaitTool: "Bash"})
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Fatalf("the ping made %d asks, want 1", atomic.LoadInt64(&asker.calls))
	}
	if !res.Substituted || !res.Held || res.Offered != 1 || res.Proposed != 1 {
		t.Fatalf("result = %+v, want substituted, held, 1 offered, 1 proposed", res)
	}
	if asker.sess() != key {
		t.Errorf("asked for session %q, want the thread key %q", asker.sess(), key)
	}
	// POINT 8: evidence is on by default, so the question carries it and explains it.
	if !strings.Contains(asker.ask(), "evidence: ") || !strings.Contains(asker.ask(), `HOW TO READ THE "evidence" FIELD`) {
		t.Errorf("the default question carries no evidence:\n%s", asker.ask())
	}
	// No output body in the question: the model reads it from the cache.
	if strings.Contains(asker.ask(), before) {
		t.Error("the output body was copied into the question")
	}
}

// POINT 1: no second question for a conversation already asked about. The second ping of an idle span
// is a plain ping; asking again would pay for the same answer twice.
func TestASecondPingInTheSameSpanIsAPlainPing(t *testing.T) {
	asker := pingAsker()
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "ping-twice", asker, newMemStore(), deepCandidates(3))
	dispatch, _, _, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatal("first ping was not offered the question")
	}
	dispatch(time.Second, SweepPingWait{})
	if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonAlreadyAsked {
		t.Fatalf("second ping: ok=%v reason=%q, want already_asked", ok, reason)
	}
	if atomic.LoadInt64(&asker.calls) != 1 {
		t.Errorf("asked %d times for one conversation", atomic.LoadInt64(&asker.calls))
	}
}

// A new request re-registers the thread, so the next ping asks again about the NEW conversation.
func TestANewRequestEarnsANewQuestion(t *testing.T) {
	asker := pingAsker()
	e := shippedSweep(t, "")
	st := newMemStore()
	key, _ := registered(t, e, "ping-rereg", asker, st, deepCandidates(2))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	dispatch(time.Second, SweepPingWait{})
	registered(t, e, "ping-rereg", asker, st, deepCandidates(3))
	if _, info, reason, ok := SweepPingSubstitute(key); !ok || info.Offered != 3 {
		t.Fatalf("after a new request: ok=%v offered=%d reason=%q, want a question for 3", ok, info.Offered, reason)
	}
}

// POINT 2: the old minimum is gone, but an operator can bring one back.
func TestMinInventoryDefaultsToOneAndCanBeRaised(t *testing.T) {
	if defaultMinInventory != 1 {
		t.Fatalf("defaultMinInventory = %d, want 1: ask whenever there is a candidate", defaultMinInventory)
	}
	if shippedSweep(t, "").minInventory != 1 {
		t.Fatal("the shipped sweep does not ask with one candidate")
	}
	e := shippedSweep(t, "min_inventory: 3\n")
	key, _ := registered(t, e, "ping-min", pingAsker(), newMemStore(), deepCandidates(2))
	if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonBelowMin {
		t.Fatalf("2 candidates against min_inventory 3: ok=%v reason=%q, want below_min_inventory", ok, reason)
	}
}

// POINT 2: with nothing to offer there is no question, and the reason is counted.
func TestNoCandidatesMeansNoQuestion(t *testing.T) {
	e := shippedSweep(t, "")
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("hello"), assistantMsg("hi")}}
	key, _ := registered(t, e, "ping-empty", pingAsker(), newMemStore(), req)
	if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonNoInventory {
		t.Fatalf("ok=%v reason=%q, want no_inventory", ok, reason)
	}
	if _, _, reason, ok := SweepPingSubstitute("never-registered"); ok || reason != SweepReasonNoCandidate {
		t.Fatalf("ok=%v reason=%q, want no_candidate", ok, reason)
	}
}

// ⭐ POINT 3 AND 5: the answer is held per thread in the Store, and it changes NO request. The same
// session replays cg:res: decisions at any depth; a held verdict must not live there.
func TestAHeldAnswerChangesNoRequest(t *testing.T) {
	for _, name := range []string{"shadow on"} {
		cfg := ""
		t.Run(name, func(t *testing.T) {
			asker := pingAsker()
			e := shippedSweep(t, cfg)
			st := newMemStore()
			req := deepCandidates(4)
			key, _ := registered(t, e, "held-"+name, asker, st, req)
			dispatch, _, _, _ := SweepPingSubstitute(key)
			res := dispatch(time.Second, SweepPingWait{WaitingFor: "human"})
			if !res.Held || res.Proposed != 4 {
				t.Fatalf("result = %+v, want a held answer proposing all 4", res)
			}
			asked, proposed, waiting, ok := HeldSweepAnswerForTest(st, key)
			if !ok || asked != 4 || proposed != 4 || waiting != "human" {
				t.Fatalf("held answer = (%d asked, %d proposed, wait %q, ok=%v)", asked, proposed, waiting, ok)
			}
			// THE NEXT REQUEST, on every cache phase there is.
			for _, tc := range []struct {
				name string
				cold bool
				idle int64
			}{{"warm", false, 10_000}, {"cold", true, 10 * 60 * 1000}, {"a minute from expiry", false, 4 * 60 * 1000}} {
				next := deepCandidates(4)
				want := make([]string, len(next.Input))
				for i := range next.Input {
					want[i] = schema.MessageText(next.Input[i])
				}
				c := preExpiryCtx("held-"+name, asker, st)
				c.ColdCache, c.IdleMs = tc.cold, tc.idle
				if _, err := e.Offload(next, &components.Report{}, c); err != nil {
					t.Fatal(err)
				}
				for i := range next.Input {
					if schema.MessageText(next.Input[i]) != want[i] {
						t.Fatalf("%s request: message %d changed. A held answer must not change any request in this PR", tc.name, i)
					}
				}
			}
			// And nothing was written where the replay looks.
			for i := 1; i <= 4; i++ {
				ck := contentKey(schema.MessageText(req.Input[i]))
				if _, hit := st.Get(resultKey("held-"+name, ck)); hit {
					t.Fatalf("output %d has a cg:res: record: the same-session replay would apply it", i)
				}
			}
		})
	}
}

// POINT 5: per thread. A subagent's held answer is not the main thread's.
func TestHeldAnswersAreKeptPerThread(t *testing.T) {
	asker := pingAsker()
	e := shippedSweep(t, "")
	st := newMemStore()
	for _, th := range []string{"", "sub-1"} {
		c := preExpiryCtx("held-threads", asker, st)
		c.Thread = th
		req := deepCandidates(2)
		if th != "" {
			req = deepCandidates(5)
		}
		if _, err := e.Offload(req, &components.Report{}, c); err != nil {
			t.Fatal(err)
		}
		key := thread.Key("held-threads", th)
		t.Cleanup(func() { clearSweepPing(key) })
		dispatch, _, _, ok := SweepPingSubstitute(key)
		if !ok {
			t.Fatalf("thread %q offered no question", th)
		}
		dispatch(time.Second, SweepPingWait{})
	}
	if asked, _, _, _ := HeldSweepAnswerForTest(st, thread.Key("held-threads", "")); asked != 2 {
		t.Errorf("main thread holds %d outputs, want 2", asked)
	}
	if asked, _, _, _ := HeldSweepAnswerForTest(st, thread.Key("held-threads", "sub-1")); asked != 5 {
		t.Errorf("subagent holds %d outputs, want 5", asked)
	}
}

// POINT 5: the record is in a pinned namespace, so LRU pressure cannot evict a paid answer.
func TestTheHeldNamespaceIsPinned(t *testing.T) {
	found := false
	for _, p := range store.DefaultPinPrefixes {
		if p == store.SweepHeldPrefix {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q is not in store.DefaultPinPrefixes", store.SweepHeldPrefix)
	}
	if heldKey("t") == resultKey("t", "x") || strings.HasPrefix(heldKey("t"), store.ResultPrefix) {
		t.Fatal("the held key shares the replay's namespace")
	}
}

// POINT 11 (#422): a model whose cache the question would break gets a plain ping, and the skip is
// counted as sweep_model_declined.
func TestModelsWhereTheQuestionBreaksTheCacheGetAPlainPing(t *testing.T) {
	thinking := func() bschemas.ChatMessage {
		m := assistantMsg("let me look")
		m.ChatAssistantMessage = &bschemas.ChatAssistantMessage{}
		r := "step by step"
		m.ChatAssistantMessage.Reasoning = &r
		return m
	}
	withThinking := func(n int) *bschemas.BifrostChatRequest {
		req := deepCandidates(n)
		req.Input = append([]bschemas.ChatMessage{req.Input[0], thinking()}, req.Input[1:]...)
		req.Input = append(req.Input, assistantMsg("done"))
		return req
	}
	for _, tc := range []struct {
		name     string
		model    string
		thinking bool
		declined bool
	}{
		{"Haiku 4.5 with thinking blocks", "claude-haiku-4-5", true, true},
		{"older Sonnet with thinking blocks", "claude-sonnet-4-5", true, true},
		{"older Opus with thinking blocks", "claude-opus-4-1", true, true},
		{"Haiku 4.5, no thinking blocks to strip", "claude-haiku-4-5", false, false},
		{"current Sonnet keeps all thinking", "claude-sonnet-5", true, false},
		{"current Opus keeps all thinking", "claude-opus-4-8", true, false},
		{"a model the registry does not name", "some-other-model", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := shippedSweep(t, "")
			c := preExpiryCtx("decl-"+tc.name, pingAsker(), newMemStore())
			c.ModelName = tc.model
			req := deepCandidates(3)
			if tc.thinking {
				req = withThinking(3)
			}
			if _, err := e.Offload(req, &components.Report{}, c); err != nil {
				t.Fatal(err)
			}
			key := thread.Key(c.Session, "")
			t.Cleanup(func() { clearSweepPing(key) })
			before := SweepPingCounters().ModelDeclined
			_, _, reason, ok := SweepPingSubstitute(key)
			declined := reason == SweepReasonModelDeclined
			if declined != tc.declined || ok == tc.declined {
				t.Fatalf("ok=%v reason=%q, declined want %v", ok, reason, tc.declined)
			}
			got := SweepPingCounters().ModelDeclined - before
			if (got == 1) != tc.declined {
				t.Fatalf("sweep_model_declined moved by %d, want it to move exactly when declined (%v)", got, tc.declined)
			}
		})
	}
}

// FAIL OPEN: a question that cannot read the cache is never repaired with the full-price fallback.
// A ping has no request to fall back for, and the fallback would copy output content into a fresh
// prompt: the cost of the whole transcript where a cache read was meant.
func TestThePingQuestionNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		asker *labelAsker
	}{
		{"the cache read nothing", func() *labelAsker { a := pingAsker(); a.cacheRead = 0; return a }()},
		{"the ask failed", func() *labelAsker { a := pingAsker(); a.err = errors.New("upstream 529"); return a }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := &pingFallbackModel{}
			e := shippedSweep(t, "")
			c := preExpiryCtx("nofb-"+tc.name, tc.asker, newMemStore())
			c.Model = components.ModelSpec{Incoming: fb, Static: fb}
			if _, err := e.Offload(deepCandidates(3), &components.Report{}, c); err != nil {
				t.Fatal(err)
			}
			key := thread.Key(c.Session, "")
			t.Cleanup(func() { clearSweepPing(key) })
			// Registration does not copy the request model, so the fallback has none to call. Give
			// the registered context one, or this test passes without noFallback (found by mutation).
			sweepPingMu.Lock()
			sweepPingCand[key].ctx.Model = components.ModelSpec{Incoming: fb, Static: fb}
			sweepPingMu.Unlock()
			dispatch, _, _, ok := SweepPingSubstitute(key)
			if !ok {
				t.Fatal("no question offered")
			}
			res := dispatch(time.Second, SweepPingWait{})
			if fb.calls.Load() != 0 {
				t.Fatalf("the fallback model was called %d times from a ping", fb.calls.Load())
			}
			if res.Substituted {
				t.Fatalf("a question that did not use the cache was counted as the ping: %+v", res)
			}
			if _, _, _, held := HeldSweepAnswerForTest(c.Store, key); held {
				t.Fatal("an answer that never arrived was held")
			}
		})
	}
}

type pingFallbackModel struct{ calls atomic.Int64 }

func (m *pingFallbackModel) Complete(context.Context, string) (string, error) {
	m.calls.Add(1)
	return "[]", nil
}

// A question that reads the cache but whose reply cannot be parsed still refreshed the prefix, so it
// stands in for the ping; nothing is held.
func TestAnUnparseableReplyStillRefreshesButHoldsNothing(t *testing.T) {
	asker := &fakeAsker{reply: "I cannot decide.", cacheRead: 19595}
	e := shippedSweep(t, "")
	st := newMemStore()
	key, _ := registered(t, e, "unparse", asker, st, deepCandidates(3))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	res := dispatch(time.Second, SweepPingWait{})
	if !res.Substituted || res.Held {
		t.Fatalf("result = %+v, want substituted and not held", res)
	}
	if _, _, _, ok := HeldSweepAnswerForTest(st, key); ok {
		t.Fatal("an unparseable reply was held")
	}
}

// "Keep everything" is an answer. It is held, so asked and proposed are both countable.
func TestKeepEverythingIsAHeldAnswer(t *testing.T) {
	asker := &labelAsker{verdict: "keep", needed: "a", quote: "Find the auth timeout"}
	asker.cacheRead = 19595
	e := shippedSweep(t, "")
	st := newMemStore()
	key, _ := registered(t, e, "keepall", asker, st, deepCandidates(3))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	res := dispatch(time.Second, SweepPingWait{WaitingFor: "own_tool", WaitTool: "Bash"})
	asked, proposed, waiting, ok := HeldSweepAnswerForTest(st, key)
	if !res.Held || !ok || asked != 3 || proposed != 0 || waiting != "own_tool" {
		t.Fatalf("res=%+v held=(%d asked, %d proposed, %q, ok=%v)", res, asked, proposed, waiting, ok)
	}
}

// POINT 12: the wait cause is stored with the answer. Nothing reads it to decide.
func TestTheWaitCauseIsStoredWithTheAnswer(t *testing.T) {
	asker := pingAsker()
	e := shippedSweep(t, "")
	st := newMemStore()
	key, _ := registered(t, e, "waitcause", asker, st, deepCandidates(2))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	dispatch(time.Second, SweepPingWait{WaitingFor: "own_tool", WaitTool: "Bash"})
	h, ok := getHeld(st, key)
	if !ok || h.WaitingFor != "own_tool" || h.WaitTool != "Bash" {
		t.Fatalf("held = %+v ok=%v, want the wait cause stored", h, ok)
	}
}

// Every judged output keeps its tool_use id and a content hash of the message as the agent sent it,
// and the length of the conversation it was judged in.
func TestTheHeldAnswerKeepsIdHashAndLength(t *testing.T) {
	asker := pingAsker()
	e := shippedSweep(t, "")
	st := newMemStore()
	req := deepCandidates(3)
	key, _ := registered(t, e, "idhash", asker, st, req)
	dispatch, _, _, _ := SweepPingSubstitute(key)
	dispatch(time.Second, SweepPingWait{})
	h, ok := getHeld(st, key)
	if !ok || len(h.Outputs) != 3 {
		t.Fatalf("held = %+v ok=%v", h, ok)
	}
	if h.Judged != len(req.Input) {
		t.Errorf("judged length = %d, want %d", h.Judged, len(req.Input))
	}
	for _, o := range h.Outputs {
		idx, found := rawToolMessage(req.Input, o.ToolUseID)
		if !found {
			t.Fatalf("held id %q is not in the conversation", o.ToolUseID)
		}
		if o.Hash != outputHash(req.Input[idx]) || o.Hash == "" {
			t.Errorf("hash for %q = %q, want the SpanHash of the agent's message", o.ToolUseID, o.Hash)
		}
		if !o.Drop || o.Saves <= 0 || o.Tokens <= o.Saves {
			t.Errorf("output %+v: want a drop that saves some of its tokens", o)
		}
	}
}

// An output with no tool id cannot be found again, so it is not held (and is counted).
func TestAnOutputWithNoToolIDIsNotHeld(t *testing.T) {
	asker := pingAsker()
	e := shippedSweep(t, "")
	st := newMemStore()
	req := deepCandidates(2)
	req.Input = append(req.Input, toolResultMsg(strings.Repeat("anonymous record line\n", 900)))
	key, _ := registered(t, e, "noid", asker, st, req)
	dispatch, _, _, _ := SweepPingSubstitute(key)
	before := SweepPingCounters().HoldNoToolID
	dispatch(time.Second, SweepPingWait{})
	if asked, _, _, _ := HeldSweepAnswerForTest(st, key); asked != 2 {
		t.Errorf("held %d outputs, want the 2 with ids", asked)
	}
	if SweepPingCounters().HoldNoToolID-before != 1 {
		t.Error("the output with no id was not counted")
	}
}

// Registration is bounded and cleared by the paths the keeper already uses.
func TestRegistrationIsClearedByTheKeepAliveClearers(t *testing.T) {
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "clearing", pingAsker(), newMemStore(), deepCandidates(2))
	if _, _, _, ok := SweepPingSubstitute(key); !ok {
		t.Fatal("not registered")
	}
	ClearKeepAliveCandidate(key)
	if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonNoCandidate {
		t.Fatalf("ClearKeepAliveCandidate left the sweep registered (ok=%v reason=%q)", ok, reason)
	}
	registered(t, e, "clearing", pingAsker(), newMemStore(), deepCandidates(2))
	ClearStaleKeepAliveCandidate(key, time.Now().Add(time.Minute))
	if _, _, _, ok := SweepPingSubstitute(key); ok {
		t.Fatal("ClearStaleKeepAliveCandidate left an older registration")
	}
	registered(t, e, "clearing", pingAsker(), newMemStore(), deepCandidates(2))
	ClearStaleKeepAliveCandidate(key, time.Now().Add(-time.Minute))
	if _, _, _, ok := SweepPingSubstitute(key); !ok {
		t.Fatal("ClearStaleKeepAliveCandidate cleared a registration made by THIS request")
	}
}

// A request on a route with no prefix asker, or one the host marked as observe-only, registers nothing.
func TestNoAskerOrObserveModeRegistersNothing(t *testing.T) {
	e := shippedSweep(t, "")
	for _, tc := range []struct {
		name string
		mod  func(*components.Ctx)
	}{
		{"no prefix asker", func(c *components.Ctx) { c.PrefixAsk = nil }},
		{"observe mode", func(c *components.Ctx) { c.Mode = components.ModeObserve }},
		{"bypass", func(c *components.Ctx) { c.Bypass = true }},
	} {
		c := preExpiryCtx("noreg-"+tc.name, pingAsker(), newMemStore())
		tc.mod(c)
		if _, err := e.Offload(deepCandidates(2), &components.Report{}, c); err != nil {
			t.Fatal(err)
		}
		if _, _, reason, ok := SweepPingSubstitute(thread.Key(c.Session, "")); ok || reason != SweepReasonNoCandidate {
			t.Errorf("%s: ok=%v reason=%q, want nothing registered", tc.name, ok, reason)
		}
	}
}

// The ping-time question reaches the stale-prefix guard with every candidate inside the cached
// prefix: the stash holds the registering request's whole body by the time a ping falls due.
func TestThePingChecksCoverageOfTheDeepestCandidate(t *testing.T) {
	asker := &coverAsker{}
	asker.verdict, asker.needed, asker.cacheRead = "drop", "none", 19595
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "cover", asker, newMemStore(), deepCandidates(3))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	res := dispatch(time.Second, SweepPingWait{})
	if asker.checked != 4 {
		t.Fatalf("coverage was checked for %d messages, want the whole conversation up to the deepest candidate (4)", asker.checked)
	}
	if res.Held {
		t.Fatal("a stale stash must not produce a held answer")
	}
	if atomic.LoadInt64(&asker.calls) != 0 {
		t.Fatal("asked against a stash that does not cover the candidates")
	}
}

type coverAsker struct {
	labelAsker
	checked int
}

func (c *coverAsker) CoversSpan(_ string, count int, _ string) bool {
	if count > c.checked {
		c.checked = count
	}
	return false
}

// ⭐ Until PR C, the ping stays plain unless shadow mode is on: a held answer is used by nothing, so
// asking would only cost money. The reason is counted as sweep_ask_needs_shadow.
func TestWithShadowOffThePingStaysPlain(t *testing.T) {
	c, err := newExtractSweep([]byte("min_tokens: 2000\n")) // as shipped: shadow off
	if err != nil {
		t.Fatal(err)
	}
	e := c.(*ExtractSweep)
	asker := pingAsker()
	key, _ := registered(t, e, "needs-shadow", asker, newMemStore(), deepCandidates(3))
	before := SweepPingCounters().NeedsShadow
	if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonNeedsShadow {
		t.Fatalf("ok=%v reason=%q, want ask_needs_shadow", ok, reason)
	}
	if SweepPingCounters().NeedsShadow-before != 1 {
		t.Error("sweep_ask_needs_shadow was not counted")
	}
	if asker.calls != 0 {
		t.Errorf("asked %d times with shadow off", asker.calls)
	}
}

// panicAsker panics inside Ask, as a bug in the client or the verdict code would.
type panicAsker struct{ calls atomic.Int64 }

func (a *panicAsker) Ask(context.Context, string, string) (string, components.PrefixUsage, error) {
	a.calls.Add(1)
	panic("boom in Ask")
}

// panicStore panics on every Get once armed. Collection reads the Store for each output, so this
// panics inside collect.
type panicStore struct {
	store.Store
	armed atomic.Bool
}

func (s *panicStore) Get(k string) ([]byte, bool) {
	if s.armed.Load() {
		panic("boom in Store.Get")
	}
	return s.Store.Get(k)
}

// ⭐ FAIL OPEN (review round 1, blocking). The question runs on the keeper's own goroutine, which no
// pipeline recover covers. A panic anywhere in it must leave the ping a plain ping, count the panic,
// and keep the process up. Both the ask and the collection are covered.
func TestAPanicInThePingQuestionLeavesAPlainPing(t *testing.T) {
	t.Run("in Ask", func(t *testing.T) {
		asker := &panicAsker{}
		e := shippedSweep(t, "")
		key, _ := registered(t, e, "panic-ask", asker, newMemStore(), deepCandidates(3))
		before := SweepPingCounters().Panics
		dispatch, _, _, ok := SweepPingSubstitute(key)
		if !ok {
			t.Fatal("no question offered")
		}
		res := dispatch(time.Second, SweepPingWait{}) // must not panic
		if res.Substituted || res.Held || res.Reason != SweepReasonPanic {
			t.Fatalf("result = %+v, want a plain ping with reason panic", res)
		}
		if SweepPingCounters().Panics-before != 1 {
			t.Error("the panic was not counted")
		}
		// The conversation stays marked asked, so the next ping is plain too.
		if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonAlreadyAsked {
			t.Fatalf("after a panic: ok=%v reason=%q, want already_asked", ok, reason)
		}
		// The process continues: another thread still gets its question.
		other := &labelAsker{verdict: "drop", needed: "none"}
		other.cacheRead = 19595
		key2, _ := registered(t, e, "panic-ask-other", other, newMemStore(), deepCandidates(2))
		if d, _, _, ok := SweepPingSubstitute(key2); !ok || !d(time.Second, SweepPingWait{}).Held {
			t.Fatal("a panic on one thread broke the next thread's question")
		}
	})
	t.Run("in collect", func(t *testing.T) {
		st := &panicStore{Store: newMemStore()}
		e := shippedSweep(t, "")
		key, _ := registered(t, e, "panic-collect", pingAsker(), st, deepCandidates(3))
		st.armed.Store(true)
		before := SweepPingCounters().Panics
		dispatch, _, reason, ok := SweepPingSubstitute(key) // must not panic
		st.armed.Store(false)
		if ok || dispatch != nil || reason != SweepReasonPanic {
			t.Fatalf("ok=%v reason=%q, want a plain ping with reason panic", ok, reason)
		}
		if SweepPingCounters().Panics-before != 1 {
			t.Error("the panic was not counted")
		}
		if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonAlreadyAsked {
			t.Fatalf("after a collect panic: ok=%v reason=%q, want already_asked", ok, reason)
		}
	})
}

// ⭐ H3. A held answer never reaches another thread, a side request (p:) or a fork (a:). Each thread's
// registration, stash read, hold and shadow report use thread.Key(session, thread).
func TestHeldAnswerNeverReachesAnotherThread(t *testing.T) {
	asker := pingAsker()
	e := shippedSweep(t, "")
	st := newMemStore()
	main := preExpiryCtx("h3", asker, st)
	if _, err := e.Offload(refConversation(3), &components.Report{}, main); err != nil {
		t.Fatal(err)
	}
	mainKey := thread.Key("h3", "")
	t.Cleanup(func() { clearSweepPing(mainKey) })
	dispatch, _, _, ok := SweepPingSubstitute(mainKey)
	if !ok {
		t.Fatal("main thread offered no question")
	}
	dispatch(time.Second, SweepPingWait{})
	if _, ok := getHeld(st, mainKey); !ok {
		t.Fatal("main thread holds nothing")
	}
	for _, th := range []string{"p:side", "a:fork", "sub-1"} {
		if _, ok := getHeld(st, thread.Key("h3", th)); ok {
			t.Errorf("thread %q reads the main thread's held answer", th)
		}
		// A COLD request on that thread, with the same session and store, reports and consumes nothing.
		c := preExpiryCtx("h3", asker, st)
		c.Thread, c.ColdCache = th, true
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(refConversation(3), rep, c); err != nil {
			t.Fatal(err)
		}
		for ev := range rep.Events {
			if strings.HasPrefix(ev, "sweep_shadow_") {
				t.Errorf("thread %q reported the main thread's answer: %s", th, ev)
			}
		}
		clearSweepPing(thread.Key("h3", th))
	}
	if _, ok := getHeld(st, mainKey); !ok {
		t.Fatal("another thread's request consumed the main thread's answer")
	}
}

// ⭐ H1, THE MAIN TEST. After an answer is held, every warm request is byte-identical to the same
// request with the sweep off. "Byte-identical" is measured on the FULL serialized request (model,
// params and every message, not only message text), over several requests that grow the conversation,
// on every cache phase a warm request can have.
func TestH1EveryRequestAfterAnAnswerIsByteIdenticalToTheSweepOff(t *testing.T) {
	temp := 0.2
	build := func(n int) *bschemas.BifrostChatRequest {
		req := refConversation(3)
		for i := 0; i < n; i++ {
			req.Input = append(req.Input, assistantMsg(fmt.Sprintf("step %d", i)), userMsg("ok"))
		}
		req.Provider, req.Model = bschemas.Anthropic, "claude-sonnet-5"
		req.Params = &bschemas.ChatParameters{Temperature: &temp}
		return req
	}
	asker := pingAsker()
	e := shippedSweep(t, "")
	st := newMemStore()
	key, _ := registered(t, e, "h1", asker, st, build(0))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	if res := dispatch(time.Second, SweepPingWait{}); !res.Held || res.Proposed != 3 {
		t.Fatalf("fixture: want a held answer proposing 3 drops, got %+v", res)
	}
	phases := []struct {
		name string
		mod  func(*components.Ctx)
	}{
		{"warm, just after the ping", func(c *components.Ctx) { c.IdleMs = 5_000 }},
		{"warm, a minute from expiry", func(c *components.Ctx) { c.IdleMs = 4 * 60 * 1000 }},
		{"warm, deep cache boundary", func(c *components.Ctx) { c.IdleMs = 10_000; c.MaxCachedIdx = 3 }},
	}
	for n := 1; n <= 4; n++ {
		for _, ph := range phases {
			want, err := json.Marshal(build(n)) // the sweep off: the request untouched
			if err != nil {
				t.Fatal(err)
			}
			got := build(n)
			c := preExpiryCtx("h1", asker, st)
			ph.mod(c)
			if _, err := e.Offload(got, &components.Report{}, c); err != nil {
				t.Fatal(err)
			}
			gotB, _ := json.Marshal(got)
			if !bytes.Equal(gotB, want) {
				t.Fatalf("request %d (%s) differs from the sweep-off request:\n got %.200s\nwant %.200s", n, ph.name, gotB, want)
			}
		}
	}
	if _, ok := getHeld(st, key); !ok {
		t.Fatal("fixture: the answer must still be held after warm requests")
	}
}

// Review round 1, C: the concurrency bound is checked BEFORE candidate collection, so many due threads
// cannot all run the hashing and the co-reference pass at once.
func TestTheConcurrencyBoundIsCheckedBeforeCollection(t *testing.T) {
	st := &panicStore{Store: newMemStore()}
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "slots-first", pingAsker(), st, deepCandidates(3))
	for i := 0; i < cap(sweepSlots); i++ {
		sweepSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(sweepSlots); i++ {
			<-sweepSlots
		}
	}()
	st.armed.Store(true) // collection would read the Store and panic
	before := SweepPingCounters().Panics
	_, _, reason, ok := SweepPingSubstitute(key)
	st.armed.Store(false)
	if ok || reason != SweepReasonConcurrencyFull {
		t.Fatalf("ok=%v reason=%q, want concurrency_full", ok, reason)
	}
	if SweepPingCounters().Panics != before {
		t.Fatal("collection ran although every slot was taken")
	}
}
