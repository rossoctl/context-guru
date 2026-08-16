package apply

import (
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

	// Mode is the operating mode. Empty means components.ModeSync, so a caller that does
	// not know about modes gets exactly today's behavior.
	Mode components.Mode
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
