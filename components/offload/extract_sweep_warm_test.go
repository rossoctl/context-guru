package offload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// Tests for the WARM PATH (#491).

const warmCfg = "warm: true\nwarm_every_n: 3\nwarm_min_age_turns: 5\nwarm_max_turns: 60\n"

// tailOf is a text of about n tokens.
func tailOf(n int) string {
	unit := "alpha beta gamma delta epsilon zeta eta theta "
	per := schema.TextTokens(unit)
	if per < 1 {
		per = 1
	}
	return strings.Repeat(unit, n/per+1)
}

// warmConv is the conversation at step `extra`: 3 big outputs, one tail message after them, then
// `extra` exchanges. Built twice from the same arguments it is byte-identical.
func warmConv(extra, tailTokens int) *bschemas.BifrostChatRequest {
	return warmConvN(3, extra, tailTokens)
}

// warmConvN is warmConv with n tool outputs.
func warmConvN(n, extra, tailTokens int) *bschemas.BifrostChatRequest {
	req := refConversation(n)
	req.Provider, req.Model = bschemas.Anthropic, "claude-sonnet-5"
	if tailTokens > 0 {
		req.Input = append(req.Input, userMsg(tailOf(tailTokens)))
	}
	for i := 0; i < extra; i++ {
		req.Input = append(req.Input, assistantMsg("step done"), userMsg("ok"))
	}
	return req
}

// outputSaves is what dropping the three outputs removes, by the sweep's own ruler.
func outputSaves(req *bschemas.BifrostChatRequest) int {
	n := 0
	for _, i := range toolIndices(req) {
		text := schema.MessageText(req.Input[i])
		n += schema.TextTokens(text) - schema.TextTokens(sweepDescriptor(text))
	}
	return n
}

// calibratedTail returns the tail size that puts the gate's margin at `margin` times the break-even
// at ratio 1: saving / rewrite = margin. With the default rates, a ratio of 1 breaks even at 11.5.
func calibratedTail(turns float64, margin float64) int {
	r := float64(outputSaves(warmConv(8, 0)))
	// saving = r*0.3*turns, rewrite = w*3.45 (ratio 1). margin = saving/rewrite.
	w := r * 0.3 * turns / (margin * 3.45)
	return int(w)
}

type warmRig struct {
	t       *testing.T
	e       *ExtractSweep
	session string
	st      store.Store
	asker   *labelAsker
	// askerAny, when set, is the asker the rig uses instead of asker (any PrefixAsker).
	askerAny components.PrefixAsker
	tail     int
	ratio    float64
	step     int
	// outputs is how many tool outputs the conversation has; 0 reads as the usual 3.
	outputs int
}

// conv builds this rig's conversation at the given step.
func (r *warmRig) conv(step int) *bschemas.BifrostChatRequest {
	if r.outputs <= 0 {
		return warmConv(step, r.tail)
	}
	return warmConvN(r.outputs, step, r.tail)
}

func newWarmRig(t *testing.T, session, cfg string, tail int, ratio float64) *warmRig {
	t.Helper()
	resetWarmForTest()
	resetSweepLedgerForTest()
	t.Cleanup(func() { resetWarmForTest(); resetSweepLedgerForTest() })
	t.Cleanup(func() { clearSweepPing(thread.Key(session, "")) })
	return &warmRig{t: t, e: shippedSweep(t, cfg), session: session, st: newMemStore(), asker: pingAsker(),
		tail: tail, ratio: ratio, step: 8}
}

func (r *warmRig) key() string { return thread.Key(r.session, "") }

// request sends the next request of the thread through Offload. billed is the provider's input count for
// the previous turn: ratio times the previous request's local size.
func (r *warmRig) request(c *components.Ctx) (*bschemas.BifrostChatRequest, *components.Report) {
	r.t.Helper()
	req := r.conv(r.step)
	prev := r.conv(r.step - 1)
	r.step++
	if c == nil {
		c = warmCtx(r.session, r.st)
	}
	if r.askerAny != nil {
		c.PrefixAsk = r.askerAny
	} else {
		c.PrefixAsk = r.asker
	}
	if c.PrevBilledInput == 0 && r.ratio > 0 {
		c.PrevBilledInput = int(r.ratio * float64(localTokens(prev.Input)))
	}
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := r.e.Offload(req, rep, c); err != nil {
		r.t.Fatal(err)
	}
	return req, rep
}

// forward stands for the host after the request went out: it runs the registered question and waits
// until it is done.
func (r *warmRig) forward() {
	r.t.Helper()
	ResolveWarmQuestion(r.session, "", true, 0)
	// k batches in sequence, each a question over a large prefix, and a slow -race build: 60 s.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		warmMu.Lock()
		t := warmThreads[r.key()]
		busy := t != nil && (t.asking || t.warmPending())
		warmMu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.t.Fatal("the warm question did not finish")
}

func (r *warmRig) held() bool {
	warmMu.Lock()
	defer warmMu.Unlock()
	t := warmThreads[r.key()]
	return t != nil && t.held != nil
}

// toHeld runs 3 warm requests (the third fires the trigger), forwards, and checks an answer is waiting.
func (r *warmRig) toHeld() {
	r.t.Helper()
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	if !r.held() {
		r.t.Fatalf("no answer is waiting after the question: %+v", SweepWarmCounters())
	}
}

// payingTail is a tail that makes the gate pay at ratio 1 (margin 3x) and refuse at ratio 6.
func payingTail() int { return calibratedTail(2, 3) } // turns at apply: run 4 x weight 0.5

// ⭐ TRIGGER. Once per N warm requests, never on a cold request, never when the candidates cannot pay.
func TestTheTriggerFiresOncePerNWarmRequestsAndNeverOnACold(t *testing.T) {
	r := newWarmRig(t, "w-trig", warmCfg, payingTail(), 1.0)
	fired := func() int64 { return SweepWarmCounters().Fired }
	for i := 1; i <= 7; i++ {
		r.request(nil)
		ResolveWarmQuestion(r.session, "", false, 0) // the forward failed: no question, the registration is dropped
		want := int64(i / 3)
		if got := fired(); got != want {
			t.Fatalf("after %d warm requests the trigger fired %d times, want %d", i, got, want)
		}
	}
	// A cold request restarts the count and never fires, even after warm requests that nearly earned a question.
	resetWarmForTest()
	r2 := newWarmRig(t, "w-trig-cold", warmCfg, payingTail(), 1.0)
	for i := 0; i < 9; i++ {
		c := warmCtx(r2.session, r2.st)
		if i%3 == 2 {
			c = coldCtx(r2.session, r2.st) // every third request is cold: the count never reaches 3
		}
		r2.request(c)
		ResolveWarmQuestion(r2.session, "", false, 0)
	}
	if got := fired(); got != 0 {
		t.Fatalf("the trigger fired %d times on cold requests", got)
	}
}

func TestTheTriggerAsksNothingWhenTheCandidatesCannotPayBack(t *testing.T) {
	t.Run("a tail so large that even dropping everything loses", func(t *testing.T) {
		// saving at the cap: removed x 0.3 x 60 turns. Make the rewrite 10x that.
		r := newWarmRig(t, "w-nopay", warmCfg, calibratedTail(60, 0.1), 1.0)
		for i := 0; i < 9; i++ {
			r.request(nil)
		}
		if c := SweepWarmCounters(); c.Fired != 0 || c.NotWorth == 0 {
			t.Fatalf("counters = %+v, want no question and not_worth counted", c)
		}
	})
	t.Run("outputs too young", func(t *testing.T) {
		r := newWarmRig(t, "w-young", strings.Replace(warmCfg, "warm_min_age_turns: 5", "warm_min_age_turns: 500", 1), payingTail(), 1.0)
		for i := 0; i < 9; i++ {
			r.request(nil)
		}
		if c := SweepWarmCounters(); c.Fired != 0 || c.NotWorth == 0 {
			t.Fatalf("counters = %+v, want no question for young outputs", c)
		}
	})
	t.Run("no billed size yet", func(t *testing.T) {
		r := newWarmRig(t, "w-nobilled", warmCfg, payingTail(), 0)
		for i := 0; i < 9; i++ {
			r.request(nil)
		}
		if c := SweepWarmCounters(); c.Fired != 0 || c.NoBilled == 0 {
			t.Fatalf("counters = %+v, want no question without a billed size", c)
		}
	})
}

// ⭐ AFTER THE FORWARD. The question runs only once the host says the request went out, and the request
// it follows is byte-identical to the one with warm off.
func TestTheQuestionRunsAfterTheForwardAndTheForwardedRequestIsUnchanged(t *testing.T) {
	r := newWarmRig(t, "w-after", warmCfg, payingTail(), 1.0)
	off := &warmRig{t: t, e: shippedSweep(t, ""), session: "w-after-off", st: newMemStore(), asker: pingAsker(),
		tail: r.tail, ratio: 1.0, step: 8}
	t.Cleanup(func() { clearSweepPing(off.key()) })
	for i := 0; i < 3; i++ {
		a, _ := r.request(nil)
		b, _ := off.request(nil)
		ja, _ := json.Marshal(a.Input)
		jb, _ := json.Marshal(b.Input)
		if string(ja) != string(jb) {
			t.Fatalf("request %d: the forwarded request differs from the one with warm off", i+1)
		}
	}
	time.Sleep(150 * time.Millisecond) // a question started too early would have reached the model by now
	if n := atomic.LoadInt64(&r.asker.calls); n != 0 {
		t.Fatalf("the question ran %d times before the forward", n)
	}
	if SweepWarmCounters().Fired != 1 {
		t.Fatalf("the trigger did not register a question: %+v", SweepWarmCounters())
	}
	r.forward()
	if n := atomic.LoadInt64(&r.asker.calls); n != 1 {
		t.Fatalf("the question ran %d times after the forward, want 1", n)
	}
	if !r.held() {
		t.Fatal("the answer is not waiting")
	}
}

// ⭐ MEMORY ONLY. The waiting answer is never written to the Store, and a restart loses it.
func TestTheWaitingAnswerIsNeverWrittenToTheStoreAndARestartLosesIt(t *testing.T) {
	r := newWarmRig(t, "w-mem", warmCfg, payingTail(), 1.0)
	rs := &recStore{Store: r.st}
	r.st = rs
	r.toHeld()
	for _, k := range rs.keys() {
		if strings.HasPrefix(k, store.SweepHeldPrefix) {
			t.Fatalf("the warm answer was written to the Store under %q", k)
		}
	}
	for _, p := range rs.payloads() {
		if strings.Contains(p, "toolu_wire_") || strings.Contains(p, `"judged_msgs"`) {
			t.Fatalf("a Store write carries the warm answer: %.120s", p)
		}
	}
	if _, ok := getHeld(r.st, r.key()); ok {
		t.Fatal("the Store holds a held answer")
	}
	// A new process: no state. The next warm request applies nothing.
	resetWarmForTest()
	req, rep := r.request(nil)
	for i, m := range req.Input {
		if expand.HasPlaceholder(schema.MessageText(m)) {
			t.Fatalf("message %d carries a marker after a restart", i)
		}
	}
	if rep.Events[eventAppliedWarm] != 0 {
		t.Fatalf("events = %v, a restart must lose the answer", rep.Events)
	}
}

type recStore struct {
	store.Store
	mu sync.Mutex
	ks []string
	ps []string
}

func (s *recStore) Put(k string, p []byte) {
	s.mu.Lock()
	s.ks, s.ps = append(s.ks, k), append(s.ps, string(p))
	s.mu.Unlock()
	s.Store.Put(k, p)
}
func (s *recStore) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ks...)
}
func (s *recStore) payloads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ps...)
}

// ⭐ THE GATE, in billed sizes. The same conversation: at a billed ratio of 1 it pays, at 6 it does not.
// At ratio 1 the local count and the billed count agree; at 6 the local count alone would still say "pays".
func TestTheGateAcceptsACaseThatPaysAndRefusesOneThatDoesNot(t *testing.T) {
	t.Run("pays", func(t *testing.T) {
		r := newWarmRig(t, "w-gate-yes", warmCfg, payingTail(), 1.0)
		r.toHeld()
		req, rep := r.request(nil)
		if rep.Events[eventAppliedWarm] != 3 || rep.Events["sweep_warm_gate_accept"] != 1 {
			t.Fatalf("events = %v, want 3 applied warm and an accept", rep.Events)
		}
		if rep.Events["sweep_warm_rewrite_tokens"] <= 0 {
			t.Fatalf("events = %v, the rewrite must be booked on the request that paid it", rep.Events)
		}
		for _, i := range toolIndices(req) {
			if !expand.HasPlaceholder(schema.MessageText(req.Input[i])) {
				t.Fatalf("output %d is not dropped", i)
			}
		}
		if c := SweepWarmCounters(); c.GateAccept != 1 || c.GateRefuse != 0 {
			t.Fatalf("counters = %+v", c)
		}
	})
	t.Run("does not pay in billed sizes although it pays in local sizes", func(t *testing.T) {
		tail := payingTail()
		// The local sizes: the gate with the ratio forced to 1 says pays.
		prev := warmConv(10, tail)
		raw := warmConv(11, tail)
		e := shippedSweep(t, warmCfg)
		removed := outputSaves(raw)
		var rm []warmRemoval
		for _, i := range toolIndices(raw) {
			text := schema.MessageText(raw.Input[i])
			rm = append(rm, warmRemoval{i, schema.TextTokens(text) - schema.TextTokens(sweepDescriptor(text))})
		}
		w, _ := warmRewrite(raw, rm[0].idx, len(raw.Input)-1, rm)
		_ = prev
		tot := e.warmTurnsLeft(nil, 4, 0, 0).N * warmWeight(raw.Input)
		localSaving, localRewrite := warmEcon(warmEconIn{Removed: removed, Rewritten: w, RatioContent: 1, RatioFull: 1,
			Read: agentCacheReadPerMTok / 1e6, Write: agentCacheWritePerMTok / 1e6, Turns: tot})
		if !(localSaving > localRewrite) {
			t.Fatalf("fixture: the local count must say pays (saving %.6f, rewrite %.6f)", localSaving, localRewrite)
		}
		// The billed count: the previous turn bills 6x its local size. The cause is what the local count
		// misses in the cached tail (a large fixed prompt, thinking blocks, tool calls, images), which only
		// the REWRITE carries; the removed outputs are plain tool text.
		r := newWarmRig(t, "w-gate-no", warmCfg, tail, 6.0)
		r.toHeld()
		req, rep := r.request(nil)
		if rep.Events[eventAppliedWarm] != 0 {
			t.Fatalf("events = %v, the billed sizes say it does not pay", rep.Events)
		}
		for _, i := range toolIndices(req) {
			if expand.HasPlaceholder(schema.MessageText(req.Input[i])) {
				t.Fatalf("output %d was dropped although the gate refused", i)
			}
		}
		if c := SweepWarmCounters(); c.GateRefuse != 1 || c.Reasons["does_not_pay"]+c.Reasons["does_not_pay_deep"] != 1 {
			t.Fatalf("counters = %+v, want one refusal for does_not_pay", c)
		}
		// A refusal CARRIES the surviving drops forward (accumulation, #491): they stay in memory, marked as
		// tried, to add up with the next question's. They are NOT retried on the next warm requests, which
		// would be refused again on nearly the same numbers.
		if !r.held() {
			t.Fatal("a refused answer's surviving drops were not carried forward")
		}
		gateBefore := SweepWarmCounters().GateRefuse
		for i := 0; i < 2; i++ {
			if _, rep := r.request(nil); rep.Events[eventAppliedWarm] != 0 {
				t.Fatal("a later request applied a carried set the gate had refused")
			}
		}
		if got := SweepWarmCounters().GateRefuse; got != gateBefore {
			t.Fatalf("the gate was retried on a carried set (%d -> %d refusals): it must wait for a new question", gateBefore, got)
		}
	})
}

func TestTheGateRatiosFollowTheBilledSizes(t *testing.T) {
	if _, _, ok := warmRatios("claude-sonnet-5", 0, 1000); ok {
		t.Error("an unknown billed size must not give a ratio")
	}
	if _, _, ok := warmRatios("claude-sonnet-5", 1000, 0); ok {
		t.Error("an unknown local size must not give a ratio")
	}
	if _, _, ok := warmRatios("claude-sonnet-5", 100, 1000); ok {
		t.Error("a billed size under the local size is a bad sample and must not give a ratio")
	}
	if _, _, ok := warmRatios("claude-sonnet-5", 50_000, 1000); ok {
		t.Error("a ratio past the bound is a bad sample and must not give a ratio")
	}
	content, full, ok := warmRatios("claude-sonnet-5", 6000, 1000)
	if !ok || full != 6 || content >= full {
		t.Errorf("content=%.3f full=%.3f ok=%v: the removed content must scale by the family factor, the rewrite by the whole prompt", content, full, ok)
	}
	if content, _, _ := warmRatios("claude-sonnet-5", 1200, 1000); content != 1.2 {
		t.Errorf("content ratio = %.3f, want it capped by the full ratio 1.2", content)
	}
}

// ⭐ AN APPLIED WARM DROP: expand gives it back byte for byte, later requests replay the same bytes.
func TestAnAppliedWarmDropExpandsByteForByteAndLaterRequestsReplayIt(t *testing.T) {
	r := newWarmRig(t, "w-apply", warmCfg, payingTail(), 1.0)
	orig := texts(warmConv(8, r.tail))
	r.toHeld()
	req, rep := r.request(nil)
	if rep.Events[eventAppliedWarm] != 3 || len(rep.CacheKeys) == 0 && false {
		t.Fatalf("events = %v", rep.Events)
	}
	for _, i := range toolIndices(req) {
		now := schema.MessageText(req.Input[i])
		got, ok := expand.Resolve(r.st, firstMarkerID(t, now))
		if !ok || got != orig[i] {
			t.Fatalf("output %d: expand is not byte-identical to the original", i)
		}
	}
	rec, ok := getDrop(r.st, r.key(), contentKey(orig[1]))
	if !ok || rec.Kind != dropKindWarm {
		t.Fatalf("drop record = %+v ok=%v, want a warm drop", rec, ok)
	}
	if q := SweepDropCounters().Overall; q.Applied != 3 || q.AppliedWarm != 3 || q.AppliedCold != 0 {
		t.Fatalf("ledger = %+v, want 3 applied warm and no cold", q)
	}
	want := texts(req)[:len(warmConv(8, r.tail).Input)]
	for n := 0; n < 3; n++ {
		next, rep := r.request(nil)
		got := texts(next)
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("request %d after the apply: message %d differs from the bytes the apply sent", n+1, i)
			}
		}
		if rep.Replays != 3 {
			t.Errorf("request %d replayed %d drops, want 3", n+1, rep.Replays)
		}
	}
}

// ⭐ A CHANGED PREFIX is the cold apply's case (#695): the drops after the change point go in with no gate.
func TestAChangedPrefixBehavesAsTheColdApplyDefinesIt(t *testing.T) {
	r := newWarmRig(t, "w-chg", warmCfg, payingTail(), 6.0) // the gate would refuse
	r.toHeld()
	req := warmConv(r.step, r.tail)
	req.Input[0] = userMsg("a different first message") // an edited turn: the prefix changed at 0
	c := warmCtx(r.session, r.st)
	c.PrefixAsk = r.asker
	c.PrevBilledInput = 6 * localTokens(warmConv(r.step-1, r.tail).Input)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := r.e.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	if rep.Events[eventAppliedChangedPrefix] != 3 || rep.Events[eventAppliedWarm] != 0 {
		t.Fatalf("events = %v, want 3 applied after a changed prefix and no warm apply", rep.Events)
	}
	if c := SweepWarmCounters(); c.GateAccept+c.GateRefuse != 0 {
		t.Fatalf("the gate ran on a changed prefix: %+v", c)
	}
}

// A cold request that meets a waiting answer applies it as the cold apply does: free, no gate.
func TestAColdRequestAppliesAWaitingAnswerWithoutTheGate(t *testing.T) {
	r := newWarmRig(t, "w-cold", warmCfg, payingTail(), 6.0)
	r.toHeld()
	req, rep := r.request(coldCtx(r.session, r.st))
	if rep.Events[eventAppliedCold] != 3 {
		t.Fatalf("events = %v, want 3 applied cold", rep.Events)
	}
	for _, i := range toolIndices(req) {
		if !expand.HasPlaceholder(schema.MessageText(req.Input[i])) {
			t.Fatalf("output %d is not dropped", i)
		}
	}
}

// ⭐ FAIL OPEN.
func TestFailOpenInTheWarmPath(t *testing.T) {
	assertUnchanged := func(t *testing.T, req *bschemas.BifrostChatRequest, want []string) {
		t.Helper()
		for i, w := range want {
			if schema.MessageText(req.Input[i]) != w {
				t.Fatalf("message %d changed", i)
			}
		}
	}
	assertNoLockHeld := func(t *testing.T) {
		t.Helper()
		if !dropMu.TryLock() {
			t.Fatal("dropMu is still held")
		}
		dropMu.Unlock()
		if !warmMu.TryLock() {
			t.Fatal("warmMu is still held")
		}
		warmMu.Unlock()
	}
	t.Run("a panic in the trigger", func(t *testing.T) {
		r := newWarmRig(t, "w-fo-trig", warmCfg, payingTail(), 1.0)
		want := texts(warmConv(8, r.tail))
		warmTriggerHook = func() { panic("boom in the trigger") }
		t.Cleanup(func() { warmTriggerHook = nil })
		req, _ := r.request(nil)
		assertUnchanged(t, req, want)
		assertNoLockHeld(t)
		if SweepWarmCounters().Panics != 1 {
			t.Errorf("counters = %+v, want the panic counted", SweepWarmCounters())
		}
	})
	t.Run("a panic in the apply", func(t *testing.T) {
		r := newWarmRig(t, "w-fo-apply", warmCfg, payingTail(), 1.0)
		r.toHeld()
		ps := &putPanicStore{Store: r.st, after: 3}
		r.st = ps
		want := texts(warmConv(r.step, r.tail))
		req, rep := r.request(nil)
		assertUnchanged(t, req, want)
		assertNoLockHeld(t)
		if rep.Events[eventApplyPanic] != 1 {
			t.Errorf("events = %v, want the apply panic counted", rep.Events)
		}
		if q := SweepDropCounters().Overall; q.Applied != 0 {
			t.Errorf("ledger counts %d applied after a panic", q.Applied)
		}
		if _, ok := getDrop(ps.Store, thread.Key("w-fo-apply", ""), contentKey(want[1])); ok {
			t.Error("a drop record was written for a request that was restored")
		}
	})
	t.Run("a store that cannot hold the originals", func(t *testing.T) {
		r := newWarmRig(t, "w-fo-store", warmCfg, payingTail(), 1.0)
		r.toHeld()
		spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
		r.st = spy
		want := texts(warmConv(r.step, r.tail))
		req, rep := r.request(nil)
		assertUnchanged(t, req, want)
		if rep.Events[eventApplyDeclined] != 3 {
			t.Errorf("events = %v, want 3 declined", rep.Events)
		}
		for _, k := range spy.puts {
			if strings.HasPrefix(k, store.SweepDropPrefix) || strings.HasPrefix(k, store.SweepExpandPrefix) {
				t.Errorf("a record %q was written for a drop that did not happen", k)
			}
		}
		assertNoLockHeld(t)
	})
}

// ⭐ ONE QUESTION AT A TIME, with the ping's own guards.
func TestAWarmQuestionAndAPingQuestionNeverRunTogetherOnAThread(t *testing.T) {
	t.Run("a held ping claim stops the warm question", func(t *testing.T) {
		r := newWarmRig(t, "w-guard-a", warmCfg, payingTail(), 1.0)
		for i := 0; i < 3; i++ {
			r.request(nil)
		}
		release, ok := ClaimThread(r.key())
		if !ok {
			t.Fatal("could not take the thread")
		}
		r.forward()
		release()
		if n := atomic.LoadInt64(&r.asker.calls); n != 0 {
			t.Fatalf("the warm question ran %d times while a ping question held the thread", n)
		}
		if SweepWarmCounters().ThreadBusy != 1 {
			t.Fatalf("counters = %+v, want thread_busy counted", SweepWarmCounters())
		}
	})
	t.Run("a running warm question stops a ping question", func(t *testing.T) {
		r := newWarmRig(t, "w-guard-b", warmCfg, payingTail(), 1.0)
		blk := &blockingAsker{labelAsker: pingAsker(), in: make(chan struct{}), out: make(chan struct{})}
		r.asker = blk.labelAsker
		r.asker = nil
		for i := 0; i < 3; i++ {
			c := warmCtx(r.session, r.st)
			c.PrefixAsk = blk
			c.PrevBilledInput = localTokens(warmConv(r.step-1, r.tail).Input)
			req := warmConv(r.step, r.tail)
			r.step++
			if _, err := r.e.Offload(req, &components.Report{Component: "extract_llm_sweep"}, c); err != nil {
				t.Fatal(err)
			}
		}
		ResolveWarmQuestion(r.session, "", true, 0)
		select {
		case <-blk.in:
		case <-time.After(5 * time.Second):
			t.Fatal("the warm question never reached the model")
		}
		if claim := ClaimPing(r.key()); claim.Choice != PingPlain {
			claim.Release()
			t.Fatalf("a ping claim during a warm question chose %q, want plain", claim.Choice)
		}
		close(blk.out)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if claim := ClaimPing(r.key()); claim.Choice != PingPlain {
				claim.Release()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the thread was never released after the warm question")
	})
}

type blockingAsker struct {
	*labelAsker
	in, out chan struct{}
	once    sync.Once
}

func (b *blockingAsker) Ask(ctx context.Context, session, ask string) (string, components.PrefixUsage, error) {
	b.once.Do(func() { close(b.in) })
	<-b.out
	return b.labelAsker.Ask(ctx, session, ask)
}

// ⭐ FILE-READ EVIDENCE, from tool names and tool input paths.
func TestAFileReadThatTheAgentLaterEditedCarriesThatEvidence(t *testing.T) {
	call := func(id, name, args string) bschemas.ChatMessage {
		n, i := name, id
		m := assistantMsg("")
		m.ChatAssistantMessage = &bschemas.ChatAssistantMessage{ToolCalls: []bschemas.ChatAssistantMessageToolCall{{
			ID: &i, Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &n, Arguments: args}}}}
		return m
	}
	big := func(i string) string { return strings.Repeat("candidate "+i+" distinct line\n", 900) }
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("do the thing"),
		call("toolu_e0", "Edit", `{"file_path":"/work/secret/a.txt","old_string":"w","new_string":"x"}`), toolResultMsgWithID("toolu_e0", "ok"),
		call("toolu_r1", "Read", `{"file_path":"/work/secret/a.txt"}`), toolResultMsgWithID("toolu_r1", big("a")),
		call("toolu_r2", "Read", `{"file_path":"/work/secret/b.txt"}`), toolResultMsgWithID("toolu_r2", big("b")),
		call("toolu_r3", "Bash", `{"command":"ls"}`), toolResultMsgWithID("toolu_r3", big("c")),
		call("toolu_e1", "Edit", `{"file_path":"/work/secret/a.txt","old_string":"x","new_string":"y"}`), toolResultMsgWithID("toolu_e1", "ok"),
		call("toolu_r4", "Read", `{"file_path":"/work/secret/b.txt"}`), toolResultMsgWithID("toolu_r4", "short"),
	}}
	for i := 0; i < 8; i++ {
		req.Input = append(req.Input, assistantMsg("step done"), userMsg("ok"))
	}
	e := shippedSweep(t, warmCfg)
	st := newMemStore()
	askOf := func(warm bool) string {
		asker := pingAsker()
		c := warmCtx("w-file", st)
		c.PrefixAsk = asker
		cand := e.snapshotCandidate(c, req)
		cand.warm = warm
		rep := &components.Report{Component: "extract_llm_sweep"}
		cands := cand.collect(rep)
		if len(cands) != 3 {
			t.Fatalf("collected %d candidates, want 3", len(cands))
		}
		cand.ask(cands, rep, 0, SweepPingWait{})
		return asker.ask()
	}
	ask := askOf(true)
	for _, want := range []string{
		"file_read=yes later_edits=1 later_rereads=0", // a.txt: edited after the read
		"file_read=yes later_edits=0 later_rereads=1", // b.txt: read again
	} {
		if !strings.Contains(ask, want) {
			t.Errorf("the warm question lacks %q:\n%s", want, ask)
		}
	}
	if strings.Count(ask, "file_read=yes later_edits") != 2 {
		t.Errorf("only the two file reads carry the clause, the Bash output must not:\n%s", ask)
	}
	if strings.Contains(ask, "/work/secret") || strings.Contains(ask, "a.txt") {
		t.Errorf("a path reached the question:\n%s", ask)
	}
	if !strings.Contains(ask, "FILE READS.") {
		t.Error("the question carries the clause but does not explain it")
	}
	// The ping's question is unchanged.
	if plain := askOf(false); strings.Contains(plain, "file_read") || strings.Contains(plain, "FILE READS") {
		t.Errorf("the ping's question carries file evidence:\n%s", plain)
	}
}

// The held answer waits no longer than its age limit.
func TestAWaitingAnswerOlderThanTheLimitIsDropped(t *testing.T) {
	r := newWarmRig(t, "w-age", warmCfg, payingTail(), 1.0)
	r.toHeld()
	warmMu.Lock()
	warmThreads[r.key()].heldAt = time.Now().Add(-2 * warmHeldMaxAge)
	warmMu.Unlock()
	_, rep := r.request(nil)
	if rep.Events[eventAppliedWarm] != 0 || SweepWarmCounters().HeldExpired != 1 {
		t.Fatalf("events=%v counters=%+v, want the old answer dropped", rep.Events, SweepWarmCounters())
	}
}

// The warm state is bounded.
func TestTheWarmStateIsBounded(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	for i := 0; i < maxWarmThreads*2; i++ {
		warmMu.Lock()
		warmFor(thread.Key("bound-"+string(rune('a'+i%26))+strings.Repeat("x", i), ""))
		n := len(warmThreads)
		warmMu.Unlock()
		if n > maxWarmThreads {
			t.Fatalf("%d threads kept, bound is %d", n, maxWarmThreads)
		}
	}
}

// With warm off nothing is registered, counted or held.
func TestWithWarmOffNothingHappens(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	r := &warmRig{t: t, e: shippedSweep(t, ""), session: "w-off", st: newMemStore(), asker: pingAsker(), tail: payingTail(), ratio: 1, step: 8}
	t.Cleanup(func() { clearSweepPing(r.key()) })
	for i := 0; i < 12; i++ {
		r.request(nil)
	}
	r.forward()
	if c := SweepWarmCounters(); c.Fired+c.NotWorth+c.NoBilled+c.GateAccept+c.GateRefuse != 0 || r.held() {
		t.Fatalf("warm off, yet counters = %+v", c)
	}
	warmMu.Lock()
	n := len(warmThreads)
	warmMu.Unlock()
	if n != 0 {
		t.Fatalf("warm off, yet %d threads are tracked", n)
	}
}

func TestTheWarmSettingsHaveSafeDefaults(t *testing.T) {
	e := shippedSweep(t, "")
	if e.warm {
		t.Error("warm must be off by default")
	}
	if e.warmEveryN != 30 || e.warmMinAge != 30 || e.warmMaxTurns != 60 {
		t.Errorf("defaults = %d/%d/%d, want 30/30/60", e.warmEveryN, e.warmMinAge, e.warmMaxTurns)
	}
	e = shippedSweep(t, "warm: true\n")
	if !e.warm {
		t.Error("warm: true did not turn it on")
	}
}

func TestTheWaitCauseWeightsFollowWhatWokeTheThread(t *testing.T) {
	tool := []bschemas.ChatMessage{userMsg("x"), toolResultMsgWithID("t", "out")}
	human := []bschemas.ChatMessage{toolResultMsgWithID("t", "out"), userMsg("now do this")}
	asst := []bschemas.ChatMessage{userMsg("x"), assistantMsg("thinking")}
	if warmWeight(tool) != warmWeightOwnTool || warmWeight(human) != warmWeightHuman || warmWeight(asst) != warmWeightUnknown {
		t.Errorf("weights = %v %v %v", warmWeight(tool), warmWeight(human), warmWeight(asst))
	}
	e := shippedSweep(t, "warm: true\nwarm_max_turns: 10\n")
	if got := e.warmTurnsLeft(nil, 500, 0, 0).N * warmWeight(tool); got != 10 {
		t.Errorf("expected warm turns = %v, want the run capped at 10", got)
	}
}

// An expand of a drop applied WARM is told apart from a cold one, so the dashboard can split the expands.
func TestAnExpandOfAWarmDropIsMarkedWarm(t *testing.T) {
	r := newWarmRig(t, "w-expand", warmCfg, payingTail(), 1.0)
	r.toHeld()
	req, _ := r.request(nil)
	id := firstMarkerID(t, schema.MessageText(req.Input[toolIndices(req)[0]]))
	note := NoteSweepExpand(r.st, id, 100, r.key())
	if !note.Sweep || !note.First || !note.Warm {
		t.Fatalf("note = %+v, want a first expand of a warm sweep drop", note)
	}
	// A cold drop is not warm.
	e, st, key := heldApply(t, "w-expand-cold", 3)
	creq := applyConv(3, 1)
	if _, err := e.Offload(creq, &components.Report{Component: "extract_llm_sweep"}, coldCtx("w-expand-cold", st)); err != nil {
		t.Fatal(err)
	}
	cid := firstMarkerID(t, schema.MessageText(creq.Input[toolIndices(creq)[0]]))
	if n := NoteSweepExpand(st, cid, 100, key); !n.Sweep || n.Warm {
		t.Fatalf("note = %+v, a cold drop must not read as warm", n)
	}
}

// ⭐ OPTION B. The warm path keeps no conversation copy of its own. A thread the keep-alive cannot ping,
// or one with no registry entry, never asks, and that is counted.
func TestAThreadTheKeepAliveCannotPingNeverAsksAWarmQuestion(t *testing.T) {
	t.Run("keep-alive cannot ping the thread", func(t *testing.T) {
		r := newWarmRig(t, "w-nokeep", warmCfg, payingTail(), 1.0)
		for i := 0; i < 3; i++ {
			c := warmCtx(r.session, r.st)
			c.CanKeepAlive = func() bool { return false }
			r.request(c)
		}
		r.forward()
		if n := atomic.LoadInt64(&r.asker.calls); n != 0 {
			t.Fatalf("a thread the keep-alive cannot ping asked %d times", n)
		}
		if c := SweepWarmCounters(); c.NotRegistered == 0 || c.Fired != 0 {
			t.Fatalf("counters = %+v, want not_registered counted and no question", c)
		}
	})
	t.Run("no registry entry (under the floor or cleared)", func(t *testing.T) {
		r := newWarmRig(t, "w-noreg", warmCfg, payingTail(), 1.0)
		for i := 0; i < 3; i++ {
			r.request(nil)
		}
		clearSweepPing(r.key()) // the ping registry has no copy for this thread
		r.forward()
		if n := atomic.LoadInt64(&r.asker.calls); n != 0 {
			t.Fatalf("a thread with no registry copy asked %d times", n)
		}
		if SweepWarmCounters().NotRegistered == 0 {
			t.Fatalf("counters = %+v, want not_registered counted", SweepWarmCounters())
		}
	})
	t.Run("the warm path keeps no copy of its own", func(t *testing.T) {
		r := newWarmRig(t, "w-nocopy", warmCfg, payingTail(), 1.0)
		for i := 0; i < 3; i++ {
			r.request(nil)
		}
		warmMu.Lock()
		pending := warmThreads[r.key()].warmPending()
		warmMu.Unlock()
		if !pending {
			t.Fatal("the trigger did not fire")
		}
		// The question runs from the registry's slices: same backing array, not a second copy.
		sweepPingMu.Lock()
		reg := sweepPingCand[r.key()]
		sweepPingMu.Unlock()
		if reg == nil {
			t.Fatal("no registry entry")
		}
		var seen *sweepPingCandidate
		warmAskSpy = func(c *sweepPingCandidate) { seen = c }
		t.Cleanup(func() { warmAskSpy = nil })
		r.forward()
		if seen == nil || &seen.input[0] != &reg.input[0] || &seen.raw[0] != &reg.raw[0] {
			t.Fatal("the warm question was given its own copy of the conversation")
		}
	})
}

// ⭐ R1-1. A warm question sets the registry candidate's latch, so the ping that follows sends a plain ping
// and a thread is never paid twice for one conversation. And the other way round.
func TestAWarmQuestionStopsThePingFromAskingTheSameConversationAgain(t *testing.T) {
	r := newWarmRig(t, "w-latch", warmCfg, payingTail(), 1.0)
	r.toHeld()
	if _, _, reason, ok := SweepPingSubstitute(r.key()); ok || reason != SweepReasonAlreadyAsked {
		t.Fatalf("after a warm question the ping was offered a question (ok=%v reason=%q), want already_asked", ok, reason)
	}
	if n := atomic.LoadInt64(&r.asker.calls); n != 1 {
		t.Fatalf("the model was asked %d times, want 1", n)
	}
	if _, ok := getHeld(r.st, r.key()); ok {
		t.Fatal("a second held answer is in the Store")
	}
}

func TestAHeldPingAnswerStopsAWarmQuestionOnTheSameThread(t *testing.T) {
	r := newWarmRig(t, "w-latch2", warmCfg, payingTail(), 1.0)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	// The ping asks first and holds its answer in the Store.
	dispatch, _, reason, ok := SweepPingSubstitute(r.key())
	if !ok {
		t.Fatalf("no ping question: %s", reason)
	}
	if res := dispatch(time.Second, SweepPingWait{}); !res.Held {
		t.Fatalf("fixture: the ping answer is not held: %+v", res)
	}
	calls := atomic.LoadInt64(&r.asker.calls)
	r.forward()
	if n := atomic.LoadInt64(&r.asker.calls); n != calls {
		t.Fatalf("a warm question was asked although the ping's answer was held (%d -> %d calls)", calls, n)
	}
	if r.held() || SweepWarmCounters().AlreadyAsked != 1 {
		t.Fatalf("held=%v counters=%+v, want no warm answer and already_asked counted", r.held(), SweepWarmCounters())
	}
}

// A warm question that never reached the provider gives the latch back, so the ping may still ask.
func TestAWarmQuestionThatNeverAskedLeavesThePingFree(t *testing.T) {
	r := newWarmRig(t, "w-latch3", warmCfg, payingTail(), 1.0)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	release, ok := ClaimThread(r.key())
	if !ok {
		t.Fatal("could not take the thread")
	}
	r.forward() // thread busy: no question
	release()
	if _, _, reason, ok := SweepPingSubstitute(r.key()); !ok {
		t.Fatalf("the ping was refused (%q) although no warm question was paid for", reason)
	}
}

// ⭐ GATE MODEL (#491, David). The provider reads back only up to a cache entry written BEFORE the change.
// A drop deeper than the look-back from the last breakpoint rewrites the WHOLE conversation; a drop inside
// it rewrites only the tail from the drop.
func deepFixture(exchanges int) (*bschemas.BifrostChatRequest, []warmRemoval, int) {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg(tailOf(60000)), // a large early part of the conversation, before the first output
		userMsg("go"),
	}}
	req.Input = append(req.Input, toolResultMsgWithID("toolu_d0", refOutput(0)))
	first := len(req.Input) - 1
	for i := 0; i < exchanges; i++ {
		req.Input = append(req.Input, assistantMsg("step done"), userMsg("ok"))
	}
	text := schema.MessageText(req.Input[first])
	saves := schema.TextTokens(text) - schema.TextTokens(sweepDescriptor(text))
	return req, []warmRemoval{{first, saves}}, first
}

func TestADropAtDepthRewritesTheWholeConversation(t *testing.T) {
	req, rm, first := deepFixture(warmLookbackBlocks) // 2*20 blocks after the output: far past the look-back
	end := len(req.Input) - 1
	got, deep := warmRewrite(req, first, end, rm)
	whole := localTokens(req.Input) - rm[0].saves
	if !deep || got != whole {
		t.Fatalf("deep=%v rewrite=%d, want a deep drop that rewrites the whole conversation (%d)", deep, got, whole)
	}
}

func TestADropNearTheTailRewritesOnlyTheTail(t *testing.T) {
	req, rm, first := deepFixture(3) // 6 blocks after the output: inside the look-back
	end := len(req.Input) - 1
	got, deep := warmRewrite(req, first, end, rm)
	tail := 0
	for j := first; j <= end; j++ {
		tail += schema.TextTokens(schema.MessageText(req.Input[j]))
	}
	tail -= rm[0].saves
	if deep || got != tail {
		t.Fatalf("deep=%v rewrite=%d, want a shallow drop that rewrites only the tail (%d)", deep, got, tail)
	}
	if whole := localTokens(req.Input); got >= whole/2 {
		t.Fatalf("the shallow rewrite %d is not far under the conversation %d", got, whole)
	}
}

// The gate refuses a drop the OLD model (rewrite from the first drop on) accepted.
func TestTheGateRefusesADeepDropTheOldModelAccepted(t *testing.T) {
	req, rm, first := deepFixture(warmLookbackBlocks)
	e := shippedSweep(t, warmCfg)
	c := warmCtx("w-model", newMemStore())
	c.ModelName = "claude-sonnet-5"
	c.PrevBilledInput = localTokens(req.Input) // ratio 1
	res := recheckResult{Valid: []heldOutput{{ToolUseID: "toolu_d0", Saves: rm[0].saves}}}
	_, read, write := agentRates(c)
	oldRewrite := 0
	for j := first; j < len(req.Input); j++ {
		oldRewrite += schema.TextTokens(schema.MessageText(req.Input[j]))
	}
	oldRewrite -= rm[0].saves
	turns := float64(e.warmMaxTurns)
	oldSaving, oldCost := warmEcon(warmEconIn{Removed: rm[0].saves, Rewritten: oldRewrite, RatioContent: 1, RatioFull: 1,
		Read: read, Write: write, Turns: turns})
	if !(oldSaving > oldCost) {
		t.Fatalf("fixture: the old model must accept (saving %.6f, rewrite %.6f)", oldSaving, oldCost)
	}
	wa := &warmApply{}
	ok, reason := e.warmGate(c, req, e.warmMaxTurns*2, 0, 0, localTokens(req.Input), wa)(res, req.Input)
	if ok || reason != "does_not_pay_deep" {
		t.Fatalf("gate = %v %q, want a refusal for a deep drop", ok, reason)
	}
	// The same drop near the tail pays, so the refusal is the depth and nothing else.
	near, nrm, _ := deepFixture(3)
	nres := recheckResult{Valid: []heldOutput{{ToolUseID: "toolu_d0", Saves: nrm[0].saves}}}
	nc := warmCtx("w-model", newMemStore())
	nc.ModelName, nc.PrevBilledInput = "claude-sonnet-5", localTokens(near.Input)
	if ok, reason := e.warmGate(nc, near, 3, 0, 0, localTokens(near.Input), &warmApply{})(nres, near.Input); !ok {
		t.Fatalf("a shallow drop was refused (%q), so the fixture does not isolate the depth", reason)
	}
}

// ⭐ MASS GATE. Ask only when the removable mass is a real share of the conversation.
func TestTheTriggerAsksNothingWhenTheRemovableMassIsTooSmall(t *testing.T) {
	cfg := warmCfg + "warm_min_mass_frac: 0.95\n"
	r := newWarmRig(t, "w-mass", cfg, 0, 1.0)
	// A small tail: the three outputs are about 91% of the conversation, under the 95% asked for.
	r.tail = outputSaves(warmConv(8, 0)) / 10
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	c := SweepWarmCounters()
	if c.Fired != 0 || c.TriggerReasons["below_mass"] != 1 {
		t.Fatalf("counters = %+v, want no question and one below_mass refusal", c)
	}
	// With the default (0.20) the same conversation asks.
	r2 := newWarmRig(t, "w-mass2", warmCfg, outputSaves(warmConv(8, 0))/10, 1.0)
	for i := 0; i < 3; i++ {
		r2.request(nil)
	}
	if SweepWarmCounters().Fired != 1 {
		t.Fatalf("counters = %+v, want a question at the default mass share", SweepWarmCounters())
	}
}

func TestTheMassSettingHasADefaultAndIgnoresNonsense(t *testing.T) {
	if e := shippedSweep(t, "warm: true\n"); e.warmMinMassFrac != 0.20 {
		t.Errorf("default = %v, want 0.20", e.warmMinMassFrac)
	}
	if e := shippedSweep(t, "warm: true\nwarm_min_mass_frac: 7\n"); e.warmMinMassFrac != 0.20 {
		t.Errorf("a share over 1 = %v, want it read as the default", e.warmMinMassFrac)
	}
}

// Every refusal at the trigger is counted by reason.
func TestEveryTriggerRefusalIsCountedByReason(t *testing.T) {
	r := newWarmRig(t, "w-reasons", warmCfg, payingTail(), 0) // no billed size
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	if got := SweepWarmCounters().TriggerReasons["no_billed_size"]; got != 1 {
		t.Fatalf("reasons = %v, want no_billed_size once", SweepWarmCounters().TriggerReasons)
	}
	r2 := newWarmRig(t, "w-reasons2", strings.Replace(warmCfg, "warm_min_age_turns: 5", "warm_min_age_turns: 500", 1), payingTail(), 1.0)
	for i := 0; i < 3; i++ {
		r2.request(nil)
	}
	if got := SweepWarmCounters().TriggerReasons["no_candidate"]; got != 1 {
		t.Fatalf("reasons = %v, want no_candidate once", SweepWarmCounters().TriggerReasons)
	}
}

// The trigger's expected turns are the warm turns run so far: a thread with a short run is not asked.
func TestTheTriggerUsesTheWarmRunSoFarAsItsEstimateOfTurnsLeft(t *testing.T) {
	// A tail that pays at 60 turns but not at 3: the first check (3 warm requests) must refuse.
	tail := calibratedTail(60, 3)
	r := newWarmRig(t, "w-run", warmCfg, tail, 1.0)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	if c := SweepWarmCounters(); c.Fired != 0 || c.TriggerReasons["does_not_pay"] != 1 {
		t.Fatalf("counters = %+v, want a refusal at 3 turns of run", c)
	}
}

// ⭐ #725. The warm question judges its cache read by prefixAskCacheOK against this turn's read, and has
// no write check of its own.
func TestTheWarmQuestionIsJudgedByTheSharedCacheReadRule(t *testing.T) {
	run := func(turnRead int) (held bool, gates map[string]int64) {
		r := newWarmRig(t, "w-read", warmCfg, payingTail(), 1.0)
		for i := 0; i < 3; i++ {
			r.request(nil)
		}
		ResolveWarmQuestion(r.session, "", true, turnRead)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			warmMu.Lock()
			th := warmThreads[r.key()]
			busy := th != nil && (th.asking || th.warmPending())
			warmMu.Unlock()
			if !busy {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return r.held(), copyWarmGates()
	}
	if held, _ := run(1000); !held {
		t.Fatal("a read at or above the turn's read must be accepted")
	}
	held, gates := run(1_000_000) // the ask read far less than the turn did
	if held || gates["sweep_prefix_cache_read_below_turn"] == 0 {
		t.Fatalf("held=%v gates=%v, want the answer refused for a read below the turn's", held, gates)
	}
}

// ⭐ LIVE TEST NUMBERS. Each applied warm sweep leaves a record with the tokens removed, the conversation
// before and after, the gate's numbers and the net, in /stats and as events on the dashboard row.
func TestEachWarmSweepRecordsItsNumbers(t *testing.T) {
	r := newWarmRig(t, "w-record", warmCfg, payingTail(), 1.0)
	r.toHeld()
	before := localTokens(warmConv(r.step, r.tail).Input)
	req, rep := r.request(nil)
	if rep.Events[eventAppliedWarm] != 3 {
		t.Fatalf("events = %v", rep.Events)
	}
	for _, ev := range []string{"sweep_warm_rewrite_tokens", "sweep_warm_removed_tokens", "sweep_warm_expected_turns"} {
		if rep.Events[ev] <= 0 {
			t.Errorf("event %s = %d, want it booked on the request that applied", ev, rep.Events[ev])
		}
	}
	got := SweepWarmCounters().Recent
	if len(got) != 1 {
		t.Fatalf("recent sweeps = %+v, want one", got)
	}
	w := got[0]
	if w.Drops != 3 || w.RemovedLocal <= 0 || w.RemovedBilled <= 0 || w.RewriteBilled <= 0 || w.ExpectedTurns <= 0 ||
		w.NetUSD <= 0 || w.ConvBefore != before || w.ConvAfter != localTokens(req.Input) || w.ConvAfter >= w.ConvBefore {
		t.Fatalf("record = %+v (conversation before should be %d)", w, before)
	}
	if strings.Contains(w.Thread, "w-record") {
		t.Errorf("the record carries the session id: %q", w.Thread)
	}
}

// ⭐ R1-N2. A thread with a waiting answer is never evicted, and the table never passes its bound.
func TestAThreadWithAWaitingAnswerIsNeverEvictedAndTheTableStaysBounded(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	warmHold("keeper", heldAnswer{Thread: "keeper", Outputs: []heldOutput{{ToolUseID: "x"}}})
	for i := 0; i < maxWarmThreads*2; i++ {
		warmMu.Lock()
		warmFor(thread.Key("churn-"+strings.Repeat("x", i), ""))
		n := len(warmThreads)
		warmMu.Unlock()
		if n > maxWarmThreads {
			t.Fatalf("%d threads kept, bound is %d", n, maxWarmThreads)
		}
	}
	if _, ok := (memHeld{"keeper"}).get(""); !ok {
		t.Fatal("a thread with a waiting answer was evicted")
	}
	// Now fill the table with threads that all hold an answer: a new one is refused and counted.
	resetWarmForTest()
	for i := 0; i < maxWarmThreads; i++ {
		warmHold("h"+strings.Repeat("y", i), heldAnswer{Outputs: []heldOutput{{ToolUseID: "x"}}})
	}
	warmMu.Lock()
	got := warmFor("one-too-many")
	n := len(warmThreads)
	warmMu.Unlock()
	if got != nil || n != maxWarmThreads || SweepWarmCounters().ThreadsFull != 1 {
		t.Fatalf("new thread = %v, table = %d, counters = %+v; want it refused and counted", got, n, SweepWarmCounters())
	}
}

// ⭐ R1-N3/N4. A warm question with nothing to offer is counted, and its gates and label are the warm path's.
func TestWarmQuestionGatesAndLabelAreTheWarmPathsOwn(t *testing.T) {
	r := newWarmRig(t, "w-gates", warmCfg, payingTail(), 1.0)
	before := SweepPingCounters().Gates["sweep_kept"] + SweepPingCounters().Gates["sweep_dropped"]
	r.toHeld()
	after := SweepPingCounters().Gates["sweep_kept"] + SweepPingCounters().Gates["sweep_dropped"]
	if after != before {
		t.Errorf("the warm question's gates were booked in the ping's counters")
	}
	if len(SweepWarmCounters().Gates) == 0 {
		t.Error("the warm question's gates were not booked in the warm counters")
	}
	// Nothing to offer: counted.
	r2 := newWarmRig(t, "w-noinv", warmCfg, payingTail(), 1.0)
	for i := 0; i < 3; i++ {
		r2.request(nil)
	}
	sweepPingMu.Lock()
	reg := sweepPingCand[r2.key()]
	for i := range reg.input {
		reg.input[i] = userMsg("nothing big here")
	}
	sweepPingMu.Unlock()
	r2.forward()
	if SweepWarmCounters().NoInventory == 0 {
		t.Errorf("counters = %+v, want no_inventory counted", SweepWarmCounters())
	}
}

func TestTheWarmStatsBlockShowsAnythingThatHappened(t *testing.T) {
	for name, w := range map[string]SweepWarmStats{
		"panics": {Panics: 1}, "held expired": {HeldExpired: 1}, "failed": {Failed: 1},
		"thread busy": {ThreadBusy: 1}, "concurrency full": {ConcurrencyFull: 1}, "held": {Held: 1},
		"no inventory": {NoInventory: 1}, "threads full": {ThreadsFull: 1}, "already asked": {AlreadyAsked: 1},
	} {
		if !w.Any() {
			t.Errorf("%s: a block with only that counter moved reads as nothing happened", name)
		}
	}
	if (SweepWarmStats{}).Any() {
		t.Error("an idle warm path must show nothing")
	}
}

// A ping answer still waiting in the Store stops a warm question, even after a new request has
// re-registered the thread (a fresh latch). Two held answers for one thread would be paid twice.
func TestAPingAnswerStillInTheStoreStopsAWarmQuestionAfterAReRegistration(t *testing.T) {
	r := newWarmRig(t, "w-latch4", warmCfg, payingTail(), 1.0)
	r.request(nil)
	dispatch, _, reason, ok := SweepPingSubstitute(r.key())
	if !ok {
		t.Fatalf("no ping question: %s", reason)
	}
	if res := dispatch(time.Second, SweepPingWait{}); !res.Held {
		t.Fatalf("fixture: the ping answer is not held: %+v", res)
	}
	// Two more requests re-register the thread (a fresh, unlatched registry entry) and fire the trigger.
	r.request(nil)
	r.request(nil)
	calls := atomic.LoadInt64(&r.asker.calls)
	r.forward()
	if n := atomic.LoadInt64(&r.asker.calls); n != calls {
		t.Fatalf("a warm question was asked while the ping's answer waited in the Store (%d -> %d)", calls, n)
	}
	if SweepWarmCounters().AlreadyAsked != 1 {
		t.Fatalf("counters = %+v, want already_asked counted", SweepWarmCounters())
	}
}

// ⭐ warm_gate_price_model (DIAGNOSTIC). A model priced at $0 gives saving = rewrite = 0, so the gate
// refuses everything and a run tests nothing. With the override the gate decides with another model's
// rates; without it nothing changes.
var opusRates = components.TokenRates{Input: 15e-6, Output: 75e-6, CacheRead: 1.5e-6, CacheWrite: 18.75e-6}

// zeroPriced stands for a gateway model whose price row makes the gate's ratio useless: a cache write
// priced a billion times its read. The gate decides on (write - read) / read only, so what a model needs for the
// override to matter is a skewed ratio, not a zero price (a zero tier falls back to the built-in rates).
var zeroPriced = components.TokenRates{Input: 1e-6, Output: 1e-6, CacheRead: 1e-12, CacheWrite: 1e-3}

func gateCtx(t *testing.T, model string) *components.Ctx {
	c := warmCtx("w-price", newMemStore())
	c.ModelName = model
	c.SelfRates = zeroPriced
	c.RatesFor = func(m string) components.TokenRates {
		if m == "claude-opus-5" {
			return opusRates
		}
		return components.TokenRates{}
	}
	return c
}

func TestTheGatePriceModelGivesAZeroPricedModelRealDecisions(t *testing.T) {
	req, rm, _ := deepFixture(3) // a shallow drop that pays at real prices
	res := recheckResult{Valid: []heldOutput{{ToolUseID: "toolu_d0", Saves: rm[0].saves}}}
	decide := func(cfg string) (bool, string) {
		e := shippedSweep(t, warmCfg+cfg)
		c := gateCtx(t, "glm-test")
		c.PrevBilledInput = localTokens(req.Input) // ratio 1
		return e.warmGate(c, req, 10, 0, 0, localTokens(req.Input), &warmApply{})(res, req.Input)
	}
	// Without the override: the model's own skewed ratio makes the rewrite look dear, refused.
	if ok, reason := decide(""); ok || reason != "does_not_pay" {
		t.Fatalf("without the override: ok=%v reason=%q, want a $0-priced model to be refused", ok, reason)
	}
	// With it: Opus prices, the same drop pays.
	if ok, reason := decide("warm_gate_price_model: claude-opus-5\n"); !ok {
		t.Fatalf("with the override: refused (%q), want Opus decisions", reason)
	}
	// And a drop that does not pay at Opus prices is still refused: it is a decision, not a free pass.
	deep, drm, _ := deepFixture(warmLookbackBlocks)
	dres := recheckResult{Valid: []heldOutput{{ToolUseID: "toolu_d0", Saves: drm[0].saves}}}
	e := shippedSweep(t, warmCfg+"warm_gate_price_model: claude-opus-5\n")
	c := gateCtx(t, "glm-test")
	c.PrevBilledInput = localTokens(deep.Input)
	if ok, _ := e.warmGate(c, deep, 3, 0, 0, localTokens(deep.Input), &warmApply{})(dres, deep.Input); ok {
		t.Fatal("a deep drop with few turns was accepted under the override")
	}
}

func TestTheGatePriceModelChangesNothingWhenUnset(t *testing.T) {
	e := shippedSweep(t, warmCfg)
	c := warmCtx("w-price2", newMemStore())
	c.SelfRates = opusRates
	r, w := e.warmGateRates(c)
	_, wantR, wantW := agentRates(c)
	if r != wantR || w != wantW {
		t.Fatalf("rates = %v/%v, want the request's own %v/%v", r, w, wantR, wantW)
	}
	if e.warmGatePriceModel != "" {
		t.Error("the diagnostic must be off by default")
	}
}

// The override is used by the gate only: the dashboard prices the real model.
func TestTheGatePriceModelDoesNotTouchTheDashboardPrice(t *testing.T) {
	c := warmCtx("w-price3", newMemStore())
	c.SelfRates = zeroPriced
	c.RatesFor = func(string) components.TokenRates { return opusRates }
	e := shippedSweep(t, warmCfg+"warm_gate_price_model: claude-opus-5\n")
	e.warmGateRates(c)
	if c.SelfRates != zeroPriced {
		t.Fatal("the gate changed the request's own rates")
	}
}

// A model the rate card does not know falls back to the built-in rates and is counted, never silently.
func TestAnUnknownGatePriceModelIsCounted(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	e := shippedSweep(t, warmCfg+"warm_gate_price_model: no-such-model\n")
	c := gateCtx(t, "glm-test")
	if r, w := e.warmGateRates(c); r <= 0 || w <= 0 {
		t.Fatalf("rates = %v/%v, want the built-in fallback", r, w)
	}
	if SweepWarmCounters().PriceModelUnknown != 1 {
		t.Fatalf("counters = %+v, want price_model_unknown counted", SweepWarmCounters())
	}
}

// The diagnostic shows in /stats so a run with it is never mistaken for a normal run.
func TestTheGatePriceModelShowsInTheStats(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	if SweepWarmCounters().GatePriceModel != "" {
		t.Fatal("a normal run shows a gate price model")
	}
	shippedSweep(t, warmCfg+"warm_gate_price_model: claude-opus-5\n")
	w := SweepWarmCounters()
	if w.GatePriceModel != "claude-opus-5" || !w.Any() {
		t.Fatalf("stats = %+v, want gate_price_model claude-opus-5 and a visible block", w)
	}
}

// ⭐ A gateway that reports no cache_write still gets a gate decision: the billed size is input +
// cache_read, never refused as no_billed_size.
func TestAMissingCacheWriteDoesNotMakeTheGateRefuseForNoBilledSize(t *testing.T) {
	req, rm, _ := deepFixture(3)
	res := recheckResult{Valid: []heldOutput{{ToolUseID: "toolu_d0", Saves: rm[0].saves}}}
	e := shippedSweep(t, warmCfg+"warm_gate_price_model: claude-opus-5\n")
	c := gateCtx(t, "glm-test")
	// PrevBilledInput as the host records it: fresh + cache_read + cache_write, with cache_write = 0.
	fresh, read, write := 500, localTokens(req.Input)*2-500, 0
	c.PrevBilledInput = fresh + read + write
	ok, reason := e.warmGate(c, req, 10, 0, 0, localTokens(req.Input), &warmApply{})(res, req.Input)
	if reason == "no_billed_size" {
		t.Fatalf("a missing cache_write made the gate refuse for no_billed_size")
	}
	_ = ok
	// And a billed size of exactly 0 is still the honest "unknown".
	c.PrevBilledInput = 0
	if _, reason := e.warmGate(c, req, 10, 0, 0, localTokens(req.Input), &warmApply{})(res, req.Input); reason != "no_billed_size" {
		t.Fatalf("reason = %q, want no_billed_size for an unknown billed size", reason)
	}
}

// The trigger's mass test uses the same marker rule as the question's candidate scan (carriesRealMarker):
// an output that only QUOTES marker syntax is a candidate, and one that really carries our marker is not.
func TestTheTriggerCountsAnOutputThatOnlyQuotesMarkerSyntaxAsRemovable(t *testing.T) {
	fixture := func(session, body string) *warmRig {
		r := newWarmRig(t, session, warmCfg, 0, 1.0)
		return r
	}
	conv := func(quote string) *bschemas.BifrostChatRequest {
		req := refConversation(3)
		req.Provider, req.Model = bschemas.Anthropic, "claude-sonnet-5"
		for _, i := range toolIndices(req) {
			s := schema.MessageText(req.Input[i]) + quote
			schema.SetMessageText(&req.Input[i], s)
		}
		for i := 0; i < 8; i++ {
			req.Input = append(req.Input, assistantMsg("step done"), userMsg("ok"))
		}
		return req
	}
	removable := func(session, quote string, real bool) bool {
		r := fixture(session, quote)
		c := warmCtx(session, r.st)
		c.ModelName = "claude-sonnet-5"
		req := conv(quote)
		if real {
			r.st.Put("deadbeefdeadbeefdeadbeef", []byte("the original"))
		}
		c.PrevBilledInput = localTokens(req.Input)
		return r.e.warmWorthAsking(c, req, localTokens(req.Input), 60, 0, 0)
	}
	quote := "\nsee <<cg:deadbeefdeadbeefdeadbeef>> in the docs\n"
	if !removable("w-quote-a", quote, false) {
		t.Fatal("an output that only quotes marker syntax was not counted as removable")
	}
	if removable("w-quote-b", quote, true) {
		t.Fatal("an output that really carries our marker was counted as removable")
	}
}

// ---------------------------------------------------------------------------------------------------
// Decision records (#491, David): every trigger and apply-gate decision, accepted or refused.

func decisionsOf(stage string) []WarmDecision {
	var out []WarmDecision
	for _, d := range SweepWarmCounters().RecentDecisions {
		if d.Stage == stage {
			out = append(out, d)
		}
	}
	return out
}

func TestAnAcceptedApplyGateDecisionRecordsItsNumbers(t *testing.T) {
	r := newWarmRig(t, "w-dec-yes", warmCfg, payingTail(), 1.0)
	r.toHeld()
	r.request(nil)
	ds := decisionsOf("apply")
	if len(ds) != 1 {
		t.Fatalf("apply decisions = %+v, want one", SweepWarmCounters().RecentDecisions)
	}
	d := ds[0]
	if !d.Accept || d.Reason != "" || d.RemovedLocal <= 0 || d.RemovedBilled <= 0 || d.RewrittenLocal <= 0 ||
		d.RewrittenBilled <= 0 || d.Turns <= 0 || d.Weight <= 0 || d.RatioFull < 1 || d.RatioContent <= 0 ||
		!(d.Saving > d.Rewrite) {
		t.Fatalf("decision = %+v, want an accept with every number set", d)
	}
	if strings.Contains(d.Thread, "w-dec-yes") {
		t.Errorf("the decision carries the session id: %q", d.Thread)
	}
}

func TestARefusedApplyGateDecisionRecordsItsNumbersAndReason(t *testing.T) {
	r := newWarmRig(t, "w-dec-no", warmCfg, payingTail(), 6.0) // billed 6x local: the gate refuses
	r.toHeld()
	req, rep := r.request(nil)
	if rep.Events[eventAppliedWarm] != 0 {
		t.Fatalf("fixture: the gate must refuse (events %v)", rep.Events)
	}
	d := decisionsOf("apply")
	if len(d) != 1 || d[0].Accept || d[0].Reason == "" || d[0].RemovedLocal <= 0 || d[0].RewrittenLocal <= 0 ||
		d[0].RatioFull < 5 || d[0].Turns <= 0 || !(d[0].Rewrite >= d[0].Saving) {
		t.Fatalf("decisions = %+v, want one refusal with its reason and numbers", d)
	}
	// And on the row, as events, so the dashboard shows a refusal as numbers.
	for _, ev := range []string{"sweep_warm_refused_removed_tokens", "sweep_warm_refused_rewrite_tokens", "sweep_warm_refused_expected_turns"} {
		if rep.Events[ev] <= 0 {
			t.Errorf("event %s = %d on a refused sweep, want it booked", ev, rep.Events[ev])
		}
	}
	_ = req
}

func TestEveryTriggerDecisionIsRecordedAcceptOrRefuse(t *testing.T) {
	// A refusal (no billed size) and an accept (a paying thread) both leave a record.
	r := newWarmRig(t, "w-dec-t1", warmCfg, payingTail(), 0)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	ds := decisionsOf("trigger")
	if len(ds) != 1 || ds[0].Accept || ds[0].Reason != "no_billed_size" {
		t.Fatalf("trigger decisions = %+v, want one no_billed_size refusal", ds)
	}
	r2 := newWarmRig(t, "w-dec-t2", warmCfg, payingTail(), 1.0)
	for i := 0; i < 3; i++ {
		r2.request(nil)
	}
	ds = decisionsOf("trigger")
	if len(ds) != 1 || !ds[0].Accept || ds[0].RemovedLocal <= 0 || ds[0].RewrittenLocal <= 0 || ds[0].Turns <= 0 ||
		!(ds[0].Saving > ds[0].Rewrite) {
		t.Fatalf("trigger decisions = %+v, want one accept with its numbers", ds)
	}
	// A mass refusal records D and the reason.
	r3 := newWarmRig(t, "w-dec-t3", warmCfg+"warm_min_mass_frac: 0.95\n", 0, 1.0)
	r3.tail = outputSaves(warmConv(8, 0)) / 10
	for i := 0; i < 3; i++ {
		r3.request(nil)
	}
	ds = decisionsOf("trigger")
	if len(ds) != 1 || ds[0].Reason != "below_mass" || ds[0].RemovedLocal <= 0 {
		t.Fatalf("trigger decisions = %+v, want a below_mass refusal carrying D", ds)
	}
}

func TestTheDecisionLogKeepsTheLastFiftyAndOneInfoLineEach(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	for i := 0; i < 60; i++ {
		noteWarmDecision("t", WarmDecision{Stage: "trigger", Reason: fmt.Sprintf("r%d", i)})
	}
	got := SweepWarmCounters().RecentDecisions
	if len(got) != maxWarmRecent || got[len(got)-1].Reason != "r59" || got[0].Reason != "r10" {
		t.Fatalf("kept %d, first %q last %q; want the last %d", len(got), got[0].Reason, got[len(got)-1].Reason, maxWarmRecent)
	}
}

// ---------------------------------------------------------------------------------------------------
// The turns-left model (#491, David)

func turnsCtx(window int, exact bool, billed int, cutFrac float64) *components.Ctx {
	return &components.Ctx{CtxWindow: window, CtxWindowExact: exact, PrevBilledInput: billed, CutFrac: cutFrac}
}

// (a) warm_expected_turns: N = max(expected - turns so far, 0).
func TestExpectedTurnsSetsTheTurnsLeft(t *testing.T) {
	e := shippedSweep(t, "warm: true\nwarm_expected_turns: 100\nwarm_max_turns: 1000\n")
	c := turnsCtx(1_000_000, true, 200_000, 0.9)
	if te := e.warmTurnsLeft(c, 30, 100_000, 10); te.Model != turnsModelExpected || te.N != 70 || te.Raw != 70 {
		t.Fatalf("estimate = %+v, want expected: 100 - 30 = 70", te)
	}
	// Past the expected length there is nothing left.
	if te := e.warmTurnsLeft(c, 130, 100_000, 10); te.N != 0 || te.Raw != -30 || te.Model != turnsModelExpected {
		t.Fatalf("estimate = %+v, want N 0 (raw -30) once the session is past its expected length", te)
	}
	// It wins over the cut model even when that could be computed.
	if te := e.warmTurnsLeft(c, 30, 100_000, 10); te.Model != turnsModelExpected {
		t.Fatalf("model = %q, want expected to win", te.Model)
	}
}

// (b) the cut model, with a summarizer in the pipeline (cut at its fraction of the window) ...
func TestTheCutModelRunsToTheSummarizersFractionOfTheWindow(t *testing.T) {
	e := shippedSweep(t, "warm: true\nwarm_max_turns: 1000\n")
	// window 1,000,000, cut at 0.9 = 900,000; billed now 300,000; it grew 100,000 -> 300,000 over 20 turns
	// = 10,000 per turn; (900,000 - 300,000) / 10,000 = 60 turns.
	c := turnsCtx(1_000_000, true, 300_000, 0.9)
	te := e.warmTurnsLeft(c, 20, 100_000, 20)
	if te.Model != turnsModelCut || te.Cut != 900_000 || te.GrowthPerTurn != 10_000 || te.N != 60 || te.Raw != 60 {
		t.Fatalf("estimate = %+v, want cut 900,000, growth 10,000, N 60", te)
	}
}

// ... and without one, the full window.
func TestTheCutModelRunsToTheFullWindowWithoutASummarizer(t *testing.T) {
	e := shippedSweep(t, "warm: true\nwarm_max_turns: 1000\n")
	c := turnsCtx(1_000_000, true, 300_000, 0)
	te := e.warmTurnsLeft(c, 20, 100_000, 20)
	if te.Model != turnsModelCut || te.Cut != 1_000_000 || te.N != 70 {
		t.Fatalf("estimate = %+v, want cut 1,000,000 and N (1,000,000 - 300,000) / 10,000 = 70", te)
	}
	// The cut uses the conversation's own compaction point when the host knows it (FillDenominator).
	c.CompactionPoint = 500_000
	if te := e.warmTurnsLeft(c, 20, 100_000, 20); te.Cut != 500_000 || te.N != 20 {
		t.Fatalf("estimate = %+v, want the compaction point as the denominator", te)
	}
}

// Fallbacks to the warm turns so far, each with its reason.
func TestTheCutModelFallsBackToTheTurnsSoFarAndSaysWhy(t *testing.T) {
	e := shippedSweep(t, "warm: true\nwarm_max_turns: 1000\n")
	for name, tc := range map[string]struct {
		c           *components.Ctx
		from, turns int
		why         string
	}{
		"growth unknown (no turns yet)": {turnsCtx(1_000_000, true, 300_000, 0.9), 100_000, 0, turnsWhyNoGrowth},
		"growth not positive":           {turnsCtx(1_000_000, true, 100_000, 0.9), 100_000, 5, turnsWhyNoGrowth},
		"no billed size":                {turnsCtx(1_000_000, true, 0, 0.9), 100_000, 5, turnsWhyNoBilled},
		"window only a guess":           {turnsCtx(1_000_000, false, 300_000, 0.9), 100_000, 20, turnsWhyWindowNotExact},
		"already past the cut":          {turnsCtx(1_000_000, true, 950_000, 0.9), 100_000, 20, turnsWhyBeyondCut},
	} {
		te := e.warmTurnsLeft(tc.c, 20, tc.from, tc.turns)
		if te.Model != turnsModelSoFar || te.N != 20 || te.Why != tc.why {
			t.Errorf("%s: estimate = %+v, want so_far_fallback N 20 because %q", name, te, tc.why)
		}
	}
}

// warm_max_turns caps N, and the raw value is kept for the log.
func TestTheCapAppliesToEveryTurnsModel(t *testing.T) {
	e := shippedSweep(t, "warm: true\nwarm_max_turns: 25\nwarm_expected_turns: 500\n")
	if te := e.warmTurnsLeft(turnsCtx(1_000_000, true, 300_000, 0.9), 10, 0, 0); te.N != 25 || te.Raw != 490 {
		t.Fatalf("expected model: %+v, want N capped at 25 with raw 490", te)
	}
	e = shippedSweep(t, "warm: true\nwarm_max_turns: 25\n")
	if te := e.warmTurnsLeft(turnsCtx(1_000_000, true, 300_000, 0.9), 20, 100_000, 20); te.N != 25 || te.Raw != 60 {
		t.Fatalf("cut model: %+v, want N capped at 25 with raw 60", te)
	}
	if te := e.warmTurnsLeft(turnsCtx(1_000_000, true, 0, 0), 99, 0, 0); te.N != 25 || te.Raw != 99 {
		t.Fatalf("so-far fallback: %+v, want N capped at 25 with raw 99", te)
	}
}

func TestWarmExpectedTurnsIsOffByDefault(t *testing.T) {
	if shippedSweep(t, "warm: true\n").warmExpectedTurns != 0 {
		t.Fatal("warm_expected_turns must default to 0 (unset)")
	}
}

// The growth per turn restarts at a shrink, so a compaction does not poison the average.
func TestTheGrowthRunRestartsWhenTheBilledPromptShrinks(t *testing.T) {
	var th warmThread
	for _, b := range []int{100_000, 110_000, 120_000, 130_000} {
		th.noteBilledGrowth(b)
	}
	if th.growthFrom != 100_000 || th.growthTurns != 3 {
		t.Fatalf("growth = from %d over %d turns, want from 100,000 over 3", th.growthFrom, th.growthTurns)
	}
	th.noteBilledGrowth(40_000) // a compaction
	if th.growthFrom != 40_000 || th.growthTurns != 0 {
		t.Fatalf("after a shrink growth = from %d over %d turns, want a restart at 40,000", th.growthFrom, th.growthTurns)
	}
	th.noteBilledGrowth(0) // an unknown billed size changes nothing
	if th.growthFrom != 40_000 {
		t.Fatalf("an unknown size moved the run: %+v", th)
	}
}

// The pipeline hands the summarizer's trigger fraction to the sweep, without the sweep reaching into it.
func TestThePipelineHandsTheSummarizersCutFractionToTheSweep(t *testing.T) {
	sum, err := newCacheAwareSummarizer([]byte("trigger:\n  min_request_frac: 0.7\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := sum.(components.CutFracProvider).CutFrac(); got != 0.7 {
		t.Fatalf("the summarizer reports a cut fraction of %v, want 0.7", got)
	}
	spy := &cutSpy{}
	pipe := components.NewPipeline([]components.Component{sum, spy}, nil)
	c := &components.Ctx{Ctx: context.Background(), Session: "cutfrac", Store: newMemStore(), CtxWindow: 1_000_000}
	pipe.Run(&bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("hi")}}, c)
	if spy.seen != 0.7 {
		t.Fatalf("a later component saw CutFrac %v, want 0.7", spy.seen)
	}
	// No summarizer in the pipeline: no fraction.
	spy2 := &cutSpy{}
	c2 := &components.Ctx{Ctx: context.Background(), Session: "cutfrac2", Store: newMemStore(), CtxWindow: 1_000_000}
	components.NewPipeline([]components.Component{spy2}, nil).Run(&bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("hi")}}, c2)
	if spy2.seen != 0 {
		t.Fatalf("with no summarizer CutFrac = %v, want 0", spy2.seen)
	}
}

type cutSpy struct{ seen float64 }

func (*cutSpy) Name() string                 { return "cut_spy" }
func (*cutSpy) Enabled(*components.Ctx) bool { return true }
func (s *cutSpy) Offload(req *bschemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) ([]string, error) {
	s.seen = c.CutFrac
	return nil, nil
}

// The decision log records which model produced N.
func TestTheDecisionRecordsWhichTurnsModelProducedN(t *testing.T) {
	req, rm, _ := deepFixture(3)
	res := recheckResult{Valid: []heldOutput{{ToolUseID: "toolu_d0", Saves: rm[0].saves}}}
	e := shippedSweep(t, "warm: true\nwarm_max_turns: 1000\n")
	c := warmCtx("w-model-dec", newMemStore())
	c.ModelName, c.PrevBilledInput = "claude-sonnet-5", localTokens(req.Input)
	c.CtxWindow, c.CtxWindowExact, c.CutFrac = 10_000_000, true, 0.9
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	e.warmGate(c, req, 5, c.PrevBilledInput/2, 5, localTokens(req.Input), &warmApply{})(res, req.Input)
	ds := decisionsOf("apply")
	if len(ds) != 1 || ds[0].TurnsModel != turnsModelCut || ds[0].Cut != 9_000_000 || ds[0].GrowthPerTurn <= 0 || ds[0].TurnsRaw <= 5 {
		t.Fatalf("decision = %+v, want the cut model with its cut, growth and raw N", ds)
	}
}

// The TRIGGER uses the same turns-left model as the gate and records which one, so a thread is not asked
// about while the gate would refuse it for want of turns.
func TestTheTriggerRecordsAndUsesTheSameTurnsModel(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	e := shippedSweep(t, "warm: true\nwarm_max_turns: 1000\nwarm_min_age_turns: 5\nwarm_min_mass_frac: 0.01\n")
	req := warmConv(8, payingTail())
	c := warmCtx("w-trig-model", newMemStore())
	c.ModelName, c.PrevBilledInput = "claude-sonnet-5", localTokens(req.Input)
	c.CtxWindow, c.CtxWindowExact, c.CutFrac = 10_000_000, true, 0.9
	e.warmWorthAsking(c, req, localTokens(req.Input), 4, c.PrevBilledInput/2, 4)
	ds := decisionsOf("trigger")
	if len(ds) != 1 || ds[0].TurnsModel != turnsModelCut || ds[0].Cut != 9_000_000 || ds[0].GrowthPerTurn <= 0 || ds[0].Turns <= 4 {
		t.Fatalf("decision = %+v, want the cut model's N (above the 4 turns so far) with its cut and growth", ds)
	}
	// With the cut model unavailable it falls back to the turns so far and says why.
	c.CtxWindowExact = false
	resetWarmForTest()
	e.warmWorthAsking(c, req, localTokens(req.Input), 4, c.PrevBilledInput/2, 4)
	ds = decisionsOf("trigger")
	if len(ds) != 1 || ds[0].TurnsModel != turnsModelSoFar || ds[0].TurnsWhy != turnsWhyWindowNotExact || ds[0].Turns != 4 {
		t.Fatalf("decision = %+v, want the so-far fallback (N 4) because the window is not exact", ds)
	}
}

// With two cutting components the SMALLEST fraction wins: the session is cut at the first one.
func TestThePipelineHandsTheSmallestCutFraction(t *testing.T) {
	a, err := newCacheAwareSummarizer([]byte("trigger:\n  min_request_frac: 0.9\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := newCacheAwareSummarizer([]byte("trigger:\n  min_request_frac: 0.6\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]components.Component{{a, b}, {b, a}} {
		spy := &cutSpy{}
		c := &components.Ctx{Ctx: context.Background(), Session: "cutfrac3", Store: newMemStore(), CtxWindow: 1_000_000}
		components.NewPipeline(append(append([]components.Component{}, order...), spy), nil).
			Run(&bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("hi")}}, c)
		if spy.seen != 0.6 {
			t.Fatalf("with 0.9 and 0.6 the sweep saw %v, want the smallest, 0.6", spy.seen)
		}
	}
}

// A component that is configured but DISABLED for this request cuts nothing, so its fraction is not handed
// to the sweep.
type disabledCutter struct{}

func (disabledCutter) Name() string                 { return "disabled_cutter" }
func (disabledCutter) Enabled(*components.Ctx) bool { return false }
func (disabledCutter) CutFrac() float64             { return 0.3 }
func (disabledCutter) Offload(*bschemas.BifrostChatRequest, *components.Report, *components.Ctx) ([]string, error) {
	return nil, nil
}

func TestADisabledCuttingComponentIsNotCountedAsACut(t *testing.T) {
	spy := &cutSpy{}
	c := &components.Ctx{Ctx: context.Background(), Session: "cutfrac4", Store: newMemStore(), CtxWindow: 1_000_000}
	components.NewPipeline([]components.Component{disabledCutter{}, spy}, nil).
		Run(&bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("hi")}}, c)
	if spy.seen != 0 {
		t.Fatalf("a disabled component's fraction reached the sweep: CutFrac = %v, want 0", spy.seen)
	}
}

// MODEL_PRICES supplies the window. The cut model needs an EXACT window (the summarizer's own trigger
// refuses a guessed one). An operator's price table is authoritative: a `window:` for a model makes it
// exact, so the cut model runs for a model the public map does not know, such as a GLM behind a gateway.
// This goes through the real modelinfo resolver, not a hand-set Ctx.
func TestAWindowFromMODELPRICESMakesTheCutModelRunForANonClaudeModel(t *testing.T) {
	const model = "rits/zai-org/glm-5-3"
	table, err := modelinfo.ParseTable([]byte("models:\n  - {match: \"" + model + "\", in: 1.0, out: 4.0, window: 400000}\n"))
	if err != nil {
		t.Fatal(err)
	}
	w, exact, ok := table.WindowExact(context.Background(), model)
	if !ok || !exact || w != 400000 {
		t.Fatalf("the table gave window %d exact=%v ok=%v, want 400000 exact", w, exact, ok)
	}
	// Without the table entry the same model has no exact window, so the cut model must not run.
	if _, exact, ok := modelinfo.DefaultBuiltin().WindowExact(context.Background(), model); ok && exact {
		t.Fatalf("the public map claims an exact window for %s: the fixture proves nothing", model)
	}

	e := shippedSweep(t, "warm: true\nwarm_max_turns: 1000\n")
	// The context as apply builds it from the resolver: the window and its exactness.
	c := &components.Ctx{CtxWindow: w, CtxWindowExact: exact, PrevBilledInput: 100_000}
	te := e.warmTurnsLeft(c, 20, 40_000, 20) // grew 40,000 -> 100,000 over 20 turns = 3,000 per turn
	if te.Model != turnsModelCut || te.Cut != 400_000 || te.GrowthPerTurn != 3_000 || te.N != 100 {
		t.Fatalf("estimate = %+v, want the cut model: cut 400,000 (the table's window), 3,000 a turn, N 100", te)
	}
	// And the same model WITHOUT an exact window falls back and says why.
	c2 := &components.Ctx{CtxWindow: 200_000, CtxWindowExact: false, PrevBilledInput: 100_000}
	if te := e.warmTurnsLeft(c2, 20, 40_000, 20); te.Model != turnsModelSoFar || te.Why != turnsWhyWindowNotExact {
		t.Fatalf("estimate = %+v, want the fallback with window_not_exact", te)
	}
}

// ---------------------------------------------------------------------------------------------------
// The warm ask uses the SAME batching function as the cold ask (#747, #491).

// batchAsker answers every question by dropping every label in it, and counts the questions.
type batchAsker struct {
	mu    sync.Mutex
	calls int
	fn    func(n int, ask string) (string, components.PrefixUsage, error)
}

func (a *batchAsker) Ask(_ context.Context, _, ask string) (string, components.PrefixUsage, error) {
	a.mu.Lock()
	n := a.calls
	a.calls++
	a.mu.Unlock()
	if a.fn != nil {
		return a.fn(n, ask)
	}
	return dropAll(ask), components.PrefixUsage{CacheRead: 19595, Fresh: 40, Output: 90}, nil
}

func (a *batchAsker) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

// bigWarmRig is a warm rig whose conversation has n outputs, asked by a batchAsker. The tail is sized so
// the gate pays for the whole set.
func bigWarmRig(t *testing.T, session string, n int, asker *batchAsker) *warmRig {
	t.Helper()
	r := newWarmRig(t, session, warmCfg+"warm_min_mass_frac: 0.01\n", 0, 1.0)
	r.outputs = n
	r.asker = nil
	r.tail = 0
	// Size the tail so margin = 3 for the WHOLE set at the gate's 2 expected turns.
	save := outputSavesN(n)
	r.tail = int(float64(save) * 0.3 * 2 / (3 * 3.45))
	r.askerAny = asker
	return r
}

func outputSavesN(n int) int {
	req := warmConvN(n, 8, 0)
	total := 0
	for _, i := range toolIndices(req) {
		text := schema.MessageText(req.Input[i])
		total += schema.TextTokens(text) - schema.TextTokens(sweepDescriptor(text))
	}
	return total
}

// ⭐ A warm sweep over 30 outputs asks 3 batches (the SAME askInBatches the cold path uses), holds ONE
// answer for all 30, and the next warm request makes ONE gate decision and ONE cut.
func TestAWarmSweepOverManyOutputsAsksBatchesAndMakesOneGateDecisionAndOneCut(t *testing.T) {
	a := &batchAsker{}
	r := bigWarmRig(t, "w-batch", 30, a)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	if got := a.count(); got != 3 {
		t.Fatalf("the warm question asked %d times, want 3 batches of 10", got)
	}
	if SweepBatchCounters().Batches != 3 {
		t.Errorf("sweep_ask_batches = %d, want 3", SweepBatchCounters().Batches)
	}
	if !r.held() {
		t.Fatalf("no merged answer is waiting: %+v", SweepWarmCounters())
	}
	warmMu.Lock()
	held := warmThreads[r.key()].held
	warmMu.Unlock()
	if held == nil || len(held.Outputs) != 30 {
		t.Fatalf("the waiting answer holds %d outputs, want ONE merged answer of all 30", len(held.Outputs))
	}
	seen := map[string]int{}
	for _, o := range held.Outputs {
		seen[o.ToolUseID]++
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("%s is judged %d times", id, c)
		}
	}
	// The next warm request: ONE gate decision over the merged set, and ONE cut.
	before := len(decisionsOf("apply"))
	req, rep := r.request(nil)
	ds := decisionsOf("apply")
	if len(ds)-before != 1 {
		t.Fatalf("%d apply decisions, want exactly ONE over the merged set", len(ds)-before)
	}
	d := ds[len(ds)-1]
	if !d.Accept || d.RemovedLocal <= 0 {
		t.Fatalf("decision = %+v, want an accept over the whole set", d)
	}
	if rep.Events[eventAppliedWarm] != 30 {
		t.Fatalf("events = %v, want all 30 drops applied in ONE cut", rep.Events)
	}
	dropped := 0
	for _, i := range toolIndices(req) {
		if expand.HasPlaceholder(schema.MessageText(req.Input[i])) {
			dropped++
		}
	}
	if dropped != 30 {
		t.Fatalf("%d outputs dropped, want 30", dropped)
	}
	if c := SweepWarmCounters(); c.GateAccept != 1 || c.GateRefuse != 0 {
		t.Errorf("gate counters = %+v, want exactly one accept", c)
	}
}

// Twelve or fewer outputs: one ask, as before.
func TestAWarmSweepOverTwelveOrFewerOutputsMakesOneAsk(t *testing.T) {
	a := &batchAsker{}
	r := bigWarmRig(t, "w-batch-small", 8, a)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	if got := a.count(); got != 1 {
		t.Fatalf("%d asks for 8 outputs, want exactly 1", got)
	}
}

// A failed batch fails open on the warm path too: its outputs are left out, the others still count, and the
// gate decides over what answered.
func TestAFailedWarmBatchKeepsItsOutputsAndTheOthersAreGated(t *testing.T) {
	a := &batchAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		if n == 1 {
			return "", components.PrefixUsage{}, errors.New("boom")
		}
		return dropAll(ask), components.PrefixUsage{CacheRead: 19595, Fresh: 40, Output: 90}, nil
	}}
	r := bigWarmRig(t, "w-batch-fail", 30, a)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	warmMu.Lock()
	held := warmThreads[r.key()].held
	warmMu.Unlock()
	if held == nil || len(held.Outputs) != 20 {
		t.Fatalf("the waiting answer holds %d outputs, want the 20 of the two batches that answered", len(held.Outputs))
	}
	if SweepBatchCounters().FailedBy["ask_failed"] != 1 {
		t.Errorf("batch counters = %+v, want one ask_failed", SweepBatchCounters())
	}
}

// The latch rule is the cold path's: a panic AFTER a paid call keeps the registry candidate asked, so the
// ping that follows is plain and a repeating panic does not pay on every ping.
func TestAWarmPanicAfterAPaidCallKeepsTheThreadAsked(t *testing.T) {
	a := &batchAsker{}
	r := bigWarmRig(t, "w-batch-panic", 8, a)
	sweepPanicAfterCallHook = func() { panic("boom after a paid call") }
	defer func() { sweepPanicAfterCallHook = nil }()
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	if a.count() != 1 {
		t.Fatalf("%d asks, want the one paid call", a.count())
	}
	if r.held() {
		t.Fatal("an answer is held after a panic")
	}
	sweepPingMu.Lock()
	reg := sweepPingCand[r.key()]
	sweepPingMu.Unlock()
	if reg == nil || !reg.asked.Load() {
		t.Fatal("the registry latch was released after a panic that came AFTER a paid call")
	}
	if _, _, reason, ok := SweepPingSubstitute(r.key()); ok || reason != SweepReasonAlreadyAsked {
		t.Fatalf("next ping: ok=%v reason=%q, want already_asked", ok, reason)
	}
}

// The warm ask honours max_ask_batches through the shared function: it cannot ask more than the operator's bound.
func TestTheWarmAskHonoursMaxAskBatches(t *testing.T) {
	a := &batchAsker{}
	r := bigWarmRig(t, "w-batch-bound", 60, a)
	r.e = shippedSweep(t, warmCfg+"warm_min_mass_frac: 0.01\nmax_ask_batches: 2\n")
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	if got := a.count(); got != 2 {
		t.Fatalf("the warm question asked %d times, want 2 (max_ask_batches: 2)", got)
	}
	if SweepBatchCounters().OverCap < 36 {
		t.Errorf("batch counters = %+v, want the 36 candidates beyond 2 batches counted", SweepBatchCounters())
	}
}

// The warm sweep's OUTER bound is the keeper's (300 s), as at the ping, not the 90 s of one question: a
// sweep of k batches would otherwise end after the first one. A timeout of 0 falls back to llmCallTimeout.
func TestTheWarmSweepOuterTimeoutFitsSeveralBatches(t *testing.T) {
	prev := llmCallTimeout
	llmCallTimeout = 40 * time.Millisecond // one question's bound, tiny
	defer func() { llmCallTimeout = prev }()
	slow := &batchAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		time.Sleep(30 * time.Millisecond) // each batch is under one question's bound; three together are over it
		return dropAll(ask), components.PrefixUsage{CacheRead: 19595, Fresh: 40, Output: 90}, nil
	}}
	r := bigWarmRig(t, "w-outer", 30, slow)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	if got := slow.count(); got != 3 {
		t.Fatalf("the warm sweep asked %d of 3 batches: the OUTER bound must be the keeper's, not one question's", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// Diagnosis of the live run t94 (#720): why a refused answer showed no reason.

// requestReferencingOutputs sends the next request with an assistant turn that quotes the identifiers of the
// first n tool outputs. A reference made AFTER the question is what the recheck calls "referenced": the
// output is still needed. The prefix is untouched, so this stays a WARM, unchanged-prefix apply that reaches
// the gate (changing an output's text would change its message print and send the request down the cold
// apply's changed-prefix path instead, which has no gate).
func (r *warmRig) requestReferencingOutputs(n int) (*bschemas.BifrostChatRequest, *components.Report) {
	r.t.Helper()
	req := r.conv(r.step)
	prev := r.conv(r.step - 1)
	r.step++
	var refs []string
	for i := 0; i < n; i++ {
		refs = append(refs, fmt.Sprintf("handler_%d_timeout_ms in src/api/users_%d.py:42", i, i))
	}
	req.Input = append(req.Input, assistantMsg("I will raise "+strings.Join(refs, " and ")+" now"), userMsg("ok"))
	c := warmCtx(r.session, r.st)
	c.PrefixAsk = r.asker
	c.PrevBilledInput = int(r.ratio * float64(localTokens(prev.Input)))
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := r.e.Offload(req, rep, c); err != nil {
		r.t.Fatal(err)
	}
	return req, rep
}

// ⭐ FIX 1. The recheck's reasons are booked BEFORE the gate, on the row and in the decision. Before, they
// were booked at the end of applyHeldFrom, which a refusal never reaches: a refused answer showed no reason
// for what the recheck had thrown out.
func TestARefusedWarmApplyBooksWhyTheRecheckDroppedTheJudgesDrops(t *testing.T) {
	r := newWarmRig(t, "w-recheck", warmCfg, payingTail(), 1.0)
	r.toHeld() // the judge proposes all 3 outputs as drops
	_, rep := r.requestReferencingOutputs(3)
	if rep.Events[eventAppliedWarm] != 0 {
		t.Fatalf("fixture: nothing may apply (events %v)", rep.Events)
	}
	if rep.Events["sweep_apply_dropped_referenced_later"] != 3 {
		t.Errorf("events = %v, want sweep_apply_dropped_referenced_later = 3 on the row of a REFUSED apply", rep.Events)
	}
	ds := decisionsOf("apply")
	if len(ds) != 1 {
		t.Fatalf("apply decisions = %+v, want one", SweepWarmCounters().RecentDecisions)
	}
	d := ds[0]
	if d.Accept || d.Reason != "no_valid_drop" || d.Proposed != 3 || d.Dropped["referenced_later"] != 3 {
		t.Fatalf("decision = %+v, want a refusal no_valid_drop with proposed 3 and dropped referenced_later 3", d)
	}
}

// A partial invalidation is recorded too, on an answer the gate then accepts or refuses on what is left.
func TestAPartialRecheckLossIsRecordedWithItsReason(t *testing.T) {
	r := newWarmRig(t, "w-recheck2", warmCfg, payingTail(), 1.0)
	r.toHeld()
	_, rep := r.requestReferencingOutputs(1)
	if rep.Events["sweep_apply_dropped_referenced_later"] != 1 {
		t.Fatalf("events = %v, want referenced_later = 1", rep.Events)
	}
	ds := decisionsOf("apply")
	if len(ds) != 1 || ds[0].Proposed != 3 || ds[0].Dropped["referenced_later"] != 1 {
		t.Fatalf("decisions = %+v, want proposed 3 and one referenced_later", SweepWarmCounters().RecentDecisions)
	}
}

// An accepted apply records its reasons too (none dropped: the map is empty, proposed is the count).
func TestAnAcceptedWarmApplyRecordsWhatItProposedAndThatNothingWasDropped(t *testing.T) {
	r := newWarmRig(t, "w-recheck3", warmCfg, payingTail(), 1.0)
	r.toHeld()
	_, rep := r.request(nil)
	if rep.Events[eventAppliedWarm] != 3 {
		t.Fatalf("fixture: want 3 applied (events %v)", rep.Events)
	}
	ds := decisionsOf("apply")
	if len(ds) != 1 || !ds[0].Accept || ds[0].Proposed != 3 || len(ds[0].Dropped) != 0 {
		t.Fatalf("decisions = %+v, want an accept with proposed 3 and nothing dropped", SweepWarmCounters().RecentDecisions)
	}
}

// ⭐ FIX 2. The judge proposing NOTHING is not "no valid drop": there was nothing to recheck.
func TestAnAnswerThatProposedNoDropIsLabeledNoDropProposed(t *testing.T) {
	keepAll := &batchAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		var parts []string
		for _, l := range askLabelRe.FindAllStringSubmatch(ask, -1) {
			parts = append(parts, `{"i":`+l[1]+`,"needed_by":"a","quote":"","verdict":"keep"}`)
		}
		return "[" + strings.Join(parts, ",") + "]", components.PrefixUsage{CacheRead: 19595, Fresh: 40, Output: 90}, nil
	}}
	r := bigWarmRig(t, "w-noprop", 8, keepAll)
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	if !r.held() {
		t.Fatalf("fixture: a kept-everything answer must still be held (%+v)", SweepWarmCounters())
	}
	r.request(nil)
	ds := decisionsOf("apply")
	if len(ds) != 1 || ds[0].Reason != "no_drop_proposed" || ds[0].Proposed != 0 {
		t.Fatalf("decisions = %+v, want no_drop_proposed with proposed 0", SweepWarmCounters().RecentDecisions)
	}
	if SweepWarmCounters().Reasons["no_drop_proposed"] != 1 || SweepWarmCounters().Reasons["no_valid_drop"] != 0 {
		t.Errorf("gate reasons = %v, want no_drop_proposed once and no_valid_drop never", SweepWarmCounters().Reasons)
	}
}

// ⭐ FIX 3. A request with FEWER messages than the held answer judged is a different conversation: the answer
// is dropped at once and counted, instead of blocking a new question until the next apply.
func TestAHeldAnswerIsDroppedWhenTheConversationShrinks(t *testing.T) {
	r := newWarmRig(t, "w-shrink", warmCfg, payingTail(), 1.0)
	r.toHeld()
	// The agent starts over: a conversation of 4 messages.
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("a fresh start"), assistantMsg("ok"), userMsg("go"), assistantMsg("sure")}}
	req.Provider, req.Model = bschemas.Anthropic, "claude-sonnet-5"
	c := warmCtx(r.session, r.st)
	c.PrefixAsk = r.asker
	c.PrevBilledInput = 1000
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := r.e.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	if r.held() {
		t.Fatal("the held answer for the old conversation is still held")
	}
	if SweepWarmCounters().HeldShrunk != 1 {
		t.Fatalf("counters = %+v, want held_shrunk = 1", SweepWarmCounters())
	}
	ds := decisionsOf("apply")
	if len(ds) != 1 || ds[0].Reason != "conversation_shrunk" || ds[0].Proposed != 3 || ds[0].Accept {
		t.Fatalf("decisions = %+v, want a conversation_shrunk refusal carrying proposed 3", SweepWarmCounters().RecentDecisions)
	}
	// And it is NOT the gate's: no gate decision was made on an answer that no longer applies.
	if c := SweepWarmCounters(); c.GateAccept+c.GateRefuse != 0 {
		t.Errorf("the gate decided on a voided answer: %+v", c)
	}
	// Nothing was applied to the new conversation.
	for _, i := range toolIndices(req) {
		if expand.HasPlaceholder(schema.MessageText(req.Input[i])) {
			t.Fatal("a drop was applied to a different conversation")
		}
	}
}

// A conversation that did NOT shrink keeps its answer (the guard fires only on a shorter request).
func TestAHeldAnswerSurvivesAConversationThatGrew(t *testing.T) {
	r := newWarmRig(t, "w-noshrink", warmCfg, payingTail(), 1.0)
	r.toHeld()
	r.request(nil)
	if SweepWarmCounters().HeldShrunk != 0 {
		t.Fatalf("counters = %+v, want held_shrunk = 0 for a conversation that grew", SweepWarmCounters())
	}
}

// ⭐ FIX 5. Fabricated quotes are counted per batch position, with how many candidates and verdicts each
// carried, so the next run can tell a batch effect from a model effect.
func TestFabricatedQuotesAreCountedPerBatchPosition(t *testing.T) {
	resetSweepBatchForTest()
	// Batch 0 answers with honest empty quotes; batch 1 answers every verdict with a quote that is in no message.
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		var parts []string
		for _, l := range askLabelRe.FindAllStringSubmatch(ask, -1) {
			q := ""
			if n == 1 {
				q = "a sentence that appears nowhere in the conversation at all"
			}
			needed := "none"
			if q != "" {
				needed = "a"
			}
			parts = append(parts, `{"i":`+l[1]+`,"needed_by":"`+needed+`","quote":"`+q+`","verdict":"keep"}`)
		}
		return "[" + strings.Join(parts, ",") + "]", okUse, nil
	}}
	askN(t, a, "", 20, SweepPingWait{}) // 2 batches of 10
	by := SweepBatchCounters().ByIndex
	if len(by) != 2 {
		t.Fatalf("by-index stats = %+v, want 2 batch positions", by)
	}
	if by[0].Asked != 1 || by[0].Items != 10 || by[0].Judged != 10 || by[0].Fabricated != 0 {
		t.Errorf("batch 0 = %+v, want asked 1, items 10, judged 10, fabricated 0", by[0])
	}
	if by[1].Asked != 1 || by[1].Items != 10 || by[1].Judged != 10 || by[1].Fabricated != 10 {
		t.Errorf("batch 1 = %+v, want asked 1, items 10, judged 10, fabricated 10", by[1])
	}
}

// A batch that FAILED is not tallied as judged: it has no verdicts.
func TestAFailedBatchIsNotCountedAsJudged(t *testing.T) {
	resetSweepBatchForTest()
	a := &dynAsker{fn: func(n int, ask string) (string, components.PrefixUsage, error) {
		if n == 0 {
			return "", components.PrefixUsage{}, errors.New("boom")
		}
		return dropAll(ask), okUse, nil
	}}
	askN(t, a, "", 20, SweepPingWait{})
	by := SweepBatchCounters().ByIndex
	if len(by) != 2 || by[0].Asked != 0 || by[0].Judged != 0 || by[1].Asked != 1 || by[1].Judged != 10 {
		t.Fatalf("by-index stats = %+v, want batch 0 untouched (it failed) and batch 1 judged 10", by)
	}
}

// The boundary of the shrink rule: a request with EXACTLY as many messages as the answer judged is the same
// conversation (nothing was added yet), so the answer is kept. Only a SHORTER one voids it.
func TestAHeldAnswerIsKeptWhenTheRequestHasExactlyAsManyMessagesAsItJudged(t *testing.T) {
	r := newWarmRig(t, "w-boundary", warmCfg, payingTail(), 1.0)
	r.toHeld()
	warmMu.Lock()
	judged := warmThreads[r.key()].held.Judged
	warmMu.Unlock()
	// The conversation the answer was asked about, resent unchanged: exactly `judged` messages.
	req := r.conv(r.step - 1)
	if len(req.Input) != judged {
		t.Fatalf("fixture: the resent conversation has %d messages, the answer judged %d", len(req.Input), judged)
	}
	c := warmCtx(r.session, r.st)
	c.PrefixAsk = r.asker
	c.PrevBilledInput = int(r.ratio * float64(localTokens(req.Input)))
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := r.e.Offload(req, rep, c); err != nil {
		t.Fatal(err)
	}
	if SweepWarmCounters().HeldShrunk != 0 {
		t.Fatalf("counters = %+v: an equal-length request must not void the answer", SweepWarmCounters())
	}
	// It is APPLIED (the gate decided), not voided.
	if len(decisionsOf("apply")) != 1 || decisionsOf("apply")[0].Reason == "conversation_shrunk" {
		t.Fatalf("decisions = %+v, want the gate's own decision", SweepWarmCounters().RecentDecisions)
	}
}

// ---------------------------------------------------------------------------------------------------
// ACCUMULATION (#491, the owner): refused warm drops carry forward until the gate accepts.

func heldOf(askedAtMs int64, judged int, outs ...heldOutput) heldAnswer {
	return heldAnswer{V: heldVersion, AskedAtMs: askedAtMs, Judged: judged, Outputs: outs}
}

func dropOut(id string, saves int) heldOutput {
	return heldOutput{ToolUseID: id, Hash: "h-" + id, Tokens: saves + 20, Saves: saves, Drop: true}
}

func keepOut(id string) heldOutput {
	return heldOutput{ToolUseID: id, Hash: "h-" + id, Tokens: 100, Saves: 80, Drop: false}
}

func outputIDs(h heldAnswer) []string {
	var ids []string
	for _, o := range h.Outputs {
		ids = append(ids, o.ToolUseID)
	}
	sort.Strings(ids)
	return ids
}

// The merge: by tool-use id, each output once; the NEWER verdict wins, a newer keep included; only DROP
// verdicts are carried; carried ones are marked.
func TestMergeHeldKeepsEachOutputOnceAndTheNewerVerdictWins(t *testing.T) {
	now := time.Now().UnixMilli()
	carried := heldOf(now-1000, 50, dropOut("a", 10), dropOut("b", 20), dropOut("c", 30), keepOut("k"))
	newer := heldOf(now, 60, dropOut("c", 31), keepOut("b"), dropOut("d", 40))
	m, st := mergeHeld(carried, newer)
	if got := fmt.Sprint(outputIDs(m)); got != "[a b c d]" {
		t.Fatalf("merged ids = %s, want [a b c d]: each output once, a carried keep not carried", got)
	}
	byID := map[string]heldOutput{}
	for _, o := range m.Outputs {
		byID[o.ToolUseID] = o
	}
	if !byID["a"].Drop || !byID["a"].Carried {
		t.Errorf("a = %+v, want a carried drop", byID["a"])
	}
	if byID["b"].Drop || byID["b"].Carried {
		t.Errorf("b = %+v, want the NEWER keep to override the carried drop", byID["b"])
	}
	if byID["c"].Saves != 31 || byID["c"].Carried {
		t.Errorf("c = %+v, want the newer drop (saves 31) to replace the carried one", byID["c"])
	}
	if !byID["d"].Drop || byID["d"].Carried {
		t.Errorf("d = %+v, want a new drop", byID["d"])
	}
	if st.carriedKept != 1 || st.overridden != 2 {
		t.Errorf("stats = %+v, want 1 carried kept (a) and 2 overridden (b, c)", st)
	}
	if m.Judged != 60 {
		t.Errorf("merged Judged = %d, want the newer question's 60", m.Judged)
	}
}

// The age limit counts from the OLDEST question that still contributes a verdict.
func TestTheMergedAnswerKeepsTheOldestContributingQuestionsAge(t *testing.T) {
	now := time.Now().UnixMilli()
	old := heldOf(now-20*60*1000, 50, dropOut("a", 10))
	m, _ := mergeHeld(old, heldOf(now, 60, dropOut("b", 20)))
	if m.AskedAtMs != old.AskedAtMs {
		t.Fatalf("AskedAtMs = %d, want the carried (older) question's %d", m.AskedAtMs, old.AskedAtMs)
	}
	// If the newer answer replaced EVERY carried verdict, only the newer question contributes.
	m2, _ := mergeHeld(heldOf(now-20*60*1000, 50, dropOut("a", 10)), heldOf(now, 60, keepOut("a")))
	if m2.AskedAtMs != now {
		t.Fatalf("AskedAtMs = %d, want the newer question's %d once the carried verdict is replaced", m2.AskedAtMs, now)
	}
}

// The cap: the OLDEST carried drops go first.
func TestTheCarriedSetIsCappedAndTheOldestGoFirst(t *testing.T) {
	now := time.Now().UnixMilli()
	var outs []heldOutput
	for i := 0; i < maxCarriedDrops+30; i++ {
		outs = append(outs, dropOut(fmt.Sprintf("o%04d", i), 10))
	}
	m, st := mergeHeld(heldOf(now-1, 10, outs...), heldOf(now, 10, dropOut("new", 10)))
	if len(m.Outputs) > maxCarriedDrops || st.evicted != 31 {
		t.Fatalf("merged %d outputs, evicted %d; want at most %d and 31 evicted", len(m.Outputs), st.evicted, maxCarriedDrops)
	}
	have := map[string]bool{}
	for _, o := range m.Outputs {
		have[o.ToolUseID] = true
	}
	if have["o0000"] || !have[fmt.Sprintf("o%04d", maxCarriedDrops+29)] || !have["new"] {
		t.Fatal("the cap kept an old carried drop over a newer one, or lost the new answer")
	}
}

// warmHold merges with what the thread already carries, counts it, and clears the 'tried' mark.
func TestWarmHoldMergesWithTheCarriedSetAndClearsTried(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	now := time.Now().UnixMilli()
	warmHold("acc", heldOf(now-5, 40, dropOut("a", 10), dropOut("b", 20)))
	warmMu.Lock()
	warmThreads["acc"].heldTried = true
	warmMu.Unlock()
	warmHold("acc", heldOf(now, 50, dropOut("c", 30), keepOut("a")))
	warmMu.Lock()
	th := warmThreads["acc"]
	got, tried := *th.held, th.heldTried
	warmMu.Unlock()
	if fmt.Sprint(outputIDs(got)) != "[a b c]" || tried {
		t.Fatalf("held ids %v tried=%v, want [a b c] merged and tried cleared", outputIDs(got), tried)
	}
	c := SweepWarmCounters()
	if c.CarriedMerged != 1 || c.CarriedOverridden != 1 {
		t.Fatalf("counters = %+v, want 1 carried merged (b) and 1 overridden (a)", c)
	}
}

// The age limit: a carried set older than the held answer's limit is dropped and counted when a new answer arrives.
func TestACarriedSetOlderThanTheLimitIsDroppedAndCounted(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	old := time.Now().Add(-2 * warmHeldMaxAge).UnixMilli()
	warmHold("age", heldOf(old, 40, dropOut("a", 10), dropOut("b", 20)))
	warmMu.Lock()
	warmThreads["age"].heldAt = time.Now()
	warmMu.Unlock()
	warmHold("age", heldOf(time.Now().UnixMilli(), 50, dropOut("c", 30)))
	warmMu.Lock()
	got := *warmThreads["age"].held
	warmMu.Unlock()
	if fmt.Sprint(outputIDs(got)) != "[c]" {
		t.Fatalf("held ids = %v, want only the new answer: the over-age carried drops must be gone", outputIDs(got))
	}
	if SweepWarmCounters().CarriedExpired != 2 {
		t.Fatalf("counters = %+v, want carried_expired = 2", SweepWarmCounters())
	}
	// And get() refuses an answer that is over age by its OLDEST question even if heldAt is fresh.
	warmMu.Lock()
	warmThreads["age"].held.AskedAtMs = old
	warmThreads["age"].heldAt = time.Now()
	warmMu.Unlock()
	if _, ok := (memHeld{"age"}).get(""); ok {
		t.Fatal("get() returned an answer older than the limit")
	}
}

// Nothing is written to the Store: the carried set is memory only.
func TestTheCarriedSetIsNeverWrittenToTheStore(t *testing.T) {
	r := newWarmRig(t, "w-carry-store", warmCfg, payingTail(), 6.0) // billed 6x local: the gate refuses and carries
	rs := &recStore{Store: r.st}
	r.st = rs
	r.toHeld()
	_, rep := r.request(nil)
	if rep.Events["sweep_warm_carried_drops"] != 3 {
		t.Fatalf("events = %v, want 3 drops carried after the refusal", rep.Events)
	}
	for _, k := range rs.keys() {
		if strings.HasPrefix(k, store.SweepHeldPrefix) {
			t.Fatalf("a carried drop was written to the Store under %q", k)
		}
	}
	for _, p := range rs.payloads() {
		if strings.Contains(p, "toolu_wire_") && strings.Contains(p, `"drop"`) {
			t.Fatalf("a Store write carries a held verdict: %.100s", p)
		}
	}
	if _, ok := getHeld(r.st, r.key()); ok {
		t.Fatal("the Store holds a held answer")
	}
	resetWarmForTest() // a restart: nothing carried
	if r.held() {
		t.Fatal("a restart kept a carried set")
	}
}

// A shrink clears the carried set (fix 3 covers carried drops, not only a fresh answer).
func TestAConversationShrinkClearsTheCarriedSet(t *testing.T) {
	r := newWarmRig(t, "w-carry-shrink", warmCfg, payingTail(), 6.0)
	r.toHeld()
	r.request(nil) // the gate refuses, 3 drops carried
	if !r.held() {
		t.Fatal("fixture: nothing was carried")
	}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("a fresh start"), assistantMsg("ok")}}
	req.Provider, req.Model = bschemas.Anthropic, "claude-sonnet-5"
	c := warmCtx(r.session, r.st)
	c.PrefixAsk = r.asker
	c.PrevBilledInput = 1000
	if _, err := r.e.Offload(req, &components.Report{Component: "extract_llm_sweep"}, c); err != nil {
		t.Fatal(err)
	}
	if r.held() || SweepWarmCounters().HeldShrunk != 1 {
		t.Fatalf("held=%v counters=%+v, want the carried set cleared and held_shrunk 1", r.held(), SweepWarmCounters())
	}
}

// A carried set the gate refused is NOT retried on the next warm request: it waits for a new question.
func TestACarriedSetIsNotRetriedUntilANewQuestionIsMergedIn(t *testing.T) {
	r := newWarmRig(t, "w-carry-wait", warmCfg, payingTail(), 6.0)
	r.toHeld()
	r.request(nil) // refused and carried
	before := len(decisionsOf("apply"))
	for i := 0; i < 2; i++ {
		_, rep := r.request(nil)
		// A retry could ACCEPT as well as refuse, so count decisions and applied drops, not refusals only.
		if rep.Events[eventAppliedWarm] != 0 {
			t.Fatalf("request %d applied %d warm drops from a set that was waiting", i, rep.Events[eventAppliedWarm])
		}
	}
	if got := len(decisionsOf("apply")); got != before {
		t.Fatalf("the gate was retried on a carried set: %d apply decisions -> %d", before, got)
	}
	warmMu.Lock()
	tried := warmThreads[r.key()].heldTried
	warmMu.Unlock()
	if !tried {
		t.Fatal("a refused set is not marked tried")
	}
}

// sliceAsker answers question q (0, 1, 2) over the outputs it is shown, dropping every output whose number is
// in the q-th third of n and keeping the rest. It stands for a judge that is consistent between questions. What
// differs between questions is WHICH outputs the question covers, which the test controls through the
// candidate set (a failed batch, the cap, an output that is not yet old enough). n must be 12 or fewer so a
// question is ONE batch. The output a label names is read from the ask's text (handler_<num>_...).
type sliceAsker struct {
	mu    sync.Mutex
	calls int
	n     int
	// seen records, per question, the output numbers the question named, so a test can say what it covered.
	seen [][]int
}

func (a *sliceAsker) Ask(_ context.Context, _, ask string) (string, components.PrefixUsage, error) {
	a.mu.Lock()
	q := a.calls
	a.calls++
	a.mu.Unlock()
	var parts []string
	var nums []int
	for _, l := range askLabelRe.FindAllStringSubmatch(ask, -1) {
		verdict, needed := "keep", "a"
		rest := ask[strings.Index(ask, "["+l[1]+"] "):]
		if j := strings.Index(rest, "handler_"); j >= 0 {
			var num int
			fmt.Sscanf(rest[j:], "handler_%d_", &num)
			nums = append(nums, num)
			if num/(a.n/3) == q {
				verdict, needed = "drop", "none"
			}
		}
		parts = append(parts, `{"i":`+l[1]+`,"needed_by":"`+needed+`","quote":"","verdict":"`+verdict+`"}`)
	}
	a.mu.Lock()
	a.seen = append(a.seen, nums)
	a.mu.Unlock()
	return "[" + strings.Join(parts, ",") + "]", components.PrefixUsage{CacheRead: 19595, Fresh: 40, Output: 90}, nil
}

func (a *sliceAsker) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.calls }

// ⭐ ACCUMULATION through the real trigger, question and apply: a refusal CARRIES its surviving drops, the next
// question's answer is merged with them, and the merged set is what the gate decides on. The next question
// here covers only the outputs the first did NOT drop (a smaller slice), so the carried drops are not
// re-judged and survive; the merged D is bigger than either answer alone.
func TestARefusedAnswersDropsAreCarriedAndMergedWithTheNextQuestionsAndTheGateDecidesOnTheMergedSet(t *testing.T) {
	r := newWarmRig(t, "w-acc", warmCfg, payingTail(), 6.0) // billed 6x: the gate refuses and carries
	r.toHeld()
	warmMu.Lock()
	first := *warmThreads[r.key()].held
	warmMu.Unlock()
	if len(first.Outputs) != 3 {
		t.Fatalf("fixture: question 1 judged %d outputs", len(first.Outputs))
	}
	_, rep := r.request(nil) // the gate refuses
	if rep.Events["sweep_warm_carried_drops"] != 3 {
		t.Fatalf("events = %v, want the refusal to carry 3 drops", rep.Events)
	}
	ds := decisionsOf("apply")
	if len(ds) != 1 || ds[0].Accept || ds[0].New != 3 || ds[0].Carried != 0 {
		t.Fatalf("decisions = %+v, want one refusal over 3 NEW drops", SweepWarmCounters().RecentDecisions)
	}
	// A new question's answer arrives (a DIFFERENT output set: output "toolu_wire_9" only). It is merged in.
	now := time.Now().UnixMilli()
	warmHold(r.key(), heldOf(now, first.Judged, dropOut("toolu_wire_9", 500)))
	warmMu.Lock()
	merged := *warmThreads[r.key()].held
	tried := warmThreads[r.key()].heldTried
	warmMu.Unlock()
	if tried || len(merged.Outputs) != 4 {
		t.Fatalf("merged %d outputs tried=%v, want the 3 carried + 1 new, tried cleared", len(merged.Outputs), tried)
	}
	carried, fresh := 0, 0
	for _, o := range merged.Outputs {
		if o.Carried {
			carried++
		} else {
			fresh++
		}
	}
	if carried != 3 || fresh != 1 {
		t.Fatalf("merged has %d carried and %d new, want 3 and 1", carried, fresh)
	}
}

// A carried drop that later fails the recheck leaves the set for good, and the gate decides on what survives.
func TestACarriedDropThatLaterFailsTheRecheckLeavesTheSet(t *testing.T) {
	r := newWarmRig(t, "w-acc-recheck", warmCfg, payingTail(), 6.0) // refused, so the 3 drops are carried
	r.toHeld()
	r.request(nil)
	if !r.held() {
		t.Fatal("fixture: nothing carried")
	}
	// A later turn now references output 1: the recheck must throw that carried drop out.
	warmMu.Lock()
	warmThreads[r.key()].heldTried = false // as if a new question had been merged in: the gate retries
	warmMu.Unlock()
	_, rep := r.requestReferencingOutputs(1)
	if rep.Events["sweep_apply_dropped_referenced_later"] != 1 {
		t.Fatalf("events = %v, want the carried drop referenced later to be thrown out", rep.Events)
	}
	ds := decisionsOf("apply")
	last := ds[len(ds)-1]
	// No new question was merged in between, so the survivors are still the answer's own verdicts (New), not
	// yet marked Carried: that mark is put on by the merge with the NEXT question's answer.
	if last.Proposed != 3 || last.Dropped["referenced_later"] != 1 || last.New != 2 || last.Carried != 0 {
		t.Fatalf("decision = %+v, want 3 proposed, 1 referenced_later dropped and 2 survivors", last)
	}
	// And it does not come back: the survivors that were carried again are only the 2 that passed.
	warmMu.Lock()
	h := warmThreads[r.key()].held
	warmMu.Unlock()
	if h == nil || len(h.Outputs) != 2 {
		t.Fatalf("carried set = %v, want only the 2 survivors", h)
	}
	for _, o := range h.Outputs {
		if o.ToolUseID == "toolu_wire_0" {
			t.Fatal("the dropped carried output is back in the set")
		}
	}
}

// ⭐ THE POINT OF ACCUMULATION, end to end: a refused answer's drops are too small to pay alone. The next
// question covers other outputs. Merged, they pay: the gate accepts ONCE on the merged set, one cut, and the
// set is cleared. Driven through the real warm path, with the gate's arithmetic (not a forced verdict).
func TestTwoRefusedAnswersThatCannotPayAloneAreAcceptedTogetherWithOneCut(t *testing.T) {
	const n = 8
	save := outputSavesN(n)
	per := save / n
	// Find a tail where 3 outputs fail, 4 fail and 8 pay (the gate: D x read x N against (whole - D) x (write - read)).
	found, N := 0, 2.5
	for tail := 500; tail < 600000; tail += 250 {
		whole := localTokens(warmConvN(n, 8, tail).Input)
		pays := func(k int) bool { D := k * per; return float64(D)*0.3*N > float64(whole-D)*3.45 }
		if !pays(3) && !pays(4) && pays(8) {
			found = tail
			break
		}
	}
	if found == 0 {
		t.Fatal("fixture: no tail makes 3 and 4 outputs fail but 8 pay")
	}
	a := &batchAsker{}
	r := newWarmRig(t, "w-acc2", warmCfg+"warm_min_mass_frac: 0.01\n", found, 1.0)
	r.outputs, r.askerAny = n, a
	// Question 1 (all 8 judged; we keep only 3 as drops by replacing the held answer's verdicts below).
	for i := 0; i < 3; i++ {
		r.request(nil)
	}
	r.forward()
	now := time.Now().UnixMilli()
	warmMu.Lock()
	judged, prints := warmThreads[r.key()].held.Judged, warmThreads[r.key()].held.Prints
	warmMu.Unlock()
	mk := func(ids ...int) heldAnswer {
		var outs []heldOutput
		for _, i := range ids {
			id := fmt.Sprintf("toolu_wire_%d", i)
			text := schema.MessageText(r.conv(r.step).Input[idxOfTool(r.conv(r.step), id)])
			outs = append(outs, heldOutput{ToolUseID: id, Hash: outputHash(r.conv(r.step).Input[idxOfTool(r.conv(r.step), id)]),
				Tokens: schema.TextTokens(text), Saves: per, Drop: true})
		}
		h := heldOf(now, judged, outs...)
		h.Prints = prints
		return h
	}
	// Replace question 1's answer with 3 drops, as if the judge proposed only those.
	warmMu.Lock()
	warmThreads[r.key()].held = nil
	warmMu.Unlock()
	warmHold(r.key(), mk(0, 1, 2))
	_, rep := r.request(nil)
	if rep.Events[eventAppliedWarm] != 0 || rep.Events["sweep_warm_carried_drops"] != 3 {
		t.Fatalf("3 drops: events = %v, want a refusal that carries 3", rep.Events)
	}
	// Question 2's answer: 5 other outputs. Merged: 8, which pays.
	warmHold(r.key(), mk(3, 4, 5, 6, 7))
	before := len(decisionsOf("apply"))
	req, rep := r.request(nil)
	ds := decisionsOf("apply")
	if len(ds)-before != 1 {
		t.Fatalf("%d decisions on the merged request, want exactly one", len(ds)-before)
	}
	last := ds[len(ds)-1]
	if !last.Accept || last.Carried != 3 || last.New != 5 {
		t.Fatalf("decision = %+v, want an accept over 3 carried + 5 new", last)
	}
	if rep.Events[eventAppliedWarm] != n {
		t.Fatalf("events = %v, want all %d dropped in ONE cut", rep.Events, n)
	}
	dropped := 0
	for _, i := range toolIndices(req) {
		if expand.HasPlaceholder(schema.MessageText(req.Input[i])) {
			dropped++
		}
	}
	if dropped != n {
		t.Fatalf("%d outputs dropped, want %d", dropped, n)
	}
	if r.held() {
		t.Fatal("the carried set was not cleared after the accept")
	}
	if c := SweepWarmCounters(); c.GateAccept != 1 || c.GateRefuse != 1 {
		t.Errorf("gate counters = %+v, want 1 refusal then 1 accept", c)
	}
}

func idxOfTool(req *bschemas.BifrostChatRequest, id string) int {
	i, _ := rawToolMessage(req.Input, id)
	return i
}

// A newer KEEP for a carried output overrides the carried drop, through the real merge in warmHold.
func TestANewerKeepOverridesACarriedDropInTheThreadsHeldState(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	now := time.Now().UnixMilli()
	warmHold("keepwins", heldOf(now-10, 40, dropOut("a", 10), dropOut("b", 20)))
	warmMu.Lock()
	warmThreads["keepwins"].heldTried = true
	warmMu.Unlock()
	warmHold("keepwins", heldOf(now, 50, keepOut("a"), dropOut("c", 30)))
	warmMu.Lock()
	got := *warmThreads["keepwins"].held
	warmMu.Unlock()
	var a *heldOutput
	for i := range got.Outputs {
		if got.Outputs[i].ToolUseID == "a" {
			a = &got.Outputs[i]
		}
	}
	if a == nil || a.Drop {
		t.Fatalf("output a = %+v, want the NEWER keep to have replaced the carried drop", a)
	}
	if n, _ := got.proposed(); n != 2 {
		t.Fatalf("%d drops in the merged set, want b (carried) and c (new)", n)
	}
}

// A refusal must not make a carried answer younger. heldAt is the age the in-memory source checks, so a
// reset on carry would let a thread keep a verdict alive for ever by being refused again and again.
func TestACarryOnRefusalDoesNotMakeTheAnswerYounger(t *testing.T) {
	r := newWarmRig(t, "w-age", warmCfg, payingTail(), 6.0) // billed 6x: the gate refuses and carries
	r.toHeld()
	old := time.Now().Add(-29 * time.Minute)
	warmMu.Lock()
	warmThreads[r.key()].heldAt = old
	warmMu.Unlock()
	_, rep := r.request(nil)
	if rep.Events["sweep_warm_carried_drops"] == 0 {
		t.Fatalf("fixture: the gate did not refuse and carry (events %v)", rep.Events)
	}
	warmMu.Lock()
	got := warmThreads[r.key()].heldAt
	warmMu.Unlock()
	if !got.Equal(old) {
		t.Fatalf("heldAt moved from %v to %v: a refusal made the carried answer younger", old, got)
	}
}

// A merge that evicts carried drops past the cap counts them, so a live run shows the cap biting.
func TestWarmHoldCountsTheCarriedDropsTheCapEvicts(t *testing.T) {
	resetWarmForTest()
	t.Cleanup(resetWarmForTest)
	now := time.Now().UnixMilli()
	var outs []heldOutput
	for i := 0; i < maxCarriedDrops+5; i++ {
		outs = append(outs, dropOut(fmt.Sprintf("o%04d", i), 10))
	}
	warmHold("cap", heldOf(now-5, 10, outs...))
	warmHold("cap", heldOf(now, 10, dropOut("new", 10)))
	if got := SweepWarmCounters().CarriedCapped; got != 6 {
		t.Fatalf("CarriedCapped = %d, want 6 (261 carried + 1 new - 256)", got)
	}
}
