// Package components defines context-guru's component model: the abstract API
// every context-engineering operation implements, the per-component report used
// for metrics, the runtime context handed to each component, and the pipeline
// that stacks them in configured order.
//
// The API is split by lossiness (design D3, after headroom's Rust traits) so
// reversibility is type-enforced:
//
//   - Reformat: lossless repack (re-encode, skeletonize, add cache_control).
//     No information leaves the wire, so nothing needs stashing.
//   - Offload: drops bytes and MUST return a non-optional cache_key proving it
//     stashed the original in the Store — you cannot compile an Offload that
//     silently loses data.
//
// The pipeline is fail-open (any error/panic reverts that component), applies a
// never-worse guard (a component that grows the request is reverted), and emits
// one Report per component.
package components

import (
	"context"
	"fmt"
	"time"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/store"
)

// Component is the common surface: identity + a per-request enable check.
type Component interface {
	Name() string
	Enabled(*Ctx) bool
}

// Reformat is a lossless component: it repacks the request denser in place and
// loses no information. Examples: format re-encode, code skeleton, cache_control
// injection.
type Reformat interface {
	Component
	Reformat(req *schemas.BifrostChatRequest, rep *Report, c *Ctx) error
}

// Offload is a lossy-but-reversible component: it drops bytes from the wire and
// returns the cache_keys under which it stashed the originals (via c.Store) —
// one per offloaded item. If it shrinks the request but returns no keys, the
// pipeline treats it as a failed offload and reverts (you cannot silently lose
// data) — UNLESS it set rep.Irreversible, the deliberate lossy drop a non-`full`
// marker_mode makes (summary/off: no stash, no restoration). Returning no keys
// AND leaving the request unchanged is a legitimate no-op (set rep.Skipped).
// Examples: collapse, dedup, cmdfilter, extract, smartcrush.
type Offload interface {
	Component
	Offload(req *schemas.BifrostChatRequest, rep *Report, c *Ctx) (cacheKeys []string, err error)
}

// Optional capability interfaces a component MAY also implement.

// Configurable receives its typed config block from the registry/loader.
type Configurable interface {
	Configure(raw []byte) error
}

// NeedsModel is implemented by components that call an LLM (extract's code/rlm
// strategies, summarize). A component that needs a model but finds none
// available (Ctx.Model.For returns nil) MUST degrade gracefully — fall back to a
// deterministic path or no-op — never fail the request.
type NeedsModel interface {
	NeedsModel() bool
}

// Model is the minimal LLM surface a component may call: one prompt in, text
// out. internal/cheapmodel's Anthropic and OpenAI clients implement it.
type Model interface {
	Complete(ctx context.Context, prompt string) (string, error)
}

// ModelSpec carries the LLM clients a NeedsModel component may use, resolved per
// request by the host adapter. Incoming is the proxied request's own model +
// credentials (nil when unavailable, e.g. the AuthBridge host); Static is a
// configured cheap model (nil when none is configured). A component selects one
// by its own `model.source` config via For.
type ModelSpec struct {
	Incoming Model
	Static   Model
}

// For returns the client the component's configured source asks for: "config" ->
// the static cheap model; anything else ("incoming"/unset) -> the incoming model,
// falling back to the static one when there is no incoming client. Returns nil
// when nothing is available, and the caller must degrade gracefully.
func (m ModelSpec) For(source string) Model {
	if source == "config" {
		return m.Static
	}
	if m.Incoming != nil {
		return m.Incoming
	}
	return m.Static
}

// Mode is context-guru's operating mode for one request. The host sets it
// explicitly (proxy Options / config `mode:`); it is NEVER inferred.
//
//	ModeSync    — compact inline; the caller waits and the compacted request is
//	              sent. The default, byte-identical to pre-mode behavior.
//	ModeAsync   — the request path only replays decisions that are already
//	              computed and makes no LLM call; the expensive compaction runs
//	              off-path and benefits SUBSEQUENT turns.
//	ModeObserve — the pipeline runs on a copy whose output is discarded. The agent
//	              receives the untouched original; results land in a strictly
//	              separate (hypothetical) metric namespace.
type Mode string

// The three operating modes. See Mode.
const (
	ModeSync    Mode = "sync"
	ModeAsync   Mode = "async"
	ModeObserve Mode = "observe"
)

// ParseMode validates a configured mode string; empty means sync.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeSync:
		return ModeSync, nil
	case ModeAsync:
		return ModeAsync, nil
	case ModeObserve:
		return ModeObserve, nil
	}
	return ModeSync, fmt.Errorf("mode must be sync|async|observe, got %q", s)
}

// Ctx is the per-request runtime handed to every component.
type Ctx struct {
	Ctx     context.Context
	Session string
	Store   store.Store
	Model   ModelSpec
	// Bypass short-circuits the whole pipeline (x-context-guru-bypass header).
	Bypass bool
	// CtxWindow is the model's max input tokens for THIS request, resolved by the
	// host (dynamically, via internal/modelinfo). 0 = unknown, in which case
	// fraction-based Trigger thresholds are ignored and only absolutes apply. Stored
	// as a resolved int so Trigger stays a pure, network-free, unit-testable function.
	CtxWindow int
	// CacheAware is true when this request goes to a prompt-caching backend and the
	// pipeline should avoid mutating already-cached content. When true, supersession/
	// age-based offloaders (failed_run, mask, collapse) must restrict their
	// mutations to the uncached tail (message index > MaxCachedIdx) so they don't
	// invalidate the provider's KV cache at a full→collapsed transition. Deterministic
	// tail/in-place offloaders (dedup, cmdfilter, extract) are already byte-stable on
	// the unchanged prefix and ignore this. False = legacy compact-everything.
	CacheAware bool
	// MaxCachedIdx is the highest req.Input index considered already committed to the
	// provider cache (the messages present on the previous turn of this session).
	// -1 = unknown/first turn/cache off ⇒ no tail restriction. Only meaningful when
	// CacheAware is true.
	MaxCachedIdx int
	// Mode is the operating mode this request runs under. ModeSync (the zero value
	// after the host sets it explicitly) is the default; components that behave
	// differently off-path read this rather than inferring anything.
	Mode Mode
	// Deferred marks a run that happens OFF the request path (the async worker).
	// Nothing is forwarded from it: it exists to populate the frozen state later
	// turns replay. Components may spend more time/model calls here.
	Deferred bool
	// TailCachePending turns on async mode's cache protection, and NoCacheAtOrAfter is
	// the lowest message index it covers: content that a compaction which has not landed
	// yet is expected to REPLACE. A breakpoint there would commit bytes to the provider
	// cache that we are about to rewrite, converting a 0.1x read into a 1.25x write —
	// 11.5x more expensive, and strictly worse than never going async at all.
	//
	// The protection needs its own bool rather than a sentinel index, because index 0 is
	// a legitimate value ("no breakpoint anywhere") so no integer is free to mean "off".
	// A false default also makes the zero-value Ctx unprotected rather than fully
	// blocked, which is the safe direction here: an unset field costs a missed
	// optimisation, never a wrong request. (Contrast MaxCachedIdx, whose -1 sentinel
	// fails the other way — see #25.)
	TailCachePending bool
	NoCacheAtOrAfter int
	// StripCallerBreakpoints permits taking back a cache breakpoint the CALLER set
	// inside the protected tail. Without it the protection cannot cover an agent that
	// places its own breakpoints (claude-code does), which made it a no-op on the
	// primary workload. Removing a directive an agent deliberately placed is a behavior
	// change we do not own, so it is the host's decision; the host's other option is to
	// not defer that turn at all.
	StripCallerBreakpoints bool
	// tailUnprotected is set by cacheinject when it had to decline the tail protection
	// (a caller breakpoint sat inside the protected span and stripping was not allowed).
	// The host reads it to avoid deferring a compaction it cannot protect. Written from
	// the single pipeline goroutine that owns this Ctx, read after Run returns.
	tailUnprotected bool
}

// DeclineTailProtection records that async's tail protection could not be honored on
// this request. Called by cacheinject; read by the host via TailUnprotected.
func (c *Ctx) DeclineTailProtection() {
	if c != nil {
		c.tailUnprotected = true
	}
}

// TailUnprotected reports whether DeclineTailProtection was called during this run.
func (c *Ctx) TailUnprotected() bool { return c != nil && c.tailUnprotected }

// effMode is Ctx.Mode with the zero value normalized to sync, so a Ctx built by
// older code (or a test) reports the default rather than an empty mode string.
func (c *Ctx) effMode() Mode {
	if c == nil || c.Mode == "" {
		return ModeSync
	}
	return c.Mode
}

// CacheBlocked reports whether index i must be left without a cache breakpoint
// because a not-yet-landed compaction is expected to rewrite it.
func (c *Ctx) CacheBlocked(i int) bool {
	return c != nil && c.TailCachePending && i >= c.NoCacheAtOrAfter
}

// TailOnly reports whether a supersession/age-based offloader may mutate the message
// at index i without risking the provider's cached prefix. When cache-awareness is
// off or the boundary is unknown, every index is fair game (legacy behavior).
func (c *Ctx) TailOnly(i int) bool {
	if c == nil || !c.CacheAware || c.MaxCachedIdx < 0 {
		return true
	}
	return i > c.MaxCachedIdx
}

// Report is the per-component result, modelled after lean-ctx's ToolOutput
// token accounting and headroom's record_pipeline_run inputs. The pipeline
// fills TokensBefore/After/DurationMs; the component fills CacheKey and may set
// Skipped. It feeds every Emitter.
type Report struct {
	Component    string
	Kind         string // "reformat" | "offload"
	TokensBefore int
	TokensAfter  int
	DurationMs   float64
	CacheKeys    []string // set by Offload components (one per stashed original)
	Skipped      bool     // component ran but chose not to act
	Reverted     bool     // pipeline reverted it (error/panic/never-worse)
	Irreversible bool     // Offload dropped content on purpose without stashing (marker_mode summary/off)
	Err          error
	// Mode is the operating mode the run happened under, stamped by the pipeline
	// from Ctx.Mode. Emitters MUST branch on it: an observe-mode report is a
	// HYPOTHETICAL and may never be summed into enforced savings.
	Mode Mode
	// Deferred marks a report from an OFF-PATH async run (Ctx.Deferred). Nothing it
	// produced was forwarded, so its savings must not be counted as enforced — the
	// tokens are counted when a later turn REPLAYS the frozen decision on the request
	// path. Counting both would double-count every deferred compaction.
	Deferred bool
}

// Saved returns non-negative tokens saved by this component.
func (r Report) Saved() int {
	if r.TokensAfter > r.TokensBefore {
		return 0
	}
	return r.TokensBefore - r.TokensAfter
}

// RunReport aggregates a whole pipeline run for one request.
type RunReport struct {
	Session      string
	TokensBefore int
	TokensAfter  int
	DurationMs   float64
	Components   []Report
	// Mode is the operating mode this run happened under (see Report.Mode).
	Mode Mode
	// Deferred marks an OFF-PATH async run (see Report.Deferred).
	Deferred bool
}

// Saved returns the net tokens saved across the run.
func (rr RunReport) Saved() int {
	if rr.TokensAfter > rr.TokensBefore {
		return 0
	}
	return rr.TokensBefore - rr.TokensAfter
}

// Emitter receives one Report per component and one RunReport per request.
// Defined here (not in metrics) so the pipeline has no dependency on any
// concrete telemetry backend; metrics provides the implementations.
type Emitter interface {
	Component(Report)
	Run(RunReport)
}

// NopEmitter discards all telemetry. Default when none is configured.
type NopEmitter struct{}

func (NopEmitter) Component(Report) {}
func (NopEmitter) Run(RunReport)    {}

// clock is injectable in tests; production uses time.Now.
var clock = time.Now
