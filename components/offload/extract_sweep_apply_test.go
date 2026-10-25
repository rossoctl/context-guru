package offload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// Tests for the COLD APPLY (#404 PR C): C1, C3, C4, H1 after an apply, and the quality signals.

// applyConv builds the conversation the held answer judges: n tool outputs, then `extra` more
// exchanges. Built twice from the same arguments it is byte-identical, which is what "the agent sent
// the same conversation again" means.
func applyConv(n, extra int) *bschemas.BifrostChatRequest {
	req := refConversation(n)
	req.Provider, req.Model = bschemas.Anthropic, "claude-sonnet-5"
	for i := 0; i < extra; i++ {
		req.Input = append(req.Input, assistantMsg(fmt.Sprintf("step %d done", i)), userMsg("ok"))
	}
	return req
}

// heldApply asks at the ping over applyConv(n, 0) and returns the store, the thread key and the
// component. Every output is voted drop.
func heldApply(t *testing.T, session string, n int) (*ExtractSweep, store.Store, string) {
	t.Helper()
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	e := shippedSweep(t, "")
	st := newMemStore()
	key, _ := registered(t, e, session, pingAsker(), st, applyConv(n, 0))
	dispatch, _, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered: %s", reason)
	}
	if res := dispatch(time.Second, SweepPingWait{}); !res.Held || res.Proposed != n {
		t.Fatalf("fixture: want a held answer proposing %d drops, got %+v", n, res)
	}
	return e, st, key
}

func coldCtx(session string, st store.Store) *components.Ctx {
	c := preExpiryCtx(session, nil, st)
	c.ColdCache, c.IdleMs = true, 10*60*1000
	return c
}

func warmCtx(session string, st store.Store) *components.Ctx {
	c := preExpiryCtx(session, nil, st)
	c.ColdCache, c.IdleMs = false, 20_000
	return c
}

func texts(req *bschemas.BifrostChatRequest) []string {
	out := make([]string, len(req.Input))
	for i := range req.Input {
		out[i] = schema.MessageText(req.Input[i])
	}
	return out
}

// ⭐ C1. The next cold request applies the valid drops. Each expand gives back the original, byte for
// byte.
func TestC1TheNextColdRequestAppliesTheValidDropsAndExpandGivesThemBack(t *testing.T) {
	e, st, key := heldApply(t, "c1", 3)
	original := texts(applyConv(3, 1))

	req := applyConv(3, 1)
	req.Input = append(req.Input, assistantMsg("raising handler_1_timeout_ms in src/api/users_1.py:42"))
	original = append(original, "")
	rep := &components.Report{Component: "extract_llm_sweep"}
	keys, err := e.Offload(req, rep, coldCtx("c1", st))
	if err != nil {
		t.Fatal(err)
	}
	// Output 1 was referenced after the question, so the recheck keeps it. Outputs 0 and 2 are dropped.
	if rep.Events[eventAppliedCold] != 2 || rep.Events["sweep_apply_dropped_referenced_later"] != 1 {
		t.Fatalf("events = %v, want 2 applied cold and 1 referenced_later", rep.Events)
	}
	if len(keys) != 2 {
		t.Fatalf("the Offload returned %d stash keys, want 2 (the pipeline reverts a drop with none)", len(keys))
	}
	dropped := 0
	for i := range req.Input {
		now := schema.MessageText(req.Input[i])
		if i >= 1 && i <= 3 {
			switch i {
			case 2: // toolu_wire_1: referenced, kept verbatim
				if now != original[i] {
					t.Errorf("the referenced output was changed")
				}
				continue
			}
			if !expand.HasPlaceholder(now) {
				t.Fatalf("output %d carries no marker after the cold apply: %.80q", i, now)
			}
			dropped++
			// THE REVERSIBILITY CLAIM: every marker resolves, and to the exact original bytes.
			id := firstMarkerID(t, now)
			got, ok := expand.Resolve(st, id)
			if !ok {
				t.Fatalf("marker %s does not resolve", id)
			}
			if got != original[i] {
				t.Fatalf("expand of output %d is not byte-identical to the original", i)
			}
			if schema.TextTokens(now) >= schema.TextTokens(original[i]) {
				t.Errorf("output %d did not shrink", i)
			}
		}
	}
	if dropped != 2 {
		t.Fatalf("dropped %d outputs, want 2", dropped)
	}
	if _, ok := getHeld(st, key); ok {
		t.Error("the held answer survived a cold apply: it would apply twice")
	}
	if got := SweepDropCounters().Overall; got.Applied != 2 || got.AppliedCold != 2 {
		t.Errorf("ledger = %+v, want 2 applied cold", got)
	}
}

func firstMarkerID(t *testing.T, text string) string {
	t.Helper()
	m := wireMarkerRe.FindStringSubmatch(text)
	if m == nil {
		// the plain (unescaped) spelling
		i := strings.Index(text, "<<cg:")
		if i < 0 {
			t.Fatalf("no marker in %.80q", text)
		}
		rest := text[i+5:]
		return rest[:strings.Index(rest, ">>")]
	}
	return m[1]
}

// After the cold apply, every later request replays the SAME bytes: the cache the cold request wrote
// is read byte-identically on the next warm request.
func TestAColdAppliedDropIsReplayedByteIdenticallyOnLaterRequests(t *testing.T) {
	e, st, _ := heldApply(t, "replay", 3)
	cold := applyConv(3, 1)
	if _, err := e.Offload(cold, &components.Report{Component: "extract_llm_sweep"}, coldCtx("replay", st)); err != nil {
		t.Fatal(err)
	}
	want := texts(cold)
	for n := 1; n <= 3; n++ {
		next := applyConv(3, n+1) // the same conversation, grown by the agent
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(next, rep, warmCtx("replay", st)); err != nil {
			t.Fatal(err)
		}
		got := texts(next)
		for i := 0; i < len(want); i++ {
			if got[i] != want[i] {
				t.Fatalf("warm request %d: message %d differs from the cold request's bytes", n, i)
			}
		}
		if rep.Replays != 3 {
			t.Errorf("warm request %d replayed %d drops, want 3", n, rep.Replays)
		}
	}
}

// ⭐ H1 STILL PASSES. A warm request after a cold apply is byte-identical to the one before it, except
// for the messages the agent added.
func TestH1AWarmRequestAfterAColdApplyIsIdenticalToTheOneBeforeItBesideTheNewMessages(t *testing.T) {
	e, st, _ := heldApply(t, "h1-after", 3)
	cold := applyConv(3, 1)
	if _, err := e.Offload(cold, &components.Report{Component: "extract_llm_sweep"}, coldCtx("h1-after", st)); err != nil {
		t.Fatal(err)
	}
	prev := cold
	for n := 2; n <= 5; n++ {
		next := applyConv(3, n)
		if _, err := e.Offload(next, &components.Report{Component: "extract_llm_sweep"}, warmCtx("h1-after", st)); err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(prev.Input)
		b, _ := json.Marshal(next.Input[:len(prev.Input)])
		if !bytes.Equal(a, b) {
			t.Fatalf("warm request %d: the first %d messages are not byte-identical to the previous request's", n, len(prev.Input))
		}
		prev = next
	}
}

// A warm request that is the first after the ping applies nothing: the answer stays held until a cold
// one. (The H1 test in extract_sweep_ping_test.go covers the same from the sweep-off side.)
func TestAWarmRequestAppliesNothingAndKeepsTheAnswerHeld(t *testing.T) {
	e, st, key := heldApply(t, "warm-held", 3)
	req := applyConv(3, 1)
	want := texts(req)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, warmCtx("warm-held", st)); err != nil {
		t.Fatal(err)
	}
	for i, w := range want {
		if schema.MessageText(req.Input[i]) != w {
			t.Fatalf("a warm request changed message %d", i)
		}
	}
	if _, ok := getHeld(st, key); !ok {
		t.Fatal("a warm request consumed the held answer")
	}
	if rep.Events[eventAppliedCold] != 0 {
		t.Errorf("a warm request applied: %v", rep.Events)
	}
}

// ⭐ C3. A changed-prefix request applies only the drops AFTER the change point. The drops before it
// stay held. They are counted apart.
func TestC3AChangedPrefixRequestAppliesOnlyTheDropsAfterTheChangePoint(t *testing.T) {
	e, st, key := heldApply(t, "c3", 4)
	// The agent EDITED the first assistant-visible turn: the user's first message differs, then the
	// four outputs follow unchanged. Message 0 changed, so everything is after the change point.
	// A change at message 3 (the second output's position) leaves outputs 0 and 1 before it.
	req := applyConv(4, 1)
	req.Input[3] = toolResultMsgWithID("toolu_wire_2", refOutput(2)+"an extra line the agent added\n")
	original := texts(req)
	// toolu_wire_2 changed content, so the recheck voids it (content_changed). Output 3 comes after the
	// change point and is applied. Outputs 0 and 1 are before it: held, not applied.
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, warmCtx("c3", st)); err != nil {
		t.Fatal(err)
	}
	got := texts(req)
	for i := 1; i <= 2; i++ {
		if got[i] != original[i] {
			t.Errorf("output %d is BEFORE the change point and must stay verbatim", i)
		}
	}
	if !expand.HasPlaceholder(got[4]) {
		t.Errorf("output 3 is after the change point and must be dropped: %.80q", got[4])
	}
	if rep.Events[eventAppliedChangedPrefix] != 1 || rep.Events[eventAppliedCold] != 0 {
		t.Errorf("events = %v, want exactly 1 applied_changed_prefix and no applied_cold", rep.Events)
	}
	q := SweepDropCounters().Overall
	if q.AppliedChangedPrefix != 1 || q.AppliedCold != 0 {
		t.Errorf("ledger = %+v, want the changed-prefix drop counted apart", q)
	}
	// The drops before the change point stay held for a later cold request.
	h, ok := getHeld(st, key)
	if !ok {
		t.Fatal("the drops before the change point were not kept held")
	}
	held := map[string]bool{}
	for _, o := range h.Outputs {
		held[o.ToolUseID] = true
	}
	if !held["toolu_wire_0"] || !held["toolu_wire_1"] || held["toolu_wire_3"] {
		t.Errorf("held = %v, want outputs 0 and 1 held and output 3 spent", held)
	}
	// And a later COLD request applies them.
	cold := applyConv(4, 1)
	cold.Input[3] = toolResultMsgWithID("toolu_wire_2", refOutput(2)+"an extra line the agent added\n")
	rep2 := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(cold, rep2, coldCtx("c3", st)); err != nil {
		t.Fatal(err)
	}
	if rep2.Events[eventAppliedCold] != 2 {
		t.Errorf("the later cold request applied %v, want the 2 held drops", rep2.Events)
	}
}

// A warm request whose conversation did not change before the answer was judged applies nothing, even
// if the agent only appended: that is H1, and it is what keeps the change-point rule from firing on
// ordinary growth.
func TestGrowthAloneIsNotAChangedPrefix(t *testing.T) {
	e, st, _ := heldApply(t, "growth", 3)
	req := applyConv(3, 4)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, warmCtx("growth", st)); err != nil {
		t.Fatal(err)
	}
	if rep.Events[eventAppliedChangedPrefix] != 0 || rep.Events[eventAppliedCold] != 0 {
		t.Fatalf("ordinary growth applied drops: %v", rep.Events)
	}
}

// ⭐ C4. Fail open. A Store error or a recheck panic forwards the request unchanged.
func TestC4AStoreErrorOrARecheckPanicForwardsTheRequestUnchanged(t *testing.T) {
	t.Run("a store that cannot hold the originals", func(t *testing.T) {
		e, st, _ := heldApply(t, "c4-store", 3)
		spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
		// Move the held answer into the spy store, which refuses every payload.
		h, _ := getHeld(st, thread.Key("c4-store", ""))
		putHeld(spy, h)
		req := applyConv(3, 1)
		want := texts(req)
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(req, rep, coldCtx("c4-store", spy)); err != nil {
			t.Fatalf("Offload must fail open: %v", err)
		}
		for i, w := range want {
			if schema.MessageText(req.Input[i]) != w {
				t.Fatalf("message %d was rewritten although its original could not be stored", i)
			}
		}
		if rep.Events[eventApplyDeclined] != 3 {
			t.Errorf("events = %v, want 3 declined", rep.Events)
		}
		if got := spy.decisionWrites(); len(got) != 0 {
			t.Errorf("decisions were recorded for drops that did not happen: %v", got)
		}
		for _, k := range spy.puts {
			if strings.HasPrefix(k, store.SweepDropPrefix) || strings.HasPrefix(k, store.SweepExpandPrefix) {
				t.Errorf("a drop record %q was written for a drop that did not happen", k)
			}
		}
		if q := SweepDropCounters().Overall; q.Applied != 0 {
			t.Errorf("ledger counts %d applied for drops that did not happen", q.Applied)
		}
	})
	t.Run("a panic in the apply restores every message it touched", func(t *testing.T) {
		e, st, _ := heldApply(t, "c4-panic", 3)
		// A store whose Put panics once the first drop is in, so the panic comes mid-apply.
		ps := &putPanicStore{Store: st}
		req := applyConv(3, 1)
		want := texts(req)
		ps.after = 3 // let a few Puts through (the stash and owner records of the first drop)
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(req, rep, coldCtx("c4-panic", ps)); err != nil {
			t.Fatalf("Offload must fail open: %v", err)
		}
		for i, w := range want {
			if schema.MessageText(req.Input[i]) != w {
				t.Fatalf("message %d was left rewritten after a panic in the apply", i)
			}
		}
		if rep.Events[eventApplyPanic] != 1 {
			t.Errorf("events = %v, want the panic counted", rep.Events)
		}
		if q := SweepDropCounters().Overall; q.Applied != 0 {
			t.Errorf("ledger counts %d applied after a panic", q.Applied)
		}
		if _, ok := getDrop(ps.Store, thread.Key("c4-panic", ""), contentKey(want[1])); ok {
			t.Error("a drop record was written for a request that was restored")
		}
	})
	t.Run("a panic in the recheck", func(t *testing.T) {
		e, st, _ := heldApply(t, "c4-recheck", 3)
		// The recheck's expanded-output check reads the Store (isKeptVerbatim -> Get). Arm a Store that
		// panics on that read, but only once the held answer has been read, so the panic is INSIDE the
		// recheck and not in the lookups before it.
		ps := &getPanicStore{Store: st, key: "cg:keep:"}
		req := applyConv(3, 1)
		want := texts(req)
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(req, rep, coldCtx("c4-recheck", ps)); err != nil {
			t.Fatalf("Offload must fail open: %v", err)
		}
		if ps.hit == 0 {
			t.Fatal("the recheck never reached the poisoned read, so the panic path was not exercised")
		}
		for i, w := range want {
			if schema.MessageText(req.Input[i]) != w {
				t.Fatalf("message %d changed after a panic in the recheck", i)
			}
		}
		if rep.Events[eventApplyPanic] != 1 {
			t.Errorf("events = %v, want the panic counted", rep.Events)
		}
	})
}

// getPanicStore panics on a Get of any key with the given prefix.
type getPanicStore struct {
	store.Store
	key string
	hit int
}

func (s *getPanicStore) Get(k string) ([]byte, bool) {
	if strings.HasPrefix(k, s.key) {
		s.hit++
		panic("boom in Store.Get " + k)
	}
	return s.Store.Get(k)
}

// putPanicStore panics on a Put after `after` earlier Puts.
type putPanicStore struct {
	store.Store
	after, n int
}

func (s *putPanicStore) Put(k string, p []byte) {
	s.n++
	if s.n > s.after {
		panic("boom in Store.Put")
	}
	s.Store.Put(k, p)
}

func (s *putPanicStore) PutStash(k string, p []byte) bool {
	s.n++
	if s.n > s.after {
		panic("boom in Store.PutStash")
	}
	if st, ok := s.Store.(store.Stasher); ok {
		return st.PutStash(k, p)
	}
	s.Store.Put(k, p)
	return true
}

// Shadow mode applies NOTHING, even on a cold request, and writes no drop record.
func TestShadowModeStillAppliesNothingOnACOLDRequest(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	e := shippedSweep(t, "shadow: true\n")
	st := newMemStore()
	key, _ := registered(t, e, "shadow-cold", pingAsker(), st, applyConv(3, 0))
	dispatch, _, _, _ := SweepPingSubstitute(key)
	dispatch(time.Second, SweepPingWait{})
	req := applyConv(3, 1)
	want := texts(req)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, coldCtx("shadow-cold", st)); err != nil {
		t.Fatal(err)
	}
	for i, w := range want {
		if schema.MessageText(req.Input[i]) != w {
			t.Fatalf("shadow mode changed message %d", i)
		}
	}
	if rep.Events["sweep_shadow_valid"] != 3 {
		t.Errorf("shadow did not report the recheck: %v", rep.Events)
	}
	if q := SweepDropCounters().Overall; q.Applied != 0 {
		t.Errorf("shadow mode applied %d drops", q.Applied)
	}
}

// ---------------------------------------------------------------------------------------------------
// The quality signals (#404 point 7).

// An expand of a sweep marker is charged to the sweep, once per drop; an expand of anything else is not.
func TestAnExpandOfASweepMarkerIsChargedToTheSweep(t *testing.T) {
	e, st, _ := heldApply(t, "q-expand", 3)
	req := applyConv(3, 1)
	if _, err := e.Offload(req, &components.Report{Component: "extract_llm_sweep"}, coldCtx("q-expand", st)); err != nil {
		t.Fatal(err)
	}
	id := firstMarkerID(t, schema.MessageText(req.Input[1]))
	// A marker some OTHER component made is not the sweep's.
	if note := NoteSweepExpand(st, "deadbeefdeadbeef", 10, ""); note.Sweep {
		t.Error("an expand of a foreign marker was charged to the sweep")
	}
	n1 := NoteSweepExpand(st, id, 1200, "")
	if !n1.Sweep || !n1.First {
		t.Fatalf("first expand of a sweep marker: %+v, want Sweep and First", n1)
	}
	n2 := NoteSweepExpand(st, id, 1200, "")
	if !n2.Sweep || n2.First {
		t.Fatalf("second expand of the same drop: %+v, want Sweep and not First", n2)
	}
	q := SweepDropCounters().Overall
	if q.Expanded != 1 || q.ExpandEvents != 2 || q.ExpandTokens != 2400 {
		t.Errorf("ledger = %+v, want 1 expanded drop over 2 expand events and 2400 tokens", q)
	}
	if q.FalseDrops() != 1 {
		t.Errorf("false drops = %d, want 1", q.FalseDrops())
	}
	if r := q.FalseDropRate(); r < 0.33 || r > 0.34 {
		t.Errorf("false-drop rate = %v, want 1 of 3", r)
	}
	// The record says so, so a replay leaves it verbatim (kept-verbatim is the host's mark; here the
	// record carries the fact).
	rec, _ := getDrop(st, thread.Key("q-expand", ""), contentKey(refOutput(0)))
	if !rec.Expanded {
		t.Error("the drop record does not say it was expanded")
	}
}

// A later turn that uses something a dropped output introduced, without expanding it, counts. A turn
// that does not, does not. A reference made BEFORE the drop is not counted (the model saw that).
func TestALaterReferenceToADroppedOutputCounts(t *testing.T) {
	e, st, _ := heldApply(t, "q-ref", 3)
	cold := applyConv(3, 1)
	if _, err := e.Offload(cold, &components.Report{Component: "extract_llm_sweep"}, coldCtx("q-ref", st)); err != nil {
		t.Fatal(err)
	}
	if q := SweepDropCounters().Overall; q.Referenced != 0 {
		t.Fatalf("a drop was counted referenced with no later turn: %+v", q)
	}
	// A later turn that uses nothing from the dropped outputs.
	quiet := applyConv(3, 3)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(quiet, rep, warmCtx("q-ref", st)); err != nil {
		t.Fatal(err)
	}
	if q := SweepDropCounters().Overall; q.Referenced != 0 {
		t.Fatalf("an unrelated turn was counted as a reference: %+v", q)
	}
	// A later turn that uses an identifier output 2 introduced.
	talk := applyConv(3, 3)
	talk.Input = append(talk.Input, assistantMsg("the limit lives in handler_2_timeout_ms at src/api/users_2.py:42"))
	rep2 := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(talk, rep2, warmCtx("q-ref", st)); err != nil {
		t.Fatal(err)
	}
	q := SweepDropCounters().Overall
	if q.Referenced != 1 || rep2.Events[eventFalseRef] != 1 {
		t.Fatalf("ledger = %+v events = %v, want 1 referenced drop", q, rep2.Events)
	}
	// Counted once: the same reference on the next request is not a second false drop.
	if _, err := e.Offload(talk, &components.Report{Component: "extract_llm_sweep"}, warmCtx("q-ref", st)); err != nil {
		t.Fatal(err)
	}
	if q := SweepDropCounters().Overall; q.Referenced != 1 {
		t.Errorf("the same reference was counted again: %+v", q)
	}
	// And a drop that is BOTH referenced and expanded is one false drop, not two.
	id := firstMarkerID(t, schema.MessageText(cold.Input[3]))
	NoteSweepExpand(st, id, 1200, "")
	if q := SweepDropCounters().Overall; q.FalseDrops() != 1 {
		t.Errorf("a drop that is referenced AND expanded counted as %d false drops, want 1", q.FalseDrops())
	}
}

// The quality records hold METADATA ONLY: ids, hashes, counts. Not a byte of the output.
func TestTheDropRecordHoldsNoCustomerContent(t *testing.T) {
	e, st, _ := heldApply(t, "q-meta", 2)
	req := applyConv(2, 1)
	if _, err := e.Offload(req, &components.Report{Component: "extract_llm_sweep"}, coldCtx("q-meta", st)); err != nil {
		t.Fatal(err)
	}
	tkey := thread.Key("q-meta", "")
	secret := []string{"handler_0_timeout_ms", "src/api/users_0.py", "timeout_ms = 30"}
	for _, ck := range getDropIndex(st, tkey).Cks {
		b, ok := st.Get(dropKey(tkey, ck))
		if !ok {
			t.Fatalf("no record for %s", ck)
		}
		for _, s := range secret {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("the drop record holds content %q: %s", s, b)
			}
		}
	}
	for _, th := range SweepDropCounters().Threads {
		if strings.Contains(th.Label, "q-meta") {
			t.Errorf("a thread label carries the session id: %q", th.Label)
		}
	}
}

// ITEM F (#508 review). Only extract_llm writes cg:res: now. The sweep reads its OWN drop record, so:
//
//	(a) an output the sweep dropped is not offered to the next question (its record says so), and
//	(b) a cg:res: record some OTHER component wrote for the same bytes is NOT the sweep's drop. It is
//	    neither replayed by the sweep nor hidden from its question.
func TestAnAlreadyDroppedOutputIsNotOfferedAgain(t *testing.T) {
	e := shippedSweep(t, "")
	st := newMemStore()
	req := deepCandidates(3)
	key, _ := registered(t, e, "f-dropped", pingAsker(), st, req)
	// The sweep dropped output 1 on an earlier request: its record exists, though the snapshot still
	// holds the full text (the drop was made after the snapshot).
	ck := contentKey(schema.MessageText(req.Input[2]))
	putDrop(st, sweepDropRec{Thread: key, ToolUseID: "toolu_wire_1", CK: ck, Marker: "m1"})
	dispatch, info, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered: %s", reason)
	}
	if info.Offered != 2 {
		t.Errorf("offered %d candidates, want 2: the dropped output must not be asked about again", info.Offered)
	}
	dispatch(time.Second, SweepPingWait{})
}

func TestAnExtractLLMRecordIsNotTheSweepsDrop(t *testing.T) {
	e := shippedSweep(t, "")
	st := newMemStore()
	req := deepCandidates(3)
	key, _ := registered(t, e, "f-foreign", pingAsker(), st, req)
	// extract_llm recorded a decision for the same bytes in cg:res:.
	c := preExpiryCtx("f-foreign", nil, st)
	putResult(c, contentKey(schema.MessageText(req.Input[2])), "a short projection", "")
	_, info, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered: %s", reason)
	}
	if info.Offered != 3 {
		t.Errorf("offered %d, want 3: another component's cg:res: record hid a candidate from the sweep", info.Offered)
	}
	// And a request does not replay it as a sweep drop.
	next := deepCandidates(3)
	want := texts(next)
	if _, err := e.Offload(next, &components.Report{Component: "extract_llm_sweep"}, warmCtx("f-foreign", st)); err != nil {
		t.Fatal(err)
	}
	for i, w := range want {
		if schema.MessageText(next.Input[i]) != w {
			t.Fatalf("the sweep replayed another component's cg:res: record onto message %d", i)
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// putHeld (#508 review, required before any code reads a held answer to change a request).

// slowStore sleeps after every Get and every Put, so a read-check-write that is not serialised is
// interleaved with another every time and not by luck.
type slowStore struct{ store.Store }

func (s *slowStore) Get(k string) ([]byte, bool) {
	b, ok := s.Store.Get(k)
	time.Sleep(4 * time.Millisecond)
	return b, ok
}

func (s *slowStore) Put(k string, p []byte) {
	time.Sleep(4 * time.Millisecond)
	s.Store.Put(k, p)
}

func heldAt(key string, at int64, n int) heldAnswer {
	h := heldAnswer{Thread: key, AskedAtMs: at, Judged: 4}
	for i := 0; i < n; i++ {
		h.Outputs = append(h.Outputs, heldOutput{ToolUseID: fmt.Sprintf("t%d", i), Hash: "h", Drop: true, Saves: 10})
	}
	return h
}

// A claim that read the OLD answer must not overwrite a NEW answer a concurrent ask just wrote.
func TestAClaimNeverOverwritesANewerAnswer(t *testing.T) {
	for _, claim := range []string{"report", "spend"} {
		t.Run(claim, func(t *testing.T) {
			st := &slowStore{Store: newMemStore()}
			putHeld(st, heldAt("th", 100, 2))
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				if claim == "report" {
					claimHeldReport(st, "th")
				} else {
					claimHeldSpend(st, "th")
				}
			}()
			go func() {
				defer wg.Done()
				time.Sleep(2 * time.Millisecond) // lands while the claim is between its read and its write
				putHeld(st, heldAt("th", 200, 3))
			}()
			wg.Wait()
			h, ok := getHeld(st.Store, "th")
			if !ok || h.AskedAtMs != 200 || len(h.Outputs) != 3 {
				t.Fatalf("the newer answer was lost: ok=%v %+v", ok, h)
			}
		})
	}
}

// Two asks of one thread landing out of order: the older never replaces the newer.
func TestAnOlderAnswerNeverReplacesANewerOne(t *testing.T) {
	st := newMemStore()
	putHeld(st, heldAt("th", 200, 3))
	putHeld(st, heldAt("th", 100, 1))
	h, ok := getHeld(st, "th")
	if !ok || h.AskedAtMs != 200 || len(h.Outputs) != 3 {
		t.Fatalf("an older answer replaced a newer one: ok=%v %+v", ok, h)
	}
	// Equal or newer is written, including the tombstone a claim writes (same AskedAtMs).
	if _, won := claimHeldSpend(st, "th"); !won {
		t.Fatal("the claim could not spend the answer")
	}
	if _, ok := getHeld(st, "th"); ok {
		t.Fatal("the spent tombstone was refused as stale")
	}
	putHeld(st, heldAt("th", 300, 2))
	if h, ok := getHeld(st, "th"); !ok || h.AskedAtMs != 300 {
		t.Fatalf("a newer ask after a spend was refused: %+v", h)
	}
}

// ---------------------------------------------------------------------------------------------------
// The summary and the sweep on one cold request (#404 point 10 follow-up).

// A cold request on which cache_aware_summarizer has replaced a span with its summary (it runs FIRST in
// xhigh, so the sweep sees its output) and the sweep applies. Rule: the sweep never touches what the
// summary replaced. It finds a held output by tool id in the request it is given, so an output inside the
// summarized span is simply absent and is skipped; the rest are dropped. Nothing is double-counted, no
// record is written for the skipped output, and every marker that does go out expands.
func TestTheSummaryAndTheSweepOnOneColdRequest(t *testing.T) {
	e, st, key := heldApply(t, "both", 4)
	agent := applyConv(4, 2)
	// The pipeline's copy: the summarizer replaced messages 1-2 (the first output and its neighbour)
	// with ONE summary message, so the count changed and every index moved.
	piped := &bschemas.BifrostChatRequest{Input: append([]bschemas.ChatMessage{agent.Input[0],
		userMsg("[summary of the first two exchanges]")}, agent.Input[3:]...)}
	if len(piped.Input) == len(agent.Input) {
		t.Fatal("fixture: the count did not change")
	}
	orig := texts(piped)
	c := coldCtx("both", st)
	c.RawInput = agent.Input
	rep := &components.Report{Component: "extract_llm_sweep"}
	keys, err := e.Offload(piped, rep, c)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Events[eventAppliedCold] != 2 || len(keys) != 2 {
		t.Fatalf("events = %v keys = %d, want the 2 outputs outside the summarized span dropped", rep.Events, len(keys))
	}
	if schema.MessageText(piped.Input[1]) != orig[1] {
		t.Error("the sweep changed the summary message")
	}
	for i := 2; i < len(piped.Input); i++ {
		now := schema.MessageText(piped.Input[i])
		if piped.Input[i].Role != bschemas.ChatMessageRoleTool || now == orig[i] {
			continue
		}
		id := firstMarkerID(t, now)
		if got, ok := expand.Resolve(st, id); !ok || got != orig[i] {
			t.Errorf("message %d: the marker does not expand to the original", i)
		}
	}
	for _, i := range []int{1, 2} { // the two outputs the summary replaced
		if _, ok := getDrop(st, key, contentKey(schema.MessageText(agent.Input[i]))); ok {
			t.Errorf("a drop record was written for output %d, which the summary had replaced", i)
		}
	}
	if q := SweepDropCounters().Overall; q.Applied != 2 {
		t.Errorf("ledger counts %d applied, want 2 (the summarized output is not double-counted)", q.Applied)
	}
	if _, ok := getHeld(st, key); ok {
		t.Error("the answer is still held after the cold apply")
	}
}

// A panic in the recheck on a cold apply forwards the request unchanged, is counted, and hands the answer
// back ONCE so the drops are not lost silently; a second panic leaves it spent.
func TestAColdApplyPanicIsCountedAndRetriedOnce(t *testing.T) {
	e, st, key := heldApply(t, "retry", 3)
	before := SweepApplyPanics()
	ps := &getPanicStore{Store: st, key: "cg:keep:"}
	for attempt := 1; attempt <= 3; attempt++ {
		req := applyConv(3, 1)
		want := texts(req)
		rep := &components.Report{Component: "extract_llm_sweep"}
		if _, err := e.Offload(req, rep, coldCtx("retry", ps)); err != nil {
			t.Fatal(err)
		}
		for i, w := range want {
			if schema.MessageText(req.Input[i]) != w {
				t.Fatalf("attempt %d: message %d changed after a panic", attempt, i)
			}
		}
		_, held := getHeld(st, key)
		switch attempt {
		case 1:
			if rep.Events[eventApplyPanic] != 1 || !held {
				t.Fatalf("attempt 1: panic events %v, held=%v; want counted and handed back", rep.Events, held)
			}
		case 2:
			if rep.Events[eventApplyPanic] != 1 || held {
				t.Fatalf("attempt 2: panic events %v, held=%v; want counted and now left spent", rep.Events, held)
			}
		case 3:
			if rep.Events[eventApplyPanic] != 0 {
				t.Fatalf("attempt 3 ran the apply on a spent answer: %v", rep.Events)
			}
		}
	}
	if got := SweepApplyPanics() - before; got != 2 {
		t.Errorf("process panic counter rose by %d, want 2", got)
	}
}

// The ask-level slot refusal is sweep_concurrency_full, not a failed ask.
func TestAFullAskSlotCountsAsConcurrencyFull(t *testing.T) {
	e := shippedSweep(t, "")
	key, _ := registered(t, e, "slot-full", pingAsker(), newMemStore(), deepCandidates(2))
	dispatch, _, _, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatal("no question offered")
	}
	for i := 0; i < cap(sweepSlots); i++ {
		sweepSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(sweepSlots); i++ {
			<-sweepSlots
		}
	}()
	before := SweepPingCounters().ConcurrencyFull
	res := dispatch(time.Second, SweepPingWait{})
	if res.Reason != SweepReasonConcurrencyFull || res.Substituted {
		t.Fatalf("result = %+v, want a concurrency_full refusal", res)
	}
	if SweepPingCounters().ConcurrencyFull-before != 1 {
		t.Error("the refusal was not counted as sweep_concurrency_full")
	}
}

// ---------------------------------------------------------------------------------------------------
// Round 1 review fixes (#695): false-drop accuracy, the commit window, the change point, shadow replay.

// R1-2. Two threads drop the SAME bytes, so they share one marker id. An expand in thread B credits
// thread B and only B; an expand with no thread known credits every owner.
func TestAnExpandOfASharedMarkerCreditsOnlyTheThreadThatExpanded(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	st := newMemStore()
	content := refOutput(7)
	marker := hashKey(content)
	SeedSweepDropForTest(st, "thread-A", content, marker)
	SeedSweepDropForTest(st, "thread-B", content, marker) // B writes second: must not displace A
	note := NoteSweepExpand(st, marker, 500, "thread-B")
	if !note.Sweep || !note.First {
		t.Fatalf("note = %+v, want a first sweep expand", note)
	}
	ck := contentKey(content)
	if a, _ := getDrop(st, "thread-A", ck); a.Expanded {
		t.Error("thread A was credited for an expand that thread B made")
	}
	if b, _ := getDrop(st, "thread-B", ck); !b.Expanded {
		t.Error("thread B was not credited for its own expand")
	}
	var a, b SweepQuality
	for _, th := range SweepDropCounters().Threads {
		switch th.Label {
		case threadLabel("thread-A"):
			a = th.SweepQuality
		case threadLabel("thread-B"):
			b = th.SweepQuality
		}
	}
	if a.Expanded != 0 || b.Expanded != 1 || b.ExpandEvents != 1 {
		t.Errorf("ledger A=%+v B=%+v, want only B credited", a, b)
	}
	// A thread that does not carry the marker is credited with nothing.
	if n := NoteSweepExpand(st, marker, 500, "thread-C"); n.Sweep {
		t.Error("a thread that does not carry the marker was credited")
	}
	// Unknown thread (the request-path repair): every owner carries it, so every owner is credited.
	NoteSweepExpand(st, marker, 500, "")
	if a, _ := getDrop(st, "thread-A", ck); !a.Expanded {
		t.Error("an expand with no thread known did not credit owner A")
	}
}

// R1-3. scanReferences and NoteSweepExpand both read-modify-write one drop record. Slowed, an unlocked
// pair loses a flag every time.
func TestAReferenceAndAnExpandOnOneDropBothLand(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	e := shippedSweep(t, "")
	base := newMemStore()
	st := &slowStore{Store: base}
	tkey := thread.Key("race", "")
	content := refOutput(3)
	marker := hashKey(content)
	ck := contentKey(content)
	putDrop(st, sweepDropRec{Thread: tkey, ToolUseID: "t", CK: ck, Marker: marker, Seen: 0,
		Idents: []string{identHash("handler_3_timeout_ms")}})
	addToDropIndex(st, tkey, []string{ck})
	dropMu.Lock()
	addMarkerOwner(st, marker, tkey, ck)
	dropMu.Unlock()
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("go"),
		assistantMsg("the limit lives in handler_3_timeout_ms")}}
	c := warmCtx("race", st)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); e.scanReferences(req, &components.Report{}, c, tkey) }()
	go func() { defer wg.Done(); time.Sleep(3 * time.Millisecond); NoteSweepExpand(st, marker, 100, tkey) }()
	wg.Wait()
	rec, ok := getDrop(base, tkey, ck)
	if !ok || !rec.Referenced || !rec.Expanded {
		t.Fatalf("a flag was lost: %+v", rec)
	}
}

// commitPanicStore panics on the Nth drop-record Put after arming, counted from the first drop-record write.
type commitPanicStore struct {
	store.Store
	armed  bool
	writes int
	at     int
}

func (s *commitPanicStore) Put(k string, p []byte) {
	if s.armed && strings.HasPrefix(k, store.SweepDropPrefix) {
		s.writes++
		if s.writes == s.at {
			panic("boom inside the commit loop")
		}
	}
	s.Store.Put(k, p)
}

// R1-1 / R2-1. A panic INSIDE the commit loop, after the first record is written. The request is
// restored, so every record this attempt wrote is tombstoned: the next request does not replay a form
// this one never sent, and nothing is counted for drops the request does not carry.
func TestAPanicInsideTheCommitLoopLeavesNoRecordAndNoCount(t *testing.T) {
	e, base, key := heldApply(t, "commitwin", 3)
	ps := &commitPanicStore{Store: base, armed: true, at: 2} // first record lands, the second panics
	req := applyConv(3, 1)
	want := texts(req)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, coldCtx("commitwin", ps)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	if ps.writes < 2 {
		t.Fatalf("the panic never fired inside the commit loop (%d drop writes)", ps.writes)
	}
	for i, w := range want {
		if schema.MessageText(req.Input[i]) != w {
			t.Fatalf("message %d changed after a panic in the commit", i)
		}
	}
	for i := 1; i <= 3; i++ {
		ck := contentKey(want[i])
		if _, ok := getDrop(base, key, ck); ok {
			t.Errorf("output %d has a live drop record for a request that was restored", i)
		}
		if ref := getMarkerRef(base, hashKey(want[i])); len(ref.Owners) != 0 {
			t.Errorf("output %d still has marker owners %+v", i, ref.Owners)
		}
	}
	if q := SweepDropCounters().Overall; q.Applied != 0 {
		t.Errorf("ledger counts %d applied for drops the request does not carry", q.Applied)
	}
	if rep.Events[eventAppliedCold] != 0 {
		t.Errorf("sweep_applied_cold = %d for a restored request", rep.Events[eventAppliedCold])
	}
	// And the next request, with a healthy store, replays nothing from the failed attempt.
	next := applyConv(3, 2)
	wantNext := texts(next)
	rep2 := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(next, rep2, warmCtx("commitwin", base)); err != nil {
		t.Fatal(err)
	}
	for i, w := range wantNext {
		if schema.MessageText(next.Input[i]) != w {
			t.Fatalf("the next request replayed message %d from a failed attempt", i)
		}
	}
}

// R1-4. The change point is computed from the answer the claim returns. A newer answer lands between the
// read and the claim; the split must use IT. Driven through a store that swaps the answer on the first
// Get after the read.
type swapAnswerStore struct {
	store.Store
	key   string
	newer []byte
	reads int
}

// Get swaps the stored answer for the newer one right AFTER applyHeld's first read (the one its change
// point is computed from) and before the claim's read.
func (s *swapAnswerStore) Get(k string) ([]byte, bool) {
	b, ok := s.Store.Get(k)
	if k == s.key && ok {
		s.reads++
		if s.reads == 1 {
			s.Store.Put(k, s.newer)
		}
	}
	return b, ok
}

func TestTheChangePointComesFromTheClaimedAnswer(t *testing.T) {
	e, base, key := heldApply(t, "stale-cp", 4)
	old, _ := getHeld(base, key)
	// The NEWER answer judged a conversation whose first three messages are unchanged and which is
	// longer; it holds the same four outputs. Build it by re-asking over a grown conversation.
	newer := old
	newer.AskedAtMs = old.AskedAtMs + 1000
	newer.Prints = append([]uint32(nil), old.Prints...)
	// A request whose message 3 differs from BOTH answers' prints at index 3 is a changed prefix with
	// the change point at 3 for the OLD answer. Give the newer answer prints that already match the
	// request (it judged the edited conversation), so its change point is -1.
	req := applyConv(4, 1)
	req.Input[3] = toolResultMsgWithID("toolu_wire_2", refOutput(2)+"an edit\n")
	newer.Prints = msgPrints(req.Input[:len(old.Prints)])
	nb, _ := json.Marshal(newer)
	ss := &swapAnswerStore{Store: base, key: heldKey(key), newer: nb}
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, warmCtx("stale-cp", ss)); err != nil {
		t.Fatal(err)
	}
	// With the OLD change point (3) the sweep would have applied output 3 (index 4). With the claimed
	// answer there is no change point: a warm request, nothing to apply, the answer given back unspent.
	if rep.Events[eventAppliedChangedPrefix] != 0 || rep.Events[eventAppliedCold] != 0 {
		t.Fatalf("applied on the strength of a stale change point: %v", rep.Events)
	}
	if h, ok := getHeld(base, key); !ok || h.AskedAtMs != newer.AskedAtMs {
		t.Errorf("the newer answer was not left held: ok=%v %+v", ok, h)
	}
}

// Shadow mode applies nothing NEW but keeps replaying drops that already exist, or switching it on
// would flip them back to full inside the cached prefix.
func TestShadowModeKeepsReplayingExistingDrops(t *testing.T) {
	e, st, _ := heldApply(t, "shadow-replay", 3)
	cold := applyConv(3, 1)
	if _, err := e.Offload(cold, &components.Report{Component: "extract_llm_sweep"}, coldCtx("shadow-replay", st)); err != nil {
		t.Fatal(err)
	}
	want := texts(cold)
	sh := shippedSweep(t, "shadow: true\n")
	next := applyConv(3, 2)
	rep := &components.Report{Component: "extract_llm_sweep"}
	keys, err := sh.Offload(next, rep, warmCtx("shadow-replay", st))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(want); i++ {
		if schema.MessageText(next.Input[i]) != want[i] {
			t.Fatalf("shadow mode stopped replaying message %d", i)
		}
	}
	if rep.Replays != 3 || len(keys) != 3 {
		t.Errorf("replays %d keys %d, want 3 and 3 (the pipeline reverts a splice with no keys)", rep.Replays, len(keys))
	}
}

// A panic AFTER the records are written (at the held-answer trim of a changed-prefix apply) still
// restores the request, tombstones the records, and counts nothing: the ledger is bumped last.
type trimPanicStore struct {
	store.Store
	armed bool
}

func (s *trimPanicStore) Put(k string, p []byte) {
	if s.armed && strings.HasPrefix(k, store.SweepHeldPrefix) {
		var h heldAnswer
		if json.Unmarshal(p, &h) == nil && !h.Spent && len(h.Outputs) > 0 && h.Judged < 4 {
			panic("boom at the held trim")
		}
	}
	s.Store.Put(k, p)
}

func TestAPanicAfterTheCommitStillCountsNothing(t *testing.T) {
	e, base, key := heldApply(t, "trimwin", 4)
	savedBefore := extractGrossSaved("extract_llm_sweep")
	ps := &trimPanicStore{Store: base, armed: true}
	req := applyConv(4, 1)
	req.Input[3] = toolResultMsgWithID("toolu_wire_2", refOutput(2)+"an edit\n") // change point 3: 0,1 stay held
	want := texts(req)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, warmCtx("trimwin", ps)); err != nil {
		t.Fatal(err)
	}
	for i, w := range want {
		if schema.MessageText(req.Input[i]) != w {
			t.Fatalf("message %d changed after a panic at the held trim", i)
		}
	}
	if rep.Events[eventApplyPanic] != 1 {
		t.Fatalf("the trim panic never fired: %v", rep.Events)
	}
	if q := SweepDropCounters().Overall; q.Applied != 0 || q.AppliedChangedPrefix != 0 {
		t.Errorf("ledger counts %+v for a restored request", q)
	}
	if rep.Events[eventAppliedChangedPrefix] != 0 {
		t.Errorf("event counted for a restored request: %v", rep.Events)
	}
	if _, ok := getDrop(base, key, contentKey(want[4])); ok {
		t.Error("a live drop record survived the restore")
	}
	// The held answer is given back WHOLE (all four outputs, untrimmed), so the drops after the change
	// point are not lost: a trimmed give-back would keep only the two before it.
	h, ok := getHeld(base, key)
	if !ok || len(h.Outputs) != 4 || h.Judged != 5 || h.Attempts != 1 {
		t.Errorf("given-back answer = ok=%v outputs=%d judged=%d attempts=%d, want the whole answer (4, 5, 1)",
			ok, len(h.Outputs), h.Judged, h.Attempts)
	}
	if got := extractGrossSaved("extract_llm_sweep") - savedBefore; got != 0 {
		t.Errorf("savings metrics were booked (%d tokens) for a restored request", got)
	}
}

// ROUND 2 BLOCKER. A panic inside the scan's locked window must not leave dropMu held. The scan's
// function-level recover swallows the panic and the request completes; a plain Unlock after the panic
// point would be skipped, and every later scan, expand note and commit would block forever. Bounded by a
// timeout so a regression fails the test and does not hang the suite.
type scanPanicStore struct {
	store.Store
	armed bool
	puts  int
}

func (s *scanPanicStore) Put(k string, p []byte) {
	if s.armed && strings.HasPrefix(k, store.SweepDropPrefix) {
		s.puts++
		panic("boom inside the scan's locked window")
	}
	s.Store.Put(k, p)
}

func TestAPanicInTheScansLockedWindowDoesNotWedgeDropMu(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	e := shippedSweep(t, "")
	base := newMemStore()
	tkey := thread.Key("wedge", "")
	content := refOutput(5)
	marker := hashKey(content)
	ck := contentKey(content)
	putDrop(base, sweepDropRec{Thread: tkey, ToolUseID: "t", CK: ck, Marker: marker,
		Idents: []string{identHash("handler_5_timeout_ms")}})
	addToDropIndex(base, tkey, []string{ck})
	func() {
		dropMu.Lock()
		defer dropMu.Unlock()
		addMarkerOwner(base, marker, tkey, ck)
	}()
	ps := &scanPanicStore{Store: base, armed: true}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("go"),
		assistantMsg("see handler_5_timeout_ms")}}
	e.scanReferences(req, &components.Report{}, warmCtx("wedge", ps), tkey) // swallows the panic
	if ps.puts == 0 {
		t.Fatal("the panic never fired inside the locked window, so the test proves nothing")
	}
	ps.armed = false
	done := make(chan struct{})
	go func() {
		defer close(done)
		NoteSweepExpand(base, marker, 10, tkey)
		e.scanReferences(req, &components.Report{}, warmCtx("wedge", base), tkey)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("dropMu is still held after a panic in the scan: every later expand and scan blocks")
	}
}

// An expand of a marker that two threads carry, with no thread known, is ONE expand in the total and
// one in each owner's row.
func TestASharedMarkerExpandCountsOnceInTheTotal(t *testing.T) {
	resetSweepLedgerForTest()
	t.Cleanup(resetSweepLedgerForTest)
	st := newMemStore()
	content := refOutput(9)
	marker := hashKey(content)
	SeedSweepDropForTest(st, "thread-A", content, marker)
	SeedSweepDropForTest(st, "thread-B", content, marker)
	NoteSweepExpand(st, marker, 700, "")
	q := SweepDropCounters().Overall
	if q.ExpandEvents != 1 || q.ExpandTokens != 700 {
		t.Errorf("total = %+v, want 1 expand event and 700 tokens, not one per owner", q)
	}
	if q.Expanded != 2 {
		t.Errorf("total expanded drops = %d, want 2 (one drop per owning thread)", q.Expanded)
	}
	rows := 0
	for _, th := range SweepDropCounters().Threads {
		if th.ExpandEvents == 1 && th.Expanded == 1 {
			rows++
		}
	}
	if rows != 2 {
		t.Errorf("%d thread rows carry the expand, want 2", rows)
	}
}
