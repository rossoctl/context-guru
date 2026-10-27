package dash

import (
	"strings"
	"time"

	bschemas "github.com/maximhq/bifrost/core/schemas"
	"github.com/rossoctl/context-guru/apply"
	"github.com/rossoctl/context-guru/internal/modelinfo"
	"github.com/rossoctl/context-guru/internal/tokens"
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

// Cache-miss causes (requests.cache_miss_cause). cache_miss_reason keeps its five values for
// old readers. The cause says WHAT changed, and it comes from a comparison with the previous
// request of the same thread (see PrefixFacts), never from a guess. Only the first cause that
// applies is stored, in this order.
const (
	// CauseModelChanged: the thread switched model. Each model has its own cache.
	CauseModelChanged = "model_changed"
	// CauseCompactionAfterRewrite: the agent sent an unmodified request (bypass) after
	// context-guru had rewritten earlier turns of the thread. The cache holds the rewritten
	// history. The bypass forwards the original, so the prefix differs.
	CauseCompactionAfterRewrite = "compaction_after_rewrite"
	// CauseBypassFlip: bypass turned on or off with no earlier rewrite to explain it.
	CauseBypassFlip = "bypass_flip"
	// CauseToolsChanged: the tool declarations differ (digest, or count when no digest).
	CauseToolsChanged = "tools_changed"
	// CauseSystemChanged: the system prompt differs (hash, or block count when no hash).
	CauseSystemChanged = "system_changed"
	// CauseEffortChanged: reasoning effort, thinking mode or thinking budget differs.
	CauseEffortChanged = "effort_changed"
	// CauseConcurrent: nothing measured changed and the previous request began less than
	// concurrentGapMs ago. A cache entry is readable only after the first response starts,
	// so parallel sub-agent requests miss. The reason is "unknown", not "prefix_change".
	CauseConcurrent = "concurrent"
	// CauseFirstOnModel: no successful earlier request of the thread ran this model, and the
	// request is short or an earlier one failed. It is not stored as a cause: the reason is
	// cold_start. ObservePrefix returns it so AttributeCache agrees with the thread facts.
	CauseFirstOnModel = "first_on_model"
)

// concurrentGapMs is the gap below which an unexplained miss reads as concurrent. Time to
// first byte on these models is a few seconds, and the cache entry exists only after it.
const concurrentGapMs = 5000

// "Why didn't you compact this?" — a first-class reason bucket, not an absence of
// data. An empty string means we DID compact.
const (
	ReasonBypassed   = "bypassed"      // x-context-guru-bypass on this request
	ReasonNoMessages = "no_messages"   // nothing to operate on
	ReasonAllFrozen  = "cache_frozen"  // eligible tail was empty (cache safety)
	ReasonNoSavings  = "found_nothing" // components ran, found nothing to remove
	ReasonReverted   = "reverted"      // components acted but were all reverted
	// No component mutated anything. These two replace ReasonBelowTrigger, which named a
	// mechanism that was not the cause and cost a whole investigation to disprove:
	// components.Trigger is not consulted in this decision at all, and of the eight
	// components in the default preset only extract_llm ever calls Trigger.Fires. The label
	// nonetheless read "every component's trigger declined", which was reported upward as
	// "$744.62 of spend was gated by the trigger" and sent an agent off tuning triggers whose
	// measured counterfactual on interactive traffic was $0.00.
	//
	// What actually happens, measured over the 7,660 affected requests on the production
	// corpus: every deployed offloader and both JSON reformatters gate on `role == tool`, so a
	// request carrying no tool output has nothing any of them can rewrite, whatever any
	// trigger says. 4,199 of those requests (206.9M tokens, 54.3% of the gated total) carry
	// four messages or fewer and therefore hold no tool_result block at all — the largest
	// single group being 734 claude-cli requests averaging 120,245 tokens with ONE message and
	// ZERO tools, which is Claude Code's own summarisation call flattening a transcript into a
	// single user text block. The rest are long transcripts 65–89% frozen whose eligible tail
	// holds no recognised candidate.
	//
	// So the reason splits in two, on a fact the row already carries:
	ReasonNoToolOutput = "no_tool_output" // the request declared no tools: no tool_result to rewrite
	ReasonNoCandidate  = "no_candidate"   // tools present, nothing any component recognised
	// ReasonBelowTrigger is retained for READING rows written before the rename. Never emitted.
	// The UI labels it with the same words as ReasonNoCandidate so one history does not read as
	// two mechanisms.
	ReasonBelowTrigger = "below_trigger"
)

// Operating modes, as rendered in the UI.
const (
	ModeActive  = "active"
	ModeBypass  = "bypass"
	ModeObserve = "observe"
)

// WaitCauseResult is issue #424's RECORD half's output for one request — see Event.WaitCause,
// which runs the computation that fills this, off the request path.
type WaitCauseResult struct {
	WaitingFor, WaitTool, WakeCause string
	OpenBackground, OpenTeammate    int
	MonitorExpiryMs                 int64
}

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
	// ThreadID names the thread of the session this request belongs to: "" for the session's
	// primary thread — the main agent, told apart as the first thread without an agent header
	// that declares tools (see internal/thread) — "a:<agent id>" for a subagent that named itself
	// in a header, and "p:<fingerprint>" for a thread told apart by prefix continuity. Each thread
	// has its own provider cache entry, so recency and miss attribution are per thread; reporting
	// stays per session. See internal/thread and issue #423.
	//
	// A row keeps the id it was written with. A tool-less request that spoke before the main agent
	// held "" at the time, so its row reads "" although the tracker later moved that thread to a
	// "p:" id.
	ThreadID string `json:"thread_id"`
	// Issue #424's RECORD half: why this thread is about to wait, and why it woke up — read
	// from the request body and the response's own tool calls/stop reason, by string match
	// and structural tool_use reads, no LLM. See internal/waitcause, which defines every
	// value and never guesses: a signal this proxy's client (Claude Code) does not send, or
	// another agent/provider entirely, reads "unknown" rather than a label chosen for it.
	// This package NEVER reads these columns to choose a keep-alive ping budget or change
	// compaction behaviour — it only writes them, so a later change can.
	//
	// WaitingFor is set on the row whose OWN response ends a turn with nothing more to do
	// right now (own_tool|human|background|teammate|retry|unknown); WaitTool is the LAST
	// tool_use name on that response, filled only when WaitingFor is "own_tool". WakeCause
	// is set on the row that ENDS a wait (tool_result|background|teammate|human|system|
	// retry|unknown), read off its own last message. OpenBackground/OpenTeammate are the
	// open-task/open-teammate counts as of THIS request's body (the "waiting_on" detail);
	// MonitorExpiryMs is the latest known absolute Monitor expiry (epoch ms) among them, 0
	// when none is open or none stated one. All six are "" / 0 on a row written before this
	// column existed (see dash/schema.go's additiveColumns: nullable, no default, so an old
	// row reads SQL NULL rather than a value this package never computed for it).
	WaitingFor      string `json:"waiting_for"`
	WaitTool        string `json:"wait_tool"`
	WakeCause       string `json:"wake_cause"`
	OpenBackground  int    `json:"open_background"`
	OpenTeammate    int    `json:"open_teammate"`
	MonitorExpiryMs int64  `json:"monitor_expiry_ms"`
	// WaitCause, when set, is invoked by the WRITER goroutine (Recorder.run's write(),
	// right beside e.Redact() and for the identical reason) to fill the six fields above —
	// never on the request path. The actual body rescan this runs
	// (waitcause.Tracker.Observe) measured ~11ms on a 783 KB body; called from finish()
	// (which runs on the handler's own defer, BEFORE the handler returns) that delayed the
	// NEXT request on a keep-alive connection by the same amount, which is exactly what
	// this repo's own TestDashboardAddsNoRequestLatencyWithContentCapture-style tests exist
	// to catch. nil on any row this does not apply to — a keep-alive ping's row never sets
	// it at all, which is also how a ping is guaranteed to carry no wait-cause label (issue
	// #424 requirement). Never serialized (there is nothing to marshal); the SIX FIELDS it
	// fills are what reaches JSON and the database.
	WaitCause func() WaitCauseResult `json:"-"`
	// Lifecycle is the opt-in lifecycle-signals row (dash/lifecycle.go), nil when the flag is
	// off; LifecycleFn fills it on the writer goroutine, for the same reason WaitCause runs
	// there (it rescans the request body). Neither is part of the JSON event.
	Lifecycle   *LifecycleRow        `json:"-"`
	LifecycleFn func() *LifecycleRow `json:"-"`
	Model       string               `json:"model"`
	// BypassCount, when set, fills tokens_before/after, attempted_tokens and frozen_tokens of a
	// BYPASSED request, which the pipeline does not count on the request path (counting a large
	// transcript took ~0.35 s per MiB). It runs on the recorder's count worker, never on the
	// writer or the request path, after the row is inserted. nil on every other row.
	BypassCount func() (before, attempted int) `json:"-"`
	// BypassBytes is the size of the bypassed request body BypassCount will tokenize; the count
	// queue is bounded by the total of it (see countMaxBytes).
	BypassBytes int `json:"-"`
	// BilledTokenFactor is the tokenizer correction applied to this row's COUNTERFACTUAL
	// dollars — baseline_cost_usd's delta and every request_components.saved_usd. It is
	// tokens.BilledDeltaFactor(Model), resolved once in Price.
	//
	// It is published rather than folded in silently: this factor is the whole difference
	// between what the dashboard reported before #240 and after it, and a reader who sees a
	// savings figure move needs to be able to see, and divide out, the thing that moved it.
	// 1.0 with BilledTokenFactorMeasured=false means this model's family was never measured
	// and the figure is exactly what it always was.
	BilledTokenFactor float64 `json:"billed_token_factor"`
	// BilledTokenFactorMeasured distinguishes "corrected by a measured factor" from
	// "uncorrected because nobody has measured this family" — 1.0 means both otherwise.
	BilledTokenFactorMeasured bool   `json:"billed_token_factor_measured"`
	Provider                  string `json:"provider"`
	Agent                     string `json:"agent"`
	// Source says how this row came to exist: "" for a request the proxy MEASURED as it passed,
	// SourceImported for one recovered from a Claude Code transcript after the fact.
	//
	// The two can answer different questions and the difference is not recoverable from anything
	// else on the row. A measured row saw the request, so it knows what was declared on it and
	// can say "you carried this and never called it". An imported row knows only what the
	// provider billed, because a transcript does not record the declaration list — so it can
	// report spend and must never produce a waste verdict.
	//
	// "" rather than a word for measured, because every row written before this column existed
	// is measured and cannot be back-filled.
	Source string `json:"source,omitempty"`
	// Project is the directory the agent was started in — what a user means by "project". Read
	// from the request body, since nothing else in a request identifies one: the session id is a
	// hash of the conversation head, and one session can span directories (measured: 8 of 48 do).
	// "" means unattributed, which is what a client that does not state a working directory gets.
	Project string `json:"project,omitempty"`
	Preset  string `json:"preset"`
	Mode    string `json:"mode"`
	Route   string `json:"route"`
	Status  int    `json:"status"`

	Bypassed bool `json:"bypassed"`
	// AgentCompaction marks the agent's own compaction request. It is usually not bypassed, but it
	// can be: a request with the bypass header, or a body over the size ceiling, carries both
	// flags. The replay cap reads either one as the evidence that a drop in tokens_before is the
	// client compacting.
	AgentCompaction bool `json:"agent_compaction"`
	CacheAware      bool `json:"cache_aware"`
	Messages        int  `json:"messages"`

	TokensBefore    int `json:"tokens_before"`
	TokensAfter     int `json:"tokens_after"`
	AttemptedTokens int `json:"attempted_tokens"`
	FrozenTokens    int `json:"frozen_tokens"`
	SavedUnique     int `json:"saved_unique"`

	FreshInput   int64 `json:"fresh_input"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	OutputTokens int64 `json:"output_tokens"`
	// CacheWrite1h is the part of CacheWrite the provider billed at the ONE-HOUR tier. A
	// SUBSET of CacheWrite, never an addition to it.
	//
	// It exists because pricing a 1h write as a 5m one understates that request by 0.75x of
	// its whole written prefix, and a cost that is wrong in the flattering direction is the
	// dangerous direction — it argues for spending. Measured live on this gateway: a request
	// asking for the 1h head on aws/claude-haiku-4-5 came back with 36,251 of 36,574 written
	// tokens on the 1h tier, and without this column the row priced at $0.0348 against a real
	// $0.0554.
	//
	// It is also the ONLY signal that a requested `ttl: "1h"` was honoured. The same request
	// on aws/claude-sonnet-5 returned ephemeral_1h_input_tokens = 0 with an otherwise normal
	// 200 — Bedrock's 1h support covers the Claude 4.5 family and not Sonnet 5 — so a silently
	// downgraded request is indistinguishable from a granted one without this number.
	CacheWrite1h int64 `json:"cache_write_1h"`

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
	// CacheTTL is the prompt-cache lifetime tier this request ASKED for — "ephemeral_5m",
	// "ephemeral_1h", or "" for a body that carried no cache_control at all. The pipeline has
	// always computed this (it is an input to cold-cache attribution) and then discarded it,
	// so nothing downstream could say which tier a prompt used: the strings did not appear in
	// the source at all, and a 1h prompt was priced at the 5m write rate because there was
	// nothing to price it differently by. Where a breakpoint SITS is already recorded
	// (CacheBP*); how long it was meant to live was the missing half.
	CacheTTL string `json:"cache_ttl"`
	// SSEBuffered and TTFBMs are the response-side latency the dashboard could not see.
	//
	// A third of streamed responses on measured traffic are BUFFERED rather than streamed
	// through — the client sees nothing until the whole response has arrived — and those wait
	// about 21 seconds longer for their first byte (29.0 s against 7.9 s). That is two orders
	// of magnitude more user-visible delay than anything the pipeline itself adds, and it was
	// visible only as a process-lifetime counter on /stats: unfilterable, unattributable to a
	// model or an account, and gone on restart. Persisting the pair makes it a fact about a
	// request like every other on this row.
	//
	// TTFBMs is 0 on a non-streamed response. On a BUFFERED one it is the total response time,
	// not a first-byte time: proxy.go leaves the first-byte instant zero and msSince falls back
	// to time.Now(). That is the right number for a buffered response — nothing is written to
	// the client until the whole response has arrived, so the client's first byte and its last
	// byte are the same moment — but it is a different measurement from the streamed figure,
	// which is why the flag is stored beside it and the two are never averaged together.
	SSEBuffered bool    `json:"sse_buffered"`
	TTFBMs      float64 `json:"ttfb_ms"`
	// FilteredDeclTokens is what the declaration filter stopped carrying on this request —
	// the token weight of the `tools` elements it removed under the account's opt-in list.
	//
	// It is a TOKEN count and not a dollar figure on purpose: what those tokens would have
	// cost depends on the tier this same row was billed at (cache_read 0.1x, cache creation
	// 1.25x, fresh 1.0x), and those columns are right here — so the price is computed by the
	// query that reads them together rather than frozen into a second column that could
	// disagree with them. See DB.DeclFilterSavings.
	FilteredDeclTokens int `json:"filtered_decl_tokens"`
	// DeclDigest is the declaration set this request carried (tool_declarations.digest).
	DeclDigest string `json:"decl_digest,omitempty"`

	// KeepAlive marks a row that IS a keep-alive ping rather than agent traffic: a request
	// context-guru sent on its own initiative, to re-read an idle session's cached prefix so
	// the provider refreshes its lifetime. Its cost is real and lands in CostUSD like any
	// other row's, which is the point — a mechanism that spends the caller's money while
	// they are away from the keyboard has to be auditable to the cent, and a ping that did
	// not appear as a row would be spend with no owner.
	KeepAlive bool `json:"keepalive"`
	// KeepAlivePings is, on a REAL request, how many pings the keeper DECIDED to send during the
	// idle span it just ended — which is not the same as how many were sent, and the difference is
	// why dash.kaSaved does not trust this column.
	//
	// proxy/keepalive.go increments it under the sweep's lock, before dispatch. Three paths then
	// return without a request reaching the provider and without writing a ping row: the tenant
	// limiter refusing spare headroom, an unusable body, and a transport error on send. All three
	// bump skipped/failed and leave this count already raised. Measured consequence on a
	// 133,064-row production corpus: 7 of 105 credited rows carry KeepAlivePings >= 1 with no ping
	// row anywhere in the span, two with none within 16.7 hours.
	//
	// So this is dispatch intent, and only a stored ping row (keepalive = 1, status = 200) is
	// evidence that a ping happened. The credit is gated on the latter at read time. Not fixed
	// here: the counter is in proxy/ and the figure it feeds is already protected.
	//
	// It is still what makes the saving below attributable rather than assumed: a
	// request that hits after a twenty-minute gap could have been rescued by our ping or by
	// any other session that happened to send byte-identical content, and only this
	// distinguishes them.
	KeepAlivePings int `json:"keepalive_pings"`
	// KeepAliveRefreshed is what the last ping on this session actually read from cache —
	// the tokens our ping demonstrably refreshed. Not persisted: it is the numerator's
	// CEILING, and what gets stored is the credited dollars beside the cache_read they were
	// computed from. See keepaliveSavedUSD for why the credit is the smaller of the two.
	KeepAliveRefreshed int64 `json:"-"`
	// KeepAliveSavedUSD is the cache re-creation this request avoided because a ping kept
	// its prefix alive.
	KeepAliveSavedUSD float64 `json:"keepalive_saved_usd"`
	// KeepAliveStrategyID is which manager-controlled strategy resolved the policy behind
	// this row (see proxy/keepalivestrategy.go) — "" when none matched (account config or a
	// session override supplied the policy instead). Set on a ping row for the strategy
	// that sent it, AND on the real request that followed it (whenever KeepAlivePings>0),
	// regardless of whether that request ended up credited — captured at keeper.arrive, the
	// one moment the idle span's strategy is still in scope (proxy/keepalive.go). Harmless
	// on an uncredited row: every reader of this column also filters on
	// KeepAliveSavedUSD>0. Written as SQL NULL, not '', when empty — see dash/schema.go's
	// keepalive_strategy_id column — so a row from before this feature existed is
	// distinguishable from one deliberately attributed to nothing.
	KeepAliveStrategyID string `json:"keepalive_strategy_id,omitempty"`
	// ManualPing marks a keep-alive row the USER asked for (POST /api/me/cache/ping). It stays a
	// KeepAlive row — real spend, a real refresh of the provider's entry — and carries strategy id
	// "manual"; this flag is what every strategy ledger filters on, so a button press never lands
	// in a strategy's net. ManualSavedUSD is the cache write it avoided (gross; 0 when the ping
	// read nothing). Its cost is CostUSD, as for any ping.
	ManualPing     bool    `json:"manual_ping,omitempty"`
	ManualSavedUSD float64 `json:"manual_saved_usd,omitempty"`
	// SinceLastMs is the gap from this session's previous REAL request, in milliseconds.
	// Not persisted — AttributeCache already turns it into cache_miss_reason — but the
	// keep-alive credit needs the raw number: the whole claim is that the gap exceeded the
	// provider's lifetime and the request hit anyway.
	SinceLastMs int64 `json:"-"`

	CGLatencyMs float64 `json:"cg_latency_ms"`
	UpstreamMs  float64 `json:"upstream_ms"`

	Expands      int `json:"expands"`
	ExpandTokens int `json:"expand_tokens"`
	Reverts      int `json:"reverts"`
	// SweepExpands and SweepExpandTokens are the part of Expands and ExpandTokens that restored a
	// marker the extract_llm_sweep made (#404 point 7). The sweep's credit has them taken off; the
	// totals above still carry them, so SavedAdjusted is unchanged.
	SweepExpands      int `json:"sweep_expands"`
	SweepExpandTokens int `json:"sweep_expand_tokens"`
	// SweepFalseExpands is how many drops were expanded back for the FIRST time on this request and
	// were not already counted as referenced: the expand half of the false-drop count, exact per drop.
	SweepFalseExpands int `json:"sweep_false_expands"`
	// SweepFalseExpandsWarm is the part of SweepFalseExpands whose drop was applied on a WARM request
	// (#491), so the warm false-drop rate counts expands as well as references.
	SweepFalseExpandsWarm int `json:"sweep_false_expands_warm"`
	// SweepAnswerUSD is, on a keep-alive ping row the sweep's question replaced, what the answer cost
	// beyond the read the ping would have paid anyway (#404 point 9). Booked once, on the ping row, as
	// the summary's call is (#458). It is part of CostUSD, not added to it, and it is a separate
	// column so the sweep's credit and its answer cost are never one number.
	SweepAnswerUSD float64 `json:"sweep_answer_usd,omitempty"`

	TokenAccounting    string `json:"token_accounting"`
	CacheMissReason    string `json:"cache_miss_reason"`
	UncompressedReason string `json:"uncompressed_reason"`
	// CacheMissCause says what changed on a miss that is not hit, cold_start or ttl_expiry.
	// Set only from a measured comparison (Cause* constants). "" when nothing was measured.
	CacheMissCause string `json:"cache_miss_cause,omitempty"`
	// SystemHash is the system prompt's hash, for the prefix comparison only. Not persisted.
	SystemHash string `json:"-"`

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
	// The launch directory is client-chosen; sanitise the field itself so the feed and the
	// inventory writer see what the requests table stores.
	e.Project = ProjectPath(e.Project)
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
		// And the REJECTION slug, which is the one field here that is stored whether or not
		// the account consented to content capture. It is not always a slug: the
		// "model call failed" leg carries the upstream error verbatim (cheapmodel clips a
		// non-2xx body into the message), so a gateway that echoes the presented credential
		// in a 401 would put it on disk and on a dashboard row. Cap 0: this is a short
		// diagnostic, so it is scrubbed but never truncated.
		e.Extractions[i].Rejection = RedactContent(e.Extractions[i].Rejection, 0)
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
	// Events counts what this component DID — a replay served, an output removed, an inventory
	// offered, a candidate reached past the cached boundary. The counterpart to Gates, and carried
	// separately for the reason the two metrics series are separate: they answer opposite questions.
	//
	// Without it this row was blind in proportion to how well a component was working, because the
	// names a component raises when it SUCCEEDS are the ones that moved. Observed live: the turn
	// that adjudicated twelve outputs and removed twelve showed an empty gates cell, while a turn
	// that refused everything showed a full one.
	//
	// Same omitempty reasoning as Gates, and it is load-bearing here too: an EMPTY map means "did
	// nothing counted", a MISSING field means "this row predates the column".
	Events map[string]int `json:"events"`
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
	e.CacheTTL = tr.CacheTTL
	e.FilteredDeclTokens = tr.FilteredDeclTokens
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
			row.Events = r.Events
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
		// A request that declared no tools cannot contain a tool_result, and every deployed
		// component rewrites nothing else. That is a property of the request, not a decision
		// any trigger made. Meta is filled before FromTrace runs, so Tools is in hand here.
		if e.Tools == 0 {
			return ReasonNoToolOutput
		}
		return ReasonNoCandidate
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
	// Resolve the tokenizer correction ONCE per request, before anything prices a saving.
	// It multiplies counterfactual token counts only — never a provider-reported count, so
	// CostUSD below is untouched.
	e.BilledTokenFactor, e.BilledTokenFactorMeasured = tokens.BilledDeltaFactor(e.Model)
	// See Price.CostWithCacheWrite1h for the one-hour write premium this folds in.
	e.CostUSD = p.CostWithCacheWrite1h(e.FreshInput, e.CacheRead, e.CacheWrite, e.OutputTokens, e.CacheWrite1h)
	e.BaselineCostUSD = e.CostUSD + e.baselineDeltaUSD(p)
	// What the PROVIDER's prompt cache saved on this request: every cache-read token was
	// billed at the read rate instead of the fresh rate it would have cost with no cache at
	// all. Kept as a measurement, reported as one, and never called a saving of ours — it is
	// the provider's mechanism and the agent places most of the breakpoints itself. It earns
	// its place by being the number that COLLAPSES when a compaction pipeline rewrites deep
	// history, which is the failure this project has to be able to see.
	//
	// NOT on a keep-alive ping. A ping's whole job is to read the prefix, so it reports a large
	// cache_read and would book ~$0.13 of "provider cache saving" per 48k ping against $0.0073
	// of real cost — a fabricated saving on a figure PR #83 puts on the page, and at production
	// ping volumes it is on the order of a thousand dollars. The counterfactual is the tell:
	// without the keep-alive that ping does not exist, so there is no unbatched-fresh-input
	// alternative for it to have saved anything against.
	if !e.KeepAlive {
		e.CacheSavedUSD = float64(e.CacheRead) * (p.Input - p.CacheRead)
		if e.CacheSavedUSD < 0 {
			e.CacheSavedUSD = 0 // a provider whose cache reads cost MORE than fresh input saved nothing
		}
	}
	e.CachesplitSavedUSD = e.cachesplitSavedUSD(p)
	e.KeepAliveSavedUSD = e.keepaliveSavedUSD(p)
	// Per-component dollars, same rule and same rates as baselineDeltaUSD above: the unique
	// part at the write rate it would have entered as, the re-sent remainder at the tier this
	// request actually paid. Summed over a component's turns this IS the amortization — value
	// realized turn by turn as the frozen reduction replays, not a projection.
	for i := range e.Components {
		c := &e.Components[i]
		c.SavedUSD = e.ComponentSavedUSD(p, c.Component, c.SavedGross, c.SavedUnique)
	}
}

// ComponentSavedUSD is ONE component's share of the baseline delta on this request: the
// single pricing path for a removal. Price stores it at write time; the read-time passes
// (dash/summarizeattr.go) call it again on a stored row so an old row and a new row are valued
// by the same arithmetic and cannot drift apart.
//
// The first (unique) removal is priced by uniqueRate, the re-sent remainder by repeatRate.
func (e *Event) ComponentSavedUSD(p modelinfo.Price, component string, gross, unique int) float64 {
	if gross < 0 {
		gross = 0
	}
	if unique > gross {
		unique = gross // same clamp as baselineDeltaUSD: shared content keys can over-attribute
	}
	if unique < 0 {
		unique = 0
	}
	usd := (float64(unique)*e.uniqueRate(p, component) + float64(gross-unique)*e.repeatRate(p)) *
		e.BilledTokenFactor
	return usd - e.sweepWarmRewriteUSD(p, component)
}

// sweepComponentEvent reads one event count off the sweep's row of this request.
func (e *Event) sweepComponentEvent(name string) int {
	for i := range e.Components {
		if e.Components[i].Component == SweepComponent {
			return e.Components[i].Events[name]
		}
	}
	return 0
}

// sweepWarmApplied is true on a request where the sweep applied a drop on a WARM request (#491).
func (e *Event) sweepWarmApplied() bool { return e.sweepComponentEvent("sweep_applied_warm") > 0 }

// sweepWarmRewriteUSD is what a warm drop COST on the request that paid it (#491): the cache from the
// first removed output on is written again at the write rate where it would have been read. The tokens
// are the provider's own cache-write tokens of this request when it reported a write (it cannot have
// rewritten more than it wrote), and the apply's billed-scaled estimate (event
// sweep_warm_rewrite_tokens) when it did not. Zero for every request that is not a warm apply, so the
// cold pricing is unchanged.
func (e *Event) sweepWarmRewriteUSD(p modelinfo.Price, component string) float64 {
	if component != SweepComponent || !e.sweepWarmApplied() {
		return 0
	}
	n := int64(e.sweepComponentEvent("sweep_warm_rewrite_tokens"))
	if e.CacheWrite > 0 && e.CacheWrite < n {
		n = e.CacheWrite
	}
	if n <= 0 || p.CacheWrite <= p.CacheRead {
		return 0
	}
	return float64(n) * (p.CacheWrite - p.CacheRead)
}

// SummarizerComponents is THE list of pipeline components whose removals are EARLIER transcript:
// a summary replaces turns the provider has already been sent. Every place that treats a
// component as a summarizer (the first-removal rate, the baseline split, the ledger, the cost
// attribution, the SQL) reads this list and no other. The default preset runs "summarize"; the
// medium, high and xhigh presets run "cache_aware_summarizer". Add a new summarizer here and
// nowhere else.
var SummarizerComponents = []string{"summarize", "cache_aware_summarizer"}

// IsSummarizer reports whether a component name is on SummarizerComponents.
func IsSummarizer(component string) bool {
	for _, n := range SummarizerComponents {
		if n == component {
			return true
		}
	}
	return false
}

// uniqueRate is what the FIRST removal of a component's content would have been billed at.
//
// The default is the cache-write rate: content entering a prompt for the first time is billed
// as a write. That is wrong for a summary. What a summary removes is transcript the previous
// turn already sent, and when that turn's cache was live the same tokens, left in, would have
// been READ on this turn. Pricing them as a write over-credited the summary turn by the
// write:read ratio (10x on Opus 5.5): $2.945 against a real saving of about $0.14 on request
// 15653.
//
// Whether the previous turn's cache was live comes from the verdict. ttl_expiry and cold_start
// say the entry was gone, so an uncompacted prompt would have re-created it: the write rate
// stands. Any other verdict (hit, or prefix_change, which on a summary turn is our own rewrite
// of a live entry) says it was live, so the counterfactual is a read.
func (e *Event) uniqueRate(p modelinfo.Price, component string) float64 {
	// A WARM sweep drop (#491) removes tokens the provider would have READ, not written: the cache was
	// live. Credited at the write rate it would be 12.5x too high. Its rewrite cost is charged in
	// sweepWarmRewriteUSD.
	if component == SweepComponent && e.sweepWarmApplied() {
		return p.CacheRead
	}
	if IsSummarizer(component) {
		switch e.CacheMissReason {
		case CacheTTLExpiry, CacheColdStart:
		default:
			return p.CacheRead
		}
	}
	return p.CacheWrite
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
// sessions against litellm-gateway, each started after a fresh commit so the environment snapshot
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
// a miss bills them as creation at 1.25x fresh, not at 1x. Hence CacheWrite - CacheRead, a
// 1.15x-fresh spread rather than 0.9x. The max(CacheWrite, Input) floor covers a provider that
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

// keepaliveSavedUSD is the cache re-creation a keep-alive ping avoided on THIS request.
//
// The mechanism, and why it is worth this much: the provider's default cache lifetime is
// five minutes, "measured from the start of the request that writes or reads the cache
// entry, not from the end of its response". A session idle longer than that loses its whole
// cached prefix, and the next turn re-bills every token of it at the creation rate. Measured
// over 19,805 production requests: 742 such requests cost $741.07 — 23.6% of all spend — at
// $0.9987 each against $0.1178 for a request that hit. Comparing each one against its own
// counterfactual (the same tokens billed as a read) puts the penalty at 11.35x, of which
// 91.2% is pure re-write.
//
// A cache READ refreshes the lifetime for free — "the cache is refreshed for no additional
// cost each time the cached content is used" — so a ping that re-reads the prefix at 0.1x
// buys back a re-creation at 1.25x. That 11.5:1 ratio is the whole economics, and it is why
// this is the one cache lever worth more than everything the transformation pipeline
// delivers.
//
// Four conditions, and each rules out a specific way of claiming money that is not ours:
//
//   - A ping actually happened on this session during the idle span this request ended
//     (KeepAlivePings > 0). Without it, every long-gap hit in the deployment would be
//     credited to a mechanism that was switched off.
//   - The gap exceeded the provider's lifetime. Inside the lifetime the entry would have
//     survived on its own and the ping changed nothing — those pings are the measured waste
//     the policy accepts (53 of 912 at the shipped X=280s, K=2, gated), and they must not also be
//     credited.
//   - The provider read from cache and read MORE than it wrote. A request that re-created
//     most of its prefix did not get rescued, whatever else it read.
//   - The credit is the smaller of what the ping refreshed and what this request read. The
//     ping's own response says exactly how many tokens were in the entry it touched, so
//     crediting more than that would claim tokens our ping never kept alive.
//
// One confound remains and is not removable from a single row: the provider's cache is keyed
// on CONTENT, so another session sending a byte-identical prefix inside the window would
// have refreshed the same entry for free. This is therefore the CEILING of what the ping
// earned on this row, and the counts beside it (pings sent, pings that read nothing) are what
// an operator checks it against.
//
// Priced against a cache MISS rather than fresh input, for the same reason
// cachesplitSavedUSD is: these tokens carry cache_control, so a miss bills them as creation
// at 1.25x, not at 1.0x.
func (e *Event) keepaliveSavedUSD(p modelinfo.Price) float64 {
	if e.KeepAlive || e.KeepAlivePings <= 0 || e.CacheRead <= 0 {
		return 0 // a ping never credits itself
	}
	if e.SinceLastMs <= keepaliveCreditFloorMs(e.Provider, e.Model, e.CacheTTL) {
		return 0
	}
	if e.CacheWrite >= e.CacheRead {
		return 0
	}
	n := e.CacheRead
	if e.KeepAliveRefreshed > 0 && e.KeepAliveRefreshed < n {
		n = e.KeepAliveRefreshed
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

// keepaliveCreditFloorMs is how long a gap must be before a ping can take credit for the return:
// the cache lifetime of the thread. cacheTTL is the tier the returning request ASKED for, which
// for a given client is constant across a session (Claude Code asks 1h on a subscription for every
// request). A 1-hour thread is pinged at 59 min, so crediting any gap over 5 minutes would claim
// returns the entry survived on its own.
func keepaliveCreditFloorMs(provider, model, cacheTTL string) int64 {
	if provider == string(bschemas.OpenAI) {
		if !modelinfo.GPT56OrLater(model) {
			return 1<<63 - 1 // no known lifetime means no defensible credit
		}
		return apply.OpenAIMinimumCacheTTL.Milliseconds()
	}
	if cacheTTL == apply.TTLEphemeral1h {
		return providerCacheTTL1hMs
	}
	return providerCacheTTLMs
}

// providerCacheTTLMs is the Anthropic default used by the Anthropic-only
// keep-alive. It comes from apply's lifetime source; OpenAI's minimum must not
// be used as proof that an entry has expired.
//
// The one-hour tier has its own constant, providerCacheTTL1hMs: a thread whose requests ask the 1h
// tier is pinged at 59 minutes, so both the write-side floor above and kaSaved's reachability window
// in keepalive.go use it for rows whose cache_ttl is ephemeral_1h.
const providerCacheTTLMs int64 = int64(apply.AnthropicDefaultCacheTTL / time.Millisecond)

// providerCacheTTL1hMs is the one-hour tier's lifetime.
const providerCacheTTL1hMs int64 = int64(time.Hour / time.Millisecond)

// baselineDeltaUSD is what the removed content would have cost had it been sent:
// the unique part as new input (cache-write rate), the re-sent remainder as a
// cache read — with the token counts corrected to the provider's own count.
//
// The correction (BilledTokenFactor) is issue #240: the counts here are
// schema.MessagesTokens, an o200k_base estimate, while the RATE is the provider's. Pricing
// one at the other under-reported this delta on every Claude row. See
// tokens.BilledDeltaFactor for how the factor was measured and what it does not claim.
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
	// The part of the unique removal that belongs to a summarizer is priced by its own rate (see
	// uniqueRate); every other component's unique part stays at the write rate.
	sumUnique := 0
	for _, c := range e.Components {
		if IsSummarizer(c.Component) && c.SavedUnique > 0 {
			sumUnique += min(c.SavedUnique, max(c.SavedGross, 0))
		}
	}
	sumUnique = min(sumUnique, unique)
	raw := float64(unique-sumUnique)*p.CacheWrite + float64(sumUnique)*e.uniqueRate(p, SummarizerComponents[0]) +
		float64(gross-unique)*e.repeatRate(p)
	return raw * e.BilledTokenFactor
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
//
// cause is the measured difference from the thread's previous request (Recorder.ObservePrefix):
// "" when nothing differed. A miss with no measured difference is "unknown", not "prefix_change".
func (e *Event) AttributeCache(seenSession, seenModel bool, sinceLastMs int64, ttlMs int64, cause string) {
	switch {
	case e.CacheRead > 0:
		e.CacheMissReason = CacheHit
	case !seenSession || !seenModel || cause == CauseFirstOnModel:
		e.CacheMissReason = CacheColdStart
	case ttlMs > 0 && sinceLastMs > ttlMs:
		e.CacheMissReason = CacheTTLExpiry
	case cause == "":
		e.CacheMissReason = CacheUnknown
	case cause == CauseConcurrent:
		e.CacheMissReason, e.CacheMissCause = CacheUnknown, cause
	case e.CacheWrite <= 0:
		// Nothing was written, so no prefix was re-created: a changed prefix would have
		// written it. A failed request or one with no usage lands here.
		e.CacheMissReason = CacheUnknown
	default:
		e.CachePrefixChangeReason()
		e.CacheMissCause = cause
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
