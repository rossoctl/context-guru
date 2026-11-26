package offload

import (
	"fmt"
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// refOutput is a tool output that introduces identifiers the co-reference index can track.
func refOutput(i int) string {
	return strings.Repeat(fmt.Sprintf("handler_%d_timeout_ms = 30 in src/api/users_%d.py:42\n", i, i), 900)
}

// refConversation is a conversation of n outputs with ids toolu_wire_<i> and identifiers unique to each.
func refConversation(n int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("do the thing")}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input, toolResultMsgWithID(fmt.Sprintf("toolu_wire_%d", i), refOutput(i)))
	}
	return req
}

// heldFor asks the question over req at a ping and returns the held answer. Every output is voted
// drop.
func heldFor(t *testing.T, e *ExtractSweep, session string, req *bschemas.BifrostChatRequest) (store.Store, heldAnswer) {
	t.Helper()
	st := newMemStore()
	key, _ := registered(t, e, session, pingAsker(), st, req)
	dispatch, _, reason, ok := SweepPingSubstitute(key)
	if !ok {
		t.Fatalf("no question offered: %s", reason)
	}
	dispatch(time.Second, SweepPingWait{WaitingFor: "human"})
	h, ok := getHeld(st, key)
	if !ok || len(h.Outputs) == 0 {
		t.Fatal("nothing held")
	}
	return st, h
}

func reasonsOf(r recheckResult) map[string]int { return r.reasonCounts() }

// ⭐ POINT 6: the recheck, one reason at a time. Each case changes the cold request in exactly one way
// and asserts the verdict that depends on it falls and the others stand.
func TestRecheckDropsAVerdictForEachReasonAndOnlyThen(t *testing.T) {
	e := shippedSweep(t, "")
	judged := refConversation(3)
	st, h := heldFor(t, e, "recheck", judged)
	if h.Judged != 4 {
		t.Fatalf("judged length = %d, want 4", h.Judged)
	}
	cold := func() *components.Ctx {
		c := preExpiryCtx("recheck", nil, st)
		c.ColdCache = true
		return c
	}
	text := func(i int) string { return schema.MessageText(judged.Input[i]) }

	t.Run("nothing changed: every verdict stands", func(t *testing.T) {
		res := recheckHeld(cold(), refConversation(3), h, e.minTokens)
		if len(res.Valid) != 3 || len(res.Dropped) != 0 {
			t.Fatalf("valid=%d dropped=%v, want 3 valid", len(res.Valid), res.Dropped)
		}
	})
	t.Run("the conversation grew and nothing else changed", func(t *testing.T) {
		req := refConversation(3)
		req.Input = append(req.Input, assistantMsg("moving on to the next file"), userMsg("ok"))
		if res := recheckHeld(cold(), req, h, e.minTokens); len(res.Valid) != 3 {
			t.Fatalf("a longer conversation voided verdicts: %v", reasonsOf(res))
		}
	})
	t.Run("R3 the conversation is shorter than when judged", func(t *testing.T) {
		req := &bschemas.BifrostChatRequest{Input: refConversation(3).Input[:3]}
		res := recheckHeld(cold(), req, h, e.minTokens)
		if len(res.Valid) != 0 || reasonsOf(res)[RecheckConversationShorter] != 3 {
			t.Fatalf("want all 3 dropped as conversation_shorter, got valid=%d %v", len(res.Valid), reasonsOf(res))
		}
	})
	t.Run("R1 the output is gone", func(t *testing.T) {
		req := refConversation(3)
		req.Input[2] = assistantMsg("that output was removed by the client")
		res := recheckHeld(cold(), req, h, e.minTokens)
		if reasonsOf(res)[RecheckOutputGone] != 1 || len(res.Valid) != 2 {
			t.Fatalf("want 1 output_gone and 2 valid, got valid=%d %v", len(res.Valid), reasonsOf(res))
		}
	})
	t.Run("R2 the content changed", func(t *testing.T) {
		req := refConversation(3)
		req.Input[1] = toolResultMsgWithID("toolu_wire_0", text(1)+"one more line the agent never saw judged\n")
		res := recheckHeld(cold(), req, h, e.minTokens)
		if reasonsOf(res)[RecheckContentChanged] != 1 || len(res.Valid) != 2 {
			t.Fatalf("want 1 content_changed and 2 valid, got valid=%d %v", len(res.Valid), reasonsOf(res))
		}
	})
	t.Run("R4 a later turn referenced the output", func(t *testing.T) {
		req := refConversation(3)
		req.Input = append(req.Input,
			assistantMsg("I will raise handler_1_timeout_ms in src/api/users_1.py:42 now"))
		res := recheckHeld(cold(), req, h, e.minTokens)
		if reasonsOf(res)[RecheckReferenced] != 1 || len(res.Valid) != 2 {
			t.Fatalf("want 1 referenced_later and 2 valid, got valid=%d %v", len(res.Valid), reasonsOf(res))
		}
		for _, o := range res.Valid {
			if o.ToolUseID == "toolu_wire_1" {
				t.Fatal("the referenced output is still a valid drop")
			}
		}
	})
	t.Run("R5 the agent expanded it", func(t *testing.T) {
		req := refConversation(3)
		c := cold()
		c.Store = newMemStore() // its own store: the mark must not leak into the other cases
		c.Store.Put(keptKey("recheck", contentKey(text(3))), []byte("1"))
		res := recheckHeld(c, req, h, e.minTokens)
		if reasonsOf(res)[RecheckExpanded] != 1 || len(res.Valid) != 2 {
			t.Fatalf("want 1 expanded and 2 valid, got valid=%d %v", len(res.Valid), reasonsOf(res))
		}
	})
	t.Run("outputs may move: a different position is the same output", func(t *testing.T) {
		req := refConversation(3)
		req.Input = append([]bschemas.ChatMessage{userMsg("a message inserted before them")}, req.Input...)
		if res := recheckHeld(cold(), req, h, e.minTokens); len(res.Valid) != 3 {
			t.Fatalf("verdicts are matched by tool_use id, not position: %v", reasonsOf(res))
		}
	})
}

// A reference the model already SAW is not new information. Only turns after the question count.
func TestRecheckIgnoresReferencesMadeBeforeTheQuestion(t *testing.T) {
	e := shippedSweep(t, "")
	judged := refConversation(3)
	judged.Input = append(judged.Input,
		assistantMsg("noted handler_1_timeout_ms in src/api/users_1.py:42"), userMsg("ok"))
	st, h := heldFor(t, e, "recheck-before", judged)
	c := preExpiryCtx("recheck-before", nil, st)
	same := refConversation(3)
	same.Input = append(same.Input,
		assistantMsg("noted handler_1_timeout_ms in src/api/users_1.py:42"), userMsg("ok"))
	if res := recheckHeld(c, same, h, e.minTokens); len(res.Valid) != 3 {
		t.Fatalf("a reference made before the question voided a verdict: %v", reasonsOf(res))
	}
}

// Kept verdicts are never "dropped" by the recheck: they were never going to be applied.
func TestRecheckOnlyJudgesDropVerdicts(t *testing.T) {
	h := heldAnswer{Judged: 2, Outputs: []heldOutput{
		{ToolUseID: "toolu_wire_0", Hash: "x", Saves: 10, Drop: false},
	}}
	res := recheckHeld(preExpiryCtx("k", nil, newMemStore()), refConversation(1), h, 2000)
	if len(res.Valid) != 0 || len(res.Dropped) != 0 {
		t.Fatalf("a kept verdict was rechecked: %+v", res)
	}
}

// The recheck reads the AGENT's request when the host supplies it. An earlier component that changed
// the message count (#452) moves every index in req.Input and must not move the answer.
func TestRecheckReadsTheAgentsOwnRequestWhenThePipelineChangedTheCount(t *testing.T) {
	e := shippedSweep(t, "")
	judged := refConversation(3)
	st, h := heldFor(t, e, "recheck-raw", judged)
	c := preExpiryCtx("recheck-raw", nil, st)
	c.RawInput = refConversation(3).Input
	// The pipeline's copy has lost a message (a summarizer replaced two of them with one).
	pipeline := &bschemas.BifrostChatRequest{Input: refConversation(3).Input[1:]}
	if len(pipeline.Input) == len(c.RawInput) {
		t.Fatal("fixture is wrong: the counts agree")
	}
	if res := recheckHeld(c, pipeline, h, e.minTokens); len(res.Valid) != 3 {
		t.Fatalf("the pipeline's shifted coordinates voided verdicts: %v", reasonsOf(res))
	}
}

// ⭐ POINT 7: shadow mode. Off by default and in no preset; on, it reports and changes nothing.
func TestShadowIsOffByDefaultAndInNoPreset(t *testing.T) {
	c, err := newExtractSweep(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.(*ExtractSweep).shadow {
		t.Fatal("shadow mode is on by default")
	}
	if !shippedSweep(t, "shadow: true\n").shadow {
		t.Fatal("shadow: true was not honoured")
	}
}

func TestShadowReportsTheAskThenTheRecheckAtTheColdRequest(t *testing.T) {
	e := shippedSweep(t, "shadow: true\n")
	judged := refConversation(3)
	st := newMemStore()
	key, _ := registered(t, e, "shadow-flow", pingAsker(), st, judged)
	dispatch, _, _, _ := SweepPingSubstitute(key)
	dispatch(time.Second, SweepPingWait{WaitingFor: "human"})

	// The first request after the ping is WARM: it reports the ask and leaves the answer held.
	warm := preExpiryCtx("shadow-flow", pingAsker(), st)
	warm.IdleMs = 20_000
	req := refConversation(3)
	rep := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req, rep, warm); err != nil {
		t.Fatal(err)
	}
	for ev, want := range map[string]int{
		"sweep_shadow_asked": 3, "sweep_shadow_proposed": 3, "sweep_shadow_wait_human": 1,
	} {
		if rep.Events[ev] != want {
			t.Errorf("warm request: %s = %d, want %d (events: %v)", ev, rep.Events[ev], want, rep.Events)
		}
	}
	if rep.Events["sweep_shadow_proposed_tokens"] <= 0 {
		t.Errorf("proposed tokens not reported: %v", rep.Events)
	}
	if _, has := rep.Events["sweep_shadow_valid"]; has {
		t.Errorf("a WARM request ran the cold recheck: %v", rep.Events)
	}
	if _, ok := getHeld(st, key); !ok {
		t.Fatal("a warm request consumed the held answer")
	}

	// The next request is COLD, and one of the outputs was referenced meanwhile.
	cold := preExpiryCtx("shadow-flow", pingAsker(), st)
	cold.ColdCache = true
	req2 := refConversation(3)
	req2.Input = append(req2.Input, assistantMsg("raising handler_2_timeout_ms in src/api/users_2.py:42"))
	snapshot := make([]string, len(req2.Input))
	for i := range req2.Input {
		snapshot[i] = schema.MessageText(req2.Input[i])
	}
	rep2 := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(req2, rep2, cold); err != nil {
		t.Fatal(err)
	}
	if rep2.Events["sweep_shadow_valid"] != 2 || rep2.Events["sweep_shadow_dropped_referenced_later"] != 1 {
		t.Errorf("cold recheck: valid=%d referenced=%d, want 2 and 1 (events: %v)",
			rep2.Events["sweep_shadow_valid"], rep2.Events["sweep_shadow_dropped_referenced_later"], rep2.Events)
	}
	if _, has := rep2.Events["sweep_shadow_asked"]; has {
		t.Errorf("the ask was reported twice: %v", rep2.Events)
	}
	if rep2.Events["sweep_shadow_valid_tokens"] <= 0 {
		t.Errorf("valid tokens not reported: %v", rep2.Events)
	}
	for i := range req2.Input {
		if schema.MessageText(req2.Input[i]) != snapshot[i] {
			t.Fatalf("shadow mode changed message %d", i)
		}
	}
	if _, ok := getHeld(st, key); ok {
		t.Error("the cold recheck left the answer held: it would be reported again")
	}
	// And a further cold request reports nothing.
	rep3 := &components.Report{Component: "extract_llm_sweep"}
	if _, err := e.Offload(refConversation(3), rep3, cold); err != nil {
		t.Fatal(err)
	}
	for ev := range rep3.Events {
		if strings.HasPrefix(ev, "sweep_shadow_") {
			t.Errorf("a consumed answer was reported again: %s", ev)
		}
	}
}
