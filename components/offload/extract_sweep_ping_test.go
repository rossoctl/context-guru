package offload

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// shippedSweep builds the sweep as the registry builds it for an operator, with the shipped inventory
// floor. The helpers in extract_sweep_test.go
// keep the old floor of 10; these tests are about the ping-time question and use the shipped one.
func shippedSweep(t *testing.T, extra string) *ExtractSweep {
	t.Helper()
	c, err := newExtractSweep([]byte("min_tokens: 2000\n" + extra))
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

// POINT 2: with nothing to offer there is no question, and the reason is counted. A conversation with
// no output at the floor registers nothing (#508 review D), so it reads as no_candidate; one whose big
// outputs are all already markers registers, and reads as no_inventory.
func TestNoCandidatesMeansNoQuestion(t *testing.T) {
	e := shippedSweep(t, "")
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("hello"), assistantMsg("hi")}}
	key, _ := registered(t, e, "ping-empty", pingAsker(), newMemStore(), req)
	if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonNoCandidate {
		t.Fatalf("ok=%v reason=%q, want no_candidate (nothing was big enough to register)", ok, reason)
	}
	marked := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("go")}}
	markedStore := newMemStore()
	for i := 0; i < 2; i++ {
		markedStore.Put(fmt.Sprintf("abcdef%d", i), []byte("the stashed original")) // a real marker has one
		marked.Input = append(marked.Input, toolResultMsgWithID(fmt.Sprintf("toolu_m%d", i),
			strings.Repeat("a spent line\n", 900)+expand.Marker(fmt.Sprintf("abcdef%d", i))))
	}
	key2, _ := registered(t, e, "ping-marked", pingAsker(), markedStore, marked)
	if _, _, reason, ok := SweepPingSubstitute(key2); ok || reason != SweepReasonNoInventory {
		t.Fatalf("ok=%v reason=%q, want no_inventory (big outputs, all already markers)", ok, reason)
	}
	if _, _, reason, ok := SweepPingSubstitute("never-registered"); ok || reason != SweepReasonNoCandidate {
		t.Fatalf("ok=%v reason=%q, want no_candidate", ok, reason)
	}
}

// BOUND (#508 review D): a conversation that cannot hold a candidate keeps no copy, and an old
// registration goes at the next registration, not only at the cap.
func TestRegisterPingKeepsNoCopyOfAThreadThatCannotHaveACandidate(t *testing.T) {
	e := shippedSweep(t, "")
	st := newMemStore()
	asker := pingAsker()
	key, _ := registered(t, e, "bound-big", asker, st, deepCandidates(2))
	sweepPingMu.Lock()
	_, has := sweepPingCand[key]
	sweepPingMu.Unlock()
	if !has {
		t.Fatal("fixture: a conversation with big outputs did not register")
	}
	// The same thread's next request has shrunk below the floor (a /compact): the copy is dropped.
	small := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("hi"), assistantMsg("ok")}}
	c := preExpiryCtx("bound-big", asker, st)
	if _, err := e.Offload(small, &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	sweepPingMu.Lock()
	_, has = sweepPingCand[key]
	sweepPingMu.Unlock()
	if has {
		t.Error("a thread with nothing big kept its old conversation copy")
	}
	// An old registration of ANOTHER thread goes at this registration, below the cap.
	sweepPingMu.Lock()
	sweepPingCand["bound-stale"] = &sweepPingCandidate{registeredAt: time.Now().Add(-2 * maxSweepPingAge)}
	sweepPingMu.Unlock()
	t.Cleanup(func() { clearSweepPing("bound-stale") })
	registered(t, e, "bound-fresh", asker, st, deepCandidates(2))
	sweepPingMu.Lock()
	_, stale := sweepPingCand["bound-stale"]
	sweepPingMu.Unlock()
	if stale {
		t.Error("a registration older than maxSweepPingAge survived a new registration below the cap")
	}
}

// ⭐ H1 (warm half): a held answer changes NO request while the cache is warm. The cold request is the
// apply's (extract_sweep_apply_test.go). With shadow on, even a cold request changes nothing.
func TestAHeldAnswerChangesNoRequest(t *testing.T) {
	for _, name := range []string{"shadow off", "shadow on"} {
		cfg := ""
		if name == "shadow on" {
			cfg = "shadow: true\n"
		}
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
			phases := []struct {
				name string
				cold bool
				idle int64
			}{{"warm", false, 10_000}, {"a minute from expiry", false, 4 * 60 * 1000}}
			if name == "shadow on" {
				phases = append(phases, struct {
					name string
					cold bool
					idle int64
				}{"cold", true, 10 * 60 * 1000})
			}
			for _, tc := range phases {
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
						t.Fatalf("%s request: message %d changed", tc.name, i)
					}
				}
			}
			// And nothing was written where a replay looks.
			for i := 1; i <= 4; i++ {
				ck := contentKey(schema.MessageText(req.Input[i]))
				if _, hit := st.Get(resultKey("held-"+name, ck)); hit {
					t.Fatalf("output %d has a cg:res: record", i)
				}
				if _, hit := getDrop(st, key, ck); hit {
					t.Fatalf("output %d has a drop record before any cold apply", i)
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
		// Review round 1: a thread the keep-alive will never ping keeps no conversation copy.
		{"keep-alive off for the thread", func(c *components.Ctx) { c.CanKeepAlive = func() bool { return false } }},
		{"no keep-alive hook at all", func(c *components.Ctx) { c.CanKeepAlive = nil }},
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

// ⭐ POINT 5 (PR C): the shadow-only gate is gone. With shadow OFF, as shipped, the ping asks and holds.
func TestTheSweepAsksAtThePingWithShadowOff(t *testing.T) {
	c, err := newExtractSweep([]byte("min_tokens: 2000\n")) // as shipped: shadow off
	if err != nil {
		t.Fatal(err)
	}
	e := c.(*ExtractSweep)
	if e.shadow {
		t.Fatal("fixture: shadow is on")
	}
	asker := pingAsker()
	st := newMemStore()
	key, _ := registered(t, e, "no-shadow-gate", asker, st, deepCandidates(3))
	dispatch, _, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered with shadow off: %s", reason)
	}
	if res := dispatch(time.Second, SweepPingWait{}); !res.Held || res.Proposed != 3 {
		t.Fatalf("result = %+v, want a held answer proposing 3 drops", res)
	}
	if _, ok := getHeld(st, key); !ok {
		t.Fatal("nothing held")
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

// Review round 1, finding 1: with keep-alive off for the thread, NOTHING is registered, so no
// conversation copy is held, and the miss is counted.
func TestWithKeepAliveOffNothingIsRegistered(t *testing.T) {
	e := shippedSweep(t, "")
	c := preExpiryCtx("ka-off", pingAsker(), newMemStore())
	c.CanKeepAlive = func() bool { return false }
	before := SweepPingCounters().NotRegistered
	if _, err := e.Offload(deepCandidates(3), &components.Report{}, c); err != nil {
		t.Fatal(err)
	}
	key := thread.Key("ka-off", "")
	sweepPingMu.Lock()
	_, held := sweepPingCand[key]
	sweepPingMu.Unlock()
	if held {
		t.Fatal("a conversation copy was kept for a thread the keep-alive cannot ping")
	}
	if SweepPingCounters().NotRegistered-before != 1 {
		t.Error("the unregistered request was not counted")
	}
	// And with keep-alive on, the same request registers (the control, so the test cannot pass by
	// never registering anything).
	c2 := preExpiryCtx("ka-on", pingAsker(), newMemStore())
	if _, err := e.Offload(deepCandidates(3), &components.Report{}, c2); err != nil {
		t.Fatal(err)
	}
	key2 := thread.Key("ka-on", "")
	t.Cleanup(func() { clearSweepPing(key2) })
	sweepPingMu.Lock()
	_, held2 := sweepPingCand[key2]
	sweepPingMu.Unlock()
	if !held2 {
		t.Fatal("control: keep-alive on, but nothing was registered")
	}
}

// Review round 1, finding 2: the collection slot is TAKEN, not looked at, so the bound holds for N
// goroutines at once. 16 due threads start together behind a barrier; collection is counted; at most
// cap(sweepSlots) may run it at the same time.
func TestCollectionRunsAtMostSlotsAtOnce(t *testing.T) {
	const threads = 16
	e := shippedSweep(t, "")
	keys := make([]string, threads)
	for i := 0; i < threads; i++ {
		sess := fmt.Sprintf("race-%d", i)
		// A big conversation, so collection takes long enough for the goroutines to overlap.
		st := newMemStore()
		c := preExpiryCtx(sess, pingAsker(), st)
		if _, err := e.Offload(deepCandidates(40), &components.Report{}, c); err != nil {
			t.Fatal(err)
		}
		keys[i] = thread.Key(sess, "")
		k := keys[i]
		t.Cleanup(func() { clearSweepPing(k) })
	}
	collectPeak.Store(0)
	var start, done sync.WaitGroup
	start.Add(1)
	var offered, full atomic.Int64
	for i := 0; i < threads; i++ {
		done.Add(1)
		go func(k string) {
			defer done.Done()
			start.Wait()
			_, _, reason, ok := SweepPingSubstitute(k)
			if ok {
				offered.Add(1)
			} else if reason == SweepReasonConcurrencyFull {
				full.Add(1)
			}
		}(keys[i])
	}
	start.Done()
	done.Wait()
	if peak := collectPeak.Load(); peak > int64(cap(sweepSlots)) {
		t.Fatalf("%d collections ran at once, want at most %d", peak, cap(sweepSlots))
	}
	if peak := collectPeak.Load(); peak == 0 {
		t.Fatal("no collection ran: the test proves nothing")
	}
	if offered.Load()+full.Load() == 0 {
		t.Fatal("no thread was either offered or refused")
	}
	// Every slot is back: a leak here would starve later pings forever.
	if len(sweepSlots) != 0 {
		t.Fatalf("%d slots still held after every call returned", len(sweepSlots))
	}
}

// A slot taken for collection is released on every exit, including a decline and a keeper that never
// calls dispatch (the cost ceiling): offered-but-not-dispatched must not hold a slot.
func TestACollectionSlotIsReleasedWhenDispatchIsNeverCalled(t *testing.T) {
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "slot-release", pingAsker(), newMemStore(), deepCandidates(3))
	for i := 0; i < 2*cap(sweepSlots); i++ {
		if _, _, reason, ok := SweepPingSubstitute(key); !ok {
			t.Fatalf("call %d refused (%s): a slot leaked", i, reason)
		}
	}
	if len(sweepSlots) != 0 {
		t.Fatalf("%d slots still held", len(sweepSlots))
	}
}

// Review round 2, finding 1: the lookup that found no stored decision is a MISS for the result cache's
// hit rate, recorded at collection as the in-request loop used to.
func TestCollectionRecordsTheResultCacheMiss(t *testing.T) {
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "miss-metric", pingAsker(), newMemStore(), deepCandidates(3))
	before := metrics.ExtractSnapshot(0, 0, 0, 0).ByComponent["extract_llm_sweep"]
	var b int64
	if before != nil {
		b = before.CacheLookups
	}
	if _, _, _, ok := SweepPingSubstitute(key); !ok {
		t.Fatal("no question offered")
	}
	after := metrics.ExtractSnapshot(0, 0, 0, 0).ByComponent["extract_llm_sweep"]
	if after == nil || after.CacheLookups-b != 3 {
		var got int64
		if after != nil {
			got = after.CacheLookups
		}
		t.Fatalf("collection of 3 candidates recorded %d lookups, want 3", got-b)
	}
	if after.CacheHitRate >= 1 && after.CacheLookups > 0 && after.CallsAvoided >= after.CacheLookups {
		t.Errorf("the hit rate reads %.2f with only hits recorded", after.CacheHitRate)
	}
}

// Review round 2, finding 2: a refusal because every slot is busy is counted.
func TestAConcurrencyRefusalIsCounted(t *testing.T) {
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "full-count", pingAsker(), newMemStore(), deepCandidates(2))
	for i := 0; i < cap(sweepSlots); i++ {
		sweepSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(sweepSlots); i++ {
			<-sweepSlots
		}
	}()
	before := SweepPingCounters().ConcurrencyFull
	if _, _, reason, ok := SweepPingSubstitute(key); ok || reason != SweepReasonConcurrencyFull {
		t.Fatalf("ok=%v reason=%q, want concurrency_full", ok, reason)
	}
	if SweepPingCounters().ConcurrencyFull-before != 1 {
		t.Error("sweep_concurrency_full did not move")
	}
}

// Review round 1, non-blocking: the shadow report happens once per held answer even when two requests
// of the thread arrive together.
func TestTheShadowAskIsReportedOnceUnderConcurrentRequests(t *testing.T) {
	e := shippedSweep(t, "shadow: true\n")
	st := newMemStore()
	key, _ := registered(t, e, "report-once", pingAsker(), st, deepCandidates(3))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	if !dispatch(time.Second, SweepPingWait{}).Held {
		t.Fatal("no held answer")
	}
	const n = 16
	// A store that pauses between a read and the next write widens the get-then-put window, so a claim
	// that is not atomic is caught every time rather than by luck.
	slow := &slowGetStore{Store: st}
	var start, done sync.WaitGroup
	start.Add(1)
	var reported atomic.Int64
	for i := 0; i < n; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			rep := &components.Report{Component: "extract_llm_sweep"}
			c := preExpiryCtx("report-once", pingAsker(), slow)
			c.IdleMs = 5_000
			e.shadowStep(c, &bschemas.BifrostChatRequest{Input: deepCandidates(3).Input}, rep)
			reported.Add(int64(rep.Events[shadowAsked]))
		}()
	}
	start.Done()
	done.Wait()
	// shadowAsked carries len(Outputs) (3) from the one request that won the claim.
	if reported.Load() != 3 {
		t.Fatalf("shadow_asked summed to %d across %d concurrent requests, want 3 (one report of 3 outputs)", reported.Load(), n)
	}
}

// slowGetStore sleeps after every Get, so two goroutines that both read before either writes overlap.
type slowGetStore struct{ store.Store }

func (s *slowGetStore) Get(k string) ([]byte, bool) {
	b, ok := s.Store.Get(k)
	time.Sleep(5 * time.Millisecond)
	return b, ok
}

// The cold recheck is reported once too: two cold requests of one thread arriving together do not both
// report it. The claim that consumes the answer is atomic.
func TestTheColdRecheckIsReportedOnceUnderConcurrentRequests(t *testing.T) {
	e := shippedSweep(t, "shadow: true\n")
	st := newMemStore()
	key, _ := registered(t, e, "recheck-once", pingAsker(), st, deepCandidates(3))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	if !dispatch(time.Second, SweepPingWait{}).Held {
		t.Fatal("no held answer")
	}
	slow := &slowGetStore{Store: st}
	const n = 16
	var start, done sync.WaitGroup
	start.Add(1)
	var rechecks atomic.Int64
	for i := 0; i < n; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			rep := &components.Report{Component: "extract_llm_sweep"}
			c := preExpiryCtx("recheck-once", pingAsker(), slow)
			c.ColdCache = true
			e.shadowStep(c, &bschemas.BifrostChatRequest{Input: deepCandidates(3).Input}, rep)
			if _, ok := rep.Events[shadowValid]; ok {
				rechecks.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()
	if rechecks.Load() != 1 {
		t.Fatalf("%d of %d concurrent cold requests reported the recheck, want exactly 1", rechecks.Load(), n)
	}
}

// #721: the sweep judges the question by what it READ against the last turn's read, not by its write.
func TestThePingGateJudgesTheReadAgainstTheLastTurn(t *testing.T) {
	for _, tc := range []struct {
		name              string
		read, write, turn int
		held              bool
		gate              string
	}{
		{"A5: read equals the turn's, small write", 25512, 53, 25512, true, ""},
		{"a tail write after a text turn is fine", 25516, 83, 25516, true, ""},
		{"model switch: read far below the turn", 13440, 12000, 41886, false, "sweep_prefix_cache_read_below_turn"},
		{"read of zero", 0, 0, 25512, false, "sweep_prefix_cache_read_ZERO"},
		{"typed prompt: a 300-token tail write passes", 25516, 300, 25516, true, ""},
		{"large write is refused by the cap", 25512, 12142, 25512, false, "sweep_prefix_cache_write"},
		{"unknown turn read: read above 0 and a small write pass", 13440, 100, 0, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker := pingAsker()
			asker.cacheRead, asker.cacheWrite = tc.read, tc.write
			e := shippedSweep(t, "")
			key, _ := registered(t, e, "gate-721-"+tc.name, asker, newMemStore(), deepCandidates(2))
			dispatch, _, reason, ok := SweepPingSubstitute(key)
			if !ok {
				t.Fatalf("no question offered (reason %q)", reason)
			}
			res := dispatch(time.Second, SweepPingWait{WaitingFor: "own_tool", WaitTool: "Bash", LastTurnRead: tc.turn})
			if res.Held != tc.held {
				t.Fatalf("held = %v, want %v (gates %v)", res.Held, tc.held, res.Gates)
			}
			if tc.gate != "" && res.Gates[tc.gate] == 0 {
				t.Errorf("gate %s not recorded (gates %v)", tc.gate, res.Gates)
			}
			if tc.gate == "" && res.Gates["sweep_prefix_cache_write"] != 0 {
				t.Errorf("a write was refused as a rebuild (gates %v)", res.Gates)
			}
		})
	}
}

// #721: a tool output that QUOTES marker text is a candidate. A marker the sweep made is not.
func TestThePingOffersAnOutputThatOnlyQuotesAMarker(t *testing.T) {
	st := newMemStore()
	req := deepCandidates(2)
	quote := "grep result: <<cg:notinstore123>> appears in expand.go\n"
	real := "<<cg:realmarker456>>\n"
	st.Put("realmarker456", []byte("the original"))
	req.Input[1] = toolResultMsgWithID("toolu_wire_0", quote+strings.Repeat("candidate 0 distinct line\n", 900))
	req.Input[2] = toolResultMsgWithID("toolu_wire_1", real+strings.Repeat("candidate 1 distinct line\n", 900))
	asker := pingAsker()
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "quote-721", asker, st, req)
	dispatch, info, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered (reason %q)", reason)
	}
	if info.Offered != 1 {
		t.Fatalf("offered %d, want exactly the quoting output", info.Offered)
	}
	_ = dispatch
}
