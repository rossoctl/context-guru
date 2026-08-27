package apply

import (
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/components"
	"github.com/rossoctl/context-guru/modes"
)

// Opts is BodyOpts' input: everything the positional BodyFull takes, plus the operating
// mode (#31) and the per-session boundary tracker.
//
// A struct rather than more positional arguments: the parameter list was already at the
// limit of readability, and these fields are set by one host only.
type Opts struct {
	Provider bschemas.ModelProvider
	Body     []byte
	// Session is the host-supplied session id ("" => content hash).
	Session string
	// Tenant namespaces the session id in a hosted, multi-tenant deployment. Empty
	// in single-tenant use, which leaves session keys byte-identical to before this
	// field existed.
	//
	// It has to exist because the fallback session id is a content hash of the system
	// prompt plus the first user message. Two people running the same agent on the
	// same repository produce the SAME hash — so without a namespace they would share
	// one session key, and therefore one sticky offload set and one cached-prefix
	// boundary. That is a cross-tenant state collision arrived at by nobody doing
	// anything wrong.
	Tenant string
	Bypass bool
	Models components.ModelSpec
	// Window is the model's resolved context window (max input tokens; 0 = unknown).
	Window int
	// CacheMode is "auto" (default) | "on" | "off" — see resolveCacheAware.
	CacheMode string
	// Now is the clock, injected so idle-time reasoning is testable. Zero means time.Now().
	Now time.Time
	// SelfRates are the per-token rates of the model a NeedsModel component would call via
	// `model.source: incoming` — i.e. the request's own model. Supplied by the host, which is
	// the only layer with a Pricer. Zero means unknown and the component falls back.
	SelfRates components.TokenRates
	// RatesFor resolves the operator's rate card for a NAMED model, so a component that
	// compacts with a model other than the request's own can price its own calls from the
	// same card the bill is computed from. nil means unavailable; the component falls back.
	RatesFor func(model string) components.TokenRates

	// Mode is the operating mode. Empty means components.ModeSync, so a caller that does
	// not know about modes gets exactly today's behavior.
	Mode components.Mode
	// HeadTTL1h asks for the provider's one-hour cache tier on the HEAD breakpoints
	// (`tools`, `system`) while the trailing message breakpoint stays at five minutes —
	// the documented mixed-TTL shape. Off by default; see headttl.go for the measured reason,
	// which is that Bedrock grants the tier for the Claude 4.5 family and silently downgrades
	// it for the Opus 5 / Sonnet 5 models this service actually runs.
	//
	// NOTE for anyone reading the cold-cache logic: setting this makes bodyAsksExtendedTTL
	// true, so cacheTTL returns 1h and cacheIsCold becomes correspondingly permissive. That is
	// the right behaviour when the tier is granted and a deliberate over-estimate when it is
	// not (the safe direction — believing a cache is warm only forgoes an optimisation). It
	// cannot affect anything today because this is off, but it is the one way this field
	// touches shared behaviour.
	HeadTTL1h bool
	// HeadTTLMinTokens is the size gate on HeadTTL1h, in estimated tokens. 0 disables the
	// upgrade entirely rather than defaulting, so a host that forgets to resolve its
	// configuration asks for nothing instead of asking on every request.
	HeadTTLMinTokens int
	// PrefixAsk, when set, lets a component put a question to the request's own model with the
	// previous turn's SENT body as the cached prefix. See components.PrefixAsker. nil => a component
	// that wants one gets none and decides for itself; today's behaviour for every caller that does
	// not set it.
	PrefixAsk components.PrefixAsker
	// Tracker, when set, owns the per-session cached-prefix boundary. Supplying it also
	// removes the concurrent-turn race in the legacy read-then-deferred-write of prevLen.
	// nil => the legacy store-backed path, unchanged for library callers and /compact.
	Tracker *modes.Tracker
}

// Result is BodyOpts' output.
//
// The embedded Trace carries everything observational: the resolved Session, the
// pipeline's Run report (which observe mode reads as its ONLY output, since the body
// is thrown away), the cache-awareness facts, and each rewritten message's
// before/after text for the dashboard. It is embedded rather than duplicated so
// there is exactly one Session and one Run in the codebase — two copies of the same
// value is how one of them goes stale.
type Result struct {
	// Body is the body to forward. Always valid: on any trouble it is the input.
	Body []byte
	// Changed is false when Body is the untouched input.
	Changed bool
	Trace
}

// nowMs is the request's wall clock in unix milliseconds.
func (o Opts) nowMs() int64 {
	if o.Now.IsZero() {
		return time.Now().UnixMilli()
	}
	return o.Now.UnixMilli()
}
