package offload

import (
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/internal/thread"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

func assertSweepRejection(t *testing.T, rep *components.Report, want string) {
	t.Helper()
	if len(rep.Calls) == 0 {
		t.Fatal("no ModelCall row was reported, so there is no rejection reason to assert on")
	}
	for _, call := range rep.Calls {
		if call.Rejection != want {
			t.Errorf("rejection = %q, want %q.\nAn operator reading the ledger cannot tell a "+
				"turn where nothing was worth dropping from a turn where everything was and the "+
				"store had no room — and those want opposite responses", call.Rejection, want)
		}
		if call.Accepted {
			t.Error("the row says accepted=true alongside a rejection reason: the same " +
				"self-contradictory row this change set out to remove")
		}
	}
}

// A declined replay must record nothing.
//
// The fixture sits in a NARROW gap: the descriptor alone is smaller than the content, but the
// descriptor PLUS the marker is not. A drop record exists for the output, so the replay path is
// entered; it declines at the marker-inclusive never-worse check; the message goes upstream verbatim
// and neither a replay nor a saving is booked. Without the precondition below the test would pass on
// a request nothing touched.
func TestSweepRecordsNoReplayForADropItDeclined(t *testing.T) {
	content := sweepDeclineFixture(t)
	e := newSweepSmall(t, "")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("do the thing"), toolResultMsg(content),
	}}
	c := preExpiryCtx("s", nil, st)
	// PRICED HIGH, so a wrongly booked saving is visible (round4 would hide a few tokens at real rates).
	c.SelfRates = components.TokenRates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50}
	tkey := thread.Key("s", "")
	ck := extract.ContentKey(content)
	putDrop(st, sweepDropRec{Thread: tkey, ToolUseID: "t", CK: ck, Marker: hashKey(content), Tokens: 1})
	addToDropIndex(st, tkey, []string{ck})
	rep := &components.Report{Component: "extract_llm_sweep"}
	before, hitsBefore := sweepValueUSD(), sweepCacheHits()
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	// PRECONDITION: the replay path was entered (the lookup is recorded on entry, before any decline).
	if got := sweepCacheHits() - hitsBefore; got == 0 {
		t.Fatalf("the sweep looked up no drop record, so the replay path was not entered and the "+
			"assertions below would pass on a request nothing touched (gates: %v)", rep.Gates)
	}
	if got := schema.MessageText(req.Input[1]); got != content {
		t.Fatalf("the fixture was spliced after all (%q), so there is no declined replay", got)
	}
	if rep.Replays != 0 {
		t.Errorf("rep.Replay fired %d time(s) for a replay that did not splice", rep.Replays)
	}
	if after := sweepValueUSD(); after > before {
		t.Errorf("sweep gross value rose from %v to %v for a drop that did not happen", before, after)
	}
}

// sweepDeclineFixture finds a tool output for which the sweep's shape descriptor is a real
// reduction and the descriptor PLUS the marker is not. Fails loudly rather than returning a
// best effort: a fixture that misses the gap would make the caller pass against the bug.
func sweepDeclineFixture(t *testing.T) string {
	t.Helper()
	for n := 1; n <= 400; n++ {
		content := strings.Repeat("record of the audit log\n", n)
		desc := sweepDescriptor(content)
		withMarker := desc + "\n" + expand.Marker(hashKey(content)) +
			" [full output: call " + expand.ToolName + "]"
		if schema.TextTokens(desc) < schema.TextTokens(content) &&
			schema.TextTokens(withMarker) >= schema.TextTokens(content) {
			return content
		}
	}
	t.Fatal("no content size makes the descriptor a win while the descriptor+marker is not; the " +
		"never-worse decline this test needs is unreachable, so it would pass vacuously")
	return ""
}

func sweepValueUSD() float64 {
	if s := metricsExtractByComponent("extract_llm_sweep"); s != nil {
		return s.GrossValueUSD
	}
	return 0
}

// sweepCacheHits counts result-cache HITS, not lookups. CacheLookups counts misses too, so a
// precondition built on it was satisfied by the MISS recorded further down the loop — it "proved"
// the replay branch had been entered when it had not.
func sweepCacheHits() int64 {
	if s := metricsExtractByComponent("extract_llm_sweep"); s != nil {
		return s.CallsAvoided
	}
	return 0
}

// agentdiet pays a reflection model call over a whole step, and every plan it produces wants a
// payload. If the reserve cannot take even the smallest of them, the call is paid and every plan
// is then declined at commitMark — a model call for a step guaranteed to be left verbatim.
//
// Weaker than summarize's check on purpose: it probes with the SMALLEST candidate, so it skips
// only when nothing at all can be admitted and never declines a step whose plans might still fit.
func TestAgentDietDoesNotPayForAReflectionItCannotStash(t *testing.T) {
	model := &silentModel{}
	c, err := newAgentDiet([]byte("delay_steps: 0\ncontext_steps: 1\nmin_step_tokens: 10\n" +
		"min_saved_tokens: 1\nmax_keep_ratio: 1.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := c.(*AgentDiet)
	d.modelClient = model
	d.mode = markerFull

	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("run the tests"),
		callMsg("t1"), toolResultMsgWithID("t1", strings.Repeat("pytest output line\n", 200)),
		callMsg("t2"), toolResultMsgWithID("t2", strings.Repeat("more output here\n", 200)),
	}}
	// A reserve with no room at all.
	st := store.NewMemory(store.Options{MaxEntries: 1})
	rep := &components.Report{Component: "agentdiet"}
	if _, err := d.Offload(req, rep, ctxFor(st)); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if n := atomic.LoadInt64(&model.calls); n != 0 {
		t.Errorf("the reflection model was called %d time(s) although the reserve could not "+
			"hold ANY payload: every plan it produced would be declined at commitMark, so the "+
			"call is paid for a step that is guaranteed to be left verbatim", n)
	}
	if rep.Gates["stash_reserve_exhausted"] == 0 {
		t.Errorf("the skip was not recorded as stash_reserve_exhausted, so a run that stopped "+
			"reducing looks like a run with nothing to reduce (gates: %v)", rep.Gates)
	}
}
