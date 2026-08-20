package offload

import (
	"context"
	"testing"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/store"
)

// The full-body ("allow_cached_prefix") path. Reaching the provider's cached prefix costs a
// cache-write, so it is guarded by two gates the tail path does not have: the co-reference
// index as a free eligibility pre-filter, and the S*T > 11.5*W break-even on the batch.
//
// The fixtures reuse coref's request (see corefReq): index 2 is a tool output whose novel
// identifier the NEXT assistant turn quotes (referenced => open), index 5 is a listing whose
// identifier nothing ever uses (unreferenced => spent). prefix_min_later_turns: 0 keeps the
// fixture short — the opportunity floor is coref's concern and is tested there.

func newPrefixComponent(t *testing.T, model components.Model, extraYAML string) *ExtractLLM {
	t.Helper()
	return newCtxGuardComponent(t, model, "prefix_min_later_turns: 0\n"+extraYAML)
}

// cache-aware Ctx whose ENTIRE transcript is already cached, so every candidate is prefix
// work and nothing qualifies as tail.
func prefixCtx(req int) *components.Ctx {
	return &components.Ctx{Ctx: context.Background(), Session: "s",
		Store:      store.NewMemory(store.Options{}),
		CacheAware: true, MaxCachedIdx: req - 1, CtxWindow: 200000}
}

// Default OFF: prefix content is refused exactly as before, and the model is never called.
// This is the regression guard for every workload the published numbers came from.
func TestPrefixReachIsOffByDefault(t *testing.T) {
	m := &silentModel{}
	e := newPrefixComponent(t, m, "")
	req := corefReq()
	var rep components.Report
	if _, err := e.Offload(req, &rep, prefixCtx(len(req.Input))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["cached_prefix"] == 0 {
		t.Fatalf("prefix content must be refused by default; gates=%v", rep.Gates)
	}
	if m.calls != 0 {
		t.Fatalf("no model call may be made for prefix content by default, got %d", m.calls)
	}
}

// With prefix reach ON, an output whose identifiers a later turn carried forward must be
// refused by the INDEX — before any model call. This is the gate that makes the feature
// affordable: the expensive component never pays to look at content a free pass can clear.
func TestPrefixReachSkipsStillReferencedContentWithoutCallingTheModel(t *testing.T) {
	m := &silentModel{}
	e := newPrefixComponent(t, m, "allow_cached_prefix: true\n")
	req := corefReq()
	var rep components.Report
	if _, err := e.Offload(req, &rep, prefixCtx(len(req.Input))); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["prefix_still_referenced"] == 0 {
		t.Fatalf("a referenced prefix output must be gated by the index; gates=%v", rep.Gates)
	}
	if rep.Gates["cached_prefix"] != 0 {
		t.Errorf("with prefix reach on, the blanket cached_prefix gate must not fire; gates=%v", rep.Gates)
	}
}

// The complement: an output nothing referred back to IS eligible, so the model gets
// consulted about how much of it to keep. Index decides WHETHER, model decides HOW MUCH.
func TestPrefixReachConsultsTheModelForSpentContent(t *testing.T) {
	m := &silentModel{}
	e := newPrefixComponent(t, m, "allow_cached_prefix: true\n")
	req := corefReq()
	var rep components.Report
	if _, err := e.Offload(req, &rep, prefixCtx(len(req.Input))); err != nil {
		t.Fatal(err)
	}
	if m.calls == 0 {
		t.Fatalf("an unreferenced prefix output must reach the model; gates=%v", rep.Gates)
	}
}

// The break-even must be able to decline. A window barely above the request leaves no turns
// to amortize over (estimateTurnsRemaining -> ~0), so the rewrite cannot be repaid and the
// batch is dropped — with a reason, not silently.
func TestPrefixBatchDeclinesWhenTheRewriteCannotBeRepaid(t *testing.T) {
	m := &silentModel{}
	e := newPrefixComponent(t, m,
		"allow_cached_prefix: true\nmodel_max_input_tokens: 400000\n")
	req := corefReq()
	c := prefixCtx(len(req.Input))
	c.CtxWindow = 900 // no headroom: T collapses, so 11.5*W can never be recovered
	var rep components.Report
	if _, err := e.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["prefix_rewrite_not_repaid"] == 0 {
		t.Fatalf("an unrepayable prefix rewrite must be declined; gates=%v", rep.Gates)
	}
}

// A failed prefix batch must not suppress TAIL work. The tail costs no cache-write, so it
// was already free and profitable; dropping it because the prefix batch failed would make
// enabling the feature strictly worse than leaving it off.
func TestFailedPrefixBatchDoesNotSuppressTailWork(t *testing.T) {
	m := &silentModel{}
	e := newPrefixComponent(t, m,
		"allow_cached_prefix: true\nmodel_max_input_tokens: 400000\n")
	req := corefWithSecondListing() // index 7 is a fresh unreferenced output
	// Everything except the last two messages is cached, so index 7 is TAIL.
	c := &components.Ctx{Ctx: context.Background(), Session: "s",
		Store:      store.NewMemory(store.Options{}),
		CacheAware: true, MaxCachedIdx: len(req.Input) - 3, CtxWindow: 900}
	var rep components.Report
	if _, err := e.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["prefix_rewrite_not_repaid"] == 0 {
		t.Fatalf("expected the prefix batch to be declined at this window; gates=%v", rep.Gates)
	}
	if m.calls == 0 {
		t.Fatalf("tail work must survive a declined prefix batch; gates=%v", rep.Gates)
	}
}

// prefix_classes decides what the model is even asked about, so the two classes it may name
// are asserted, and the two it may not are REFUSED at construction rather than silently
// widening the pre-filter to "consider everything" — which would destroy the only property
// that makes prefix reach affordable.
func TestPrefixClassesRejectsClassesThatAreNotEvidenceOfSpentContent(t *testing.T) {
	for _, bad := range []string{"open", "opaque"} {
		if _, err := newExtractLLM([]byte("allow_cached_prefix: true\nprefix_classes: [" + bad + "]\n")); err == nil {
			t.Errorf("prefix_classes: [%s] must be refused: it is not evidence of spent content", bad)
		}
	}
	if _, err := newExtractLLM([]byte("prefix_classes: [nonsense]\n")); err == nil {
		t.Error("an unknown prefix_classes entry must be refused, not ignored")
	}
	for _, good := range []string{"unreferenced", "closed", "unreferenced, closed"} {
		if _, err := newExtractLLM([]byte("prefix_classes: [" + good + "]\n")); err != nil {
			t.Errorf("prefix_classes: [%s] must be accepted: %v", good, err)
		}
	}
}

// Narrowing to `closed` alone must actually narrow: the fixture's unreferenced output stops
// being a candidate, so the model is not consulted about it. This is the knob the LOCA replay
// needs, where handing the model `unreferenced` content bought nothing.
func TestPrefixClassesClosedOnlyExcludesUnreferenced(t *testing.T) {
	m := &silentModel{}
	e := newPrefixComponent(t, m,
		"allow_cached_prefix: true\nprefix_classes: [closed]\nmodel_max_input_tokens: 400000\n")
	req := corefReq() // its only spent output is UNREFERENCED, never closed
	var rep components.Report
	if _, err := e.Offload(req, &rep, prefixCtx(len(req.Input))); err != nil {
		t.Fatal(err)
	}
	if m.calls != 0 {
		t.Fatalf("closed-only must not consult the model about unreferenced content, got %d calls", m.calls)
	}
	if rep.Gates["prefix_still_referenced"] == 0 {
		t.Fatalf("the unreferenced output should now be filtered out; gates=%v", rep.Gates)
	}
}
