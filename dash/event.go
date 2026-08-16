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
	CGLatencyMs     float64 `json:"cg_latency_ms"`
	UpstreamMs      float64 `json:"upstream_ms"`

	Expands      int `json:"expands"`
	ExpandTokens int `json:"expand_tokens"`
	Reverts      int `json:"reverts"`

	TokenAccounting    string `json:"token_accounting"`
	CacheMissReason    string `json:"cache_miss_reason"`
	UncompressedReason string `json:"uncompressed_reason"`

	Components []CompRow    `json:"components,omitempty"`
	Content    []ContentRow `json:"content,omitempty"`

	// ContentCap is the per-blob byte cap Redact applies. Set at the capture site and
	// consumed by the writer goroutine; not persisted (a knob, not a fact about the
	// request).
	ContentCap int `json:"-"`
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
	for i := range e.Content {
		e.Content[i].Before = RedactContent(e.Content[i].Before, e.ContentCap)
		e.Content[i].After = RedactContent(e.Content[i].After, e.ContentCap)
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
	Component   string  `json:"component"`
	Kind        string  `json:"kind"`
	Acted       bool    `json:"acted"`
	Mutated     bool    `json:"mutated"`
	Reverted    bool    `json:"reverted"`
	Skipped     bool    `json:"skipped"`
	SavedGross  int     `json:"saved_gross"`
	SavedUnique int     `json:"saved_unique"`
	DurationMs  float64 `json:"duration_ms"`
	Err         string  `json:"err,omitempty"`
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

// FromTrace fills the pipeline-derived half of an Event from an apply.Trace.
// Usage/cost/latency come from the response and are filled by the caller.
func (e *Event) FromTrace(tr apply.Trace, uniqueSaved map[string]int) {
	e.SessionID = tr.Session
	e.Bypassed = tr.Bypassed
	e.CacheAware = tr.CacheAware
	e.Messages = tr.Messages
	e.AttemptedTokens = tr.AttemptedTokens
	e.FrozenTokens = tr.FrozenTokens
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
			if r.Reverted {
				e.Reverts++
			}
			e.SavedUnique += row.SavedUnique
			e.Components = append(e.Components, row)
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
//   - Pricing everything at the cache-WRITE rate. That rate (11.5x a read) is
//     right for content entering the prompt for the first time. The re-sent
//     remainder would have been served from the provider's cache, so the most it
//     could have been billed at is the cache-READ rate. Pricing it as a write
//     multiplies the overcount by another ~11.5.
//
// So: unique savings at the write rate, the re-sent remainder at the read rate.
// Restored (expanded) content is content we removed and then had to serve back,
// so it is added to the ACTUAL cost side, never subtracted from baseline.
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
	return float64(unique)*p.CacheWrite + float64(gross-unique)*p.CacheRead
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
