package offload

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"

	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/internal/extract"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// rideAsker answers the triage ask by label from a script and records the role it was sent under.
type rideAsker struct {
	fakeAsker
	script map[int]string // label -> JSON object tail, e.g. `"action":"park"`
	role   atomic.Value
}

func (r *rideAsker) AskAs(ctx context.Context, session, ask, role string) (string, components.PrefixUsage, error) {
	r.role.Store(role)
	parts := []string{}
	for _, l := range askLabelRe.FindAllStringSubmatch(ask, -1) {
		n, _ := strconv.Atoi(l[1])
		a, ok := r.script[n]
		if !ok {
			a = `"action":"keep"`
		}
		parts = append(parts, `{"label":`+l[1]+`,`+a+`}`)
	}
	r.fakeAsker.reply = "[" + strings.Join(parts, ",") + "]"
	return r.fakeAsker.Ask(ctx, session, ask)
}

func (r *rideAsker) sentRole() string { s, _ := r.role.Load().(string); return s }

func newRide(t *testing.T, yaml string) *ExtractRide {
	t.Helper()
	c, err := newExtractRide([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return c.(*ExtractRide)
}

// rideReq: a user turn, then n big tool outputs each followed by an assistant turn.
func rideReq(n int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("investigate the audit logs")}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input,
			toolResultMsgWithID("toolu_ride_"+strconv.Itoa(i),
				"header "+strconv.Itoa(i)+"\n"+strings.Repeat("noise record "+strconv.Itoa(i)+" of the audit log\n", 400)+
					"ERROR payments timeout at gateway.go:"+strconv.Itoa(100+i)+"\n"),
			assistantMsg("noted "+strconv.Itoa(i)))
	}
	return req
}

func rideCtx(session string, asker components.PrefixAsker, st store.Store) *components.Ctx {
	c := preExpiryCtx(session, asker, st)
	c.IdleMs = 10 * 1000 // warm: only the size / pending triggers can fire
	return c
}

func TestRideAppliesExtractAndParkAndKeeps(t *testing.T) {
	asker := &rideAsker{script: map[int]string{
		0: `"action":"park"`,
		1: `"action":"extract","extract":"SUMMARY: noise dropped\nERROR payments timeout at gateway.go:101"`,
		3: `"action":"extract","extract":"an invented line that is not in the output at all"`,
	}}
	asker.cacheRead = 30000
	e := newRide(t, "econ: false\ninstruction_role: user\n")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	req := rideReq(4)
	orig := make([]string, len(req.Input))
	for i := range req.Input {
		orig[i] = schema.MessageText(req.Input[i])
	}
	rep := &components.Report{Component: "extract_llm_ride"}
	if _, err := e.Offload(req, rep, rideCtx("ride-a", asker, st)); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("one batched ask expected, got %d (gates %v)", n, rep.Gates)
	}
	got := func(i int) string { return schema.MessageText(req.Input[i]) }
	if !strings.Contains(got(1), "context-guru removed") || !strings.Contains(got(1), "<<cg:") {
		t.Errorf("park must leave a descriptor and a marker, got %q", got(1))
	}
	if !strings.Contains(got(3), "gateway.go:101") || !strings.Contains(got(3), "<<cg:") {
		t.Errorf("extract must leave the extract and a marker, got %q", got(3))
	}
	if got(5) != orig[5] {
		t.Error("a keep verdict must leave the output verbatim")
	}
	if got(7) != orig[7] || rep.Gates["ride_extract_ungrounded"] != 1 {
		t.Errorf("an ungrounded extract must be refused (gates %v)", rep.Gates)
	}
	if len(rep.Calls) != 1 {
		t.Fatalf("one call record expected, got %d", len(rep.Calls))
	}
	rc := rep.Calls[0]
	if rc.CacheRead != 30000 || rc.CacheWrite != 0 || rc.PromptTokens != 40 || rc.CompletionTokens != 90 ||
		rc.Strategy != "ride_user" || !rc.Accepted || rc.SavedTokens <= 0 ||
		!strings.Contains(rc.GateReason, "trigger=size+pending") || !strings.Contains(rc.GateReason, "instr_tokens=") {
		t.Errorf("call record incomplete: %+v", rc)
	}
	if asker.sentRole() != "user" {
		t.Errorf("an explicit user role must send a user turn, got %q", asker.sentRole())
	}
}

func TestRideDecisionsAreFrozenAndReplayedByteIdentically(t *testing.T) {
	asker := &rideAsker{script: map[int]string{0: `"action":"park"`, 1: `"action":"extract","extract":"ERROR payments timeout at gateway.go:101"`}}
	asker.cacheRead = 30000
	e := newRide(t, "econ: false\n")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	first := rideReq(4)
	if _, err := e.Offload(first, &components.Report{Component: "extract_llm_ride"}, rideCtx("ride-b", asker, st)); err != nil {
		t.Fatal(err)
	}
	// A later turn: the client resends the ORIGINAL history plus more. No new ask, same bytes.
	second := rideReq(4)
	second.Input = append(second.Input, toolResultMsgWithID("toolu_new", "small"), assistantMsg("ok"))
	rep := &components.Report{Component: "extract_llm_ride"}
	if _, err := e.Offload(second, rep, rideCtx("ride-b", asker, st)); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Fatalf("the later turn must replay, not re-ask; asks = %d", n)
	}
	for i := range first.Input {
		if schema.MessageText(first.Input[i]) != schema.MessageText(second.Input[i]) {
			t.Fatalf("message %d differs between turns; the prefix would break the main cache", i)
		}
	}
	if rep.Replays != 2 {
		t.Errorf("expected 2 replays, got %v", rep.Replays)
	}
}

func TestRideSystemRoleIsHonouredWhenAskedFor(t *testing.T) {
	asker := &rideAsker{}
	asker.cacheRead = 30000
	e := newRide(t, "econ: false\ninstruction_role: system\n")
	rep := &components.Report{Component: "extract_llm_ride"}
	if _, err := e.Offload(rideReq(4), rep, rideCtx("ride-c", asker, store.NewMemory(store.Options{MaxEntries: 400}))); err != nil {
		t.Fatal(err)
	}
	if asker.sentRole() != "system" || rep.Calls[0].Strategy != "ride_system" {
		t.Errorf("role = %q, strategy = %q", asker.sentRole(), rep.Calls[0].Strategy)
	}
}

func TestRideDiscardsACallThatDidNotRideTheCache(t *testing.T) {
	asker := &rideAsker{script: map[int]string{0: `"action":"park"`}}
	asker.cacheRead, asker.cacheWrite = 0, 30000
	e := newRide(t, "econ: false\n")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	req := rideReq(4)
	rep := &components.Report{Component: "extract_llm_ride"}
	if _, err := e.Offload(req, rep, rideCtx("ride-d", asker, st)); err != nil {
		t.Fatal(err)
	}
	if rep.Gates["ride_not_cache_read"] != 1 || rep.Calls[0].Accepted || rep.Calls[0].CacheWrite != 30000 {
		t.Errorf("a non-riding call must be recorded, paid for and discarded: gates %v call %+v", rep.Gates, rep.Calls)
	}
	if strings.Contains(schema.MessageText(req.Input[1]), "<<cg:") {
		t.Error("an answer from a call that did not ride the cache must not be applied")
	}
	// Backed off: the next turns do not ask again.
	for i := 0; i < 5; i++ {
		e.Offload(rideReq(4), &components.Report{Component: "extract_llm_ride"}, rideCtx("ride-d", asker, st))
	}
	if n := atomic.LoadInt64(&asker.calls); n != 1 {
		t.Errorf("expected the failed ride to back the session off, asks = %d", n)
	}
}

func TestRideFiresOnlyOnATriggerAndHonoursTheCooldown(t *testing.T) {
	asker := &rideAsker{}
	asker.cacheRead = 30000
	e := newRide(t, "econ: false\ntrigger_size_tokens: 1000000\ntrigger_pending: 99\ntrigger_pre_expiry: false\n")
	rep := &components.Report{Component: "extract_llm_ride"}
	if _, err := e.Offload(rideReq(4), rep, rideCtx("ride-e", asker, store.NewMemory(store.Options{MaxEntries: 400}))); err != nil {
		t.Fatal(err)
	}
	if asker.calls != 0 || rep.Gates["ride_no_trigger"] != 1 {
		t.Errorf("no trigger configured to fire: asks %d gates %v", asker.calls, rep.Gates)
	}
	// fill band: a 1M window and ~16k tokens is 1.6% full
	e = newRide(t, "econ: false\ntrigger_size_tokens: 1000000\ntrigger_pending: 99\ntrigger_fill_min: 0.01\ntrigger_fill_max: 0.5\ntrigger_pre_expiry: false\n")
	rep = &components.Report{Component: "extract_llm_ride"}
	if _, err := e.Offload(rideReq(4), rep, rideCtx("ride-e2", asker, store.NewMemory(store.Options{MaxEntries: 400}))); err != nil {
		t.Fatal(err)
	}
	if asker.calls != 1 || !strings.Contains(rep.Calls[0].GateReason, "trigger=fill") {
		t.Errorf("fill band should fire: asks %d gates %v", asker.calls, rep.Gates)
	}
}

func TestRideOffersOnlyWhatThePrefixHolds(t *testing.T) {
	asker := &rideAsker{}
	asker.cacheRead = 30000
	e := newRide(t, "econ: false\nmin_inventory: 1\n")
	c := rideCtx("ride-f", asker, store.NewMemory(store.Options{MaxEntries: 400}))
	c.MaxCachedIdx = 4 // outputs at 5 and 7 are newer than the stored body
	req := rideReq(4)
	rep := &components.Report{Component: "extract_llm_ride"}
	e.Offload(req, rep, c)
	if rep.Gates["ride_candidate_not_in_prefix"] != 2 {
		t.Errorf("outputs past the cached boundary are invisible to the ask: %v", rep.Gates)
	}
	if strings.Count(asker.ask(), "tool_use id") != 2 {
		t.Errorf("only the two visible outputs belong in the inventory:\n%s", asker.ask())
	}
}

func TestRideEconDeclinesAnAskThatCannotRepayItself(t *testing.T) {
	asker := &rideAsker{script: map[int]string{0: `"action":"park"`}}
	asker.cacheRead = 30000
	req := rideReq(4)
	for _, tc := range []struct {
		name, yaml string
		asked      bool
	}{
		{"one-turn horizon", "econ: true\nhorizon_turns: 1\nmin_inventory: 1\n", false},
		{"thirty-turn horizon", "econ: true\nhorizon_turns: 30\nmin_inventory: 1\n", true},
	} {
		asker.calls = 0
		rep := &components.Report{Component: "extract_llm_ride"}
		c := rideCtx("ride-g-"+tc.name, asker, store.NewMemory(store.Options{MaxEntries: 400}))
		c.MaxCachedIdx = len(req.Input) - 1
		newRide(t, tc.yaml).Offload(rideReq(4), rep, c)
		if (asker.calls == 1) != tc.asked || (!tc.asked && rep.Gates["ride_ask_not_repaid"] != 1) {
			t.Errorf("%s: asks %d gates %v", tc.name, asker.calls, rep.Gates)
		}
	}
	_ = req
}

func TestRideConfigRejectsBadRole(t *testing.T) {
	if _, err := newExtractRide([]byte("instruction_role: developer\n")); err == nil {
		t.Fatal("unknown role must be refused")
	}
	var _ = extract.RideKeep
}

func newRideForGate(t *testing.T) *ExtractRide { return newRide(t, "econ: false\n") }

func TestRideAutoRoleIsSystemOnlyForMeasuredModels(t *testing.T) {
	e := newRide(t, "")
	for model, want := range map[string]string{
		"aws/claude-opus-5": "system", "claude-sonnet-5-5": "system", "claude-haiku-4-5": "user", "gpt-5.5": "user", "": "user",
	} {
		if got := e.resolveRole(model); got != want {
			t.Errorf("auto role for %q = %q, want %q", model, got, want)
		}
	}
	if newRide(t, "instruction_role: user\n").resolveRole("claude-opus-5") != "user" {
		t.Error("an explicit user role must win over the allow-list")
	}
}

func coldCtx(session string, asker components.PrefixAsker, st store.Store, cold bool) *components.Ctx {
	c := rideCtx(session, asker, st)
	c.MaxCachedIdx = -1
	if cold {
		c.ColdCache, c.IdleMs = true, 10*60*1000
	}
	return c
}

func TestRideColdModeHoldsThenAppliesOnAColdRequestWithoutAsking(t *testing.T) {
	asker := &rideAsker{script: map[int]string{0: `"action":"park"`, 1: `"action":"extract","extract":"ERROR payments timeout at gateway.go:101"`}}
	asker.cacheRead = 30000
	// early_apply off: only a rewriting request may apply.
	e := newRide(t, "apply_mode: cold\nearly_apply: false\nmin_inventory: 2\n")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	warm := rideReq(4)
	orig := schema.MessageText(warm.Input[1])
	rep := &components.Report{Component: "extract_llm_ride"}
	if _, err := e.Offload(warm, rep, coldCtx("cold-a", asker, st, false)); err != nil {
		t.Fatal(err)
	}
	if asker.calls != 1 || schema.MessageText(warm.Input[1]) != orig || rep.Gates["ride_held_waiting"] == 0 {
		t.Fatalf("a warm ask must be held, not applied: asks %d gates %v", asker.calls, rep.Gates)
	}
	if len(rep.Calls) != 1 || rep.Calls[0].CacheRead != 30000 || !strings.Contains(rep.Calls[0].Rejection, "held") {
		t.Errorf("the held ask must still be recorded in full: %+v", rep.Calls)
	}
	// a later warm request: still held, no new ask for the same outputs
	w2 := rideReq(4)
	e.Offload(w2, &components.Report{Component: "extract_llm_ride"}, coldCtx("cold-a", asker, st, false))
	if schema.MessageText(w2.Input[1]) != orig {
		t.Error("still warm: nothing may be applied")
	}
	// the cold request applies the stored decisions and makes no call
	cold := rideReq(4)
	rep = &components.Report{Component: "extract_llm_ride"}
	callsBefore := asker.calls
	e.Offload(cold, rep, coldCtx("cold-a", asker, st, true))
	if asker.calls != callsBefore || rep.Events["ride_applied_cold"] != 2 ||
		!strings.Contains(schema.MessageText(cold.Input[1]), "<<cg:") || !strings.Contains(schema.MessageText(cold.Input[3]), "gateway.go:101") {
		t.Fatalf("cold request must apply both held decisions without asking: calls %d events %v", asker.calls, rep.Events)
	}
	// and afterwards it is an ordinary frozen replay, byte for byte
	next := rideReq(4)
	e.Offload(next, &components.Report{Component: "extract_llm_ride"}, coldCtx("cold-a", asker, st, false))
	for i := range cold.Input {
		if schema.MessageText(cold.Input[i]) != schema.MessageText(next.Input[i]) {
			t.Fatalf("message %d differs after the cold apply", i)
		}
	}
}

func TestRideColdModeAppliesEarlyOnlyWhenTheRewriteIsRepaid(t *testing.T) {
	for _, tc := range []struct {
		yaml  string
		early bool
	}{
		{"apply_mode: cold\nmin_inventory: 2\nhorizon_turns: 1\neconl: x\n", false},
		{"apply_mode: cold\nmin_inventory: 2\nhorizon_turns: 30\n", true},
	} {
		tc.yaml = strings.Replace(tc.yaml, "econl: x\n", "econ: false\n", 1)
		asker := &rideAsker{script: map[int]string{0: `"action":"park"`, 1: `"action":"park"`, 2: `"action":"park"`}}
		asker.cacheRead = 30000
		req := rideReq(4)
		rep := &components.Report{Component: "extract_llm_ride"}
		newRide(t, tc.yaml).Offload(req, rep, coldCtx("cold-b-"+tc.yaml, asker, store.NewMemory(store.Options{MaxEntries: 400}), false))
		applied := strings.Contains(schema.MessageText(req.Input[1]), "<<cg:")
		if applied != tc.early {
			t.Errorf("%q: early apply = %v, want %v (gates %v)", tc.yaml, applied, tc.early, rep.Gates)
		}
	}
}

func TestRideColdModeAppliesWhenTheClientCompacts(t *testing.T) {
	asker := &rideAsker{script: map[int]string{0: `"action":"park"`}}
	asker.cacheRead = 30000
	e := newRide(t, "apply_mode: cold\nearly_apply: false\nmin_inventory: 2\n")
	st := store.NewMemory(store.Options{MaxEntries: 400})
	e.Offload(rideReq(5), &components.Report{Component: "extract_llm_ride"}, coldCtx("cold-c", asker, st, false))
	short := rideReq(4) // fewer messages than the previous request of this thread
	rep := &components.Report{Component: "extract_llm_ride"}
	e.Offload(short, rep, coldCtx("cold-c", asker, st, false))
	if rep.Events["ride_applied_cold"] == 0 {
		t.Errorf("a shorter transcript is a rewrite: the held decisions must apply (gates %v events %v)", rep.Gates, rep.Events)
	}
}

func TestRideCalibratedHorizon(t *testing.T) {
	e := newRide(t, "calibrated_horizon: true\n")
	for turns, want := range map[int]int{2: 21, 10: 17, 20: 12, 40: 11} {
		if got := e.horizonFor(rideReq(turns)); got != want {
			t.Errorf("horizon at %d turns = %d, want %d", turns, got, want)
		}
	}
	if newRide(t, "horizon_turns: 7\n").horizonFor(rideReq(3)) != 7 {
		t.Error("an explicit horizon is used as given")
	}
}

// blockAssistant is an assistant turn whose content is a block array, the shape a tool_use turn has on the
// wire and the only shape that can carry a cache_control mark.
func blockAssistant(text string) bschemas.ChatMessage {
	return bschemas.ChatMessage{Role: bschemas.ChatMessageRoleAssistant,
		Content: &bschemas.ChatMessageContent{ContentBlocks: []bschemas.ChatContentBlock{{Type: bschemas.ChatContentBlockTypeText, Text: &text}}}}
}

// anchoredReq is the real agent shape: user, then (assistant tool_use turn, tool result) pairs.
func anchoredReq(n int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("investigate the audit logs")}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input, blockAssistant("step "+strconv.Itoa(i)),
			toolResultMsgWithID("toolu_anchor_"+strconv.Itoa(i),
				"header "+strconv.Itoa(i)+"\n"+strings.Repeat("noise record "+strconv.Itoa(i)+" of the audit log\n", 400)+
					"ERROR payments timeout at gateway.go:"+strconv.Itoa(100+i)+"\n"))
	}
	req.Input = append(req.Input, blockAssistant("done"))
	return req
}

func TestRideAnchorMarksTheAssistantTurnBeforeTheFirstEdit(t *testing.T) {
	cfg := "apply_mode: cold\nanchor: true\nmin_inventory: 2\nhorizon_turns: 30\neconomic: x\n"
	cfg = strings.Replace(cfg, "economic: x\n", "econ: false\n", 1)
	asker := &rideAsker{script: map[int]string{0: `"action":"park"`, 1: `"action":"park"`, 2: `"action":"park"`}}
	asker.cacheRead = 30000
	marked := func(req *bschemas.BifrostChatRequest) []int {
		var at []int
		for i := range req.Input {
			if req.Input[i].Content == nil {
				continue
			}
			for _, b := range req.Input[i].Content.ContentBlocks {
				if b.CacheControl != nil {
					at = append(at, i)
				}
			}
		}
		return at
	}
	req := anchoredReq(4)
	rep := &components.Report{Component: "extract_llm_ride"}
	st0 := store.NewMemory(store.Options{MaxEntries: 400})
	c := coldCtx("anchor-a", asker, st0, false)
	st0.Put(rideEndsKey(c), []byte("[0,2,4,6]")) // earlier requests ended at these messages
	newRide(t, cfg).Offload(req, rep, c)
	first := -1
	for i := range req.Input {
		if strings.Contains(schema.MessageText(req.Input[i]), "<<cg:") {
			first = i
			break
		}
	}
	if first < 0 || rep.Events["ride_anchor_placed"] != 1 || len(marked(req)) != 1 || marked(req)[0] != first-1 {
		t.Fatalf("want one breakpoint on the assistant turn before message %d, got %v (events %v gates %v)", first, marked(req), rep.Events, rep.Gates)
	}
	// no free slot: the client already uses four
	req = anchoredReq(4)
	st1 := store.NewMemory(store.Options{MaxEntries: 400})
	c = coldCtx("anchor-b", asker, st1, false)
	st1.Put(rideEndsKey(c), []byte("[0,2,4,6]"))
	c.ExistingBreakpoints = 4
	rep = &components.Report{Component: "extract_llm_ride"}
	newRide(t, cfg).Offload(req, rep, c)
	if len(marked(req)) != 0 || rep.Events["ride_anchor_placed"] != 0 {
		t.Errorf("a fifth breakpoint is a 400: %v", marked(req))
	}
	// the entry before the edit is stale when an earlier apply happened after it
	req = anchoredReq(4)
	st := store.NewMemory(store.Options{MaxEntries: 400})
	c = coldCtx("anchor-c", asker, st, false)
	st.Put(rideApplyEndKey(c), []byte("99"))
	st.Put(rideEndsKey(c), []byte("[0,2,4,6]"))
	rep = &components.Report{Component: "extract_llm_ride"}
	newRide(t, cfg).Offload(req, rep, c)
	if len(marked(req)) != 0 {
		t.Errorf("an anchor on a stale entry must not be placed: %v", marked(req))
	}
}

func TestRideAnchorNeedsAnEntryExactlyThere(t *testing.T) {
	asker := &rideAsker{script: map[int]string{0: `"action":"park"`, 1: `"action":"park"`}}
	asker.cacheRead = 30000
	req := anchoredReq(4)
	c := coldCtx("anchor-e", asker, store.NewMemory(store.Options{MaxEntries: 400}), false)
	e := newRide(t, "apply_mode: cold\nanchor: true\nmin_inventory: 2\nhorizon_turns: 30\neconn: x\n"[0:0]+"apply_mode: cold\nanchor: true\nmin_inventory: 2\nhorizon_turns: 30\n")
	rep := &components.Report{Component: "extract_llm_ride"}
	e.Offload(req, rep, c) // first request of the thread: no earlier request ended anywhere
	if rep.Events["ride_anchor_placed"] != 0 {
		t.Error("no earlier request ended before the first edit, so there is no entry to read: no anchor")
	}
}

func TestRideColdModeAppliesWhenTheToolSetOrModelChanges(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mod   func(c *components.Ctx)
		apply bool
	}{
		{"tool set changed", func(c *components.Ctx) { c.ToolsHash = "bbb" }, true},
		{"model changed", func(c *components.Ctx) { c.ModelName = "claude-opus-5" }, true},
		{"nothing changed", func(c *components.Ctx) {}, false},
	} {
		asker := &rideAsker{script: map[int]string{0: `"action":"park"`}}
		asker.cacheRead = 30000
		e := newRide(t, "apply_mode: cold\nearly_apply: false\nmin_inventory: 2\n")
		st := store.NewMemory(store.Options{MaxEntries: 400})
		c1 := coldCtx("env-"+tc.name, asker, st, false)
		c1.ToolsHash = "aaa"
		e.Offload(rideReq(4), &components.Report{Component: "extract_llm_ride"}, c1)
		c2 := coldCtx("env-"+tc.name, asker, st, false)
		c2.ToolsHash = "aaa"
		tc.mod(c2)
		rep := &components.Report{Component: "extract_llm_ride"}
		e.Offload(rideReq(4), rep, c2)
		if (rep.Events["ride_applied_cold"] > 0) != tc.apply {
			t.Errorf("%s: applied cold = %d, want apply=%v (gates %v)", tc.name, rep.Events["ride_applied_cold"], tc.apply, rep.Gates)
		}
	}
}
