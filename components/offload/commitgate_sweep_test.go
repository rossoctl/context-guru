package offload

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"github.com/rossoctl/context-guru/thread"
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
