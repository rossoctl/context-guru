package offload

import (
	"strings"
	"testing"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/expand"
	"github.com/rossoctl/context-guru/schema"
	"github.com/rossoctl/context-guru/store"
)

// The two skip reasons are separately reportable (#201).
//
// `marker_or_kept_verbatim` was raised at eight of eleven sites for two conditions that want
// opposite readings — "already carries a marker" is benign, "the agent expanded it" is an
// established compaction being abandoned — while three offloaders already had a distinct label for
// the second. Gates reach /stats per component, so that was a published counter reporting both, and
// a published experimental figure was corrected twice off it.
func TestTheTwoSkipReasonsAreReportedApart(t *testing.T) {
	body := strings.Repeat("verbose tool output line\n", 30)
	for _, tc := range []struct {
		name string
		// prepare returns the content the offloader will see, having set up any store state.
		prepare func(st *store.Memory) string
		want    string
	}{
		{"content that already carries a marker", func(*store.Memory) string {
			return "head\n" + expand.Marker("aaaaaaaaaaaaaaaa") + "\n"
		}, GateAlreadyMarked},
		{"content the agent expanded", func(st *store.Memory) string {
			MarkKeptVerbatim(st, body)
			return body
		}, GateKeptVerbatim},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory(store.Options{})
			content := tc.prepare(st)
			m := maskFor(t)
			req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
				tool(content), tool("tail"),
			}}
			c := &components.Ctx{Session: "s", Store: st}
			var rep components.Report
			if _, err := m.Offload(req, &rep, c); err != nil {
				t.Fatal(err)
			}
			if rep.Gates[tc.want] == 0 {
				t.Fatalf("want gate %q, got %v — the two reasons are still sharing a label, so an "+
					"operator cannot tell a benign skip from an abandoned compaction", tc.want, rep.Gates)
			}
			// And it must not ALSO raise the other one, or the split buys nothing.
			other := GateAlreadyMarked
			if tc.want == GateAlreadyMarked {
				other = GateKeptVerbatim
			}
			if rep.Gates[other] != 0 {
				t.Fatalf("raised BOTH labels (%v); a skip has exactly one reason", rep.Gates)
			}
		})
	}
}

// Three offloaders have NO reapplyFrozen path at all, so `skipReduce` is the only place they can
// report an expansion — and it was the conflated label for all of them. This is where most of the
// previously invisible expansion signal sits, so it gets its own assertion rather than being assumed
// to follow from mask's.
func TestTheOffloadersWithNoReplayPathAlsoReportExpansion(t *testing.T) {
	body := strings.Repeat("verbose tool output line\n", 30)
	for _, tc := range []struct {
		name string
		make func(t *testing.T) components.Offload
	}{
		{"dedup", func(t *testing.T) components.Offload {
			c, err := newDedup([]byte("min_tokens: 5\n"))
			if err != nil {
				t.Fatal(err)
			}
			return c.(components.Offload)
		}},
		{"extract", func(t *testing.T) components.Offload {
			c, err := newExtract([]byte("min_tokens: 5\n"))
			if err != nil {
				t.Fatal(err)
			}
			return c.(components.Offload)
		}},
		{"linecap", func(t *testing.T) components.Offload {
			c, err := newLinecap([]byte("min_size: 5\n"))
			if err != nil {
				t.Fatal(err)
			}
			return c.(components.Offload)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := store.NewMemory(store.Options{})
			MarkKeptVerbatim(st, body)
			off := tc.make(t)
			// Two identical outputs, so dedup has a duplicate to consider as well.
			req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{
				tool(body), tool(body), tool("tail"),
			}}
			c := &components.Ctx{Session: "s", Store: st}
			var rep components.Report
			if _, err := off.Offload(req, &rep, c); err != nil {
				t.Fatal(err)
			}
			if rep.Gates[GateKeptVerbatim] == 0 {
				t.Fatalf("%s reported %v: expanded content is invisible in this component, which is "+
					"where most of the unattributed expansion signal was", tc.name, rep.Gates)
			}
		})
	}
}

// THE FLIP IS COUNTED, AND ONLY WHEN THERE IS ONE.
//
// Abandoning a compaction because the agent expanded it costs a suffix cache-write ONLY if the
// content was compacted before — if it never was, nothing flips. reapplyFrozen declined before
// looking, so the two were indistinguishable and an expand-induced cache-write could not be told
// from any other.
func TestOnlyAnAbandonedEstablishedCompactionCountsAsAFlip(t *testing.T) {
	body := strings.Repeat("verbose tool output line\n", 30)

	t.Run("never compacted, so nothing flips", func(t *testing.T) {
		st := store.NewMemory(store.Options{})
		MarkKeptVerbatim(st, body) // expanded, but this session never froze a decision for it
		m := maskFor(t)
		before := ExpandPrefixFlips()
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body), tool("tail")}}
		c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: 0}
		var rep components.Report
		if _, err := m.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		if got := ExpandPrefixFlips() - before; got != 0 {
			t.Fatalf("counted %d flips for content that was never compacted: the counter is "+
				"measuring expansions, not cache-writes", got)
		}
	})

	t.Run("an established compaction is abandoned", func(t *testing.T) {
		st := store.NewMemory(store.Options{})
		m := maskFor(t)
		// Turn 1 in the uncached tail: mask compacts and freezes a decision.
		req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body), tool("tail")}}
		c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: -1}
		var rep components.Report
		if _, err := m.Offload(req, &rep, c); err != nil {
			t.Fatal(err)
		}
		if schema.MessageText(req.Input[0]) == body {
			t.Fatal("turn 1 did not compact, so there is no established compaction to abandon")
		}
		// The agent expands it.
		MarkKeptVerbatim(st, body)

		before := ExpandPrefixFlips()
		// Turn 2: the output is now inside the cached prefix, and the replay declines.
		req2 := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body), tool("tail")}}
		c2 := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: 0}
		var rep2 components.Report
		if _, err := m.Offload(req2, &rep2, c2); err != nil {
			t.Fatal(err)
		}
		if schema.MessageText(req2.Input[0]) != body {
			t.Fatal("turn 2 did not send the full original, so no flip happened")
		}
		if got := ExpandPrefixFlips() - before; got != 1 {
			t.Fatalf("counted %d flips, want 1: a suffix cache-write caused by an expand is still "+
				"indistinguishable from any other cache-write", got)
		}
	})
}

// AND THE PROBE MUST NOT KEEP ALIVE WHAT IT ASKS ABOUT.
//
// store.Peek exists for this call site, but a store-side test cannot fail if this call site goes back
// to Get: the drift is between what the probe uses and what the store offers, so the assertion has to
// live where the probe is. Memory.Get slides the TTL and FrozenPrefix is a PINNED namespace, so a
// Get-based probe renews a pinned entry the branch has just decided will never be replayed — and
// pinned entries count against the shared exempt budget that gates rewind-reserve admission.
func TestTheFlipProbeDoesNotRenewTheDecisionItAsksAbout(t *testing.T) {
	body := strings.Repeat("verbose tool output line\n", 30)
	now := time.Unix(0, 0)
	st := store.NewMemory(store.Options{TTLSeconds: 100})
	st.SetClock(func() time.Time { return now })
	m := maskFor(t)

	// Turn 1 freezes a decision, then the agent expands the content.
	req := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body), tool("tail")}}
	c := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: -1}
	var rep components.Report
	if _, err := m.Offload(req, &rep, c); err != nil {
		t.Fatal(err)
	}
	MarkKeptVerbatim(st, body)
	key := frozenKey("s", m.Name(), contentKey(body))
	if !st.Peek(key) {
		t.Fatal("no frozen decision after turn 1; the fixture is not exercising the probe")
	}

	// Ten turns 60s apart: each takes the kept-verbatim branch and probes the decision. 600s total
	// against a 100s TTL, never more than 60s between probes.
	for i := 0; i < 10; i++ {
		now = now.Add(60 * time.Second)
		r := &bschemas.BifrostChatRequest{Input: []bschemas.ChatMessage{tool(body), tool("tail")}}
		cc := &components.Ctx{Session: "s", Store: st, CacheAware: true, MaxCachedIdx: 0}
		var rr components.Report
		if _, err := m.Offload(r, &rr, cc); err != nil {
			t.Fatal(err)
		}
	}
	if st.Peek(key) {
		t.Fatal("the frozen decision survived 600s of a 100s TTL: the flip probe is renewing a " +
			"PINNED entry it has just decided will never be replayed, which is back-pressure on " +
			"the rewind reserve from a diagnostic")
	}
}
