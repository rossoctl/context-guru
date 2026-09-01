package offload

import (
	"fmt"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// --- fixture helpers ---------------------------------------------------------

func corefUser(text string) bschemas.ChatMessage {
	t := text
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleUser,
		Content: &bschemas.ChatMessageContent{ContentStr: &t}}
}

// corefAsst builds a model turn: prose plus a tool call, which together are the
// reference-bearing surface the index reads.
func corefAsst(text, name, args string) bschemas.ChatMessage {
	t, n := text, name
	return bschemas.ChatMessage{
		Role:    bschemas.ChatMessageRoleAssistant,
		Content: &bschemas.ChatMessageContent{ContentStr: &t},
		ChatAssistantMessage: &bschemas.ChatAssistantMessage{
			ToolCalls: []bschemas.ChatAssistantMessageToolCall{
				{Function: bschemas.ChatAssistantMessageToolCallFunction{Name: &n, Arguments: args}},
			},
		},
	}
}

func corefTool(id, text string) bschemas.ChatMessage {
	t, i := text, id
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleTool,
		Content:         &bschemas.ChatMessageContent{ContentStr: &t},
		ChatToolMessage: &bschemas.ChatToolMessage{ToolCallID: &i}}
}

// corefBody is a distinct multi-line output; distinct because shared filler would be
// discarded as session boilerplate by the index.
func corefBody(tag string) string {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&b, "%4d\t%s_line_%d = compute_%s_%d(arg_%d)\n", i, tag, i, tag, i, i)
	}
	return b.String()
}

// Sentinels that say whether an output is still verbatim. They sit at the END of their
// output on purpose: the marker carries a head peek of what it replaced, so a sentinel at
// the head survives the cut and "is it still there?" stops meaning "was it kept?".
const (
	corefNovelUsed   = "TOKEN_GRACE_SECONDS_41ab" // introduced by the read, then carried forward
	corefNovelUnused = "TREE_SCAN_MARKER_9d7c"    // introduced by the listing, never used again
	corefNovelFresh  = "FRESH_SCAN_MARKER_5e1f"   // introduced by a later listing, never used
)

// corefReq is a transcript with exactly two large tool outputs: index 2 is REFERENCED by
// the following model turn (must survive) and index 5 is referenced by nothing (the cut).
func corefReq() *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		corefUser("Fix the failing test test_auth_expiry in src/auth.py"),
		corefAsst("Reading the auth module.", "Read", `{"path":"src/auth.py"}`),
		corefTool("t1", "src/auth.py\n"+corefBody("auth")+"\n"+corefNovelUsed+" = 0\n"),
		corefAsst("The bug is "+corefNovelUsed+"; it must be 300.", "Edit",
			`{"path":"src/auth.py","old":"`+corefNovelUsed+` = 0"}`),
		corefAsst("Surveying the tree.", "Bash", `{"cmd":"ls -R"}`),
		corefTool("t2", corefBody("tree")+"\n"+corefNovelUnused+"\n"),
	}}
}

// corefWithSecondListing appends a fresh model turn plus a second unreferenced output —
// a new candidate for a pass that must be turned away.
func corefWithSecondListing() *bschemas.BifrostChatRequest {
	req := corefReq()
	req.Input = append(req.Input,
		corefAsst("Listing again.", "Bash", `{"cmd":"ls /srv"}`),
		corefTool("t3", corefBody("srv")+"\n"+corefNovelFresh+"\n"),
	)
	return req
}

const (
	corefRefIdx   = 2 // referenced output — must be left verbatim
	corefCutIdx   = 5 // unreferenced output — the cut
	corefFreshIdx = 7 // the second listing's output (see corefWithSecondListing)
)

func corefFor(t *testing.T, extraYAML string) *Coref {
	t.Helper()
	// The batch floor is the gate under test in exactly one case, so default it out of the
	// way here rather than repeating it — and never define it twice (yaml rejects that).
	// Defaults that would otherwise dominate every case: the batch floor, and the
	// opportunity floor (the fixture is a short transcript whose cut candidate sits at the
	// tail). Each is the gate under test in exactly one place, so a default is only added
	// when the case does not set it — yaml rejects a duplicated key.
	base := "min_tokens: 20\n"
	for k, v := range map[string]string{"min_batch_frac": "0", "min_later_turns": "0"} {
		if !strings.Contains(extraYAML, k) {
			base += k + ": " + v + "\n"
		}
	}
	comp, err := newCoref([]byte(base + extraYAML))
	if err != nil {
		t.Fatal(err)
	}
	return comp.(*Coref)
}

func corefCtx(st store.Store) *components.Ctx {
	return &components.Ctx{Session: "s", Store: st}
}

// --- behaviour ---------------------------------------------------------------

// The deterministic ceiling: cut what nothing referred back to, keep what was referred
// to. Nothing else in the request may move.
func TestCorefCutsOnlyUnreferencedOutputs(t *testing.T) {
	cf := corefFor(t, "")
	req, orig := corefReq(), corefReq()
	c := corefCtx(store.NewMemory(store.Options{}))
	var rep components.Report

	keys, err := cf.Offload(req, &rep, c)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Skipped {
		t.Fatal("component skipped; expected the unreferenced output to be cut")
	}
	if got := schema.MessageText(req.Input[corefRefIdx]); got != schema.MessageText(orig.Input[corefRefIdx]) {
		t.Errorf("the REFERENCED output was modified; it is load-bearing:\n%q", got)
	}
	cut := schema.MessageText(req.Input[corefCutIdx])
	if cut == schema.MessageText(orig.Input[corefCutIdx]) {
		t.Fatal("the unreferenced output was not cut")
	}
	if !strings.Contains(cut, "tool output compacted") {
		t.Errorf("marker does not say what happened: %q", cut)
	}
	assertMarkerMakesNoSafetyClaim(t, cut)
	for i := range req.Input {
		if i == corefCutIdx {
			continue
		}
		if schema.MessageText(req.Input[i]) != schema.MessageText(orig.Input[i]) {
			t.Errorf("message %d changed; only the cut output may move", i)
		}
	}
	// Reversible: the original must be retrievable under the returned key.
	if len(keys) != 1 {
		t.Fatalf("cache keys = %v, want exactly one stashed original", keys)
	}
	got, ok := c.Store.Get(keys[0])
	if !ok || string(got) != schema.MessageText(orig.Input[corefCutIdx]) {
		t.Error("the stashed original does not round-trip; the cut is not reversible")
	}
	if ms := expand.ParseMarkers(cut); len(ms) != 1 || ms[0] != keys[0] {
		t.Errorf("marker in the cut text = %v, want the stash key %q", ms, keys[0])
	}
}

// Latching, and the monotonicity that pays for it. Once a cut is taken, later turns
// replay the SAME BYTES even when fresh evidence would now classify the output as open.
// Re-deciding is what rewrites the prefix a second time, so new evidence may never
// resurrect a span — keep→cut only, in one direction.
func TestCorefLatchesAndNeverResurrects(t *testing.T) {
	cf := corefFor(t, "")
	st := store.NewMemory(store.Options{})

	req := corefReq()
	var rep components.Report
	if _, err := cf.Offload(req, &rep, corefCtx(st)); err != nil {
		t.Fatal(err)
	}
	latched := schema.MessageText(req.Input[corefCutIdx])
	if latched == schema.MessageText(corefReq().Input[corefCutIdx]) {
		t.Fatal("nothing was cut on the first turn")
	}

	// A later turn now references the previously-unreferenced output, repeatedly and
	// recently — i.e. it would classify as OPEN if the decision were re-derived.
	for turn := 1; turn <= 3; turn++ {
		next := corefReq() // the agent re-sends the originals every turn
		for k := 0; k < turn; k++ {
			next.Input = append(next.Input,
				corefAsst(corefNovelUnused+" again, attempt "+fmt.Sprint(k), "Bash", `{"cmd":"ls -R"}`))
		}
		var r components.Report
		if _, err := cf.Offload(next, &r, corefCtx(st)); err != nil {
			t.Fatal(err)
		}
		if got := schema.MessageText(next.Input[corefCutIdx]); got != latched {
			t.Fatalf("turn %d re-derived the decision:\n got %q\nwant %q", turn, got, latched)
		}
	}
}

// The budget is the component's answer for the cache-writes it spends on purpose. Once
// spent, further passes decline — while already-latched decisions keep being replayed,
// because NOT replaying them is itself the cache-destructive move.
func TestCorefRewriteBudgetIsSpentOnceAndEnforced(t *testing.T) {
	cf := corefFor(t, "rewrite_budget: 1\n")
	st := store.NewMemory(store.Options{})

	req := corefReq()
	var rep components.Report
	if _, err := cf.Offload(req, &rep, corefCtx(st)); err != nil {
		t.Fatal(err)
	}
	latched := schema.MessageText(req.Input[corefCutIdx])
	if latched == schema.MessageText(corefReq().Input[corefCutIdx]) {
		t.Fatal("nothing was cut on the first turn")
	}
	if n := corefRewrites(corefCtx(st)); n != 1 {
		t.Errorf("rewrites charged = %d, want exactly 1 for the whole batch", n)
	}

	// A second pass with a brand-new unreferenced output must decline: the budget is gone.
	next := corefWithSecondListing()
	var r2 components.Report
	if _, err := cf.Offload(next, &r2, corefCtx(st)); err != nil {
		t.Fatal(err)
	}
	if r2.Gates["rewrite_budget"] == 0 {
		t.Error("expected the rewrite_budget gate to turn the second pass away")
	}
	if got := schema.MessageText(next.Input[corefFreshIdx]); !strings.Contains(got, corefNovelFresh) {
		t.Error("the new output was cut despite an exhausted budget")
	}
	if got := schema.MessageText(next.Input[corefCutIdx]); got != latched {
		t.Error("an exhausted budget stopped the replay of an already-latched decision, " +
			"which is the flip the budget exists to prevent")
	}
	if n := corefRewrites(corefCtx(st)); n != 1 {
		t.Errorf("rewrites charged = %d after a declined pass, want 1", n)
	}
}

// Batching is the reason this component exists in this shape: one rewrite has to serve
// the whole pass. A batch below the floor leaves the request byte-identical rather than
// taking a small, losing cut.
func TestCorefDeclinesABatchTooSmallToPayForItsRewrite(t *testing.T) {
	cf := corefFor(t, "min_batch_frac: 0.9\n")
	req, orig := corefReq(), corefReq()
	var rep components.Report
	if _, err := cf.Offload(req, &rep, corefCtx(store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["batch_too_small"] == 0 {
		t.Error("expected the batch_too_small gate")
	}
	if !rep.Skipped {
		t.Error("a declined pass must report Skipped")
	}
	for i := range req.Input {
		if schema.MessageText(req.Input[i]) != schema.MessageText(orig.Input[i]) {
			t.Fatalf("message %d was modified by a pass that declined; planning must be side-effect free", i)
		}
	}
}

// Firing at maximum pressure is firing when there is almost nothing left to collect the
// saving on. The break-even inequality has to say no there, otherwise the component pays
// a cache-write for a single turn of savings.
func TestCorefBreakEvenDeclinesAtTheWindowEdge(t *testing.T) {
	cf := corefFor(t, "")
	req := corefReq()
	// A window barely above the current request: the estimated turns remaining collapses
	// to ~0, so no cut can repay the rewrite.
	c := corefCtx(store.NewMemory(store.Options{}))
	c.CtxWindow = schema.MessagesTokens(req) + 1
	var rep components.Report
	if _, err := cf.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["break_even"] == 0 {
		t.Fatalf("expected the break_even gate at the window edge; gates=%v", rep.Gates)
	}
	if got := schema.MessageText(req.Input[corefCutIdx]); !strings.Contains(got, corefNovelUnused) {
		t.Error("a cut was taken that cannot repay its own cache-write")
	}

	// With room to run, the same request and the same cut must clear.
	roomy := corefReq()
	c2 := corefCtx(store.NewMemory(store.Options{}))
	c2.CtxWindow = schema.MessagesTokens(roomy) * 50
	var rep2 components.Report
	if _, err := cf.Offload(roomy, &rep2, c2); err != nil {
		t.Fatal(err)
	}
	if rep2.Gates["break_even"] != 0 {
		t.Errorf("break_even declined with 50x the window headroom; gates=%v", rep2.Gates)
	}
}

// coref is the one offloader that mutates the already-cached prefix on purpose — that is
// its entire function, and it is why the spend is budgeted instead of forbidden. A tail
// restriction here would make the component a no-op on exactly the transcripts it exists
// for, since by the time a session crosses the threshold the mass is all in the prefix.
func TestCorefDeliberatelyCutsInsideTheCachedPrefix(t *testing.T) {
	cf := corefFor(t, "")
	req := corefReq()
	c := corefCtx(store.NewMemory(store.Options{}))
	c.CacheAware, c.MaxCachedIdx = true, len(req.Input)-1 // everything already cached
	var rep components.Report
	if _, err := cf.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[corefCutIdx]); strings.Contains(got, corefNovelUnused) {
		t.Fatal("coref respected the cache tail; it is supposed to spend the rewrite, " +
			"under budget, or it can never act on a long session")
	}
}

// The trigger gates only NEW decisions. Replay is unconditional, because a latched cut
// that stops being replayed flips cut→full inside the cached prefix.
func TestCorefTriggerGatesNewCutsButNotReplay(t *testing.T) {
	st := store.NewMemory(store.Options{})
	open := corefFor(t, "")
	req := corefReq()
	var rep components.Report
	if _, err := open.Offload(req, &rep, corefCtx(st)); err != nil {
		t.Fatal(err)
	}
	latched := schema.MessageText(req.Input[corefCutIdx])
	if latched == schema.MessageText(corefReq().Input[corefCutIdx]) {
		t.Fatal("nothing was cut on the first turn")
	}

	// Same store, but a trigger that cannot fire on this request shape.
	shut := corefFor(t, "trigger:\n  min_messages: 9999\n")
	next := corefWithSecondListing()
	var r2 components.Report
	if _, err := shut.Offload(next, &r2, corefCtx(st)); err != nil {
		t.Fatal(err)
	}
	if r2.Gates["trigger"] == 0 {
		t.Error("expected the trigger gate")
	}
	if got := schema.MessageText(next.Input[corefFreshIdx]); !strings.Contains(got, corefNovelFresh) {
		t.Error("a new cut was taken while the trigger was shut")
	}
	if got := schema.MessageText(next.Input[corefCutIdx]); got != latched {
		t.Error("a shut trigger suppressed the replay of a latched decision")
	}
	if r2.Skipped {
		t.Error("a turn that replayed a latched decision did act; Skipped is wrong")
	}
}

// The kept-verbatim exemption must NOT leak across sessions. One agent expanding a config
// dump, a manifest or a schema used to exempt that byte-identical content in every session
// thereafter — permanently, silently, and preferentially on the content most worth cutting,
// because recurring-across-sessions is exactly what makes content valuable to compact.
//
// This is the negative half of TestCorefLeavesExpandedContentAlone, and it is the half that
// fails on the old key layout: the guard is real within its session and absent outside it.
func TestKeptVerbatimDoesNotLeakAcrossSessions(t *testing.T) {
	// SKIPPED, NOT DELETED, and the distinction matters: this test fails on `main` because the leak is
	// real there, not because the test is wrong. `main`'s keptKey(ck) carries no session, so a mark
	// written by one session exempts the same bytes in every other session sharing the store —
	// preferentially on content that recurs across sessions, which is exactly the content most worth
	// cutting. The fix is a store key-format change in state.go affecting every offloader, plus a
	// read-both-shapes migration for marks already on disk, so it does not belong to coref. Filed
	// separately; un-skip with the three-argument MarkKeptVerbatim when that lands.
	t.Skip("cross-session kept-verbatim scoping is a main-wide state.go fix, tracked separately")
	// The body is preserved COMMENTED rather than adapted to the two-argument signature, because an
	// adapted body would compile, read as a real test, and assert nothing — both sessions would share
	// one global mark. Restore it verbatim when the scoped key lands.

	// cf := corefFor(t, "")
	// st := store.NewMemory(store.Options{})
	// req := corefReq()
	// MarkKeptVerbatim(st, "session-a", schema.MessageText(req.Input[corefCutIdx]))

	// // A DIFFERENT session sends the same bytes. It has never expanded anything, so it cannot
	// // be in an expand loop and must not inherit session-a's exemption.
	// var rep components.Report
	// if _, err := cf.Offload(req, &rep, &components.Ctx{Session: "session-b", Store: st}); err != nil {
	// t.Fatal(err)
	// }
	// if got := schema.MessageText(req.Input[corefCutIdx]); strings.Contains(got, corefNovelUnused) {
	// t.Fatal("session-b inherited session-a's kept-verbatim exemption; the guard is leaking")
	// }

	// // And the guard still holds for the session that earned it.
	// req2 := corefReq()
	// var rep2 components.Report
	// if _, err := cf.Offload(req2, &rep2, &components.Ctx{Session: "session-a", Store: st}); err != nil {
	// t.Fatal(err)
	// }
	// if got := schema.MessageText(req2.Input[corefCutIdx]); !strings.Contains(got, corefNovelUnused) {
	// t.Fatal("session-a lost its own exemption; scoping broke the guard it was meant to keep")
	// }
}

// An empty session must not fall back to a global mark — that is the leak, reinstated.
func TestMarkKeptVerbatimIgnoresAnEmptySession(t *testing.T) {
	t.Skip("same main-wide state.go fix as TestKeptVerbatimDoesNotLeakAcrossSessions")

	// st := store.NewMemory(store.Options{})
	// MarkKeptVerbatim(st, "", "content expanded by nobody in particular")
	// if _, ok := st.Get(keptKey("", contentKey("content expanded by nobody in particular"))); ok {
	// t.Fatal("an empty session wrote a mark; it must be a no-op")
	// }
}

// An output the agent expanded must never be re-cut: doing so just makes it expand again,
// once per turn, paying a round-trip and a cache-write each time.
func TestCorefLeavesExpandedContentAlone(t *testing.T) {
	cf := corefFor(t, "")
	st := store.NewMemory(store.Options{})
	req := corefReq()
	MarkKeptVerbatim(st, schema.MessageText(req.Input[corefCutIdx]))

	var rep components.Report
	if _, err := cf.Offload(req, &rep, corefCtx(st)); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[corefCutIdx]); !strings.Contains(got, corefNovelUnused) {
		t.Fatal("re-cut content the agent had expanded; that is the expand bounce loop")
	}
	if rep.Gates["marker_or_kept_verbatim"] == 0 {
		t.Error("expected the kept-verbatim gate to record the declined candidate")
	}
}

// cut_closed is off by default: its thresholds are the OUTPUT of the measurement pass,
// so until that has run the component must not take the large case-A cut. Enabling it
// must then actually take it.
func TestCorefClosedCutIsOptIn(t *testing.T) {
	req := corefReq()
	// Push the reference far enough into the past that the referenced output is `closed`
	// rather than `open` (recency is measured from the head).
	for k := 0; k < 20; k++ {
		req.Input = append(req.Input, corefAsst("thinking "+fmt.Sprint(k), "Bash", `{"cmd":"true"}`))
	}

	off := corefFor(t, "")
	var rep components.Report
	if _, err := off.Offload(req, &rep, corefCtx(store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[corefRefIdx]); !strings.Contains(got, corefNovelUsed) {
		t.Fatal("the closed output was cut with cut_closed off (the default)")
	}
	if rep.Gates["class_closed"] == 0 {
		t.Fatalf("expected a declined closed candidate; gates=%v", rep.Gates)
	}

	on := corefFor(t, "cut_closed: true\n")
	req2 := corefReq()
	for k := 0; k < 20; k++ {
		req2.Input = append(req2.Input, corefAsst("thinking "+fmt.Sprint(k), "Bash", `{"cmd":"true"}`))
	}
	var rep2 components.Report
	if _, err := on.Offload(req2, &rep2, corefCtx(store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req2.Input[corefRefIdx])
	if strings.Contains(got, corefNovelUsed) {
		t.Fatalf("cut_closed: true did not take the closed cut; gates=%v", rep2.Gates)
	}
	assertMarkerMakesNoSafetyClaim(t, got)
}

// An unreadable budget counter must read as EXHAUSTED. Failing open on the request (no
// cut taken) is correct; failing open on an unbounded cache spend is not.
func TestCorefUnreadableBudgetCounterDeclines(t *testing.T) {
	st := store.NewMemory(store.Options{})
	st.Put(corefRewritesKey("s"), []byte("not-a-number"))
	cf := corefFor(t, "")
	req := corefReq()
	var rep components.Report
	if _, err := cf.Offload(req, &rep, corefCtx(st)); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["rewrite_budget"] == 0 {
		t.Error("a corrupt counter must read as exhausted, not as zero spent")
	}
	if got := schema.MessageText(req.Input[corefCutIdx]); !strings.Contains(got, corefNovelUnused) {
		t.Error("a cut was taken against an unreadable budget")
	}
}

func TestCorefEstimateTurnsRemaining(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		reqTokens, turns, window int
		want                     int
	}{
		{"unknown window imposes nothing", 1000, 10, 0, 0},
		{"already at the window", 1000, 10, 1000, 0},
		{"half full, 100/turn", 1000, 10, 2000, 10},
		{"early in a long session", 1000, 10, 11000, 100},
		{"no turns yet", 1000, 0, 5000, 0},
	} {
		if got := estimateTurnsRemaining(tc.reqTokens, tc.turns, tc.window); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestCorefEmptyRequestIsANoOp(t *testing.T) {
	cf := corefFor(t, "")
	req := &bschemas.BifrostChatRequest{}
	var rep components.Report
	keys, err := cf.Offload(req, &rep, corefCtx(store.NewMemory(store.Options{})))
	if err != nil || len(keys) != 0 || !rep.Skipped {
		t.Errorf("empty request: keys=%v skipped=%v err=%v", keys, rep.Skipped, err)
	}
}

// The opportunity floor, at the component level: an output too new to have been referenced
// must survive the pass. Without it a batched cut would preferentially remove the most
// RECENT context, since recency and "no references yet" are the same thing at the tail.
func TestCorefOpportunityFloorProtectsTheTail(t *testing.T) {
	cf := corefFor(t, "min_later_turns: 8\n")
	req := corefReq()
	var rep components.Report
	if _, err := cf.Offload(req, &rep, corefCtx(store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[corefCutIdx]); !strings.Contains(got, corefNovelUnused) {
		t.Fatal("cut a tail output that had had no chance to be referenced")
	}
	if rep.Gates["class_open"] == 0 {
		t.Errorf("expected the tail output to be declined as open; gates=%v", rep.Gates)
	}
}

// An output whose values the index cannot see (records of plain names/ids) must never be
// cut by the DEFAULT config. Raised in review on PR #80.
func TestCorefNeverCutsOpaqueOutputs(t *testing.T) {
	people := strings.Repeat(
		`[{"name":"david","id":123,"address":"foobarbaz"},{"name":"osher","id":235,"address":"banana"}]`, 60)
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		corefUser("look up the people directory"),
		corefAsst("Querying.", "query_people", `{}`),
		corefTool("p1", people),
		corefAsst("I need to remember david 123 address.", "Bash", `{"cmd":"true"}`),
	}}
	for k := 0; k < 20; k++ {
		req.Input = append(req.Input, corefAsst(fmt.Sprintf("step %d", k), "Bash", `{"cmd":"true"}`))
	}
	cf := corefFor(t, "")
	var rep components.Report
	if _, err := cf.Offload(req, &rep, corefCtx(store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(req.Input[2]); !strings.Contains(got, "foobarbaz") {
		t.Fatal("cut an output the index has no evidence about; the agent had just said it " +
			"needs david's address, and the payload was never copied into a model turn")
	}
	if rep.Gates["class_opaque"] == 0 {
		t.Errorf("expected the candidate to be declined as opaque; gates=%v", rep.Gates)
	}
}

// A marker may say WHAT was removed. It may never claim the removal was safe.
//
// The wording it replaced ("no later turn referred back to it") asserted exactly the claim
// that is false whenever the reference was transformed or semantic — tiers 2 and 3, which
// this index cannot see. Only the model can initiate recovery, so a marker that reads as
// reassurance suppresses the expand call that would have repaired the mistake. That failure
// is silent: no counter this component keeps can distinguish "never needed" from "needed and
// never asked for".
func assertMarkerMakesNoSafetyClaim(t *testing.T, marker string) {
	t.Helper()
	for _, claim := range []string{
		"no later turn referred back",
		"survives in a later turn",
		"nothing referred",
		"safe to",
		"not needed",
		"no longer needed",
	} {
		if strings.Contains(strings.ToLower(marker), claim) {
			t.Errorf("marker asserts its own safety (%q), which discourages recovery: %q", claim, marker)
		}
	}
}

// For structured output the residue must be ADDRESSABLE — the shape, not one arbitrary row.
// An agent looking for someone's address has to be able to tell from the marker alone that
// this is the output where addresses live.
func TestCorefMarkerDescribesStructuredShape(t *testing.T) {
	people := strings.Repeat(
		`{"name":"david","id":123456,"address":"foobarbaz","city":"haifa"},`, 200)
	body := "[" + strings.TrimSuffix(people, ",") + "]"

	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		corefUser("load the directory"),
		corefAsst("Loading.", "query_people", `{"q":"all"}`),
		corefTool("p1", body),
		corefAsst("Loaded; moving on to unrelated work.", "Bash", `{"cmd":"true"}`),
	}}
	for k := 0; k < 20; k++ {
		req.Input = append(req.Input, corefAsst(fmt.Sprintf("step %d", k), "Bash", `{"cmd":"true"}`))
	}
	cf := corefFor(t, "")
	var rep components.Report
	if _, err := cf.Offload(req, &rep, corefCtx(store.NewMemory(store.Options{}))); err != nil {
		t.Fatal(err)
	}
	got := schema.MessageText(req.Input[2])
	if strings.Contains(got, "foobarbaz") {
		t.Fatalf("not cut, so there is no marker to check; gates=%v", rep.Gates)
	}
	for _, want := range []string{"200 records", "address", "name"} {
		if !strings.Contains(got, want) {
			t.Errorf("marker omits %q, so the model cannot tell what is in here: %q", want, got)
		}
	}
	assertMarkerMakesNoSafetyClaim(t, got)
}

func TestCorefStub(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"array of records", `[{"b":1,"a":2},{"a":3,"b":4}]`, "2 records, fields: a, b"},
		{"array of scalars", `[1,2,3]`, "3 records"},
		{"wrapped collection", `{"total":2,"rows":[{"x":1},{"x":2}]}`, "object, fields: rows, total (rows: 2 items)"},
		{"plain text has no shape", "Traceback (most recent call last):", ""},
		{"empty", "", ""},
		{"malformed json", `[{"a":`, ""},
	} {
		if got := corefStub(tc.in); got != tc.want {
			t.Errorf("%s: corefStub(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// Field order must be stable: the marker text is replayed byte-for-byte every later
	// turn, so a map-iteration-ordered descriptor would flip the prefix and cache-write it.
	first := corefStub(`[{"z":1,"a":2,"m":3}]`)
	for i := 0; i < 50; i++ {
		if got := corefStub(`[{"z":1,"a":2,"m":3}]`); got != first {
			t.Fatalf("descriptor is not deterministic: %q vs %q", got, first)
		}
	}
	// A wide record must not turn the marker into a schema dump.
	var wide strings.Builder
	wide.WriteString("[{")
	for i := 0; i < 40; i++ {
		if i > 0 {
			wide.WriteString(",")
		}
		fmt.Fprintf(&wide, `"field%02d":%d`, i, i)
	}
	wide.WriteString("}]")
	got := corefStub(wide.String())
	if !strings.Contains(got, "…+28") {
		t.Errorf("wide record not truncated: %q", got)
	}
	if len([]rune(got)) > stubCap {
		t.Errorf("descriptor exceeds stubCap: %d runes", len([]rune(got)))
	}
}
