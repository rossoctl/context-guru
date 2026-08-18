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

// Mode is context-guru's operating mode for one request. The host sets it explicitly
// (proxy Options / config `mode:`); it is NEVER inferred.
//
//	ModeSync    — compact inline; the caller waits and the compacted request is sent.
//	              The default, byte-identical to pre-mode behavior.
//	ModeObserve — the pipeline runs on a copy whose output is discarded. The agent
//	              receives the untouched original; results land in a strictly separate
//	              (hypothetical) metric namespace.
//
// An async mode — deferring compaction off the request path — is designed and
// implemented on a separate branch (#31/#35), held back because its measured benefit
// collapsed once the expensive component stopped running on caching backends. Mode is a
// closed set here so an unknown value fails loudly rather than silently meaning sync.
type Mode string

// The operating modes. See Mode.
const (
	ModeSync    Mode = "sync"
	ModeObserve Mode = "observe"
)

// ParseMode validates a configured mode string; empty means sync.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeSync:
		return ModeSync, nil
	case ModeObserve:
		return ModeObserve, nil
	}
	return ModeSync, fmt.Errorf("mode must be sync|observe, got %q", s)
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
	// ColdCache is true when this session has been idle longer than the provider's prompt
	// cache TTL, so the cached prefix CacheAware exists to protect is certainly gone.
	//
	// It is deliberately INFORMATION, not an automatic lifting of the tail gate. Every
	// offloader could safely rewrite the whole transcript on such a turn, but flipping
	// TailOnly here would change what mask, failed_run and collapse do on live traffic the
	// moment this shipped — including for deployments that asked for none of it. So the
	// gate stays where it is and a component opts in (see extract_llm's cold_cache).
	//
	// False means "warm, or unknown". A new session, an evicted tracker entry and the first
	// turn after a restart all read false: acting on a fabricated idle time would invalidate
	// a live cache, which costs a full cache-write of the suffix at 1.25x the fresh rate.
	ColdCache bool
	// ModelName is the id of the model this request targets, so a component that reuses it
	// (model.source: incoming, the default for the LLM components) can say WHICH model it
	// called instead of recording an empty string.
	ModelName string
	// SelfRates are the per-token rates for the model a component would call itself — the
	// incoming model's, since that is what `source: incoming` uses. Zero when unknown.
	SelfRates TokenRates
	// IdleMs is how long this session was idle before this request, in milliseconds; 0 when
	// there is no previous turn on record. Carried alongside ColdCache so a component can
	// demand MORE idle time than the provider TTL implies, and so the figure can be
	// reported rather than re-derived.
	IdleMs int64
	// MaxCachedIdx is the highest req.Input index considered already committed to the
	// provider cache (the messages present on the previous turn of this session).
	// -1 = unknown/first turn/cache off ⇒ no tail restriction. Only meaningful when
	// CacheAware is true.
	MaxCachedIdx int
	// FilterStats receives cmdfilter's per-filter ledger (which command families pay
	// off, and which output shapes matched nothing). nil = not recording.
	//
	// Read it through Stats(), never directly: in observe mode nothing is forwarded, so
	// recording into an enforced-namespace field would report savings that never happened.
	FilterStats FilterStatsSink
	// ExistingBreakpoints is how many prompt-cache breakpoints the RAW request already
	// carries, counted across `system`, `tools` and `messages` — which is what the
	// provider's cap of four applies to. A component that spends breakpoint slots must
	// budget against this number rather than what it can see in Input, for two reasons
	// measured on real Claude Code traffic: 2 of its 3 breakpoints live in the
	// top-level `system` array, which components never see at all, and the third sits
	// on a `tool_result` block whose mark the host's own normalize step drops. Both
	// were invisible, so the budget came out as 3 free slots when only 1 was free —
	// enough to put 6 on the wire and take a 400 (issue #32). The host fills it from
	// the raw body; 0 means "unknown, fall back to what you can see".
	ExistingBreakpoints int
	// Mode is the operating mode this request runs under. Components that behave
	// differently off the request path read this rather than inferring anything.
	Mode Mode
}

// effMode is Ctx.Mode with the zero value normalized to sync, so a Ctx built by older
// code (or a test) reports the default rather than an empty mode string.
func (c *Ctx) effMode() Mode {
	if c == nil || c.Mode == "" {
		return ModeSync
	}
	return c.Mode
}

// FilterStatsSink records cmdfilter's per-filter/per-family ledger. metrics.Aggregator
// implements it; the pipeline depends only on this interface. Implementations must be
// safe for concurrent use.
type FilterStatsSink interface {
	// FilterAct notes one applied filter: its family (builds/tests/iac/pkg/net/...),
	// its name, the content key (so a compaction re-sent verbatim next turn is counted
	// once), and the tokens saved.
	FilterAct(family, filter, contentKey string, saved int)
	// FilterMiss notes a selector that matched no filter — the ledger that says which
	// filter is worth writing next (after rtk's parse_failures table).
	FilterMiss(selector string)
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

// Stats returns the per-filter ledger sink, or nil in observe mode.
//
// Observe computes what compaction WOULD have done and forwards the request untouched, so
// every enforced-namespace metric must stay zero: a figure that cannot be told apart from
// a real saving is worse than no figure, because it silently inflates the product's own
// headline. The savings totals are already namespaced (potential_* / projected_*), but the
// filter ledger is not — an observe-only run was reporting real-looking `cmdfilter_families`
// and `cmdfilter_filters` entries with no mode label and no hypothetical counterpart.
//
// Gating here rather than at the call site is deliberate. A component author reaching for
// c.FilterStats has no reason to think about modes, and the next sink added to Ctx would
// reproduce the bug; an accessor makes the safe path the only convenient one.
//
// Two enforced fields are deliberately NOT suppressed in observe mode, because they are real
// rather than hypothetical: cg_added_ms_avg (a true measurement of the enforced path, which
// correctly reads ~0) and context-guru's own model spend (observe measures off-path, and that
// costs real money). Those are labelled instead — see metrics.Snapshot's observe notices.
func (c *Ctx) Stats() FilterStatsSink {
	if c == nil || c.Mode == ModeObserve {
		return nil
	}
	return c.FilterStats
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
	// ChangedIdx are the req.Input indices this component modified, filled by the
	// pipeline. The writeback layer uses them to attribute a discarded change back to
	// the component that made it (see Pipeline.RecordDiscards).
	ChangedIdx []int
	// Discarded counts changes this component made that the WRITEBACK layer then threw
	// away (bifrost could not round-trip the message, so splicing would have dropped
	// provider fields). A report carrying Discarded > 0 is a follow-up attribution, not
	// a fresh run — emitters must not count it as one. A component that mutates and is
	// then silently discarded looked identical to one that works, which is how issue
	// #32 survived two benchmark studies.
	Discarded int
	// Mode is the operating mode the run happened under, stamped by the pipeline from
	// Ctx.Mode. Emitters MUST branch on it: an observe-mode report is a HYPOTHETICAL and
	// may never be summed into enforced savings.
	Mode Mode
	// Gates counts, per named gate, how many CANDIDATES this component turned away — either
	// declined outright or resolved without a fresh decision (extract_llm's reapplied_*).
	// "acted: 0" is the one number a diagnosis can't use — it cannot tell a component
	// with nothing to do from one whose guard is misfiring, which is how eight components
	// sat at zero on a whole workload without anyone being able to say which case each
	// was in. Filled by the component via Gate(); rolled up into /stats per component.
	Gates map[string]int
	// Calls records each LLM call this component made on this request. Empty for every
	// deterministic component; one entry per model call for the two that make them.
	//
	// Assigned SERIALLY by the component, never appended to from a goroutine: a Report is
	// copied by value all over this codebase (emitters take one), so it cannot carry a lock.
	// extract_llm fans out into a pre-sized slice and assigns the result once, which is the
	// same shape its projected-output collection already uses.
	Calls []ModelCall
}

// TokenRates are per-token USD rates for the model a component would call ITSELF, so a
// component that spends money can price its own calls correctly.
//
// It exists because the alternative was a constant. extract_llm priced every call it made at
// claude-haiku rates, while the shipped default routes extraction to the AGENT's model — so a
// call on a sonnet-class model was recorded, and judged by the economic gate, at roughly a
// third of what it actually cost. MEASURED on a real session: a call recorded at $0.0276 had
// really cost about $0.083.
//
// Zero means unknown, and a caller must then fall back rather than treat a call as free.
type TokenRates struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// Zero reports whether no rate is known.
func (r TokenRates) Zero() bool {
	return r.Input == 0 && r.Output == 0 && r.CacheRead == 0 && r.CacheWrite == 0
}

// Cost prices one call's four token tiers.
func (r TokenRates) Cost(fresh, output, cacheWrite, cacheRead int64) float64 {
	return float64(fresh)*r.Input + float64(output)*r.Output +
		float64(cacheWrite)*r.CacheWrite + float64(cacheRead)*r.CacheRead
}

// ModelCall is one LLM call a component made, with what it cost and what it bought.
//
// It exists because "this component spent money" was previously a single dollar figure per
// REQUEST, priced at the agent model's rate, with no record of how many calls made it up,
// which candidate each looked at, whether it was accepted, or what the gate thought. For the
// one component that can be net-negative, that is the difference between an operator being
// able to answer "was that worth it?" and having to guess.
//
// Before/After hold the candidate's text either side of the call. They are transcript
// content, so whether they are ever PERSISTED is decided downstream by the same per-account
// capture consent that governs the diff view — this struct only carries them.
type ModelCall struct {
	Component string
	Model     string
	Strategy  string
	// Aggressiveness is the compaction target asked for, so a level's real effect on this
	// workload can be read off recorded calls instead of inferred.
	Aggressiveness string
	// Cold marks a call made during a cold-cache sweep, whose economics differ by ~12.5x.
	Cold bool
	// Escalated marks a call that fell back to the agent's own model because the transcript
	// did not fit the extraction model's window.
	Escalated       bool
	CandidateTokens int
	SavedTokens     int
	LatencyMs       float64
	// Token usage of the CALL itself, split by tier, and its cost priced with the extraction
	// model's rates rather than the agent's.
	PromptTokens     int64
	CompletionTokens int64
	CacheRead        int64
	CacheWrite       int64
	CostUSD          float64
	Accepted         bool
	// GateReason is what the economic gate concluded, including when it was overridden — the
	// counterfactual an operator needs to see after choosing to override it.
	GateReason string
	Summary    string
	Before     string
	After      string
}

// Gate records that one candidate was declined by the named gate. Names are the
// component's own vocabulary (e.g. "min_size", "no_filter_match", "marker_no_win");
// keep them stable, they are read off /stats.
func (r *Report) Gate(name string) {
	if r == nil {
		return
	}
	if r.Gates == nil {
		r.Gates = map[string]int{}
	}
	r.Gates[name]++
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
