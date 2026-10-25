package offload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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
	if note := NoteSweepExpand(st, "deadbeefdeadbeef", 10); note.Sweep {
		t.Error("an expand of a foreign marker was charged to the sweep")
	}
	n1 := NoteSweepExpand(st, id, 1200)
	if !n1.Sweep || !n1.First {
		t.Fatalf("first expand of a sweep marker: %+v, want Sweep and First", n1)
	}
	n2 := NoteSweepExpand(st, id, 1200)
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
	NoteSweepExpand(st, id, 1200)
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
