package offload

import (
	"context"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/metrics"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// metricsExtractByComponent reads one component's extraction figures out of the process-global
// snapshot. Pricing is held at zero across a test's before/after pair, so the DELTA is what the
// component recorded and not what the host would charge for it.
func metricsExtractByComponent(name string) *metrics.ExtractStats {
	return metrics.ExtractSnapshot(0, 0, 0, 0).ByComponent[name]
}

// The two extract_llm sites the #188 review found, and the shape of the harm.
//
// putResult / putResultGlobal ran BEFORE apply. Once apply could refuse for want of a reserve
// slot, a declined removal left a pinned cg:res: record asserting "this session sent these
// compacted bytes" for a message that went upstream unchanged. That record is not inert: the
// same-session replay path reads it and deliberately bypasses the cache-tail gate — its comment
// reasons that the bytes were already sent, so splicing is safe at any depth — so on the first
// later turn with a free slot, the compaction lands inside the provider's cached prefix and the
// whole suffix is re-written at ~11.5x the read price.
func TestExtractLLMFreezesNoDecisionForASpliceThatDidNotHappen(t *testing.T) {
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model) // min_tokens: 1, strategy: code, gate off
	original := strings.Repeat("2026-08-31T10:00:00Z INFO  worker: processed batch\n", 400)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Summarize the worker log and tell me if any batch failed."),
		toolResultMsg(original),
	}}
	spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	c := &components.Ctx{Session: "gate-test", Store: spy, Ctx: context.Background(),
		Model: components.ModelSpec{Static: model, Incoming: model}}
	rep := &components.Report{Component: "extract_llm"}
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	// Preconditions. Without the call there is no decision to freeze, and without a candidate
	// reaching phase 3 the assertion below is vacuous — the failure mode this repo keeps hitting.
	if model.calls == 0 {
		t.Fatal("the extraction model was never called, so phase 3 was never reached and this " +
			"test proves nothing")
	}
	if got := schema.MessageText(req.Input[1]); got != original {
		t.Fatalf("the tool output was rewritten although its original could not be stored:\n%q", got)
	}
	// The invariant.
	if got := spy.decisionWrites(); len(got) != 0 {
		t.Errorf("extract_llm froze %d decision(s) for a splice that did not happen: %v.\n"+
			"The same-session replay path bypasses the cache-tail gate on the strength of such "+
			"a record, so the compaction would be spliced into the provider's cached prefix on "+
			"a later turn — a full-suffix cache write at ~11.5x the read price, which is "+
			"exactly what TestGlobalCacheHitIsNotSplicedAtDepth exists to prevent",
			len(got), got)
	}
	if _, hit := getResult(c, extract.ContentKey(original)); hit {
		t.Error("a cg:res: decision is readable for a removal that was refused; the next turn " +
			"will replay it at any depth")
	}
}

// The metrics half of the same finding: nothing may book a saving for a splice that did not
// happen.
//
// RecordExtractionValue, dbgReapply and rep.Replay all ran unconditionally around apply. The
// case that survives #188's design is the MARKER-INCLUSIVE decline: apply refuses when
// projection+marker is not smaller than the original, while the savings figure was computed from
// the projection ALONE — so a message left verbatim still booked the tokens the projection would
// have saved, and rep.Replay claimed a replay that never went out.
//
// The fixture sits deliberately in that gap: the projection is smaller than the content, and the
// marker plus its recovery hint takes the rewrite back over.
func TestExtractLLMReportsNoSavingsForASpliceItDeclined(t *testing.T) {
	// ~5 tokens of head, so the projection wins a little and the marker (a 16-hex id plus
	// " [full output: call context_guru_expand]") loses more.
	original := "alpha beta gamma delta epsilon zeta eta theta\n"
	projected := "alpha beta gamma delta\n"
	if schema.TextTokens(projected) >= schema.TextTokens(original) {
		t.Fatal("the fixture's projection is not smaller than its content, so apply would " +
			"decline for the wrong reason")
	}
	withMarker := projected + "\n" + expand.Marker(hashKey(original)) +
		" [full output: call " + expand.ToolName + "]"
	if schema.TextTokens(withMarker) < schema.TextTokens(original) {
		t.Fatal("the fixture's projection still wins WITH the marker, so apply will splice and " +
			"this test cannot observe a declined splice")
	}

	st := store.NewMemory(store.Options{MaxEntries: 400}) // healthy: the reserve is not the gate
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model)
	c := &components.Ctx{Session: "replay-test", Store: st, Ctx: context.Background(),
		Model: components.ModelSpec{Static: model, Incoming: model}}
	id := extract.ContentKey(original)
	putResult(c, id, projected, "")
	if _, hit := getResult(c, id); !hit {
		t.Fatal("the fixture's frozen decision is not readable, so the replay path will not be " +
			"taken and this test would pass vacuously")
	}

	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Summarize the worker log."), toolResultMsg(original),
	}}
	rep := &components.Report{Component: "extract_llm"}
	// The savings counter is process-global and read through a priced snapshot, so the honest
	// reading is the DELTA over this one call, with pricing held constant.
	valueUSD := func() float64 {
		if s := metricsExtractByComponent("extract_llm"); s != nil {
			return s.GrossValueUSD
		}
		return 0
	}
	before := valueUSD()
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if got := schema.MessageText(req.Input[1]); got != original {
		t.Fatalf("the fixture was spliced after all (%q), so there is no declined splice to "+
			"assert about", got)
	}
	if rep.Replays != 0 {
		t.Errorf("rep.Replay fired %d time(s) for a splice that was declined: the message went "+
			"upstream verbatim, so no replay reached the model", rep.Replays)
	}
	if after := valueUSD(); after > before {
		t.Errorf("extraction gross value rose from %v to %v for a splice that did not happen; "+
			"the savings figure now includes tokens that were never saved", before, after)
	}
}

// The replay path must NOT refuse when the payload has gone — it must replay and report.
//
// This is the deliberate asymmetry in #188 after the review, and it is easy to get backwards.
// A NEW removal declines when the reserve cannot back it, because nothing has been promised yet.
// A REPLAY of a decision already stamped and already sent cannot decline: the provider's cached
// prefix holds the compacted bytes, so sending the original back in full is itself the
// cache-destructive move, and it cannot un-send a marker that went out turns ago. So the splice
// proceeds, and the dangling promise is counted (stash_missing) rather than obeyed.
func TestExtractLLMReplaysADanglingDecisionRatherThanFlippingCachedBytes(t *testing.T) {
	original := strings.Repeat("2026-08-31T10:00:00Z INFO  worker: processed batch\n", 400)
	// A store holding the frozen decision but refusing every payload: exactly the state a
	// saturated reserve leaves behind for a decision made on an earlier turn.
	spy := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model)
	c := &components.Ctx{Session: "dangling-test", Store: spy, Ctx: context.Background(),
		Model: components.ModelSpec{Static: model, Incoming: model}}
	putResult(c, extract.ContentKey(original), "one line kept", "")
	if _, hit := getResult(c, extract.ContentKey(original)); !hit {
		t.Fatal("the fixture's frozen decision is not readable, so the replay path will not be taken")
	}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("Summarize the worker log."), toolResultMsg(original),
	}}
	rep := &components.Report{Component: "extract_llm"}
	refusedBefore, missingBefore := StashRefusals(), StashMissing()
	if _, err := e.Offload(req, rep, c); err != nil {
		t.Fatalf("Offload: %v", err)
	}
	if got := schema.MessageText(req.Input[1]); got == original {
		t.Error("the replay was declined and the original was sent in full: the provider's " +
			"cached prefix holds the COMPACTED form, so this flips already-cached content and " +
			"re-writes the whole suffix at ~11.5x the read price — to protect a reversibility " +
			"promise that a refusal here cannot restore anyway")
	}
	if got := StashMissing() - missingBefore; got == 0 {
		t.Error("StashMissing() did not move: a marker was replayed with no payload behind it " +
			"and nothing reported the dangling promise")
	}
	if got := StashRefusals() - refusedBefore; got != 0 {
		t.Errorf("StashRefusals() moved by %d on a replay path, want 0: that counter promises "+
			"the operator that nothing became irreversible", got)
	}
}

// A DANGLING replay is the opposite outcome from a declined removal, and the two must not share
// a counter.
//
// Every operator-facing description of stash_refused promises that "the content was left
// verbatim and nothing became irreversible" — true of a declined removal, and false of a replay
// whose payload has gone, which is the only case that actually breaks the guarantee #187 was
// about. Counting them together meant the number an operator watches to confirm nothing broke
// was incremented by things breaking.
func TestADanglingReplayIsCountedApartFromADeclinedRemoval(t *testing.T) {
	// A store that accepts nothing, so the refresh of an already-stamped marker fails: the
	// payload is not there and cannot be put back.
	st := &spyStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	c := &components.Ctx{Session: "s", Store: st, Ctx: context.Background()}
	refusedBefore, missingBefore := StashRefusals(), StashMissing()

	// The refresh path: a decision this session already stamped and sent.
	original := strings.Repeat("a line of output\n", 50)
	replacement := "[older tool output masked] " + expand.Marker(hashKey(original))
	st.Memory.Put(frozenKey("s", "mask", contentKey(original)), []byte(replacement))
	m := bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool}
	schema.SetMessageText(&m, original)
	keys, _, ok := reapplyFrozen(c, "mask", &m)
	if !ok || len(keys) == 0 {
		t.Fatal("the frozen decision was not replayed, so no marker was re-sent and this test " +
			"proves nothing about a dangling one")
	}
	// The replay MUST still have happened — declining would flip an already-cached message.
	if got := schema.MessageText(m); got != replacement {
		t.Errorf("the replay was declined: got %q, want the frozen replacement %q.\nRefusing "+
			"here sends the original where the provider's cached prefix holds the masked form, "+
			"which is the cache-destructive direction and cannot un-send the marker anyway",
			got, replacement)
	}
	if got := StashMissing() - missingBefore; got != 1 {
		t.Errorf("StashMissing() moved by %d, want 1: a marker went out with no payload behind "+
			"it and nothing counted the dangling promise", got)
	}
	if got := StashRefusals() - refusedBefore; got != 0 {
		t.Errorf("StashRefusals() moved by %d for a DANGLING replay, want 0. That counter tells "+
			"the operator 'nothing became irreversible'; a dangling marker is precisely the "+
			"case where something did", got)
	}
}

// The THIRD pre-gate site, which the review did not name and the twelve-caller audit found: a
// CROSS-SESSION cache hit froze a decision into this session before the splice.
//
// The comment on that branch is explicit that it is a NEW decision for this session — "THIS
// session never sent these compacted bytes, so the provider's cached prefix holds the ORIGINAL"
// — which is precisely why it is tail-gated. So its payload write can refuse like any other new
// removal, and freezing ahead of it produced the same dangling record: the same-session replay
// path then splices at any depth on a later turn, inside the cached prefix that the tail gate
// two blocks above exists to protect.
//
// THE STATE THIS NEEDS, and it took a wrong fixture to see it: a payload is keyed by its CONTENT
// hash, so a second session compacting the same output finds the first session's payload already
// there and its write is a refresh that cannot refuse. The refusal is reachable only when the
// payload is GONE while the global decision naming it survives — which is the ordinary outcome
// of the two having different durabilities: cg:xres: is PINNED (exempt from LRU eviction) and the
// payload is not, which is #187 itself.
//
// Session A runs against a healthy store; its cg:xres: entry is then carried over to a store that
// holds no payload and accepts none. The global key is read out of the recorded writes rather than
// recomputed, because it is built from the component's own extCfg and a fixture that rebuilt it
// would pass while the real key drifted.
func TestExtractLLMFreezesNoDecisionForACrossSessionHitItDidNotSplice(t *testing.T) {
	original := strings.Repeat("2026-08-31T10:00:00Z INFO  worker: processed batch\n", 400)
	model := &shrinkingModel{}
	e := newTimeoutTestComponent(t, model)
	newReq := func() *bschemas.BifrostChatRequest {
		return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("Summarize the worker log."), toolResultMsg(original),
		}}
	}

	// --- Session A, healthy store: publishes the result to the GLOBAL namespace.
	stA := &gatedStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	a := &components.Ctx{Session: "sessA", Store: stA, Ctx: context.Background(),
		Model: components.ModelSpec{Static: model, Incoming: model}}
	reqA := newReq()
	if _, err := e.Offload(reqA, &components.Report{Component: "extract_llm"}, a); err != nil {
		t.Fatal(err)
	}
	if schema.MessageText(reqA.Input[1]) == original {
		t.Fatal("session A did not compact, so nothing was published globally and session B " +
			"cannot take the cross-session branch")
	}
	var gkey string
	for _, k := range stA.puts {
		if strings.HasPrefix(k, store.XResultPrefix) {
			gkey = k
			break
		}
	}
	if gkey == "" {
		t.Fatal("session A published no cg:xres: entry, so the cross-session hit under test " +
			"cannot occur")
	}
	gval, ok := stA.Memory.Get(gkey)
	if !ok {
		t.Fatalf("the global entry %q is not readable back", gkey)
	}

	// --- Session B: the global decision survives, its payload does not, and no payload can be
	// stored. Exactly the durability gap #187 is about.
	stB := &refusingStore{Memory: store.NewMemory(store.Options{MaxEntries: 400})}
	stB.Memory.Put(gkey, gval)
	if _, ok := expand.Resolve(stB, hashKey(original)); ok {
		t.Fatal("the fixture's store already holds the payload, so session B's write would be a " +
			"refresh and could not refuse")
	}
	stB.puts = nil
	b := &components.Ctx{Session: "sessB", Store: stB, Ctx: context.Background(),
		Model: components.ModelSpec{Static: model, Incoming: model}}
	reqB := newReq()
	repB := &components.Report{Component: "extract_llm"}
	callsBefore := model.calls
	hitsBefore := extractCacheHits("extract_llm")
	if _, err := e.Offload(reqB, repB, b); err != nil {
		t.Fatal(err)
	}
	// PRECONDITION: the cross-session branch really was taken. Evidenced by the CACHE HIT it
	// records on entry — not by rep.Replay, which now (correctly) fires only after the splice and
	// so is absent in exactly the case under test. A fresh model call instead of a hit would mean
	// the global lookup missed and every assertion below is about the wrong path.
	if extractCacheHits("extract_llm")-hitsBefore == 0 {
		t.Fatalf("session B recorded no result-cache hit (calls %d->%d, gates %v, events %v); "+
			"the cross-session branch was not entered", callsBefore, model.calls,
			repB.Gates, repB.Events)
	}
	if model.calls != callsBefore {
		t.Fatalf("session B made a fresh model call, so it did not take the cross-session branch")
	}
	if repB.Gates["stash_reserve_exhausted"] == 0 {
		t.Fatalf("session B's payload write was not refused, so there is no declined splice to "+
			"assert about (gates: %v)", repB.Gates)
	}
	if got := schema.MessageText(reqB.Input[1]); got != original {
		t.Errorf("session B's output was rewritten although its original could not be stored")
	}
	if got := stB.decisionWrites(); len(got) != 0 {
		t.Errorf("session B froze %d decision(s) for a cross-session hit it did not splice: %v.\n"+
			"Its own replay path reads that record on a later turn and bypasses the tail gate, "+
			"splicing into the cached prefix the gate above it exists to protect", len(got), got)
	}
}

// extractCacheHits reads one component's result-cache HIT count out of the process-global
// snapshot. It is what proves a replay branch was entered even when the branch then declines.
func extractCacheHits(name string) int64 {
	if s := metricsExtractByComponent(name); s != nil {
		return s.CallsAvoided
	}
	return 0
}

// refusingStore holds whatever is Put into it but accepts NO rewind payload, and records the
// writes. Distinct from gatedStore, whose closed mode still honours a refresh of a payload that
// is present: here the payload is absent by construction and must stay unstorable.
type refusingStore struct {
	*store.Memory
	puts []string
}

func (r *refusingStore) Put(key string, payload []byte) {
	r.puts = append(r.puts, key)
	r.Memory.Put(key, payload)
}

func (r *refusingStore) PutStash(string, []byte) bool { return false }
func (r *refusingStore) StashRoom(int) bool           { return false }

func (r *refusingStore) decisionWrites() []string {
	var out []string
	for _, k := range r.puts {
		for _, p := range decisionPrefixes {
			if strings.HasPrefix(k, p) {
				out = append(out, k)
				break
			}
		}
	}
	return out
}
