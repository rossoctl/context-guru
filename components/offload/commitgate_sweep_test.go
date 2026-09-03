package offload

import (
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// The sweep's phase 3 had the same defect as extract_llm's, one file over.
//
// putResult ran BEFORE applySweepDrop, so a drop refused for want of a reserve slot left a frozen
// cg:res: record for a removal that never happened. The same-session replay path above reads that
// record and bypasses the depth gate on the reasoning that the bytes were already sent — so on a
// later turn with a free slot, the output is removed from inside the provider's cached prefix.
func TestSweepFreezesNoDecisionForADropThatDidNotHappen(t *testing.T) {
	asker := &labelAsker{verdict: "drop", needed: "none"}
	asker.cacheRead = 19595
	e := newSweepSmall(t, "")
	req := sweepReqStocked()
	originals := make([]string, len(req.Input))
	for i := range req.Input {
		originals[i] = schema.MessageText(req.Input[i])
	}
	spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	rep := &components.Report{}
	if _, err := e.Offload(req, rep, preExpiryCtx("s", asker, spy)); err != nil {
		t.Fatalf("Offload must fail open: %v", err)
	}
	// PRECONDITIONS. The adjudicator must have been asked and must have said "drop", or phase 3
	// was never reached and every assertion below passes on a transcript nothing touched.
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("expected ONE prefix ask, got %d (gates: %v)", n, rep.Gates)
	}
	if rep.Gates["stash_reserve_exhausted"] == 0 {
		t.Fatalf("no drop was refused for want of a reserve slot, so the defect's precondition "+
			"never held (gates: %v, events: %v)", rep.Gates, rep.Events)
	}
	// The invariant.
	if got := spy.decisionWrites(); len(got) != 0 {
		t.Errorf("the sweep froze %d decision(s) for a drop that did not happen: %v.\nA later "+
			"turn replays such a record at any depth, removing the output from inside the "+
			"provider's cached prefix", len(got), got)
	}
	for i := range req.Input {
		if got := schema.MessageText(req.Input[i]); got != originals[i] {
			t.Errorf("message %d was rewritten although its original could not be stored", i)
		}
	}
}

// The metrics half on the sweep path: a refused drop must not book its token savings.
//
// RecordExtractionValue sat ABOVE the ok check on applySweepDrop's return, so a drop the reserve
// declined still credited the tokens it would have saved. rep.Replay on that path was already
// guarded correctly, which is what made the unguarded metric next to it easy to miss.
func TestSweepReportsNoSavingsForARefusedReplay(t *testing.T) {
	// The fixture has to sit in a NARROW gap, and getting it wrong makes this test vacuous: the
	// saving is computed from the descriptor ALONE, so the descriptor must be smaller than the
	// content (or no saving would be booked either way and the mutation escapes), while
	// descriptor+marker must NOT be (or the splice succeeds and there is no declined replay).
	// Searched rather than hard-coded, because both sides depend on the tokenizer.
	content := sweepDeclineFixture(t)
	e := newSweepSmall(t, "")
	asker := &labelAsker{verdict: "keep", needed: "none"}
	asker.cacheRead = 19595
	st := store.NewMemory(store.Options{MaxEntries: 400})
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("do the thing"), toolResultMsg(content),
	}}
	c := preExpiryCtx("s", asker, st)
	// PRICED HIGH, deliberately. The saving under test is a few tokens, and at real cache-read
	// rates (~3e-7 USD/token) that is ~1e-6 USD — which round4 reports as 0.0000, so a saving
	// wrongly booked would be indistinguishable from none and this test would pass against the
	// very defect it exists to catch. A rate of 1 USD/token makes the counter observable without
	// changing which BRANCH runs: savedTokenValue reads SelfRates.CacheRead on a warm cache-aware
	// turn, which is what preExpiryCtx sets up.
	c.SelfRates = components.TokenRates{Input: 10, CacheRead: 1, CacheWrite: 12.5, Output: 50}
	// Freeze a decision whose descriptor is LONGER than the content once the marker is added, so
	// the replay path is entered and then declines.
	putResult(c, extract.ContentKey(content), sweepDescriptor(content), "")
	if _, hit := getResult(c, extract.ContentKey(content)); !hit {
		t.Fatal("the fixture's frozen decision is not readable, so the replay path is not taken")
	}
	rep := &components.Report{Component: "extract_llm_sweep"}
	before, hitsBefore := sweepValueUSD(), sweepCacheHits()
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	// PRECONDITION, and this test was vacuous without it: an unchanged message is ALSO what a
	// component that never ran leaves behind, so "the message is verbatim and no saving was
	// booked" proves nothing until the replay branch is known to have been entered. The cache
	// lookup is recorded on entry to that branch, before any of the checks that can decline.
	if got := sweepCacheHits() - hitsBefore; got == 0 {
		t.Fatalf("the sweep recorded no result-cache HIT, so the replay path was not entered and "+
			"the assertions below would pass on a transcript nothing touched (gates: %v)", rep.Gates)
	}
	if got := schema.MessageText(req.Input[1]); got != content {
		t.Fatalf("the fixture was spliced after all (%q), so there is no declined replay to "+
			"assert about", got)
	}
	if rep.Replays != 0 {
		t.Errorf("rep.Replay fired %d time(s) for a replay that did not splice", rep.Replays)
	}
	if after := sweepValueUSD(); after > before {
		t.Errorf("sweep gross value rose from %v to %v for a drop that did not happen; the "+
			"savings figure now includes tokens that were never saved", before, after)
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
