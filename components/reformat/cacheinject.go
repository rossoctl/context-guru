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
// only pays when p = P(gap > 5 min) exceeds (2.0−1.25)/(1.25−0.1) = 65.2%. Measured
// over 1,905 real agent turns: p = 0.00% (median gap 7.6 s, max 75 s). 1h is for
// human-in-the-loop sessions, not for an autonomous agent loop.
type Cacheinject struct {
	// TTL is the cache lifetime to request: "5m" (default) or "1h".
	//
	// Leave it at 5m unless the deployment shape demands otherwise. By rule 2, a 1h
	// write costs 2.0x instead of 1.25x and only pays when p = P(the entry lapses
	// before its next reuse) exceeds (2.0-1.25)/(1.25-0.1) = 65.2%.
	//
	// The denominator is the 5m WRITE rate minus the read rate, not 1 minus the read rate.
	// A lapsed entry does not re-bill at the fresh 1.0x: those tokens still carry
	// cache_control, so the provider CREATES a new entry at 1.25x. Production ttl_expiry
	// rows show it directly — cache_write averages 178,793 against fresh_input 1,712. The
	// documented figure was 83.3%, wrong by 18 points; the conclusion is unchanged. Two
	// things make p
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
	if err := components.Decode(raw, c); err != nil {
		return nil, err
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

func (Cacheinject) Enabled(c *components.Ctx) bool { return true }

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

	applied := 0
	// Breakpoints the caller already set occupy provider slots, and an agent that sets
	// its own (claude-code does) is already at the optimum. Two separate things must
	// happen with them: the positions we can SEE are dropped from `want` (never mark
	// twice), and the BUDGET is computed from the host's raw-body count, which also
	// sees the ones we cannot — the `system` array components never receive, and
	// `tool_result` blocks whose mark apply's own normalize drops. On real
	// Claude Code traffic that is all 3 of them, so counting only Input gave a budget
	// of 3 free slots when 1 was free: 6 on the wire, and a 400 (issue #32).
	visible := 0
	for i := range req.Input {
		if hasBreakpoint(&req.Input[i]) {
			visible++
			delete(want, i)
		}
	}
	existing := visible
	if c != nil && c.ExistingBreakpoints > visible {
		existing = c.ExistingBreakpoints
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

// --------------------------------------------------------------------------- //

func init() { components.Register("cachesplit", newCachesplit) }

// Cachesplit is a marker component: it carries no logic of its own, and exists so a
// preset can enable the volatile-tail split (apply/prefixsplit.go) WITHOUT also
// enabling cacheinject's breakpoint placement.
//
// The two were one config entry until #32, which separated them because their evidence
// is not comparable. The split is measured: −34.1% cost and 0% → 96.7% cache hit in an
// isolated A/B, because it moves a churning env snapshot out of a hashed prefix.
// Placement has never been measured at all — until #32 its breakpoints never reached
// the provider. So the split ships on by default and placement does not.
//
// It is a Reformat that always skips: the actual rewrite is body-level (it edits the
// top-level `system` array, which components never see) and lives in `apply`, gated on
// this name being present. ponytail: a marker beats plumbing a new config flag through
// every host.
type Cachesplit struct{}

// newCachesplit takes no configuration. It decodes STRICTLY into an empty struct, so a
// `cachesplit:` block with anything in it is an error rather than silence: the signature
// used to be `newCachesplit([]byte)`, which discarded whatever was written there — a
// config saying `cachesplit: {ttl: 1h}` looked configured and was not.
func newCachesplit(raw []byte) (components.Component, error) {
	var none struct{}
	if err := components.Decode(raw, &none); err != nil {
		return nil, fmt.Errorf("cachesplit: takes no configuration: %w", err)
	}
	return Cachesplit{}, nil
}

func (Cachesplit) Name() string { return "cachesplit" }

func (Cachesplit) Enabled(*components.Ctx) bool { return true }

// Reformat is intentionally a no-op — see the type doc. The split happens in apply, which
// reports it here through Ctx.SystemSplit.
//
// That flag is the whole reason this method is not a bare `rep.Skipped = true`. Every consumer
// of a component report — /stats, the Prometheus component counters, the dashboard's components
// table — answers "did this component do anything?" from Skipped. Reporting it unconditionally
// made the measured -34.1%-cost mechanism read "declined" on the requests where it had just
// run.
//
// The dashboard's prefix-cache saving no longer reads this flag: it is priced from
// apply.Trace.SplitStableTokens, the size of the half the split moved, which is set on exactly
// the same requests and also says HOW MUCH. Keeping the report honest still matters for
// everything above, and for anyone comparing the two.
//
// It is Mutated but never Acted: the split removes no content tokens, it moves them out of
// the hashed prefix. cacheinject reads the same way, and the dashboard has a verdict for it.
func (Cachesplit) Reformat(_ *schemas.BifrostChatRequest, rep *components.Report, c *components.Ctx) error {
	rep.Skipped = c == nil || !c.SystemSplit
	return nil
}

func init() {
	components.RegisterFields("cacheinject", Cacheinject{}, []components.Field{
		{Key: "ttl", Type: components.FieldEnum, Default: "5m", Options: []string{"5m", "1h"},
			Hint: "Cache lifetime to request. Leave 5m: a 1h write costs 2.0x instead of 1.25x and only pays when reuse is genuinely sparse (task starts more than 5 minutes apart), because every read refreshes the TTL for free."},
	})
	// cachesplit takes no configuration at all, and says so: an empty descriptor is what
	// the settings page draws as "no options", and the strict decode in newCachesplit now
	// REJECTS a block instead of accepting one and discarding it.
	components.RegisterFields("cachesplit", struct{}{}, nil)
}
