package offload

import (
	"strings"
	"testing"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// deepReq builds a transcript whose FIRST tool output is inside the already-cached prefix
// (MaxCachedIdx = 0) and whose last message is the uncached tail.
func deepReq(body string) *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body), tool("tail")}}
}

func coldGateCtx(st store.Store, cold bool) *components.Ctx {
	return &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: 0, ColdCache: cold}
}

// run offloads one component over a fresh store and returns the resulting text of the deep
// message plus the gates the component reported.
func run(t *testing.T, comp components.Offload, body string, cold bool) (string, map[string]int) {
	t.Helper()
	req := deepReq(body)
	var rep components.Report
	if _, err := comp.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), cold)); err != nil {
		t.Fatal(err)
	}
	return schema.MessageText(req.Input[0]), rep.Gates
}

// coldComponents is the three age/supersession offloaders that accept cold_cache, each with
// a fixture its own trigger recognizes.
func coldComponents(t *testing.T, optIn string) []struct {
	name string
	comp components.Offload
	body string
} {
	t.Helper()
	mk := func(name, yaml string) components.Offload {
		var (
			c   components.Component
			err error
		)
		switch name {
		case "mask":
			c, err = newMask([]byte(yaml))
		case "failed_run":
			c, err = newFailedRun([]byte(yaml))
		case "collapse":
			c, err = newCollapse([]byte(yaml))
		}
		if err != nil {
			t.Fatal(err)
		}
		return c.(components.Offload)
	}
	// A failed pytest run, then a later run, so failed_run sees a superseded failure.
	return []struct {
		name string
		comp components.Offload
		body string
	}{
		{"mask", mk("mask", "keep_recent: 1\nmin_tokens: 5\n"+optIn), strings.Repeat("verbose tool output line\n", 30)},
		{"failed_run", mk("failed_run", "min_tokens: 5\n"+optIn),
			"=== FAILURES ===\n" + strings.Repeat("assertion detail line\n", 30) + "1 failed, 2 passed in 3.2s\n"},
		{"collapse", mk("collapse", "max_tokens: 10\nhead_lines: 2\ntail_lines: 2\n"+optIn),
			strings.Repeat("verbose tool output line\n", 30)},
	}
}

// failed_run needs a SECOND, later run in the tail to have anything to supersede.
func failedRunReq(body string) *bschemas.BifrostChatRequest {
	return &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
		tool(body), tool("=== FAILURES ===\n" + strings.Repeat("later run line\n", 30) + "1 failed, 2 passed in 4.0s\n"),
	}}
}

// The regression that matters: on a WARM turn the cold_cache setting must change nothing at
// all. Same store, same messages, byte-identical output — whichever way it is set.
func TestColdCacheOffIsByteIdentical(t *testing.T) {
	for i, off := range coldComponents(t, "cold_cache: false\n") {
		on := coldComponents(t, "cold_cache: true\n")[i]
		body := off.body
		if off.name == "failed_run" {
			// The deep-message helper needs the paired later run for this component.
			reqOff, reqOn := failedRunReq(body), failedRunReq(body)
			for _, pair := range []struct {
				req  *bschemas.BifrostChatRequest
				comp components.Offload
			}{{reqOff, off.comp}, {reqOn, on.comp}} {
				var rep components.Report
				if _, err := pair.comp.Offload(pair.req, &rep, coldGateCtx(store.NewMemory(store.Options{}), false)); err != nil {
					t.Fatal(err)
				}
			}
			if schema.MessageText(reqOff.Input[0]) != schema.MessageText(reqOn.Input[0]) {
				t.Fatalf("%s: cold_cache changed a WARM turn", off.name)
			}
			continue
		}
		gotOff, _ := run(t, off.comp, body, false)
		gotOn, _ := run(t, on.comp, body, false)
		if gotOff != gotOn {
			t.Fatalf("%s: cold_cache changed a WARM turn:\n off %q\n on  %q", off.name, gotOff, gotOn)
		}
		if gotOff != body {
			t.Fatalf("%s: a never-frozen message in the cached prefix must stay verbatim", off.name)
		}
	}
}

// The escape hatch. `cold_cache: false` must restore the tail restriction on a cold turn:
// the lift is per component and an operator who wants none of it gets none. Without this
// the *bool in the config would be indistinguishable from a plain bool that ignores the
// setting, which is exactly the bug a defaults flip invites.
func TestColdCacheFalseKeepsTheTailGateOnAColdTurn(t *testing.T) {
	for _, cc := range coldComponents(t, "cold_cache: false\n") {
		if cc.name == "failed_run" {
			req := failedRunReq(cc.body)
			var rep components.Report
			if _, err := cc.comp.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), true)); err != nil {
				t.Fatal(err)
			}
			if schema.MessageText(req.Input[0]) != cc.body {
				t.Fatal("failed_run acted at depth on a cold turn without cold_cache")
			}
			if rep.Gates["cached_prefix"] == 0 {
				t.Fatal("failed_run must report why it declined")
			}
			continue
		}
		got, gates := run(t, cc.comp, cc.body, true)
		if got != cc.body {
			t.Fatalf("%s acted at depth on a cold turn without cold_cache", cc.name)
		}
		if gates["cached_prefix"] == 0 {
			t.Fatalf("%s must report cached_prefix when it declines, got %v", cc.name, gates)
		}
	}
}

// With the opt-in ON and the cache provably cold, depth stops mattering. Runs the empty
// config too, because that is the DEFAULT since 2026-08 and the whole point of the change:
// on `ttl_expiry` turns production was freezing 90.8% of the context (38.4M of 42.3M
// tokens) to protect a prompt cache that had already expired.
func TestColdCacheLiftsDepthWhenOptedIn(t *testing.T) {
	for _, optIn := range []string{"cold_cache: true\n", ""} {
		t.Run("optin="+optIn, func(t *testing.T) { coldLiftsDepth(t, optIn) })
	}
}

func coldLiftsDepth(t *testing.T, optIn string) {
	for _, cc := range coldComponents(t, optIn) {
		if cc.name == "failed_run" {
			req := failedRunReq(cc.body)
			var rep components.Report
			if _, err := cc.comp.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), true)); err != nil {
				t.Fatal(err)
			}
			if schema.MessageText(req.Input[0]) == cc.body {
				t.Fatal("failed_run must collapse a superseded failure at depth on a cold turn")
			}
			continue
		}
		got, _ := run(t, cc.comp, cc.body, true)
		if got == cc.body {
			t.Fatalf("%s must act at depth on a cold turn when opted in", cc.name)
		}
	}
}

// The frozen-prefix invariant, across the boundary that matters: a decision taken at depth
// on a COLD turn must be replayed byte-identically on every later WARM turn, or the very
// next request flips it back to full inside the provider's fresh cache.
func TestColdDecisionStaysFrozenOnLaterWarmTurns(t *testing.T) {
	for _, optIn := range []string{"cold_cache: true\n", ""} {
		t.Run("optin="+optIn, func(t *testing.T) { coldFrozenOnWarm(t, optIn) })
	}
}

func coldFrozenOnWarm(t *testing.T, optIn string) {
	for _, cc := range coldComponents(t, optIn) {
		if cc.name == "failed_run" {
			continue // covered by the pair-shaped case below
		}
		st := store.NewMemory(store.Options{})
		var first string
		for turn := 0; turn < 20; turn++ {
			req := deepReq(cc.body)
			var rep components.Report
			// Only turn 0 is cold; every later turn is warm with the message deep in the prefix.
			if _, err := cc.comp.Offload(req, &rep, coldGateCtx(st, turn == 0)); err != nil {
				t.Fatal(err)
			}
			got := schema.MessageText(req.Input[0])
			if turn == 0 {
				if got == cc.body {
					t.Fatalf("%s: cold turn must compact at depth", cc.name)
				}
				first = got
			}
			if got != first {
				t.Fatalf("%s: turn %d flipped representation after a cold decision (cache-destructive):\n want %q\n got  %q",
					cc.name, turn, first, got)
			}
		}
	}
}

// collapse used to carry no depth restriction at all — it rewrote the whole transcript on
// every turn, contradicting the contract in components/component.go. A NEW collapse must
// now stay in the tail, and the freeze must keep the decision alive at depth afterwards.
func TestCollapseRespectsTailOnlyAndFreezes(t *testing.T) {
	built, err := newCollapse([]byte("max_tokens: 10\nhead_lines: 2\ntail_lines: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	comp := built.(components.Offload)
	body := strings.Repeat("verbose tool output line\n", 30)
	st := store.NewMemory(store.Options{})

	// Turn 0: nothing cached yet, so the output is in the tail and gets collapsed.
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body)}}
	var rep components.Report
	if _, err := comp.Offload(req, &rep, &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: -1}); err != nil {
		t.Fatal(err)
	}
	collapsed := schema.MessageText(req.Input[0])
	if collapsed == body {
		t.Fatal("turn 0 must collapse an oversized output in the tail")
	}
	// Later turns: the agent re-sends the original at depth. The freeze must replay the
	// same bytes, and a message that was never collapsed must stay verbatim at depth.
	for turn := 1; turn < 10; turn++ {
		req := deepReq(body)
		var rep components.Report
		if _, err := comp.Offload(req, &rep, coldGateCtx(st, false)); err != nil {
			t.Fatal(err)
		}
		if got := schema.MessageText(req.Input[0]); got != collapsed {
			t.Fatalf("turn %d: frozen collapse not replayed at depth\n want %q\n got  %q", turn, collapsed, got)
		}
	}
	fresh := strings.Repeat("a different oversized output line\n", 30)
	req = deepReq(fresh)
	rep = components.Report{}
	if _, err := comp.Offload(req, &rep, coldGateCtx(store.NewMemory(store.Options{}), false)); err != nil {
		t.Fatal(err)
	}
	if schema.MessageText(req.Input[0]) != fresh {
		t.Fatal("a never-collapsed output inside the cached prefix must stay verbatim")
	}
	if rep.Gates["cached_prefix"] == 0 {
		t.Fatalf("collapse must report cached_prefix, got %v", rep.Gates)
	}
}
