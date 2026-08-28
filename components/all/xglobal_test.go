package all_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/config"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
	"gopkg.in/yaml.v3"
)

// TestExtractResultCacheHitsAcrossSessions is the headline acceptance criterion for issue
// #28 part C: identical content in a DIFFERENT session must reuse the prior extraction
// instead of paying for it again. Before the global re-key the result cache carried a
// session prefix, so the second session re-derived a result the system already had —
// measured wasteful on 82 of 103 unique contents.
func TestExtractResultCacheHitsAcrossSessions(t *testing.T) {
	// economic_gate: false isolates the CACHE behavior under test from the gate's
	// (separately tested) spending decision.
	off := newComp(t, "extract_llm", "strategy: code\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{}) // one store, as a real proxy has
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	cm := &countingModel{resp: filter}
	pad := strings.Repeat("padding ", 40)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`

	runIn := func(session string) *bschemas.BifrostChatRequest {
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
			userMsg("find the keep records"), toolMsg(body),
		}}
		c := &components.Ctx{Ctx: context.Background(), Session: session, Store: st,
			Model: components.ModelSpec{Static: cm}}
		var rep components.Report
		if _, err := off.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		return req
	}

	req1 := runIn("session-A")
	if cm.calls != 1 {
		t.Fatalf("first session must call the model once, calls=%d", cm.calls)
	}
	out1 := schema.MessageText(req1.Input[1])
	if strings.Contains(out1, "drop this") {
		t.Fatal("first session should have reduced the output")
	}

	// A DIFFERENT session, same content. This is the case the session-scoped key missed.
	req2 := runIn("session-B-completely-different")
	if cm.calls != 1 {
		t.Fatalf("a different session must REUSE the cached extraction (no new model call), calls=%d", cm.calls)
	}
	out2 := schema.MessageText(req2.Input[1])
	if strings.Contains(out2, "drop this") {
		t.Fatal("cross-session reuse must still drop the non-keep record")
	}

	// A third, also free.
	runIn("session-C")
	if cm.calls != 1 {
		t.Fatalf("every later session must reuse, calls=%d", cm.calls)
	}
}

// The gate must actually suppress in a real pipeline run on a cache-aware request with a
// small output — the Terminal-Bench losing case, end to end rather than in unit isolation.
func TestExtractGateSuppressesInPipelineWhenCacheAware(t *testing.T) {
	off := newComp(t, "extract_llm", "strategy: code\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{})
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	cm := &countingModel{resp: filter}
	pad := strings.Repeat("padding ", 40) // ~400 tokens: far below the ~12.7k cached break-even
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`

	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	c := &components.Ctx{Ctx: context.Background(), Session: "s1", Store: st,
		Model: components.ModelSpec{Static: cm}, CacheAware: true, MaxCachedIdx: -1,
		CtxWindow: 1_000_000}
	var rep components.Report
	if _, err := off.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if cm.calls != 0 {
		t.Fatalf("a small output on a cache-aware request must not be worth a call, calls=%d", cm.calls)
	}
	if schema.MessageText(req.Input[1]) != body {
		t.Fatal("a suppressed candidate must be left verbatim (fail open)")
	}
}

// Backward compatibility: an existing config that pins min_tokens must keep working
// unchanged — the smarter trigger is the DEFAULT only when nothing was configured.
func TestExplicitMinTokensConfigStillReduces(t *testing.T) {
	// A pinned min_tokens plus the pre-#28 gate setting reproduces old behavior exactly.
	off := newComp(t, "extract_llm", "strategy: code\nmin_tokens: 1\neconomic_gate: false\nmodel:\n  source: config\n")
	st := store.NewMemory(store.Options{})
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	cm := &countingModel{resp: filter}
	pad := strings.Repeat("padding ", 40)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	// A tiny context window would make the derived trigger decline; an explicit
	// min_tokens must override that.
	c := &components.Ctx{Ctx: context.Background(), Session: "s1", Store: st,
		Model: components.ModelSpec{Static: cm}, CtxWindow: 1_000_000}
	var rep components.Report
	keys, err := off.Offload(req, &rep, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("explicit min_tokens must still reduce (skipped=%v calls=%d)", rep.Skipped, cm.calls)
	}
}

// A CROSS-SESSION global hit must not be spliced into the provider's cached prefix.
// The same-session replay path is safe at any depth because this session already sent
// those compacted bytes — the cached prefix holds the COMPACTED form. A global hit is
// different: THIS session never compacted this message, so the cached prefix holds the
// ORIGINAL, and splicing at depth mutates already-cached content and forces a suffix
// re-write at 11.5x the read price. Same churn #40's tail gate exists to prevent, just
// sourced from a cache rather than a model call.
func TestGlobalCacheHitIsNotSplicedAtDepth(t *testing.T) {
	cfg := "strategy: code\nmin_tokens: 1\nallow_on_caching_backend: true\neconomic_gate: false\n" +
		"model:\n  source: config\ntrigger:\n  min_request_tokens: 1\n"
	st := store.NewMemory(store.Options{})
	filter := "data = json.decode(INPUT)\nOUTPUT = json.encode([r for r in data if \"keep\" in r[\"name\"]])\n"
	pad := strings.Repeat("padding ", 40)
	body := `[{"id":1,"name":"keep this ` + pad + `"},{"id":2,"name":"drop this ` + pad + `"}]`

	// Session A populates the GLOBAL cache from its own tail.
	offA := newComp(t, "extract_llm", cfg)
	cmA := &countingModel{resp: filter}
	reqA := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	var repA components.Report
	if _, err := offA.Offload(reqA, &repA, &components.Ctx{Ctx: context.Background(),
		Session: "A", Store: st, Model: components.ModelSpec{Static: cmA},
		CacheAware: true, MaxCachedIdx: -1, CtxWindow: 200_000}); err != nil {
		t.Fatal(err)
	}
	if schema.MessageText(reqA.Input[1]) == body {
		t.Fatal("session A must compact (fixture no longer exercises the path)")
	}

	// Session B sees the SAME content, but at DEPTH (MaxCachedIdx >= 1). B never compacted
	// it, so B's provider prefix holds the ORIGINAL — the global hit must NOT be applied.
	offB := newComp(t, "extract_llm", cfg)
	cmB := &countingModel{resp: filter}
	reqB := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body), toolMsg("tail"),
	}}
	var repB components.Report
	if _, err := offB.Offload(reqB, &repB, &components.Ctx{Ctx: context.Background(),
		Session: "B", Store: st, Model: components.ModelSpec{Static: cmB},
		CacheAware: true, MaxCachedIdx: 1, CtxWindow: 200_000}); err != nil {
		t.Fatal(err)
	}
	if got := schema.MessageText(reqB.Input[1]); got != body {
		t.Fatalf("a cross-session global hit must leave a cached-prefix message VERBATIM:\n got=%q", got)
	}
	// The depth message must not have been re-derived either: a global miss falls through to
	// the normal tail-gated flow, never to a depth-lifted model call (#40's stance). B's own
	// tail message (Input[2]) may legitimately be compacted, so assert on the DEPTH message
	// rather than on the call count — the marker is what proves Input[1] was left alone.

	// In the TAIL, the same global hit IS applied — and without a model call, which is the
	// whole point of the cross-session cache.
	offC := newComp(t, "extract_llm", cfg)
	cmC := &countingModel{resp: filter}
	reqC := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the keep records"), toolMsg(body),
	}}
	var repC components.Report
	if _, err := offC.Offload(reqC, &repC, &components.Ctx{Ctx: context.Background(),
		Session: "C", Store: st, Model: components.ModelSpec{Static: cmC},
		CacheAware: true, MaxCachedIdx: -1, CtxWindow: 200_000}); err != nil {
		t.Fatal(err)
	}
	if schema.MessageText(reqC.Input[1]) == body {
		t.Fatal("a tail-position global hit must be applied")
	}
	if cmC.calls != 0 {
		t.Fatalf("a global hit must avoid the model call, got %d", cmC.calls)
	}
}

// housellmBlock returns one component's block EXACTLY as the housellm preset ships it, so the
// guards here test the shipped configuration instead of a transcription of it.
func housellmBlock(t *testing.T, name string) string {
	t.Helper()
	cfg, err := config.LoadBytes([]byte("preset: housellm\n"))
	if err != nil {
		t.Fatalf("load housellm preset: %v", err)
	}
	node, ok := cfg.Components[name]
	if !ok {
		t.Fatalf("housellm preset no longer configures %s; this guard is testing nothing", name)
	}
	raw, err := yaml.Marshal(&node)
	if err != nil {
		t.Fatalf("marshal %s block: %v", name, err)
	}
	return string(raw)
}

func housellmExtractLLM(t *testing.T) string { return housellmBlock(t, "extract_llm") }

// TestHousellmSweepActuallyFires is the other half of
// TestDefaultConfigsSpendOnlyOnTheUncachedTail.
//
// Every extraction call this service has ever made was on a turn whose cache had gone, so the sweep's
// min_tokens is the single knob deciding whether the compaction-model pass does anything at all. It was
// 3000, and at 3000 production recorded `below_output_floor` on all 36 sweeping turns and zero
// extractions across 3,437 requests — the component was configured into a no-op while looking fully
// enabled. A candidate of ~1,500 tokens is the size that regression turned away, so that is what this
// asserts on: the preset, not a copy of it, must ask on a turn inside the pre-expiry window.
//
// It drives the sweep through a PREFIX ASKER rather than a Model, because that is the only way the
// component reaches a model at all: it asks the REQUEST's own model over that model's prompt cache. A
// stubbed Model would leave it declining with sweep_no_asker, which is the "configured into a no-op"
// failure this guard exists to catch, one layer down.
//
// Raising the preset's floor above ~1,500 fails this; re-adding allow_on_caching_backend fails the
// warm guard above. The pair pins the economics from both sides.
func TestHousellmSweepActuallyFires(t *testing.T) {
	off := newComp(t, "extract_llm_sweep", housellmBlock(t, "extract_llm_sweep"))
	// ~1,500 tokens of the noise the sweep is meant to remove.
	body := `[`
	for i := 0; i < 120; i++ {
		if i > 0 {
			body += ","
		}
		body += `{"id":` + strconv.Itoa(i) + `,"name":"record ` + strings.Repeat("payload ", 6) + `"}`
	}
	body += `]`
	asker := &stubAsker{reply: `[{"i":0,"needed_by":"none","quote":"","verdict":"drop"}]`, cacheRead: 19595}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("summarize the records"),
	}}
	// ENOUGH CANDIDATES TO CLEAR THE PRESET'S INVENTORY FLOOR, which is part of what this guard pins.
	// A single candidate is the per-output shape the design refutes at 6% live-kept, and the shipped
	// component now declines it rather than asking — so a one-output fixture would assert that the
	// preset does something it is deliberately unwilling to do, and would read as "configured into a
	// no-op" for the wrong reason. Ten is the shipped default, so this fixture is the smallest
	// transcript the preset will actually act on.
	for i := 0; i < 10; i++ {
		req.Input = append(req.Input, toolMsg(body))
	}
	// INSIDE THE PRE-EXPIRY WINDOW: the cache still exists (idle below the TTL) and is within a minute
	// of expiring, which is where the ask can still read it and what it invalidates is nearly
	// worthless. A bare `ephemeral` mark buys the 5-minute TTL this uses.
	c := &components.Ctx{Ctx: context.Background(), Session: "prexp1",
		Store: store.NewMemory(store.Options{}), CacheAware: true, MaxCachedIdx: -1,
		CtxWindow: 1_000_000, ColdCache: false,
		IdleMs: 4 * 60 * 1000, CacheTTLMs: 5 * 60 * 1000, PrefixAsk: asker}
	var rep components.Report
	if _, err := off.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if asker.calls == 0 {
		t.Fatalf("the housellm preset made NO ask in the pre-expiry window with a ~1.5k-token "+
			"candidate — the component is configured into a no-op. gates=%v", rep.Gates)
	}
	// AND IT ACTED ON THE VERDICT. An ask that removes nothing is the same no-op from the operator's
	// side, and it is what a floor regression one layer down would look like.
	if rep.Events["sweep_dropped"] == 0 {
		t.Fatalf("the sweep asked and removed nothing. gates=%v", rep.Gates)
	}
}

// stubAsker is a components.PrefixAsker that answers without a provider, reporting the cache read the
// sweep gates on. A read of zero would make it decline, so a test that wants it to act must say the
// read happened.
type stubAsker struct {
	reply     string
	cacheRead int
	calls     int
}

func (s *stubAsker) Ask(_ context.Context, _, _ string) (string, components.PrefixUsage, error) {
	s.calls++
	return s.reply, components.PrefixUsage{CacheRead: s.cacheRead, Fresh: 40, Output: 60}, nil
}

// TestHousellmDoesNotAttemptTheTailBelowBreakEven pins the floor that makes the warm/tail
// path safe, from the loss side. TestDefaultConfigsSpendOnlyOnTheUncachedTail shows a large
// tail candidate IS worth a call now; this shows a small one is not, and that the preset
// refuses it before spending anything.
//
// The floor is derived. A call costs ~$0.0193 per ACCEPTED result — output-dominated, and
// including the 1-in-5 rejected outright that pays full price for nothing. At the cache-write
// rate plus the corrected amortization (reuses 12 on a long transcript) a saved token is worth
// ~$9.31/MTok, so a call needs ~2,073 saved tokens, which at the observed ~65% reduction needs
// a ~3,190-token candidate. The preset's floor is 3000.
//
// It is NOT 8000, which an earlier revision of this change used: tool outputs on this workload
// top out near 7,399 tokens (see TestContentClassesGateOnExpectedYield) and only 1 of 132
// production candidates reached 8000, so that floor disabled the path rather than bounding it.
//
// The assertion is on below_output_floor specifically because the floor must refuse the
// candidate BEFORE the exploration allowance can spend on it: exploration bypasses the
// arithmetic, and at a 1000 floor every warm call real sessions made was an exploration call,
// for a measured net of -$0.036.
func TestHousellmDoesNotAttemptTheTailBelowBreakEven(t *testing.T) {
	off := newComp(t, "extract_llm", housellmExtractLLM(t))
	// 1,495 tokens of log lines (measured with schema.TextTokens, not estimated — 23 tokens
	// per line, and guessing 12 put an earlier version of this fixture ABOVE the floor it was
	// meant to sit under). Above the old 1,000 floor, below the 3,000 one, and in the tail:
	// this is literally the size that lost money, since the two smallest calls real sessions
	// made were 1,357 and 1,536 tokens, saving 290 and 0 for $0.0133 and $0.0124.
	body := strings.Repeat("2024-01-01 GET /users/42 200 12ms handler=src/api/users.py\n", 65)
	cm := &countingModel{resp: "data = json.decode(INPUT)\nOUTPUT = json.encode(data[:1])\n"}
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		userMsg("find the slow handler"), toolMsg(body),
	}}
	c := &components.Ctx{Ctx: context.Background(), Session: "warm-tail",
		Store: store.NewMemory(store.Options{}), Model: components.ModelSpec{Static: cm},
		CacheAware: true, MaxCachedIdx: 0, CtxWindow: 1_000_000}
	var rep components.Report
	if _, err := off.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	if cm.calls != 0 {
		t.Fatalf("the preset spent a call on a 1,495-token tail candidate; candidates this "+
			"size saved 0-290 tokens for $0.012-0.013 each in real sessions, so the hot "+
			"floor must refuse it. calls=%d gates=%v", cm.calls, rep.Gates)
	}
	if rep.Gates["below_output_floor"] == 0 {
		t.Fatalf("expected below_output_floor to be what refused it — anything else means the "+
			"exploration budget could still spend here. gates=%v", rep.Gates)
	}
}
