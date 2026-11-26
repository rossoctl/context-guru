package accrec

import (
	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/keepalive"
	"github.com/rossoctl/context-guru/thread"
)

// ThreadID maps the primary thread to "main", the library's name for it.
func ThreadID(id string) string {
	if id == thread.Primary {
		return "main"
	}
	return id
}

// PingKey is the key the ping store keeps one thread's ping facts under: the session and the
// thread id as the accountant sees it ("main" for the primary thread).
func PingKey(session, threadID string) string { return session + "\x00" + threadID }

// PingLookup reads the ping facts a thread's next request takes. A nil store has none.
type PingLookup interface {
	Peek(key string) (accountant.PingFacts, bool)
}

// BaseTTL and OneHourTTL are the two Anthropic lifetimes, from the one table (modelinfo). The proxy reads
// them here and writes no duration of its own (#1350).
const (
	BaseTTL    = modelinfo.AnthropicCacheTTL5m
	OneHourTTL = modelinfo.AnthropicCacheTTL1h
)

// EntryFacts builds the facts the library needs about the entry this request wrote. A keep-alive
// ping for the thread since its last request is read from pings (nil = none; the capture drops it
// once the request is priced) and supplies
// LastPing, PingWindow and the ping's usage. PingRead stays nil: the keeper replays the previous
// request's forwarded body, so the library derives the extent the ping read from its usage
// (accountant design §5.2).
//
// The Lifetime kind comes from apply.CacheLifetime, the same rule the cold gate uses: an exact expiry
// for a provider with explicit breakpoints, a minimum for OpenAI's 30-minute entries, else unknown.
// An unknown lifetime cannot prove an expiry, so the library would never rescue after a gap past
// the TTL (design §5.2). The TTL is 1h only when EVERY breakpoint of the forwarded body asks for it,
// as the keeper decides: a 1h head over a 5m tail leaves a tail that expires in five minutes. The
// keeper also needs the billed tier, which is known only after the response. These facts are written
// before it, so they hold the ASKED tier. BilledTTL corrects it once the usage arrives, before the
// library prices the request (context-guru#919).
func EntryFacts(provider bschemas.ModelProvider, model string, forwarded []byte, pings PingLookup,
	session string, t accountant.Thread) accountant.EntryFacts {
	facts := accountant.EntryFacts{TTL: BaseTTL, Lifetime: accountant.LifetimeUnknown}
	// The kind comes from the cold gate's rule. The duration is taken again with the keeper's tier
	// (every breakpoint asks 1h): apply.CacheLifetime lengthens it for a single 1h ask.
	switch ttl, kind := apply.CacheLifetime(provider, model, forwarded); kind {
	case apply.CacheLifetimeExact:
		facts.TTL, facts.Lifetime = apply.EntryLifetime(apply.CacheFacts{Provider: provider, Model: model,
			LastOneHour: apply.AllBreakpointsAskExtendedTTL(forwarded)}),
			accountant.LifetimeExact
	case apply.CacheLifetimeMinimum:
		facts.TTL, facts.Lifetime = ttl, accountant.LifetimeMinimum
	}
	if pings != nil {
		if pf, ok := pings.Peek(PingKey(session, t.ID)); ok {
			pf.Apply(&facts)
		}
	}
	return facts
}

// BilledTTL decides the entry TTL after the response, when a gateway downgraded the tier the request asked for
// (1h). It runs after the usage arrives and before Compute, which reads the facts' TTL for the verdict and stores it
// as the entry's TTL. Without it the library treats a gap of 5 to 60 minutes as hot and can overstate a rescue by up
// to 55 minutes of idle time.
//
// The rule is keepalive.HeadLifetimeOneHour's (the accountant lowers only when nothing was billed at 1h): a write
// lives 1 hour when any token of it was billed at 1 hour, so the 1-hour head of a head-TTL write keeps its tier and
// only a write with no 1h token lowers the entry. The keeper keeps its own whole-entry rule for the ping plan
// (keepalive.BilledOneHour). A turn that wrote nothing bills none and keeps the thread's last billed tier (issue #1078). A
// thread with no earlier write has none to keep, so the asked tier stands. Only an exact lifetime has a TTL
// that can be lowered: a minimum (OpenAI) has no tier. SharedWarm was judged with the asked TTL at the start
// and is not redone; it matters only when another thread of the session touched the entry.
//
// key is the thread's PingKey. It returns lower=true when the caller must set the facts' TTL to 5 minutes
// and lower the TTL of every 1h breakpoint of the record with it. The tier store learns from the usage.
func BilledTTL(tiers *TierStore, key string, f accountant.EntryFacts, cacheWrite, cacheWrite1h int64) (lower bool) {
	if f.Lifetime != accountant.LifetimeExact || f.TTL <= BaseTTL {
		return false
	}
	prev, known := tiers.Get(key)
	if !known {
		prev = true // no earlier write: the asked tier stands on a read-only turn
	}
	oneHour := keepalive.HeadLifetimeOneHour(cacheWrite, cacheWrite1h, prev)
	if cacheWrite > 0 || oneHour {
		tiers.Set(key, oneHour) // a read-only turn leaves the remembered tier alone
	}
	return !oneHour
}
