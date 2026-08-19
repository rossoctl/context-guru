package dash

import (
	"strings"

	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/modelinfo"
)

// Token-accounting honesty levels. A request is only `complete` when the provider
// told us all four token tiers; `partial` means we have content-token counts but
// no billed usage (so cost is an estimate at best); `missing` means we have
// neither. The UI must never render a partial row as exact — that is how a
// dashboard becomes unfalsifiable.
const (
	AccountingComplete = "complete"
	AccountingPartial  = "partial"
	AccountingMissing  = "missing"
)

// Cache-miss attribution buckets. cold_start is NOT a failure: the first request
// of a session, or the first for a given model, has nothing to hit. TTL wins ties
// against prefix_change — a prefix that changed after the cache had already
// expired was not the cause.
const (
	CacheHit          = "hit"
	CacheColdStart    = "cold_start"
	CacheTTLExpiry    = "ttl_expiry"
	CachePrefixChange = "prefix_change"
	CacheUnknown      = "unknown"
)

// "Why didn't you compact this?" — a first-class reason bucket, not an absence of
// data. An empty string means we DID compact.
const (
	ReasonBypassed     = "bypassed"      // x-context-guru-bypass on this request
	ReasonNoMessages   = "no_messages"   // nothing to operate on
	ReasonBelowTrigger = "below_trigger" // every component's trigger declined
	ReasonAllFrozen    = "cache_frozen"  // eligible tail was empty (cache safety)
	ReasonNoSavings    = "found_nothing" // components ran, found nothing to remove
	ReasonReverted     = "reverted"      // components acted but were all reverted
)

// Operating modes, as rendered in the UI.
const (
	ModeActive  = "active"
	ModeBypass  = "bypass"
	ModeObserve = "observe"
)

// Event is one captured request, as handed to the capture channel. It is built on
// the request goroutine from values the request path already computed (no extra
// token counting, no extra allocation of the transcript) and is then owned
// entirely by the writer goroutine.
type Event struct {
	ID int64 `json:"id"`
	TS int64 `json:"ts"` // epoch ms
	// TenantID owns this row; "" in single-tenant deployments.
	TenantID  string `json:"tenant_id"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Provider  string `json:"provider"`
	Agent     string `json:"agent"`
	Preset    string `json:"preset"`
	Mode      string `json:"mode"`
	Route     string `json:"route"`
	Status    int    `json:"status"`

	Bypassed   bool `json:"bypassed"`
	CacheAware bool `json:"cache_aware"`
	Messages   int  `json:"messages"`

	TokensBefore    int `json:"tokens_before"`
	TokensAfter     int `json:"tokens_after"`
	AttemptedTokens int `json:"attempted_tokens"`
	FrozenTokens    int `json:"frozen_tokens"`
	SavedUnique     int `json:"saved_unique"`

	FreshInput   int64 `json:"fresh_input"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	OutputTokens int64 `json:"output_tokens"`

	CostUSD         float64 `json:"cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	CGLLMCostUSD    float64 `json:"cg_llm_cost_usd"`
	// CacheSavedUSD is what the provider's prompt cache saved on this request: its
	// cache-read tokens priced at the fresh rate they would have cost with no cache,
	// minus what they actually cost. A measurement of the PROVIDER's mechanism, kept
	// because it is the number that falls when a compaction pipeline destroys a prefix —
	// and reported nowhere as a saving of ours, because it is not one.
	CacheSavedUSD float64 `json:"cache_saved_usd"`
	// CachesplitSavedUSD is what the volatile-tail split saved, and it is the only cache
	// figure this project claims. See Price.
	CachesplitSavedUSD float64 `json:"cachesplit_saved_usd"`
	// SplitStableTokens is the size of the prefix half cachesplit moved the breakpoint
	// onto — the tokens it moved out of the cache-creation tier, and the numerator of the
	// figure above. Persisted so the dollar number can be checked against a token count
	// instead of taken on trust.
	SplitStableTokens int `json:"split_stable_tokens"`
	// SplitTailHash identifies the volatile half on this request. Persisted for two reasons:
	// it seeds the per-session comparison across a restart, and it lets the whole figure be
	// recomputed from the table rather than trusted.
	SplitTailHash uint64 `json:"split_tail_hash"`

	CGLatencyMs float64 `json:"cg_latency_ms"`
	UpstreamMs  float64 `json:"upstream_ms"`

	Expands      int `json:"expands"`
	ExpandTokens int `json:"expand_tokens"`
	Reverts      int `json:"reverts"`

	TokenAccounting    string `json:"token_accounting"`
	CacheMissReason    string `json:"cache_miss_reason"`
	UncompressedReason string `json:"uncompressed_reason"`

	// Meta is the request's own knobs, embedded so the JSON stays flat.
	Meta

	Components []CompRow    `json:"components,omitempty"`
	Content    []ContentRow `json:"content,omitempty"`
	// Extractions is one row per LLM call an expensive component made on this request.
	Extractions []ExtractionRow `json:"extractions,omitempty"`

	// TailChanged says the volatile tail differs from this session's previous request (or that
	// this is its first). It is the condition that makes a cache hit attributable to the
	// split: only on such a turn would the unsplit block have been re-created.
	//
	// It replaced "the session's first request", which was the wrong test on real traffic.
	// Measured on this deployment: 1,105 of 1,127 session starts were COLD — the previous
	// session's cache had expired before the next began — so a figure gated on the session's
	// first request was structurally near zero while the component was in fact serving the
	// system prompt from cache on hundreds of mid-session turns.
	TailChanged bool `json:"-"`

	// SessionFirst marks the first request captured for this session, which is what makes a
	// cache hit attributable (see Price). Not persisted because it does not need to be: a
	// nonzero cachesplit_saved_usd identifies exactly the rows that qualified, and
	// split_stable_tokens, cache_read and cache_write are all stored beside it, so the
	// figure can be recomputed from the row. (It is NOT recoverable from
	// cache_miss_reason, which reads "hit" on every qualifying row — AttributeCache
	// short-circuits on cache_read > 0 before it ever considers a cold start.)
	SessionFirst bool `json:"-"`

	// ContentCap is the per-blob byte cap Redact applies. Set at the capture site and
	// consumed by the writer goroutine; not persisted (a knob, not a fact about the
	// request).
	ContentCap int `json:"-"`
}

// Meta is the request's own metadata — the knobs the CLIENT chose, plus the stop
// reason the provider answered with. Captured because they are the levers that explain
// a cost: reasoning effort and thinking budget buy output tokens, and where the
// cache_control breakpoints sit decides how much of the prefix was billed as a read
// rather than a write, which is the whole subject of this project.
//
// Real columns rather than a JSON blob, deliberately. Every field here is either
// GROUPED BY in an aggregate (effort, thinking mode, stop reason, tool_choice, the
// breakpoint counts) or a scalar rendered on the request row; a JSON column would put
// json_extract() in the middle of every one of those queries and give the free-text tail
// somewhere to hide from the redactor. There is no long tail to justify one.
//
// Anthropic and OpenAI spell the same knobs differently; capture normalizes them (see
// proxy.metaFromBody) so one column means one thing across dialects.
type Meta struct {
	// ReasoningEffort is Anthropic's `output_config.effort` or OpenAI's
	// `reasoning_effort` — low|medium|high|xhigh|max, "" when unset. NOT a synonym for
	// thinking: on current Anthropic models effort is the depth control and
	// thinking.budget_tokens is the removed predecessor, so both are recorded.
	ReasoningEffort string `json:"reasoning_effort"`
	// ThinkingMode is Anthropic `thinking.type` — adaptive|enabled|disabled, "" absent.
	ThinkingMode string `json:"thinking_mode"`
	// ThinkingBudget is `thinking.budget_tokens` (pre-4.6 models only; 0 = unset).
	ThinkingBudget int `json:"thinking_budget"`
	// Temperature and TopP are POINTERS, and the column is NULLABLE, because "the client
	// did not set it" and "the client set it to 0" are different facts and 0 is a
	// legitimate value for both. A sentinel would make a deterministic request
	// indistinguishable from an unspecified one.
	Temperature *float64 `json:"temperature"`
	TopP        *float64 `json:"top_p"`
	// MaxTokens is the client's output cap (`max_tokens`, or OpenAI's
	// `max_completion_tokens`); 0 = unset.
	MaxTokens int  `json:"max_tokens"`
	Stream    bool `json:"stream"`
	// ToolChoice is the normalized forcing mode: auto|any|none|required|tool for the
	// object form, or the bare string OpenAI sends. The forced tool's NAME is
	// deliberately not stored — it is unbounded client text with no aggregate use.
	ToolChoice string `json:"tool_choice"`
	// Tools is how many tools the request declared; SystemBlocks how many blocks the
	// top-level `system` array carried (1 for a bare string).
	Tools        int `json:"tools"`
	SystemBlocks int `json:"system_blocks"`
	// The prompt-cache breakpoints ON ARRIVAL, by location: `tools` and `system` render
	// ahead of `messages`, so location decides how much prefix a breakpoint protects.
	// Zero on a request the pipeline never inspected (observe mode), like every other
	// pipeline-derived field on such a row.
	CacheBPSystem   int `json:"cache_bp_system"`
	CacheBPTools    int `json:"cache_bp_tools"`
	CacheBPMessages int `json:"cache_bp_messages"`
	CacheBPBlocks   int `json:"cache_bp_blocks"`
	// StopReason is the provider's own terminal reason, normalized across dialects:
	// Anthropic `stop_reason` (end_turn|max_tokens|tool_use|stop_sequence|pause_turn|
	// refusal|model_context_window_exceeded) or OpenAI `choices.0.finish_reason`
	// (stop|length|tool_calls|content_filter). "" when the response carried none.
	StopReason string `json:"stop_reason"`
}

// CacheBreakpoints is the total the provider's cap of four applies to.
func (m Meta) CacheBreakpoints() int {
	return m.CacheBPSystem + m.CacheBPTools + m.CacheBPMessages + m.CacheBPBlocks
}

// redact sanitizes the free-text metadata fields. Every one of them is a value the
// CLIENT chose, so each is attacker-influenced and none may reach the database
// unchecked — a request carrying `"reasoning_effort": "<a real api key>"` would
// otherwise write that key to disk, and to the SSE feed, forever.
//
// The numeric and boolean fields need no check: they were parsed as numbers, so they
// cannot carry a string at all.
func (m *Meta) redact() {
	m.ReasoningEffort = metaEnum(m.ReasoningEffort)
	m.ThinkingMode = metaEnum(m.ThinkingMode)
	m.ToolChoice = metaEnum(m.ToolChoice)
	m.StopReason = metaEnum(m.StopReason)
}

// Redact scrubs credential shapes from captured content and applies the size cap. The
// WRITER goroutine calls it immediately before the INSERT — never the request
// goroutine, where nine regexes over dozens of 16 KiB blobs cost ~53 ms, paid by the
// next request on a keep-alive connection.
//
// The placement is the whole security property: redaction happens before anything
// reaches the database, so a secret is never on disk and there is no redact-on-read
// filter to forget. Running it here rather than at the capture site changes WHICH
// GOROUTINE pays, not whether it runs.
//
// Idempotent, so a double call cannot corrupt a row.
func (e *Event) Redact() {
	e.Meta.redact()
	for i := range e.Content {
		e.Content[i].Before = RedactContent(e.Content[i].Before, e.ContentCap)
		e.Content[i].After = RedactContent(e.Content[i].After, e.ContentCap)
	}
	// The same treatment for a recorded call's before/after, which is the same kind of
	// material from the same transcript. The SUMMARY too: it is model-written text about a
	// tool output, so it can quote from it.
	for i := range e.Extractions {
		e.Extractions[i].Before = RedactContent(e.Extractions[i].Before, e.ContentCap)
		e.Extractions[i].After = RedactContent(e.Extractions[i].After, e.ContentCap)
		e.Extractions[i].Summary = RedactContent(e.Extractions[i].Summary, e.ContentCap)
	}
}

// Saved is this request's gross content-token saving.
func (e *Event) Saved() int {
	if e.TokensAfter > e.TokensBefore {
		return 0
	}
	return e.TokensBefore - e.TokensAfter
}

// CompRow is one component's accounting on one request.
type CompRow struct {
	Component   string `json:"component"`
	Kind        string `json:"kind"`
	Acted       bool   `json:"acted"`
	Mutated     bool   `json:"mutated"`
	Reverted    bool   `json:"reverted"`
	Skipped     bool   `json:"skipped"`
	SavedGross  int    `json:"saved_gross"`
	SavedUnique int    `json:"saved_unique"`
	// SavedUSD is this component's share of the request's baseline delta, in dollars,
	// priced at write time from the request's OWN model and the tier the request itself
	// paid — the same rule and the same rates as baselineDeltaUSD, so the per-component
	// figures sum to the request-level saving. It exists because the components view had
	// only a bare COST for the components that spend, and a dollar value improvised
	// client-side from hardcoded rates is wrong by whatever the deployment's model
	// actually charges.
	SavedUSD   float64 `json:"saved_usd"`
	DurationMs float64 `json:"duration_ms"`
	Err        string  `json:"err,omitempty"`
	// Gates counts, per named gate, the candidates this component turned away. It is the
	// only answer to "why did nothing happen?", and it never left the pipeline before:
	// /stats had it service-wide, the log line had it per request, and the dashboard —
	// the thing a user actually opens — had a table of zeros with no explanation. On a
	// Bob session almost every row is a gate name, because the offload heuristics were
	// tuned on Claude Code's tool-output shapes.
	// Not omitempty: an EMPTY map means "gated nothing" and a MISSING field means "this row
	// predates the column". The UI renders the first as a dash and the second as "unknown",
	// and omitempty made every healthy component look like the second.
	Gates map[string]int `json:"gates"`
}

// ContentRow is one rewritten message's before/after text (already redacted and
// size-capped by the caller).
type ContentRow struct {
	Path         string `json:"path"`
	BeforeTokens int    `json:"before_tokens"`
	AfterTokens  int    `json:"after_tokens"`
	Before       string `json:"before,omitempty"`
	After        string `json:"after,omitempty"`
	// Components names which components rewrote this message, in the order they touched
	// it — EXACT attribution for the diff view, so it never has to infer the author from
	// whether the after-text carries a `<<cg:HASH>>` marker. A reverted component is
	// absent. Empty on rows written before this field existed, which the UI must read as
	// "unknown", not as "nothing".
	Components []string `json:"components,omitempty"`
}

// ExtractionRow is one recorded LLM call an expensive component made. See the
// extraction_calls table for why this is not folded into CompRow.
type ExtractionRow struct {
	Component       string  `json:"component"`
	Model           string  `json:"model,omitempty"`
	Strategy        string  `json:"strategy,omitempty"`
	Aggressiveness  string  `json:"aggressiveness,omitempty"`
	Cold            bool    `json:"cold"`
	Escalated       bool    `json:"escalated,omitempty"`
	CandidateTokens int     `json:"candidate_tokens"`
	SavedTokens     int     `json:"saved_tokens"`
	PromptTokens    int64   `json:"prompt_tokens"`
	CompletionTok   int64   `json:"completion_tokens"`
	CacheRead       int64   `json:"cache_read"`
	CacheWrite      int64   `json:"cache_write"`
	CostUSD         float64 `json:"cost_usd"`
	LatencyMs       float64 `json:"latency_ms"`
	Accepted        bool    `json:"accepted"`
	GateReason      string  `json:"gate_reason,omitempty"`
	Rejection       string  `json:"rejection,omitempty"`
	Summary         string  `json:"summary,omitempty"`
	// Before/After are transcript content: persisted and served only under the same
	// per-account capture consent as the diff view.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// NetUSD is what this call was worth: the value of the tokens it removed, minus what the
// call cost. The whole point of recording calls individually — a component can be ahead
// overall while a particular kind of call is underwater.
func (r ExtractionRow) NetUSD(perSavedTokenUSD float64) float64 {
	return float64(r.SavedTokens)*perSavedTokenUSD - r.CostUSD
}

// FromTrace fills the pipeline-derived half of an Event from an apply.Trace.
// Usage/cost/latency come from the response and are filled by the caller.
func (e *Event) FromTrace(tr apply.Trace, uniqueSaved map[string]int) {
	e.SessionID = tr.Session
	e.Bypassed = tr.Bypassed
	e.CacheAware = tr.CacheAware
	e.Messages = tr.Messages
	e.AttemptedTokens = tr.AttemptedTokens
	e.FrozenTokens = tr.FrozenTokens
	// Breakpoint placement comes from the pipeline's own count, which it makes anyway to
	// respect the provider's cap — so recording it costs nothing on the request path. In
	// observe mode the trace is the zero value by design (nothing ran on the enforced
	// path), so these read zero exactly like every other trace-derived field on such a row.
	e.CacheBPSystem = tr.Breakpoints.System
	e.CacheBPTools = tr.Breakpoints.Tools
	e.CacheBPMessages = tr.Breakpoints.Messages
	e.CacheBPBlocks = tr.Breakpoints.Blocks
	e.SplitStableTokens, e.SplitTailHash = tr.SplitStableTokens, tr.SplitTailHash
	if tr.Bypassed {
		e.Mode = ModeBypass
	} else if e.Mode == "" {
		e.Mode = ModeActive
	}
	if tr.Run != nil {
		e.TokensBefore, e.TokensAfter = tr.Run.TokensBefore, tr.Run.TokensAfter
		for _, r := range tr.Run.Components {
			row := CompRow{
				Component:  r.Component,
				Kind:       r.Kind,
				Reverted:   r.Reverted,
				Skipped:    r.Skipped,
				SavedGross: r.Saved(),
				DurationMs: r.DurationMs,
			}
			row.Mutated = !r.Reverted && !r.Skipped
			row.Acted = row.Mutated && row.SavedGross > 0
			if u, ok := uniqueSaved[r.Component]; ok {
				row.SavedUnique = u
			}
			if r.Err != nil {
				row.Err = r.Err.Error()
			}
			row.Gates = r.Gates
			if r.Reverted {
				e.Reverts++
			}
			e.SavedUnique += row.SavedUnique
			e.Components = append(e.Components, row)
			for _, mc := range r.Calls {
				e.Extractions = append(e.Extractions, ExtractionRow{
					Component: mc.Component, Model: mc.Model, Strategy: mc.Strategy,
					Aggressiveness: mc.Aggressiveness, Cold: mc.Cold, Escalated: mc.Escalated,
					CandidateTokens: mc.CandidateTokens, SavedTokens: mc.SavedTokens,
					PromptTokens: mc.PromptTokens, CompletionTok: mc.CompletionTokens,
					CacheRead: mc.CacheRead, CacheWrite: mc.CacheWrite,
					CostUSD: mc.CostUSD, LatencyMs: mc.LatencyMs, Accepted: mc.Accepted,
					GateReason: mc.GateReason, Rejection: mc.Rejection, Summary: mc.Summary,
					Before: mc.Before, After: mc.After,
				})
			}
		}
	}
	for _, c := range tr.Changes {
		e.Content = append(e.Content, ContentRow{
			Path: c.Path, BeforeTokens: c.BeforeTokens, AfterTokens: c.AfterTokens,
			Before: c.Before, After: c.After, Components: c.Components,
		})
	}
	e.UncompressedReason = uncompressedReason(e, tr)
}

// uncompressedReason answers "why didn't you compact this?" from what the trace
// shows. Empty means we did compact.
func uncompressedReason(e *Event, tr apply.Trace) string {
	if tr.Bypassed {
		return ReasonBypassed
	}
	if tr.Run == nil || tr.Messages == 0 {
		return ReasonNoMessages
	}
	if e.Saved() > 0 {
		return ""
	}
	if e.Reverts > 0 && e.Reverts == len(e.Components) {
		return ReasonReverted
	}
	if tr.CacheAware && tr.AttemptedTokens == 0 && tr.Run.TokensBefore > 0 {
		return ReasonAllFrozen
	}
	acted := 0
	for _, c := range e.Components {
		if c.Mutated {
			acted++
		}
	}
	if acted == 0 {
		return ReasonBelowTrigger
	}
	return ReasonNoSavings
}

// Price fills the cost columns AT WRITE TIME, from this request's four billed
// token tiers plus a baseline counterfactual.
//
// The baseline is what the SAME request would have cost had context-guru not
// removed anything. Getting this right is the whole point of the dashboard, and
// there are two ways to get it wrong, both of which inflate it:
//
//   - Pricing GROSS savings. `Saved()` is tokens_before − tokens_after for THIS
//     turn, and the agent re-sends its whole transcript every turn, so the same
//     compaction is re-counted once per remaining turn. On a real 63-request
//     window that is a 13.1x overcount — a factor this dashboard computes and
//     displays as `overcount_ratio` right beside the dollar figure. Only
//     SavedUnique is content that genuinely never reached the provider.
//   - Pricing everything at the cache-WRITE rate. That rate (12.5x a read) is
//     right for content entering the prompt for the first time. The re-sent
//     remainder, on a turn whose cache HIT, would have been served from the
//     provider's cache, so the most it could have been billed at is the
//     cache-READ rate. Pricing it as a write multiplies the overcount by ~12.5.
//
// So: unique savings at the write rate, and the re-sent remainder at the rate the
// request itself actually paid — see repeatRate, which is where "whose cache hit"
// stopped being an assumption. Restored (expanded) content is content we removed
// and then had to serve back, so it is added to the ACTUAL cost side, never
// subtracted from baseline.
//
// accountingComplete=false leaves every cost at zero and the row is marked
// partial/missing: a cost we cannot compute must read as unknown, not as free.
func (e *Event) Price(p modelinfo.Price, accountingComplete bool) {
	if !accountingComplete || p.Zero() {
		if e.TokensBefore > 0 {
			e.TokenAccounting = AccountingPartial
		} else {
			e.TokenAccounting = AccountingMissing
		}
		return
	}
	e.TokenAccounting = AccountingComplete
	e.CostUSD = p.Cost(e.FreshInput, e.CacheRead, e.CacheWrite, e.OutputTokens)
	e.BaselineCostUSD = e.CostUSD + e.baselineDeltaUSD(p)
	// What the PROVIDER's prompt cache saved on this request: every cache-read token was
	// billed at the read rate instead of the fresh rate it would have cost with no cache at
	// all. Kept as a measurement, reported as one, and never called a saving of ours — it is
	// the provider's mechanism and the agent places most of the breakpoints itself. It earns
	// its place by being the number that COLLAPSES when a compaction pipeline rewrites deep
	// history, which is the failure this project has to be able to see.
	e.CacheSavedUSD = float64(e.CacheRead) * (p.Input - p.CacheRead)
	if e.CacheSavedUSD < 0 {
		e.CacheSavedUSD = 0 // a provider whose cache reads cost MORE than fresh input saved nothing
	}
	e.CachesplitSavedUSD = e.cachesplitSavedUSD(p)
	// Per-component dollars, same rule and same rates as baselineDeltaUSD above: the unique
	// part at the write rate it would have entered as, the re-sent remainder at the tier this
	// request actually paid. Summed over a component's turns this IS the amortization — value
	// realized turn by turn as the frozen reduction replays, not a projection.
	for i := range e.Components {
		c := &e.Components[i]
		gross, unique := c.SavedGross, c.SavedUnique
		if gross < 0 {
			gross = 0
		}
		if unique > gross {
			unique = gross // same clamp as baselineDeltaUSD: shared content keys can over-attribute
		}
		if unique < 0 {
			unique = 0
		}
		c.SavedUSD = float64(unique)*p.CacheWrite + float64(gross-unique)*e.repeatRate(p)
	}
}

// cachesplitSavedUSD is the cache saving this project is willing to sign its name to.
//
// The mechanism: Claude Code appends a live environment snapshot (branch, git status, recent
// commits) to the END of its big system block, and that block is ONE cacheable unit whose
// breakpoint sits after the churn. So the provider's hash covers the volatile tail and the
// block re-bills as cache CREATION every time the snapshot changes. cachesplit splits the
// block in two and moves the breakpoint onto the stable half — same bytes to the model, a
// hash boundary that excludes the snapshot.
//
// Three conditions, each ruling out a way of being wrong:
//
//   - SplitStableTokens > 0, i.e. the split actually happened on this request. It is set
//     only where splitVolatileTail rewrote the block, which is exactly when cachesplit
//     reports `mutated`, so this is the component test as well as the size.
//
//   - the provider READ from cache, and read AT LEAST as much as the half we split off while
//     WRITING less than that. A hit alone is not enough: on the first request after the
//     stable half itself changed — someone edited CLAUDE.md, the tool list changed — an
//     earlier agent breakpoint still hits, so cache_read > 0 while OUR half is being billed
//     as creation on that very request. The write test is what excludes it. Neither test can
//     isolate our breakpoint from the usage block, so both are deliberately blunt and both
//     err towards refusing the credit.
//
//   - the volatile TAIL CHANGED since this session's previous request (a session's first
//     request counts as changed — there was nothing there to match). This is the condition
//     that makes the hit ours: with the block unsplit, a moved snapshot re-creates the whole
//     thing, while a tail that did not move would have been served from cache either way.
//
//     It replaced "the session's first request", which sounded conservative and was simply
//     wrong about how this traffic behaves. Measured over 1,127 stored sessions: 1,105 of
//     the first requests were COLD — the previous session's cache had expired before the
//     next one began — so that test reported ~$0 while the component was demonstrably
//     serving the system prompt from cache on hundreds of mid-session turns. The turn that
//     matters is the one where the agent commits or edits and the snapshot moves, which is
//     mid-session, and which the SWE-bench A/B (0% -> 96.7% hit) was measuring all along.
//
//     The comparison survives a restart: the tail hash is stored per request and the map is
//     seeded from it (dash.Recorder.SeedSessions), so the first turn after a restart is not
//     mistaken for a change.
//
// And the amount is the STABLE HALF, not the request's whole cache_read. That distinction was
// found by running the control arm rather than by reasoning about it. Real Claude Code
// sessions against ete-litellm, each started after a fresh commit so the environment snapshot
// genuinely differed, first request of each session:
//
//	                     first-request cache_read     cache_write
//	cachesplit on                          54,304     1,030-1,065
//	cachesplit off (control)               45,805     9,555-9,564
//
// The control still HIT. Claude Code sets several breakpoints and the ones before this block
// match whatever we do — so crediting the whole 54,304 would have booked the agent's own cache
// placement as ours. What the split moved is the difference the provider reports: 8,499
// tokens, from the write tier to the read tier.
//
// We claim less than that. The split's own measurement of the half it moved is 5,654 tokens on
// that traffic, and the two disagree because they count different things — the provider reports
// block-granular usage over a prefix that also gained a block boundary, while this counts BPE
// tokens over the text. Both are evidence; the smaller one is what gets billed, because
// under-crediting is the only safe direction.
//
// In dollars on that request: $0.0099 ours against $0.0743 for the provider's whole cache read.
// 7.5x, and that 7.5x is the claim this replaced.
//
// The counterfactual is a cache MISS, not fresh input: those tokens carry cache_control, so
// a miss bills them as creation at 1.25x fresh, not at 1x. Hence CacheWrite - CacheRead, an
// 11.5x-fresh spread rather than 9x. The max(CacheWrite, Input) floor covers a provider that
// charges no write premium, where a miss still costs at least the fresh rate.
//
// It is a FLOOR: a stable prefix serves a whole session while this counts one request of it,
// and a session resumed after the TTL expires starts another first-request hit this cannot
// see. Under-crediting is the only direction a savings figure is allowed to be wrong in.
func (e *Event) cachesplitSavedUSD(p modelinfo.Price) float64 {
	if !e.TailChanged || e.SplitStableTokens <= 0 {
		return 0
	}
	// The read has to be big enough to have included our half, and the write small enough
	// not to have re-created it. Either failing means the stable half was not what got served
	// from cache on this request, whatever else was.
	n := int64(e.SplitStableTokens)
	if e.CacheRead < n || e.CacheWrite >= n {
		return 0
	}
	miss := p.CacheWrite
	if miss < p.Input {
		miss = p.Input
	}
	delta := miss - p.CacheRead
	if delta <= 0 {
		return 0
	}
	return float64(n) * delta
}

// baselineDeltaUSD is what the removed content would have cost had it been sent:
// the unique part as new input (cache-write rate), the re-sent remainder as a
// cache read.
func (e *Event) baselineDeltaUSD(p modelinfo.Price) float64 {
	unique := e.SavedUnique
	gross := e.Saved()
	// SavedUnique is attributed per component and can exceed the request's own
	// gross saving when several components stash the same content key; clamp it, so
	// the repeat term can never go negative and inflate the baseline.
	if unique > gross {
		unique = gross
	}
	if unique < 0 {
		unique = 0
	}
	return float64(unique)*p.CacheWrite + float64(gross-unique)*e.repeatRate(p)
}

// repeatRate is what the RE-SENT part of the removed content would have been billed
// at on this request.
//
// The old answer was always the cache-read rate, on the reasoning that a re-sent
// transcript is served from the provider's cache. That is true only of a request
// whose cache actually HIT. When it missed, the provider re-billed the entire prompt
// — so the content we had removed would have been re-billed too, at the
// cache-creation rate (12.5x a read on the Anthropic family), or at the fresh rate
// where nothing was written.
//
// This is not a rounding correction. On production traffic, turns whose cache had
// expired were 4% of requests and 31% of spend, all of it cache_creation; pricing
// their re-sent remainder as reads understated the value of removing it by ~12x, on
// exactly the turns where removing it is worth the most.
//
// Three guards keep this from inflating, which is the only direction that matters for a
// savings figure:
//
//   - A PARTIAL hit (some read, some written) keeps the read rate: which side of the
//     boundary the removed content sat on is not knowable from the usage block.
//   - A cache WRITE only earns the write rate when the write actually covers the prompt.
//     `cache_write > 0` does not mean the prompt was cached: a client with one breakpoint
//     after `tools` bills `cache_creation=2k, input=100k`, and the removed transcript there
//     would have been billed FRESH, not at 1.25x. So the write rate needs the written part
//     to be at least as large as the fresh part.
//   - Everything else is the fresh rate, never zero.
//
// One tempting "correction" here is wrong, and the old wording of this paragraph invited it.
// In cache-aware mode compaction only touches the UNCACHED tail, so it is true that the
// content removed on THIS turn would have been billed fresh rather than as a read. But that
// is the UNIQUE term, which is already priced at the write rate — this function only ever
// prices the REPLAY term, and replayed content is content removed on an EARLIER turn that by
// now sits deep inside the cached prefix, where CacheRead is exactly right. Replay is ~93% of
// realized value, so re-pricing it fresh on every warm turn would inflate warm-turn savings
// roughly 6x with nothing behind it.
func (e *Event) repeatRate(p modelinfo.Price) float64 {
	if e.CacheRead > 0 {
		return p.CacheRead
	}
	if e.CacheWrite > 0 && e.CacheWrite >= e.FreshInput {
		return p.CacheWrite
	}
	return p.Input
}

// AttributeCache buckets this request's cache behavior. seenSession/seenModel say
// whether we have already seen a request for this session / this model — the
// first of either is a COLD START, which is not a failure and must never be
// reported as a bust (headroom's model-aware rule). TTL wins ties: a prefix that
// changed after the entry had already expired was not the cause.
func (e *Event) AttributeCache(seenSession, seenModel bool, sinceLastMs int64, ttlMs int64, prefixChanged bool) {
	switch {
	case e.CacheRead > 0:
		e.CacheMissReason = CacheHit
	case !seenSession || !seenModel:
		e.CacheMissReason = CacheColdStart
	case ttlMs > 0 && sinceLastMs > ttlMs:
		e.CacheMissReason = CacheTTLExpiry
	case prefixChanged:
		e.CachePrefixChangeReason()
	default:
		e.CacheMissReason = CacheUnknown
	}
}

// CachePrefixChangeReason marks the miss as caused by a changed prefix.
func (e *Event) CachePrefixChangeReason() { e.CacheMissReason = CachePrefixChange }

// AgentFor classifies a client User-Agent into an agent family so the dashboard
// can filter by application.
func AgentFor(ua string) string { return agentFromUserAgent(ua) }

// agentFromUserAgent classifies the client into an agent family so the dashboard
// can filter by application. Unknown clients keep their raw first token rather
// than being lumped into "other" — a filter is useless if everything is "other".
func agentFromUserAgent(ua string) string {
	l := strings.ToLower(ua)
	for _, known := range []string{"claude-code", "claude-cli", "codex", "cursor", "cline", "aider", "gemini-cli", "bob"} {
		if strings.Contains(l, known) {
			return known
		}
	}
	if l == "" {
		return "unknown"
	}
	if i := strings.IndexAny(l, "/ "); i > 0 {
		return l[:i]
	}
	if len(l) > 32 {
		return l[:32]
	}
	return l
}
