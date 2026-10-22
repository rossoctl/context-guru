// Package keepalive is the idle keep-alive: the one cache mechanism that cannot be a component.
//
// # What it is for
//
// An Anthropic prompt-cache entry has a five-minute default lifetime, and a session idle
// longer than that loses its entire cached prefix. The next turn then re-bills every token
// of it at the cache-CREATION rate. GPT-5.6+ OpenAI Responses entries instead have a
// documented 30-minute minimum after the latest write or reuse and use a separate
// 28-minute schedule. Measured Anthropic traffic on this service over 19,805 requests and
// $3,139.97 of spend: 742 requests (3.7% of traffic) missed for that reason and cost
// $741.07 — 23.6% of everything — at $0.9987 each against $0.1178 for a request that hit.
// Against each row's own counterfactual the penalty is 11.35x, of which 91.2% is pure
// re-write. $584.83 of it, 18.6% of ALL spend, sits in misses whose idle gap was under an
// hour, and 47% of the misses missed the window by less than five minutes.
//
// The provider documents the two facts that make this fixable:
//
//	"By default, the cache has a 5-minute lifetime. The cache is refreshed for no
//	 additional cost each time the cached content is used."
//
//	"The lifetime is measured from the start of the request that writes or reads the cache
//	 entry, not from the end of its response."
//
// So a cache READ refreshes the lifetime, and a read costs 0.1x base input where re-creating
// the prefix costs 1.25x. One ping buys back one re-creation at 11.5:1.
//
// Replayed over the production window THROUGH THIS CODE (TestKeepAliveOnProductionSnapshot) at
// the defaults shipped when it was measured (X=280s, K=2): 912 pings costing $90.76 convert 148
// of 357 addressable misses worth $215.84, net **+$125.08** — 3.98% of spend, and 12.9x
// everything the whole transformation pipeline delivered over the same window ($9.72). The
// defaults have changed since (the idle default is now config.DefaultKeepAliveIdle; see Policy
// and the config defaults), and this replay has not been rerun at them, so read these figures
// as that measurement, not as what today's defaults deliver.
//
// # Why it is host-level and not a component
//
// A component runs while a request is in flight. The event this addresses happens when NO
// request is in flight: the agent is idle, and the entry lapses with nothing on the wire to
// hook. Nothing inside the pipeline can act at that moment, which is why this lives beside
// the request path rather than in it.
//
// # What it costs, and whose money it is
//
// A ping is a real upstream request billed to the caller's own credential. That is the trade
// — a small certain spend against a much larger probable one — and it is not ours to make on
// someone's behalf, so it is opt-in per account, capped per session, capped in count, and
// recorded as its own dashboard row with its own cost. See Policy.
//
// # The two ways this can be WRONG, and the guards
//
//  1. **A ping that writes instead of reads costs 12.5x rather than saving.** The prefix hash
//     is cumulative up to and including the breakpoint, so the ping's body must be
//     byte-identical over that prefix. Hence the ping resends the exact bytes that were sent
//     upstream, and changes only `max_tokens` and `stream` — fields outside the hashed
//     prefix. If a ping's usage nevertheless reports more written than read, the mechanism is
//     wrong for that session: it is logged at ERROR, counted, and the session is dropped
//     rather than pinged again.
//
//  2. **Pinging a session that has ENDED is pure waste.** It is not detectable: only 290 of
//     3,891 session-final requests (7.5%) end with `end_turn`, the other 3,476 end
//     mid-tool-loop and look exactly like an active session. K is therefore the control, and it
//     is why the default is small: the marginal ping's yield falls while its cost does not, so
//     the net flattens and then collapses — +$125.08 at K=2, +$130.80 at K=3, +$85.28 at K=1,
//     +$58.35 at K=12.
//
//     K=2 and K=3 are a TIE on money (the $5 between them is inside the run-to-run wobble), so
//     the default is 2 for the request-volume reason and not a dollar reason: it sends 26% fewer
//     pings on a gateway path that returned 180 HTTP 429s in the same window.
//
// A note on what this deliberately does NOT do: it does not stop pinging a session whose last
// response ended with `end_turn`. That reads like a session-end signal and is the opposite.
// `end_turn` means the model finished and control returned to a HUMAN, which is precisely the
// state that produces a long gap: P(gap > 300s) is 37.15% after `end_turn` against 0.74%
// after `tool_use`, a 6.7x lift, and 83.7% of the recoverable dollars sit behind it. Stopping
// there would discard most of the value the mechanism exists to capture.
package keepalive

import (
	"math"
	"os"
	"strings"
	"time"

	"github.com/rossoctl/context-guru/dispatch"
	"github.com/rossoctl/context-guru/internal/thread"
)

// Key names one tracked THREAD of one session (#423). Thread "" is the primary thread.
type Key struct{ Tenant, Session, Thread string }

// id is the live-map key. The primary thread keys as tenant NUL session, exactly as before
// threads existed; the NUL separator is what lets RetireSession and Forget match by prefix.
func (k Key) id() string { return k.Tenant + "\x00" + thread.Key(k.Session, k.Thread) }

// Usage is what one upstream response billed, in the provider's own units. A host fills it in
// Record for the real request, from whatever it already parses; for a ping the keeper reads it
// itself from the response body the Dispatcher hands back (PingUsage).
type Usage struct {
	FreshInput, CacheRead, CacheWrite, Output int64
	// CacheWrite1h is the part of CacheWrite billed at the one-hour tier.
	CacheWrite1h int64
	// StopReason is the response's own terminal stop reason; read only by a policy predictor.
	StopReason string
}

// Price is one model's per-token rates in USD — dispatch.Price, so every host's
// Dispatcher.Price and the keeper price with one formula (Price.CostWithCacheWrite1h).
type Price = dispatch.Price

// Policy is one tenant's resolved cache policy — the host-level half of the cache
// story, which no component can reach.
//
// A plain struct in this package rather than config.CacheConfig, for the reason
// ConfigBuilder exists: proxy does not depend on the configuration loader, so a library
// host or the bifrost adapter can set this directly.
type Policy struct {
	// KeepAlive turns the mechanism on for this tenant. Off by default.
	KeepAlive bool
	// Idle is X: how long a session must be idle before the first ping, measured from the
	// previous request's START per the provider's documented lifetime rule. Zero disables.
	Idle time.Duration
	// OpenAIIdle is the Responses interval for GPT-5.6+ implicit caches. Zero
	// uses the 28-minute default; it is separate from Anthropic's five-minute
	// schedule so enabling OpenAI does not send wasteful four-minute pings.
	OpenAIIdle time.Duration
	// MaxPings is K: the most pings one idle span may send. Zero disables.
	MaxPings int
	// MaxUSDPerPing refuses a ping whose projected cost exceeds this. PER PING, because ping
	// cost is bimodal (p50 $0.0004, p99 $0.2275, max $0.3780) while a per-SESSION budget
	// truncates exactly the long large-prefix sessions holding the value — capping the
	// window's pings per session at 20 drops the net from +$164 to $92.34.
	//
	// Read it through Ceiling(), never directly: zero here means UNCONFIGURED, and for a spend
	// guard that has to resolve to the default rather than to infinity.
	MaxUSDPerPing float64
	// MinPrefixTokens is the billed-prefix floor (the previous request's cache_read +
	// cache_write) a session must reach before it is pinged. With the first-request skip this
	// sends 8.7x fewer pings for 3.3% less money.
	MinPrefixTokens int
	// HeadTTL1h asks for the one-hour tier on the head breakpoints; HeadTTLMinTokens gates
	// it on request size. Passed through to apply.Opts — see apply/headttl.go.
	HeadTTL1h        bool
	HeadTTLMinTokens int
	// PredictorID optionally names a registered predictor (see predictorFor, and the proxy
	// host's knownPredictorIDs in proxy/keepalivestrategy.go) that pingable() must also clear before a
	// ping fires. "" (every account today, and every strategy created before this field
	// existed) means no predictor gate at all — unchanged behaviour.
	PredictorID string
	// PredictorThreshold is the minimum probability PredictorID must return. Ignored
	// while PredictorID is "".
	PredictorThreshold float64
	// ManualHold is the host's reading of an opt-in on THIS request (the proxy: the
	// x-context-guru-ping header): keep this thread for the Ping button even though no automatic
	// ping will be sent for it. It never turns the schedule on, and never widens what is held: the
	// entry lives ManualHoldMax and is released and zeroized then. See manualhold.go.
	ManualHold bool
}

// on reports whether this policy can ping at all.
func (p Policy) on() bool {
	return p.KeepAlive && p.Idle > 0 && p.MaxPings > 0
}

// On reports whether this policy can ping at all. The exported form of on(), for hosts.
func (p Policy) On() bool { return p.on() }

// Keeper bounds. Both are memory bounds and both are needed: a few enormous sessions and a
// great many small ones are different ways to exhaust the same 8 GiB.
const (
	// MaxSessions bounds the tracked ENTRIES, which since #423 are threads, not sessions:
	// one session holds one entry per thread that passed the gates (main agent, each subagent).
	// The name is kept so the bound stays greppable from its history. 512 still holds: an entry
	// exists only for a thread past its first request with a prefix over the floor, a finished
	// thread's entry ends at the hard deadline ((K+1) x Idle, about 14 min at the defaults), and
	// the body bound below binds first. evictLocked has no per-session fairness on purpose: it
	// drops the entry whose ping is least imminent, whichever session it belongs to.
	MaxSessions = 512
	// MaxBytes bounds the total request bodies held for replay.
	//
	// THIS is the binding ceiling, not MaxSessions — tune this one. A gated session
	// carries at least a 20k-token prefix, so ~100 KB to 1 MiB of body, and 128 MiB therefore
	// binds around 130 large sessions: well before the 512-session count. The replay reports a
	// peak of 11 live sessions, but it drives ~350-byte synthetic bodies, so it exercises the
	// count bound and says nothing about this one.
	MaxBytes = 128 << 20
	// MaxTurnKeys bounds the per-thread turn counter (per session before #423).
	// Larger than the entry bound because it holds one int rather than a body, and it has to
	// outlive the entry.
	MaxTurnKeys = 20000
	// MaxBodyBytes refuses to hold a single body larger than this. A body this big
	// is a multi-million-token request whose ping would itself cost real money, and holding
	// one would spend a fifth of the whole budget on one session.
	MaxBodyBytes = 8 << 20
	// tick is how often the sweep runs. The window it is protecting is 300s wide and the
	// default idle (config.DefaultKeepAliveIdle) fires well inside it, so a 2s granularity
	// spends at most 2s of the slack that leaves.
	tick = 2 * time.Second
)

// Ceiling is the per-ping cost guard that is actually ENFORCED.
//
// The whole of this method is the sentinel. `MaxUSDPerPing == 0` means "nobody configured
// one", and the shipped code read that as "no cap": `MaxUSDPerPing <= 0 || cost <= cap`. On
// an account whose keep-alive is OFF the config builder hands the request path a zero
// Policy, and enabling one session by hand is precisely the use case a per-session
// override exists for — so the one path where the guard was written to matter was the one
// path that ran without it, while the arm response cheerfully reported a ceiling of $0.00.
//
// Zero in a money guard resolves to the DEFAULT. A caller that genuinely wants no ceiling has
// to say so with a negative value, which is a thing somebody has to type on purpose.
func (p Policy) Ceiling() float64 {
	switch {
	case p.MaxUSDPerPing > 0:
		return p.MaxUSDPerPing
	case p.MaxUSDPerPing < 0:
		return math.Inf(1)
	default:
		return DefaultMaxUSDPerPing
	}
}

// DefaultMaxUSDPerPing mirrors config.DefaultKeepAliveMaxUSDPerPing, which this package may not
// import (proxy does not depend on the configuration loader). They are pinned equal by
// TestTheProxysPingCeilingDefaultMatchesTheConfigLoaders.
const DefaultMaxUSDPerPing = 0.50

// Disabled is the operator's kill switch, read once at construction. A single
// environment variable rather than a control-plane route: the thing an operator needs at
// 3am is one that works when the control plane is what is broken.
func Disabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CONTEXT_GURU_KEEPALIVE")))
	return v == "off" || v == "0" || v == "false"
}
