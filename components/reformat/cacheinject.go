// Package reformat holds the lossless components (they repack the request
// denser or add caching hints without losing information). Each registers via
// init(); a binary blank-imports components/all to pull them in.
package reformat

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/schema"
	"gopkg.in/yaml.v3"
)

func init() { components.Register("cacheinject", newCacheinject) }

// Cache-billing constants, from the provider docs. Identical on the Anthropic
// API, Bedrock and Vertex; see docs/components/cacheinject.md for the derivation
// these drive.
const (
	maxBreakpoints = 4  // hard provider cap on cache_control blocks per request
	lookbackBlocks = 20 // how far the provider's backward read-walk reaches
)

// Cacheinject places Anthropic-family `cache_control` breakpoints so the
// provider's KV cache is read rather than re-processed. It is a Reformat: it adds
// control directives, changes no model-visible content, and loses nothing.
//
// The placement is the solution to the billed-cost minimisation, not a heuristic.
// Writing R=0.1, W=1.25 and plain=1.0 for the read / 5m-write / uncached
// multipliers, four facts decide everything:
//
//  1. A token resent even once is cheaper written than not: W+kR < 1+k for
//     k > (W-1)/(1-R) = 0.28. So the LAST block always gets a breakpoint.
//  2. Writes are billed as a SPAN — (highest breakpoint − read point) — not per
//     breakpoint. An extra breakpoint BELOW the top therefore costs exactly
//     zero, and only adds a position a later turn's backward walk can land on.
//     So never leave one of the four slots idle.
//  3. The big lever is DIVERGENCE, not position. If this turn differs from the
//     previous one at message d, every block above d is unmatchable: a
//     trailing-only breakpoint finds nothing and the whole prefix bills at 1.0x
//     instead of 0.1x — a 10x penalty. A breakpoint at d−1 recovers it.
//  4. Anchors must sit at TURN-STABLE indices. A position is only readable on
//     turn t+1 if it was written on turn t, so an anchor counted back from the
//     end lands on a different message each turn and is never pre-warmed.
//
// v1 placed a single breakpoint on the message BEFORE the newest turn, which by
// construction shortens the cached prefix every turn: measured +5.5% on captured
// SWE-bench traffic, and +9.2% when layered on an agent that already sets its own
// breakpoints. v2 keeps every breakpoint the caller set (so a well-behaved agent
// is never made worse), then spends only the leftover slots.
//
// 1h TTL is deliberately never used. It costs 2.0x instead of 1.25x to write and
// only pays when p = P(gap > 5 min) exceeds (2.0−1.25)/(1−0.1) = 83.3%. Measured
// over 1,905 real agent turns: p = 0.00% (median gap 7.6 s, max 75 s). 1h is for
// human-in-the-loop sessions, not for an autonomous agent loop.
type Cacheinject struct {
	// TTL is the cache lifetime to request: "5m" (default) or "1h".
	//
	// Leave it at 5m unless the deployment shape demands otherwise. By rule 2, a 1h
	// write costs 2.0x instead of 1.25x and only pays when p = P(the entry lapses
	// before its next reuse) exceeds (2.0-1.25)/(1-0.1) = 83.3%. Two things make p
	// small in practice: agent turns are seconds apart (measured median 7.6 s over
	// 1,905 real turns), and every READ refreshes the TTL for free — so a shared
	// prefix touched by any session at least once per 5 minutes never lapses. On the
	// benchmark sweep a new task started every ~2.3 minutes, so 5m was strictly
	// correct.
	//
	// Set "1h" when reuse is genuinely sparse: low-concurrency sweeps with long
	// tasks (task starts more than 5 minutes apart), or a deployed agent handling a
	// few sessions per hour. That is a property of the traffic, not of the code,
	// which is why it is configuration rather than a heuristic.
	TTL string `yaml:"ttl"`
}

func newCacheinject(raw []byte) (components.Component, error) {
	c := &Cacheinject{TTL: "5m"}
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, c); err != nil {
			return nil, err
		}
	}
	if c.TTL != "5m" && c.TTL != "1h" {
		return nil, fmt.Errorf("cacheinject: ttl must be \"5m\" or \"1h\", got %q", c.TTL)
	}
	return c, nil
}

// ttl returns the cache_control TTL to emit. Anthropic treats an absent ttl as 5m,
// so it is only set explicitly for 1h — keeping the 5m wire shape byte-identical to
// what the agent would have sent.
func (c Cacheinject) ttl() *string {
	if c.TTL == "1h" {
		v := "1h"
		return &v
	}
	return nil
}

func (Cacheinject) Name() string { return "cacheinject" }

// Enabled is true except on an off-path (deferred) async run. Two reasons, and either
// alone is sufficient: a deferred run's BODY is discarded, so breakpoints it places go
// nowhere; and it keeps per-turn divergence digests, which are turn state. A deferred
// job commits some turns after the one it was built from, so committing its digests
// would replay turn N's digests over turn N+2's and make the next turn compute the
// wrong divergence point. Only an offloader's frozen decisions are meant to survive a
// deferred run.
func (Cacheinject) Enabled(c *components.Ctx) bool { return c == nil || !c.Deferred }

func (ci Cacheinject) Reformat(req *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) error {
	if !cacheAware(req.Provider) || len(req.Input) == 0 {
		rep.Skipped = true
		return nil
	}

	// Where does this turn diverge from the previous one? Compared per message on
	// a content digest, so it costs one hash per message and no stored bodies.
	prev := loadDigests(c)
	now := digests(req.Input)
	div := commonPrefix(prev, now)
	storeDigests(c, now)

	// Positions we want a breakpoint at, in message-index space.
	want := make(map[int]struct{})

	// Rule 1: the newest message. Its tokens are re-sent on every later turn, so
	// they must be written now to be read later.
	want[len(req.Input)-1] = struct{}{}

	// Rule 3: the last message that still matches the previous turn. When the
	// stream is append-only (div == len(prev)) this is the previous tail, which is
	// already inside the read span and so costs nothing. When something mutated,
	// this is the breakpoint that rescues the stable head.
	if len(prev) > 0 && div > 0 && div <= len(req.Input) {
		want[div-1] = struct{}{}
	}

	// Rule 4: spend the remaining slots on turn-stable anchors, spaced so a
	// consecutive pair always stays within the provider's backward-walk window.
	// Counting UP from the start keeps the indices fixed as the conversation
	// grows, which is what makes them pre-warmed and therefore readable.
	for i := lookbackBlocks - 1; i < len(req.Input)-1 && len(want) < maxBreakpoints; i += lookbackBlocks - 1 {
		want[i] = struct{}{}
	}

	// Async cache policy (#31). While a compaction is queued but not yet landed, the
	// tail it is going to REPLACE must not be committed to the provider cache: a
	// breakpoint at or beyond it turns what would have been a 0.1x read next turn into
	// a 1.25x write of that same span — 11.5x the cost. That is exactly the failure
	// that tripled headroom's cache-write on Terminal-Bench.
	//
	// This has to cover breakpoints the CALLER set, not just the ones we wanted. An
	// earlier version only pruned `want`, which made the whole protection a no-op on the
	// primary workload: claude-code sets its own breakpoint on the newest message, so
	// the doomed tail was cache-written anyway — async then paid the rewrite AND lost a
	// slot, strictly worse than sync. Whether we may strip that breakpoint is the
	// caller's call (StripCallerBreakpoints), because removing one an agent deliberately
	// placed changes behavior we do not own.
	if c.TailCachePending {
		for i := range want {
			if c.CacheBlocked(i) {
				delete(want, i)
			}
		}
		for i := range req.Input {
			if !c.CacheBlocked(i) || !hasBreakpoint(&req.Input[i]) {
				continue
			}
			if !c.StripCallerBreakpoints {
				// Cannot protect this turn without overriding the caller, so do not
				// pretend to: leave the request exactly as it came and tell the host,
				// which then declines to defer (see proxy.applyMode). Reporting success
				// here is what made the protection a silent no-op before.
				c.DeclineTailProtection()
				rep.Skipped = true
				return nil
			}
			unmark(&req.Input[i])
		}
		if last := c.NoCacheAtOrAfter - 1; last >= 0 && last < len(req.Input) {
			want[last] = struct{}{}
		}
	}

	applied := 0
	// Count breakpoints the caller already set: they occupy provider slots, and an
	// agent that sets its own (claude-code does) is already at the optimum.
	existing := 0
	for i := range req.Input {
		if hasBreakpoint(&req.Input[i]) {
			existing++
			delete(want, i)
		}
	}
	budget := maxBreakpoints - existing
	if budget <= 0 {
		rep.Skipped = true // no slots left; adding one would be a 400 from the provider
		return nil
	}

	// Nearest-to-the-tail first: those are the reads that pay this turn.
	idxs := sortedDesc(want)
	for _, i := range idxs {
		if applied >= budget {
			break
		}
		if mark(&req.Input[i], ci.ttl()) {
			applied++
			continue
		}
		// That message cannot carry a block-level directive (string content), so
		// walk DOWN to the nearest one that can. Rule 1 wants the longest prefix
		// written, and the next-lowest markable block still captures nearly all of
		// it — far better than writing nothing and billing the whole prefix at 1.0x.
		for j := i - 1; j >= 0; j-- {
			if hasBreakpoint(&req.Input[j]) {
				break // already covered at or below here
			}
			if mark(&req.Input[j], ci.ttl()) {
				applied++
				break
			}
		}
	}
	if applied == 0 {
		rep.Skipped = true
	}
	return nil
}

// --------------------------------------------------------------------------- //

// mark attaches a 5m ephemeral breakpoint to a message's last content block.
// String-content messages cannot carry a block-level directive, so they are
// skipped rather than restructured — that keeps this strictly lossless.
func mark(m *schemas.ChatMessage, ttl *string) bool {
	if m.Content == nil || len(m.Content.ContentBlocks) == 0 {
		return false
	}
	last := &m.Content.ContentBlocks[len(m.Content.ContentBlocks)-1]
	if last.CacheControl != nil {
		return false
	}
	last.CacheControl = &schemas.CacheControl{Type: schemas.CacheControlTypeEphemeral, TTL: ttl}
	return true
}

// unmark removes every cache_control directive from a message. Used only by async's
// tail protection, to take back a breakpoint that sits on content a pending compaction
// is about to replace.
func unmark(m *schemas.ChatMessage) {
	if m.Content == nil {
		return
	}
	for i := range m.Content.ContentBlocks {
		m.Content.ContentBlocks[i].CacheControl = nil
	}
}

func hasBreakpoint(m *schemas.ChatMessage) bool {
	if m.Content == nil {
		return false
	}
	for i := range m.Content.ContentBlocks {
		if m.Content.ContentBlocks[i].CacheControl != nil {
			return true
		}
	}
	return false
}

// digests returns a per-message content digest. Truncated to 8 bytes: this only
// ever detects inequality, and a collision costs one missed optimisation, never
// a wrong request.
func digests(msgs []schemas.ChatMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		h := sha1.Sum([]byte(string(m.Role) + "\x00" + schema.MessageText(m)))
		out[i] = hex.EncodeToString(h[:8])
	}
	return out
}

func commonPrefix(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

const digestKey = "cg:cidx:"

func loadDigests(c *components.Ctx) []string {
	if c == nil || c.Store == nil || c.Session == "" {
		return nil
	}
	b, ok := c.Store.Get(digestKey + c.Session)
	if !ok || len(b) == 0 {
		return nil
	}
	return strings.Split(string(b), ",")
}

func storeDigests(c *components.Ctx, d []string) {
	if c == nil || c.Store == nil || c.Session == "" {
		return
	}
	c.Store.Put(digestKey+c.Session, []byte(strings.Join(d, ",")))
}

func sortedDesc(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ { // insertion sort: len <= 4
		for j := i; j > 0 && out[j] > out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// cacheAware reports whether the provider honours Anthropic-style cache_control.
func cacheAware(p schemas.ModelProvider) bool {
	switch p {
	case schemas.Anthropic, schemas.Bedrock, schemas.Vertex:
		return true
	default:
		return false
	}
}
