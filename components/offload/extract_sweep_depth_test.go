package offload

import (
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

// toolResultMsgWithID is toolResultMsg carrying the wire's own tool-call id, which apply.normalize
// sets on every synthetic tool message it lifts out of an Anthropic tool_result block. The plain
// helper leaves it nil, which is why no existing fixture could catch #123.
func toolResultMsgWithID(id, text string) bschemas.ChatMessage {
	m := toolResultMsg(text)
	callID := id
	m.ChatToolMessage = &bschemas.ChatToolMessage{ToolCallID: &callID}
	return m
}

// deepCandidates builds n tool outputs each carrying a distinct wire id.
func deepCandidates(n int) *bschemas.BifrostChatRequest {
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{userMsg("do the thing")}}
	for i := 0; i < n; i++ {
		req.Input = append(req.Input, toolResultMsgWithID("toolu_wire_"+strconv.Itoa(i),
			strings.Repeat("candidate "+strconv.Itoa(i)+" distinct line\n", 900)))
	}
	return req
}

// CANDIDATES ARE THE ENTIRE TRANSCRIPT, INCLUDING WHAT IS INSIDE THE CACHED PREFIX.
//
// This is the test whose absence hid #122. Every other sweep test runs with MaxCachedIdx: -1, which
// disables the tail gate altogether, so none of them could see that the gate was refusing every
// candidate the prefix ask can actually read.
//
// The defect: the depth permission was keyed on the cache having ALREADY expired (TailOnlyCold's
// optIn && c.ColdCache), which the pre-expiry window makes false by construction. So candidates
// collapsed to the uncached tail, while the ask reads the previous turn's sent body — everything up
// to the boundary. Disjoint sets: the model was asked to judge outputs it had never read, and live
// traffic showed it dropping one having seen only a 90-char head and a token count.
//
// A real MaxCachedIdx is therefore the whole point of this fixture. With the boundary at 4, messages
// 1..4 sit INSIDE the cached prefix and must still be offered.
func TestSweepOffersCandidatesInsideTheCachedPrefix(t *testing.T) {
	req := deepCandidates(12)
	asker := &fakeAsker{reply: `[]`, cacheRead: 19595}
	e := newSweep(t, "")
	c := preExpiryCtx("s-depth", asker, store.NewMemory(store.Options{}))
	// The boundary the regression tripped over: a live cached prefix covering most of the transcript.
	c.MaxCachedIdx = 8
	rep := components.Report{}
	if _, err := e.Offload(req, &rep, c); err != nil {
		t.Fatalf("offload: %v", err)
	}

	// Precondition: the ask must have happened, or every assertion below is vacuous.
	if got := rep.Gates["sweep_offered"]; got != 12 {
		t.Fatalf("sweep_offered = %d, want 12: candidates inside the cached prefix were refused, "+
			"which is #122 — the ask reads that region and the tail gate was excluding it; gates=%v",
			got, rep.Gates)
	}
	// The refusal counter from the regression must not fire at all.
	if got := rep.Gates["cached_prefix"]; got != 0 {
		t.Errorf("cached_prefix = %d, want 0: the sweep accepts prefix invalidation by construction, "+
			"so depth is not a refusal reason for it", got)
	}
	// And the positive counter must say the component genuinely reached past the boundary. Its going
	// to zero is the signal that this regressed again, which a refusal counter could not distinguish
	// from "nothing was deep this turn".
	if got := rep.Gates["sweep_candidate_at_depth"]; got == 0 {
		t.Errorf("sweep_candidate_at_depth = 0 with MaxCachedIdx=%d over %d messages: nothing was "+
			"counted as deep, so either the boundary is not being read or the fixture is wrong; "+
			"gates=%v", c.MaxCachedIdx, len(req.Input), rep.Gates)
	}
}

// THE INVENTORY FLOOR: below it, do not ask at all.
//
// The yield of this mechanism is a property of how many candidates the model compares. Shown one
// output a model scored 6% live-kept on haiku and 14% on sonnet, both inside the drop-everything
// null model's error bar; at batch 3-6 it dropped a genuinely-spent output 2 times in 4, and at
// batch 10 it dropped it 4 in 4. Below the floor a `drop` is a guess, and a wrong drop is a silent
// permanent loss while a wrong keep costs one turn's tokens — so declining is strictly better than
// asking.
//
// Both arms share one config and differ only in how many candidates exist, so this asserts the floor
// rather than something general about small transcripts.
func TestSweepDeclinesBelowTheInventoryFloor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		n         int
		wantAsked bool
	}{
		{"below the floor", 4, false},
		{"at the floor", 10, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asker := &fakeAsker{reply: `[]`, cacheRead: 19595}
			e := newSweep(t, "") // min_inventory defaults to 10
			c := preExpiryCtx("s-floor-"+tc.name, asker, store.NewMemory(store.Options{}))
			rep := components.Report{}
			if _, err := e.Offload(deepCandidates(tc.n), &rep, c); err != nil {
				t.Fatalf("offload: %v", err)
			}
			asked := atomic.LoadInt64(&asker.calls) > 0
			if asked != tc.wantAsked {
				t.Errorf("%d candidates: asked=%v, want %v; gates=%v",
					tc.n, asked, tc.wantAsked, rep.Gates)
			}
			// The decline must be COUNTED. A component that declines is indistinguishable from one
			// that is broken unless it says so — the failure mode that hid this component's own
			// economic_gate blind spot and three vacuous trim tests before it.
			if !tc.wantAsked {
				if got := rep.Gates["sweep_inventory_below_min"]; got != tc.n {
					t.Errorf("sweep_inventory_below_min = %d, want %d: a silent decline is "+
						"indistinguishable from a broken component; gates=%v", got, tc.n, rep.Gates)
				}
			} else if rep.Gates["sweep_inventory_below_min"] != 0 {
				t.Errorf("the floor fired at %d candidates; gates=%v", tc.n, rep.Gates)
			}
		})
	}
}

// THE ANCHOR MUST BE THE WIRE'S ID, ASSERTED BY PROVENANCE RATHER THAN BY RENDERING.
//
// #123: the inventory announced `tool_use id 300c312d1492952219bfb1c4` — extract.ContentKey, our own
// store key — while the real id in that transcript was `toolu_d2`. The contract tells the model the
// id is "shown only so you can find the output in the conversation above", so shipping a string that
// appears nowhere in the conversation is worse than omitting it: it directs the model to look up a
// key that cannot be found.
//
// The previous test could not catch it because it hard-coded `ID: "toolu_abc123"` into an
// AdjudicationItem and asserted only that BuildPrefixAsk rendered whatever was in the field. That
// passes on any string. This one starts from a REQUEST and checks what the component chose to ship,
// which is where the substitution happened.
func TestSweepShipsTheWiresToolCallIDNotTheContentKey(t *testing.T) {
	req := deepCandidates(12)
	asker := &fakeAsker{reply: `[]`, cacheRead: 19595}
	e := newSweep(t, "")
	rep := components.Report{}
	if _, err := e.Offload(req, &rep,
		preExpiryCtx("s-anchor", asker, store.NewMemory(store.Options{}))); err != nil {
		t.Fatalf("offload: %v", err)
	}
	ask := asker.ask()
	if ask == "" {
		t.Fatal("no ask was recorded, so the assertion is vacuous")
	}
	// Every wire id must be present: it is the anchor, and the model is told it can be found above.
	for i := 0; i < 12; i++ {
		want := "toolu_wire_" + strconv.Itoa(i)
		if !strings.Contains(ask, want) {
			t.Errorf("the ask does not carry the wire id %q, so the locating anchor names nothing "+
				"the model can find in the transcript", want)
		}
	}
	// And our own content key must NOT be: shipping it is #123 exactly. Derived the same way the
	// component does, so this cannot drift from the implementation.
	for i := range req.Input {
		if req.Input[i].Role != bschemas.ChatMessageRoleTool {
			continue
		}
		if key := extract.ContentKey(schema.MessageText(req.Input[i])); strings.Contains(ask, key) {
			t.Errorf("the ask carries the CONTENT KEY %q, which appears nowhere in the transcript "+
				"— that is #123: the anchor directs the model to a key it cannot find", key)
		}
	}
}
