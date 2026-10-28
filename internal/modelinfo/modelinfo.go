// Package modelinfo resolves a model's context window (max input tokens) and per-token price,
// so context-guru's triggers can scale with the model rather than hard-coding thresholds. It does
// no I/O itself: Resolver, Pricer, Static, Table and Chain are pure lookups. The dynamic source —
// LiteLLM's community-maintained model_prices_and_context_window.json, fetched once and cached —
// is internal/modelinfo/litellm, a host-side sibling package: a host (proxy, cmd/) builds a
// litellm.LiteLLM and composes it into a Chain alongside this package's own Static fallback.
//
// Every lookup fails OPEN: an unknown model returns ok=false and callers fall back to absolute
// thresholds.
//
// READ THAT CAREFULLY, because an earlier version of this comment claimed more than the package
// delivers. "Any error just leaves the window unknown" is true of a single resolver in isolation
// and false of the chain it is normally used in: a Chain{litellm.LiteLLM, DefaultStatic()} answers
// an unreachable document from the embedded table, so a failed fetch does not produce "unknown",
// it produces a confident and possibly very wrong number. Callers that have been GIVEN an
// authoritative document — an explicit MODEL_INFO_URL — must not be silently answered from the
// fallback table; see modelWindows in cmd/context-guru-proxy, which probes such a URL at startup
// and refuses to run rather than guess past it.
package modelinfo

import (
	"context"
	"strings"
)

// Resolver returns a model's context window in tokens. ok=false means "unknown".
type Resolver interface {
	Window(ctx context.Context, model string) (tokens int, ok bool)
}

// ExactResolver is the optional capability a Resolver implements to say whether the window it
// returned is a MEASURED figure for that model or a GUESS — and it exists because the two are
// not interchangeable for every caller.
//
// DefaultStatic answers by substring with a `{"claude", 200000}` catch-all, so a 1M-window model
// it has no entry for comes back as 200,000 with ok=true. That is a fine floor for a caller that
// only wants "don't act on a tiny output" — a too-small window there merely raises a floor. It is
// NOT fine for a caller deciding "is this transcript 90% full", where a 5x-low window fires the
// decision five times too early. Such a caller must be able to tell the two apart, and ok=true
// cannot tell it.
//
// A Resolver that does NOT implement this is treated as inexact by Chain, deliberately: a new
// source has to say it is authoritative, because the failure of forgetting is silent and the
// failure of declaring wrongly is not.
type ExactResolver interface {
	WindowExact(ctx context.Context, model string) (tokens int, exact, ok bool)
}

// Exact reports a window plus whether it is measured rather than guessed, for any Resolver.
// A resolver without the capability answers exact=false.
func Exact(r Resolver, ctx context.Context, model string) (tokens int, exact, ok bool) {
	if r == nil {
		return 0, false, false
	}
	if er, hasCap := r.(ExactResolver); hasCap {
		return er.WindowExact(ctx, model)
	}
	w, found := r.Window(ctx, model)
	return w, false, found
}

// Price is a model's per-token USD rates, in the four tiers a prompt-caching
// provider bills. Zero rates mean "unknown" — a caller must treat a Price it did
// not get an ok=true for as "no pricing", never as free.
type Price struct {
	Input      float64 `json:"input"`       // fresh (uncached) input per token
	Output     float64 `json:"output"`      // completion per token
	CacheRead  float64 `json:"cache_read"`  // cache-hit input per token
	CacheWrite float64 `json:"cache_write"` // cache-creation input per token
}

// Cost prices one request's four token tiers in USD.
func (p Price) Cost(fresh, cacheRead, cacheWrite, output int64) float64 {
	return float64(fresh)*p.Input + float64(cacheRead)*p.CacheRead +
		float64(cacheWrite)*p.CacheWrite + float64(output)*p.Output
}

// CostWithCacheWrite1h is Cost plus the one-hour cache-write premium.
//
// Cost knows a single write rate — the 5-minute one, at roughly 1.25x base input — so tokens
// the provider billed at the 1-hour tier (2.0x) are short by the difference. Derived from the
// documented multiplier rather than from a second configured rate, because no gateway
// publishes one: 2.0x input, minus whatever the 5m write rate is, on exactly the tokens the
// response says were written at 1h.
//
// Hoisted here so dash's Event.Price and the host-level keep-alive mechanism's own cost
// bookkeeping (proxy/keepalive.go's record1) price the SAME request shape identically instead
// of each inlining this formula — the two had drifted apart once already (see issue #382's
// review) when the keep-alive mechanism was decoupled from dash's event type.
func (p Price) CostWithCacheWrite1h(fresh, cacheRead, cacheWrite, output, cacheWrite1h int64) float64 {
	cost := p.Cost(fresh, cacheRead, cacheWrite, output)
	if cacheWrite1h > 0 {
		if premium := 2*p.Input - p.CacheWrite; premium > 0 {
			cost += float64(cacheWrite1h) * premium
		}
	}
	return cost
}

// Zero reports whether no rate at all is known (so a cost figure would be a lie).
func (p Price) Zero() bool {
	return p.Input == 0 && p.Output == 0 && p.CacheRead == 0 && p.CacheWrite == 0
}

// Pricer resolves a model's per-token rates. ok=false means "unknown"; callers
// must then report cost as unavailable rather than zero.
type Pricer interface {
	Price(ctx context.Context, model string) (Price, bool)
}

// normalize lowercases a model id, strips a provider prefix, and strips a trailing bracketed
// variant suffix, so gateway route names (aws/claude-sonnet-5, anthropic/…, bedrock/…,
// us.anthropic.…, aws/claude-opus-5[1m]) match the keys LiteLLM uses. Returns the full normalized
// id and its last path segment.
//
// THE BRACKETED SUFFIX IS THE CONTEXT-LENGTH VARIANT, and not stripping it made every lookup for
// such an id miss the map entirely. The consequences ran in two directions, both bad:
//
//   - The chain fell through to DefaultStatic, whose answer is never exact. Trigger.FracResolvable
//     requires CtxWindowExact, and summarize ships min_request_frac: 0.9 — so on any id carrying a
//     suffix the fraction gate could never fire, silently, which looks exactly like a gate that is
//     working.
//   - DefaultStatic's substring table answered 200,000 for `claude-opus-5[1m]` against a real
//     1,000,000. exact=false keeps the trigger from acting on that, but OutputFloor, IsHuge and
//     extract_llm's fraction all read the non-exact Window(), where five times too low is only safe
//     in one direction.
//
// Found by a review, which then corrected itself twice about the reach: the gateway on the box it
// tested refuses these ids with a model-authorization error, and the client demonstrably works on
// that key with the un-suffixed name, so what the request body actually carries in `model` — the
// only field this reads — was never observed. So this is DEFENSIVE: the defect is real and the fix
// is right, with no demonstrated production impact. An operator can also work around it with a
// trailing `*` in a MODEL_PRICES entry.
//
// Stripped from BOTH return values, and only when the suffix closes at the very end: `[` inside a
// key would be part of the name rather than a variant marker, and a half-open bracket is a
// malformed id this must not silently rewrite.
// NormalizeID is normalize's exported half: the full normalized id, for a caller outside this
// package that needs to match a model against a table of its own and must not re-implement the
// prefix and variant-suffix rules. dash's per-model client-ceiling table is the caller.
//
// One implementation, because two would drift: this package already learned that the hard way when
// the bracketed `[1m]` suffix went unstripped and every lookup for such an id silently missed.
func NormalizeID(model string) string {
	full, _ := Normalize(model)
	return full
}

// Normalize lowercases a model id, strips a provider prefix for the tail form, and strips a
// trailing bracketed variant suffix (see stripVariant), returning both the full normalized id and
// its last path segment. Exported for internal/modelinfo/litellm, which matches against both the
// same way this package's own LiteLLM resolver used to.
func Normalize(model string) (full, tail string) {
	full = stripVariant(strings.ToLower(strings.TrimSpace(model)))
	tail = full
	if i := strings.LastIndexAny(tail, "/"); i >= 0 {
		tail = tail[i+1:]
	}
	return full, tail
}

// stripVariant removes a single trailing "[...]" suffix, e.g. claude-opus-5[1m] -> claude-opus-5.
// Leaves anything else untouched, including an id with no suffix, an unterminated bracket, and an
// id that is nothing but a bracketed group.
func stripVariant(id string) string {
	if !strings.HasSuffix(id, "]") {
		return id
	}
	i := strings.LastIndexByte(id, '[')
	if i <= 0 { // no opener, or the whole id is the group — not a variant suffix
		return id
	}
	return id[:i]
}

// Static is a tiny last-resort table keyed by substring. Used only when a dynamic
// source is unavailable. Deliberately small — it is a floor, not a registry.
type Static struct{ table []staticEntry }

type staticEntry struct {
	substr string
	window int
}

// DefaultStatic covers the common families with conservative windows.
//
// THE 1M CLAUDE FAMILIES ARE LISTED EXPLICITLY, and they had to be. The table used to carry
// `{"claude-sonnet-5", 1000000}, {"claude-opus-4", 200000}, {"claude", 200000}` — so every Opus 4.6
// and newer, Fable 5.1, Sonnet 4.6 and Opus 5 fell through to the 200,000 catch-all against a
// published 1,000,000. Five times low, with ok=true, so no caller ever saw "unknown"; they saw a
// confident wrong number. That is issue #233, and its live consumer was ExtractLLM.inputLimit, which
// reads this table directly when `model.model` is pinned in config: a 5x-low budget means large tool
// outputs are judged not to fit and are silently skipped, on exactly the long-context models the
// component exists for.
//
// The windows here follow what LiteLLM publishes, which is also the resolver that answers ahead of
// this table whenever the map is reachable. This is the floor for when it is not.
//
// IT IS STILL NEVER EXACT — see WindowExact. A caller deciding "is this transcript 90% full" must
// refuse a figure from this table however right it looks, because being right about today's model
// list is not the same as being authoritative about tomorrow's.
func DefaultStatic() Static {
	return Static{table: []staticEntry{
		// The 1M Claude families, most specific first for readability — though the lookup no longer
		// depends on order, see Window.
		{"claude-opus-5", 1000000},
		{"claude-sonnet-5", 1000000},
		{"claude-fable-5", 1000000},
		{"claude-opus-4-8", 1000000},
		{"claude-opus-4-7", 1000000},
		{"claude-opus-4-6", 1000000},
		{"claude-sonnet-4-6", 1000000},
		// 200K Claude families. haiku-4-5 is stated rather than left to the catch-all so that the
		// test which pins every shipped id has something to point at, and so a future haiku with a
		// different window fails that test instead of inheriting this one.
		{"claude-haiku-4-5", 200000},
		{"claude-opus-4", 200000},
		{"claude-sonnet-4", 200000},
		{"claude", 200000},
		{"gpt-5", 400000}, {"gpt-4o", 128000}, {"gpt-4", 128000}, {"o1", 200000}, {"o3", 200000},
		{"gemini-2", 1000000}, {"gemini", 1000000}, {"llama", 128000}, {"mistral", 32000},
	}}
}

// WindowExact: NEVER exact, and adding correct entries for today's models does not change that.
// Every answer here is a substring match against a deliberately tiny table ending in a
// `{"claude", 200000}` catch-all, so a model with no entry of its own still gets its family's floor
// rather than its own window. A floor is what this table is for; a fill percentage is not something
// it can answer, and the difference between "right about the models I listed" and "authoritative"
// is exactly what exact=false encodes.
func (s Static) WindowExact(ctx context.Context, model string) (int, bool, bool) {
	w, ok := s.Window(ctx, model)
	return w, false, ok
}

// Window resolves by LONGEST matching substring, not by first match.
//
// It used to be first-match-wins with a comment asking the reader to keep the table "most-specific
// first". That made correctness depend on nobody ever appending in the wrong place — and a table
// whose invariant is a comment is a table that gets appended to in the wrong place. Adding
// `{"claude-opus-5", 1000000}` after the `{"claude", 200000}` catch-all would have been a one-line
// change that silently did nothing.
//
// Longest-match makes the order cosmetic: `claude-opus-4-8` beats `claude-opus-4` beats `claude`
// because it is longer, wherever each sits in the slice.
func (s Static) Window(_ context.Context, model string) (int, bool) {
	m := strings.ToLower(model)
	best, longest := 0, 0
	for _, e := range s.table {
		if len(e.substr) > longest && strings.Contains(m, e.substr) {
			best, longest = e.window, len(e.substr)
		}
	}
	if longest == 0 {
		return 0, false
	}
	return best, true
}

// Chain tries each resolver in order; the first ok wins.
type Chain []Resolver

// Unresolved forwards the question to whichever element can answer it, so a caller holding a Chain —
// which is what the proxy is given — can still find out that the operator's document never loaded. A
// Chain whose live resolver is failing still ANSWERS every window lookup, from the fallback table
// behind it, which is exactly why this has to be askable through the Chain and not only on the element.
func (c Chain) Unresolved() (error, int) {
	for _, r := range c {
		if u, ok := r.(interface{ Unresolved() (error, int) }); ok {
			if err, n := u.Unresolved(); n > 0 {
				return err, n
			}
		}
	}
	return nil, 0
}

func (c Chain) Window(ctx context.Context, model string) (int, bool) {
	for _, r := range c {
		if w, ok := r.Window(ctx, model); ok {
			return w, true
		}
	}
	return 0, false
}

// WindowExact resolves as Window does and reports whether the link that ANSWERED considers its
// figure measured. First ok still wins — the exactness of a later link is irrelevant, because a
// later link was never consulted.
func (c Chain) WindowExact(ctx context.Context, model string) (int, bool, bool) {
	for _, r := range c {
		if w, exact, ok := Exact(r, ctx, model); ok {
			return w, exact, true
		}
	}
	return 0, false, false
}

// CompactionPoint resolves an operator-stated compaction point (billed input tokens). The first link that
// states one wins. Links that cannot state one are skipped.
func (c Chain) CompactionPoint(ctx context.Context, model string) (int, bool) {
	for _, r := range c {
		if cp, ok := r.(interface {
			CompactionPoint(context.Context, string) (int, bool)
		}); ok {
			if n, found := cp.CompactionPoint(ctx, model); found {
				return n, true
			}
		}
	}
	return 0, false
}

// OperatorCompactionPoint asks a resolver for the operator's stated compaction point (billed input tokens).
// ok=false when the resolver cannot state one or states none. The capability is optional, as WindowExact is.
func OperatorCompactionPoint(r Resolver, ctx context.Context, model string) (int, bool) {
	cp, has := r.(interface {
		CompactionPoint(context.Context, string) (int, bool)
	})
	if !has {
		return 0, false
	}
	return cp.CompactionPoint(ctx, model)
}

// Price tries each element that can price a model; the first ok wins. Elements
// that only resolve windows are skipped, so a Chain{LiteLLM, Static} prices from
// LiteLLM and reports unknown when it has not loaded.
func (c Chain) Price(ctx context.Context, model string) (Price, bool) {
	for _, r := range c {
		if p, ok := r.(Pricer); ok {
			if pr, found := p.Price(ctx, model); found {
				return pr, true
			}
		}
	}
	return Price{}, false
}
