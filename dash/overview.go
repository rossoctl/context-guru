package dash

import (
	"database/sql"
	"fmt"

	"golang.org/x/sync/errgroup"
)

// Denominator is one labelled savings ratio. Shipping a single "savings %" is the
// mistake both reference implementations make in different ways: a whole-request
// ratio recounts the transcript every turn, so a 200-turn session reads ~0% no
// matter how well compaction performed, while a compressible-only ratio flatters
// by excluding everything we chose not to touch. Both are true. Neither is "the"
// number. So each ratio ships with the denominator it divides by, in words.
type Denominator struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Numerator   int64   `json:"numerator"`
	Denominator int64   `json:"denominator"`
	Percent     float64 `json:"percent"`
	// Description states exactly what this ratio divides by and when to trust it.
	Description string `json:"description"`
	// Available is false when the inputs were missing; the UI must then show "n/a"
	// rather than 0%, and never a ratio computed by dividing savings by themselves.
	Available bool `json:"available"`
}

func denom(key, label string, num, den int64, desc string) Denominator {
	d := Denominator{Key: key, Label: label, Numerator: num, Denominator: den, Description: desc}
	if den > 0 {
		d.Percent = float64(num) / float64(den) * 100
		d.Available = true
	}
	return d
}

// WaterfallStep is one bar of the honest-savings waterfall: baseline cost, the
// savings that reduced it, the penalties that gave some back, and the net.
type WaterfallStep struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	DeltaUSD    float64 `json:"delta_usd"` // signed: negative reduces cost
	Description string  `json:"description"`
	// Total marks a resting point (baseline / final net) rather than a delta.
	Total bool `json:"total"`
}

// Overview is the payload behind the dashboard's headline. Every percentage here
// is derived at read time from stored absolutes, so a rate change never rewrites
// history and a filter change never needs a rebuild.
type Overview struct {
	Since    int64 `json:"since"`
	Until    int64 `json:"until"`
	Requests int64 `json:"requests"`
	Sessions int64 `json:"sessions"`

	// InvalidConfigRequests is how many of Requests ran with NO compaction because this
	// account's stored configuration failed to build — proxy/tenancy.go's build() fails open
	// on purpose (a bad config row must never take someone's agent offline) by forwarding
	// uncompacted and marking the row's preset "invalid", but until now that marker was only
	// ever visible in the proxy's own log, never on the page an account owner or a manager
	// actually looks at. A real incident (#118 removed a config key with no migration for the
	// accounts already using it) ran for hours as nine accounts' compaction silently went to
	// zero before anyone noticed from a log line rather than from here. Nonzero here means the
	// account's Settings page needs attention now, not "check the logs".
	InvalidConfigRequests int64 `json:"invalid_config_requests"`

	TokensBefore int64 `json:"tokens_before"`
	TokensAfter  int64 `json:"tokens_after"`
	// SavedGross re-counts the same compaction every turn the agent re-sends the
	// transcript. SavedUnique counts each distinct compaction once. SavedAdjusted
	// subtracts content we offloaded and then had to serve back.
	SavedGross     int64   `json:"saved_gross"`
	SavedUnique    int64   `json:"saved_unique"`
	SavedAdjusted  int64   `json:"saved_adjusted"`
	OvercountRatio float64 `json:"overcount_ratio"`

	// The replay, named as what it is.
	//
	// ReplayTokens is SavedGross − SavedUnique: the reductions this window RE-EARNED, because
	// a reduction frozen at turn N is still absent from the transcript on every later turn the
	// agent re-sends it. It is the same quantity OvercountRatio expresses as a multiple, and
	// it was previously presented only that way — as an "overcount", i.e. as a discount
	// against us. It is the opposite: it is where most of the realized dollar value comes
	// from (~93% on measured traffic), and it is already priced into BaselineCostUSD and into
	// every component's saved_usd, per turn, at the tier that turn actually paid.
	//
	// ReplayProjectedTokens is the ceiling: every unique reduction multiplied by the number of
	// later turns in its OWN session, i.e. what the replay would have come to had each
	// reduction survived to the end of its session. ReplayRealizedPct is the fraction of that
	// ceiling actually collected — 5.4% on production traffic. The gap is not an accounting
	// error, it is the cache-safety freeze declining to compact the cached prefix, and it is
	// the largest single piece of headroom this dashboard can point at. It had no field.
	ReplayTokens          int64   `json:"replay_tokens"`
	ReplayProjectedTokens int64   `json:"replay_projected_tokens"`
	ReplayRealizedPct     float64 `json:"replay_realized_pct"`

	AttemptedTokens int64 `json:"attempted_tokens"`
	FrozenTokens    int64 `json:"frozen_tokens"`

	FreshInput   int64 `json:"fresh_input"`
	CacheRead    int64 `json:"cache_read"`
	CacheWrite   int64 `json:"cache_write"`
	OutputTokens int64 `json:"output_tokens"`

	CostUSD         float64 `json:"cost_usd"`
	BaselineCostUSD float64 `json:"baseline_cost_usd"`
	CGLLMCostUSD    float64 `json:"cg_llm_cost_usd"`
	NetSavedUSD     float64 `json:"net_saved_usd"`
	// CacheSavedUSD is what the PROVIDER's prompt cache saved over paying the fresh rate
	// for the same tokens. Context, not credit: the agent places most of the breakpoints
	// itself. It is here because it is the number that collapses when a pipeline rewrites
	// deep history — a diagnostic, and the API keeps it for that. The dashboard does not
	// show it as a saving, because it is not ours.
	CacheSavedUSD float64 `json:"cache_saved_usd"`
	// CachesplitSavedUSD is the cache saving that IS ours: summed over requests where the
	// volatile-tail split ran, the snapshot had MOVED since the session's previous request,
	// and the provider then read at least the stable half from cache while writing less than
	// it. Priced against a cache miss. A floor. See Event.cachesplitSavedUSD.
	CachesplitSavedUSD float64 `json:"cachesplit_saved_usd"`
	// The three counts behind that figure, so a small number is explicable rather than
	// mysterious. This mattered: when the credit was gated on the session's FIRST REQUEST the
	// figure read ~$0 on real traffic, and the reason was visible only in these counts —
	// 1,105 of 1,127 session starts had no cache to hit, because the previous session's had
	// expired. That gate is gone (it is TailChanged now, see Event.cachesplitSavedUSD); the
	// counts stay, because the replacement gate also fires rarely and for its own reason.
	//
	// SplitRequests is requests the split ran on; SplitTailMoved is the subset whose snapshot
	// had moved (the turns it can earn on); SplitCredited is the subset that also read the
	// stable half from cache instead of re-creating it (the turns it did earn on).
	// CachesplitHistorical is what the split earned on requests that predate the
	// instrumentation for it, valued at read time and never stored. nil when it could not be
	// priced — absent, not zero. See DB.CachesplitHistoricalUSD.
	CachesplitHistorical *CachesplitHistorical `json:"cachesplit_historical,omitempty"`

	// The idle keep-alive's ledger. Four numbers, and they belong together: a mechanism that
	// spends the caller's money to avoid a larger charge is only defensible if both sides are
	// on the same page, and either alone is misleading. KeepAlivePings is how many pings were
	// sent, KeepAlivePingUSD what they cost, KeepAliveSavedUSD the re-creations they avoided,
	// and KeepAliveNetUSD the difference — which is the only one of the four worth a decision.
	//
	// KeepAliveMissesAvoided is the count behind the saving: real requests that resumed after
	// an idle gap wider than the provider's lifetime and were served from cache anyway. On
	// this traffic such a request would otherwise have cost 8.5x what it did.
	KeepAlivePings         int64   `json:"keepalive_pings"`
	KeepAlivePingUSD       float64 `json:"keepalive_ping_usd"`
	KeepAliveSavedUSD      float64 `json:"keepalive_saved_usd"`
	KeepAliveNetUSD        float64 `json:"keepalive_net_usd"`
	KeepAliveMissesAvoided int64   `json:"keepalive_misses_avoided"`

	// CacheWrite1h is how many written tokens the provider billed at the ONE-HOUR tier. It is
	// the verification path for the mixed-TTL head: asking for `ttl: "1h"` on a model that does
	// not support it returns a normal 200 with the tokens on the 5-minute tier, so this is the
	// only thing that distinguishes granted from silently downgraded. Zero on this deployment,
	// and expected to be — see apply/headttl.go.
	CacheWrite1h int64 `json:"cache_write_1h"`

	// ONE definition of "moved", used here and at the write site (Recorder.ObserveSplit): the
	// tail moved if this request's tail hash differs from the most recent PREVIOUS tail hash
	// recorded for the session, and a session with no previously recorded tail counts as moved
	// because there was nothing there to match. The read-time recomputation used to compare
	// against the previous ROW's hash including 0 — and 0 means "nothing was split on that
	// turn", not "the tail was zero" — while the write-time map only ever remembered non-zero
	// hashes. The two therefore counted different things and the page showed 844 acted / 314
	// moved / 3 credited, which reads as broken arithmetic. Under the single definition the
	// same corpus is 844 / 16 / 3.
	//
	// SplitCreditedMoved is the reconciliation: the credited requests that the recomputation
	// also calls moved. It is 0 on the stored corpus, and that is a finding rather than a
	// rounding difference — the write-time map is process-scoped, so a proxy restart or a
	// session-eviction made three mid-session turns look like first sightings and they were
	// credited for a snapshot that had not moved. The write path no longer does that (a
	// session we have SEEN but whose tail we have forgotten is not a move), so from here on
	// the two counts agree by construction. The stored $0.03 is left exactly as recorded.
	SplitRequests      int64 `json:"split_requests"`
	SplitTailMoved     int64 `json:"split_tail_moved"`
	SplitCredited      int64 `json:"split_credited"`
	SplitCreditedMoved int64 `json:"split_credited_moved"`
	// The declarations no longer sent. TWO figures, because they are two different claims about
	// the same kind of tokens and adding them together would either overstate what this product
	// did or refuse a number a reader has asked for. See dash/declcredit.go for the argument.
	//
	// DeclFilterUSD is MEASURED and OURS: `requests.filtered_decl_tokens`, written by the filter
	// on every request it acted on, priced at the tier those requests paid. It is in
	// TotalSavedUSD. It was in NO total before, and that was a bug rather than a policy — the
	// savings walk is built on tokens_before, which is message text only, and a tool declaration
	// is not in `messages`, so the biggest measured lever in the product moved none of it.
	//
	// SelfRemovedUSD is MODELLED and the USER's: the account stopped declaring something and no
	// component of ours was involved. It is NOT in TotalSavedUSD, which states that its addends
	// are all ours. SelfRemovedOverlap is how many candidate rows were dropped because the filter
	// is already credited for the same tokens.
	DeclFilterUSD      float64 `json:"decl_filter_usd"`
	DeclFilterReads    int64   `json:"decl_filter_reads"`
	DeclFilterRequests int64   `json:"decl_filter_requests"`
	SelfRemovedUSD     float64 `json:"self_removed_usd"`
	SelfRemovedReads   int64   `json:"self_removed_reads"`
	SelfRemovedItems   int64   `json:"self_removed_items"`
	SelfRemovedOverlap int64   `json:"self_removed_overlap"`
	// DeclCreditPriced is false when a contributing model had no rates: the token counts above
	// stand and the dollars are the priced subset, which a consumer must not read as the total.
	DeclCreditPriced bool `json:"decl_credit_priced"`

	// TotalSavedUSD is OUR savings together: compaction's, less our own spend; the prefix
	// components'; the keep-alive's NET, which is its saving minus what its pings cost; and the
	// declaration filter's realized saving. FOUR addends, all ours, over disjoint token sets.
	//
	// The fourth one's disjointness is the only one that needs an argument, because half of what
	// it removes (a skill's listing entry) lives in `messages`, which is exactly where
	// `tokens_before` is measured. It holds because BOTH halves of the filter run in apply BEFORE
	// the request is normalized (apply/apply.go:441) and therefore before the pipeline baselines it
	// (components/pipeline.go:35), so `tokens_before` is measured on the already-filtered body and
	// the removal is invisible to Saved() rather than counted twice. The boundary is NORMALIZE, not
	// the `msgsRaw` read above it — that one exists so a later rewrite cannot move a byte offset,
	// and a filter placed between the two would still be safe for this purpose. Confirmed by
	// mutation: moving the skill half past normalize reds the apply-side guard.
	// Measured on a live run: on every request where the filter dropped 561 tokens,
	// tokens_before == tokens_after, saved_gross == 0 and baseline_cost_usd − cost_usd == 0,
	// while tokens_before itself fell by exactly the skill entry's weight.
	// TestTheDeclarationFilterSavingIsDisjointFromCompactions holds it, because "disjoint" is a
	// claim that a later reordering in apply could quietly falsify.
	//
	// The keep-alive contributes its net rather than its gross because the ping spend is excluded
	// from CostUSD — a ping is not agent traffic — so it has nowhere else in this walk to appear.
	TotalSavedUSD float64 `json:"total_saved_usd"`
	// TotalReducedUSD is TotalSavedUSD plus what the ACCOUNT removed itself. It answers "how much
	// smaller did this account's bill get", where TotalSavedUSD answers "how much of that did
	// context-guru do". A reader wants both and they are not the same question; printing one
	// under the other's label is how a dashboard takes credit for its user's work.
	TotalReducedUSD float64 `json:"total_reduced_usd"`
	// PrefixChangeCost is a DIAGNOSTIC, not a cost subtracted from net.
	//
	// It sums cost_usd over requests whose cache missed with reason prefix_change AND whose
	// PREVIOUS request in the same session had a component that mutated the transcript. That
	// is the population where "we rewrote history and the next turn re-billed the whole
	// prompt" is a live hypothesis, and it has to be visible: size-stratified it comes to
	// roughly $24 on the current corpus, and +$39 over transcripts past 60k tokens — larger
	// than every saving on this dashboard.
	//
	// It stays observational because mutation is NOT randomly assigned. Components act where
	// there is something to act on, which are also the long, churny turns most likely to break
	// a prefix for reasons of their own; and prefix_change already loses ties to ttl_expiry, so
	// this bucket is not even a clean partition of causes. Subtracting it from net would book a
	// correlation as a debt. Settling it needs the A/B, not a bigger query.
	//
	// PrefixChangeRequests is how many turns are behind it, which was missing — a dollar
	// figure with no denominator cannot be sized. PrefixChangeCostAll / RequestsAll are the
	// UNCONDITIONAL prefix_change bucket: every turn whose cache missed on a changed prefix,
	// whether or not we had just mutated anything. That is the whole exposure of the failure
	// mode this project exists to avoid — $156.55 over 214 requests on production, against
	// $7.10 of savings on the same page — and it was buried below the fold. It belongs at the
	// same altitude as the savings, and un-netted, for exactly the reason above.
	PrefixChangeCost        float64 `json:"prefix_change_cost_usd"`
	PrefixChangeRequests    int64   `json:"prefix_change_requests"`
	PrefixChangeCostAll     float64 `json:"prefix_change_cost_all_usd"`
	PrefixChangeRequestsAll int64   `json:"prefix_change_requests_all"`

	// The response-side latency, which is where the user-visible time actually goes.
	//
	// A third of streamed responses are BUFFERED — the client sees nothing until the whole
	// response has arrived — and those wait about 21 seconds longer for a first byte. That is
	// two orders of magnitude more delay than the pipeline itself adds (154 ms), and it was
	// reported nowhere on this dashboard: it existed only as a process-lifetime gauge on
	// /stats, which cannot be filtered by model, account or hour and resets on restart.
	//
	// Both averages are kept, never blended. TTFB is 0 by construction on a buffered response
	// (there is no first byte before the whole write), so one combined average would report
	// the healthiest number for exactly the requests having the worst experience.
	// SSERecorded and CacheTTLRecorded are the COVERAGE of the two newly captured facts: how
	// many requests in the window actually carry them. Both columns default to zero/"" on
	// every row written before they existed, so without these counts a five-day history reads
	// as "0% buffered, no request uses prompt caching" — which is not a measurement, it is the
	// absence of one, and it is the single easiest way for a new column to tell a lie.
	SSERecorded      int64 `json:"sse_recorded"`
	SSEStreamRows    int64 `json:"sse_stream_rows"`
	CacheTTLRecorded int64 `json:"cache_ttl_recorded"`

	// Both averages come from the SAME ttfb_ms column but they are NOT the same measurement,
	// and that is why they are named apart rather than blended.
	//
	// On a streamed response ttfb_ms is a real time-to-first-byte. On a buffered one
	// proxy.go leaves sseFirstByte zero and msSince falls back to time.Now(), so the value is
	// the TOTAL response time — which is also, for a buffered response, the moment the client
	// receives anything at all, because nothing is written until the whole response has
	// arrived. So the figure is the right one to show; calling it a "time to first byte"
	// alongside the streamed one is what would be wrong, and the UI names it as the total.
	SSEStreamed        int64   `json:"sse_streamed"`
	SSEBuffered        int64   `json:"sse_buffered"`
	SSEBufferedPct     float64 `json:"sse_buffered_pct"`
	TTFBMsAvgStreamed  float64 `json:"ttfb_ms_avg_streamed"`
	TotalMsAvgBuffered float64 `json:"total_ms_avg_buffered"`
	// CacheTTL buckets requests by the prompt-cache lifetime tier they ASKED for:
	// ephemeral_5m, ephemeral_1h, or "" for a body that carried no cache_control. Newly
	// captured — every row written before the column exists reads "", which is why the UI
	// must distinguish "no caching asked for" from "not recorded yet" by the coverage count
	// beside it rather than by the bucket alone.
	CacheTTL map[string]int64 `json:"cache_ttl"`
	// Breakpoints is the aggregate placement of prompt-cache breakpoints BY LOCATION. Where a
	// breakpoint sits decides how much prefix it protects, so a count alone cannot answer the
	// question: system=2/tools=0/messages=0/blocks=1 and system=0/tools=0/messages=3/blocks=0
	// are both "three breakpoints" and are not remotely the same prompt. These four columns
	// have existed since the placement work and were readable only one request at a time in
	// the drawer.
	Breakpoints BreakpointTotals `json:"breakpoints"`
	// CompactionResets is how many turns arrived SMALLER than the previous turn of the same
	// session — a context-window reset, or the agent compacting its own transcript.
	//
	// DERIVED from the requests table rather than captured, because the proxy's own counter is
	// a process-lifetime number in the metrics aggregator and this question is per-window. It
	// belongs on this page because it BOUNDS the amortization: replay accrues only while the
	// removed content keeps being re-sent, and a reset is where that stops. 1,364 of them on
	// measured traffic against a replay ceiling built by assuming none.
	CompactionResets int64 `json:"compaction_resets"`
	// EstimatorDivergence is the MEDIAN per-request ratio of provider-billed input to our own
	// tokens_before, over the requests where nothing was compacted — i.e. where the two are
	// measuring the same prompt and should agree.
	//
	// It is here because the divergence is the central honesty problem with every token figure
	// on this page, and a ratio of sums hides it. Measured on 12,882 uncompacted production
	// rows: ratio-of-sums 2.87x, but the MEDIAN per-request ratio is 3.38x (p25 2.43, p75 4.64,
	// p90 6.80). The median is the right one to show, because the question a reader has is "how
	// wrong is the number in front of me", not "how wrong is the corpus in aggregate".
	//
	// The gap is mostly SCOPE rather than tokenizer error: tokens_before is
	// schema.MessagesTokens, which counts message TEXT only — no system prompt, no tool
	// declarations, no JSON envelope — while the provider bills all of it. Dollars are
	// unaffected: they come from the provider's own reported usage.
	//
	// EstimatorDivergenceRows is the population, so a ratio over four requests is not read as a
	// fact about the deployment. Zero rows leaves the ratio at 0 and the UI shows nothing.
	EstimatorDivergence     float64 `json:"estimator_divergence"`
	EstimatorDivergenceRows int64   `json:"estimator_divergence_rows"`
	// BilledInputTokens is fresh + cache reads + cache writes: the input the PROVIDER counted.
	// It is here to be compared with TokensBefore, which is what our own tokenizer counted
	// over message text only — a different unit, roughly a third the size, and the reason no
	// token figure on this page may be read as a billed-token figure. See the tile.
	BilledInputTokens int64 `json:"billed_input_tokens"`

	CGLatencyMsAvg float64 `json:"cg_latency_ms_avg"`
	UpstreamMsAvg  float64 `json:"upstream_ms_avg"`
	CGLatencyMsP95 float64 `json:"cg_latency_ms_p95"`
	UpstreamMsP95  float64 `json:"upstream_ms_p95"`

	Expands      int64   `json:"expands"`
	ExpandTokens int64   `json:"expand_tokens"`
	ExpandRate   float64 `json:"expand_rate"`
	Reverts      int64   `json:"reverts"`
	Passthroughs int64   `json:"passthroughs"`

	// Accounting counts rows by token_accounting so a viewer can see how much of
	// the window is exactly measured versus estimated.
	Accounting map[string]int64 `json:"accounting"`
	// CacheMiss buckets requests by attribution, cold_start included as a
	// non-failure.
	CacheMiss map[string]int64 `json:"cache_miss"`
	// Uncompressed answers "why didn't you compact this?" — an empty-string key
	// means we did.
	Uncompressed map[string]int64 `json:"uncompressed"`

	// Tiers is the bill split by the tier the provider charged on, priced at read time
	// because nothing stores per-tier cost. It carries AddressableUSD — the input side, the
	// only part of a bill an input-side transformation can ever reach. Absent (nil) when no
	// rates were available; a percentage against a bill we could not split must not be shown.
	Tiers *TierCosts `json:"tier_costs,omitempty"`

	Denominators []Denominator   `json:"denominators"`
	Waterfall    []WaterfallStep `json:"waterfall"`
	// SafetyCost reports what our own safety mechanisms cost, beside what they
	// bought. A compaction proxy that only reports tokens removed is unfalsifiable.
	SafetyCost SafetyCost `json:"safety_cost"`
}

// BreakpointTotals is prompt-cache breakpoint placement summed by location, plus the number
// of requests behind it. The provider caps a request at four breakpoints and its automatic
// caching consumes one of the slots, so the interesting question is never "how many" but
// "where" — and only these four numbers together answer it.
type BreakpointTotals struct {
	System   int64 `json:"system"`
	Tools    int64 `json:"tools"`
	Messages int64 `json:"messages"`
	Blocks   int64 `json:"blocks"`
	// Requests is the denominator, so a total can be read as a per-request average rather
	// than as a raw sum whose size only reflects the window.
	Requests int64 `json:"requests"`
	// Modal is the single most common (system, tools, messages, blocks) placement in the
	// window, as a display string, with ModalRequests behind it. An average placement is
	// meaningless — 1.5 breakpoints in the system block is not a thing any request did —
	// so the typical case is reported as the mode, not the mean.
	Modal         string `json:"modal"`
	ModalRequests int64  `json:"modal_requests"`
}

// SafetyCost is the price of context-guru's own protective mechanisms.
type SafetyCost struct {
	// FrozenTokens is content cache-aware compaction deliberately left alone. Its
	// benefit is the cache reads it preserved; its cost is compaction not done.
	//
	// FrozenReadUSD and FrozenWriteRiskUSD are that benefit, in dollars, which this panel
	// promised in prose and never computed: what the frozen prefix was billed as at the
	// cache-READ rate, and what re-creating it at the write rate instead would have added.
	// Both are absent (0 with Priced false) when no rates were available. Until now the
	// 396.5M frozen tokens on production were displayed as a cost with no counterpart at all.
	FrozenTokens       int64   `json:"frozen_tokens"`
	FrozenReadUSD      float64 `json:"frozen_read_usd"`
	FrozenWriteRiskUSD float64 `json:"frozen_write_risk_usd"`
	// Priced is false when the frozen figures could not be valued, so the UI shows "—"
	// rather than a benefit of zero for a mechanism whose benefit is simply unpriced.
	Priced bool `json:"priced"`
	// RestoredTokens is content we offloaded and the model asked back for — a
	// premature offload, paid for twice.
	RestoredTokens int64 `json:"restored_tokens"`
	// RevertedRuns is components the never-worse guard rolled back.
	RevertedRuns int64 `json:"reverted_runs"`
	// CGLLMCostUSD is what context-guru's own model calls cost.
	CGLLMCostUSD float64 `json:"cg_llm_cost_usd"`
	// CGLatencyMsTotal is the wall time context-guru itself added.
	CGLatencyMsTotal float64 `json:"cg_latency_ms_total"`
	Description      string  `json:"description"`
}

// splitMoved is the ONE definition of "the volatile tail moved", in SQL. It matches
// Recorder.ObserveSplit's in-process test: differs from the last non-zero tail hash recorded
// for this session, and a session with no earlier non-zero hash counts as moved. Written once
// and referenced twice so the count and its reconciliation cannot drift apart.
// withKeepAlive returns f with ping rows included. A named helper rather than an inline field
// set, so the two places that legitimately see pings are greppable.
func withKeepAlive(f Filter) Filter {
	f.WithKeepAlive = true
	return f
}

const splitMoved = `r.split_stable_tokens > 0 AND r.split_tail_hash <> 0
	AND r.split_tail_hash <> COALESCE((
		SELECT p.split_tail_hash FROM requests p
		WHERE p.session_id = r.session_id AND p.split_tail_hash <> 0
		  AND (p.ts < r.ts OR (p.ts = r.ts AND p.id < r.id))
		ORDER BY p.ts DESC, p.id DESC LIMIT 1), r.split_tail_hash + 1)`

// afterOurMutation is the ONE definition of "the session's previous turn mutated something",
// in SQL, shared by the prefix-change cost and its request count so the two cannot drift.
//
// `p.keepalive = 0` is load-bearing. A keep-alive ping is a row in `requests` like any other,
// so without the predicate a ping becomes "the previous turn"; a ping has no
// `request_components` rows, the EXISTS goes false, and a genuine prefix_change row silently
// drops out of the diagnostic. That is the flattering direction on the one figure that
// measures our OWN harm — it made context-guru look less responsible for prefix changes than
// it is. Filter.where() already keeps pings out of every agent aggregate for the same reason;
// this subquery reaches past it because it names `requests` itself.
const afterOurMutation = `EXISTS (
			SELECT 1 FROM request_components c WHERE c.mutated = 1 AND c.request_id = (
				SELECT p.id FROM requests p
				WHERE p.session_id = r.session_id AND p.keepalive = 0
				  AND (p.ts < r.ts OR (p.ts = r.ts AND p.id < r.id))
				ORDER BY p.ts DESC, p.id DESC LIMIT 1)
		)`

// Overview computes the headline aggregates for the filtered window.
func (d *DB) Overview(f Filter) (*Overview, error) {
	cond, args := f.where()
	o := &Overview{
		Since: f.Since, Until: f.Until,
		Accounting: map[string]int64{}, CacheMiss: map[string]int64{}, Uncompressed: map[string]int64{},
		CacheTTL: map[string]int64{},
	}
	var cgAvg, upAvg, ttfbAvg, upBufAvg sql.NullFloat64
	err := d.sql.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT r.session_id),
		COALESCE(SUM(r.tokens_before),0), COALESCE(SUM(r.tokens_after),0), COALESCE(SUM(r.saved_unique),0),
		COALESCE(SUM(r.attempted_tokens),0), COALESCE(SUM(r.frozen_tokens),0),
		COALESCE(SUM(r.fresh_input),0), COALESCE(SUM(r.cache_read),0), COALESCE(SUM(r.cache_write),0),
		COALESCE(SUM(r.output_tokens),0),
		COALESCE(SUM(r.cost_usd),0), COALESCE(SUM(r.baseline_cost_usd),0), COALESCE(SUM(r.cg_llm_cost_usd),0),
		COALESCE(SUM(r.cache_saved_usd),0),
		-- Our own cache saving. A plain SUM of a per-request column, because all three
		-- conditions that qualify a request were settled at write time where the model's
		-- rates and the session's history were both in hand (Event.cachesplitSavedUSD).
		-- The predecessor did the component test here as a correlated EXISTS over
		-- request_components, which was both slower and wrong: it credited every cache read
		-- in a session, including the later turns that would have hit anyway.
		COALESCE(SUM(r.cachesplit_saved_usd),0),
		-- The counts behind that dollar figure. tail_moved is derived rather than stored: a
		-- request's tail moved if the previous request in the same session carried a different
		-- one. Recomputable from the table on purpose, so the money can be audited without
		-- trusting the write path.
		COALESCE(SUM(CASE WHEN r.split_stable_tokens > 0 THEN 1 ELSE 0 END),0),
		-- "Moved", one definition, shared with the write path. The previous hash is the last
		-- NON-ZERO one in the session: 0 means nothing was split on that turn, so comparing
		-- against it counted every first split as a move and then again on the next turn.
		-- No previous non-zero hash at all still counts as moved -- there was nothing to
		-- match -- which is why the sentinel is this row's own hash plus one rather than 0.
		COALESCE(SUM(CASE WHEN `+splitMoved+` THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.cachesplit_saved_usd > 0 THEN 1 ELSE 0 END),0),
		-- The reconciliation: credited AND moved under that one definition. A gap means the
		-- write-time map had forgotten a session it had in fact seen (restart, eviction) and
		-- read a first sighting where there was none. See Overview.SplitCreditedMoved.
		COALESCE(SUM(CASE WHEN r.cachesplit_saved_usd > 0 AND `+splitMoved+` THEN 1 ELSE 0 END),0),
		-- The prefix-change diagnostic (Overview.PrefixChangeCost): what the turns cost where
		-- the cache missed on a changed prefix AND the session's previous turn had mutated
		-- something. Derived, never stored, and never netted off — see the field's comment for
		-- why a correlation this confounded may not be turned into a debt.
		COALESCE(SUM(CASE WHEN r.cache_miss_reason = 'prefix_change' AND `+afterOurMutation+` THEN r.cost_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.cache_miss_reason = 'prefix_change' AND `+afterOurMutation+` THEN 1 ELSE 0 END),0),
		-- The whole prefix_change bucket, unconditional: the total exposure of the failure
		-- mode, not only the part adjacent to one of our own mutations.
		COALESCE(SUM(CASE WHEN r.cache_miss_reason = 'prefix_change' THEN r.cost_usd ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.cache_miss_reason = 'prefix_change' THEN 1 ELSE 0 END),0),
		AVG(r.cg_latency_ms), AVG(r.upstream_ms),
		COALESCE(SUM(r.expands),0), COALESCE(SUM(r.expand_tokens),0), COALESCE(SUM(r.reverts),0),
		COALESCE(SUM(CASE WHEN r.uncompressed_reason <> '' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(r.cg_latency_ms),0),
		-- Streaming split. A "streamed" row is one with a measured first byte; a buffered row
		-- has the flag and no TTFB, by construction (proxy.go). Counting them separately is
		-- what stops the buffered third from disappearing into a healthy-looking average.
		COALESCE(SUM(CASE WHEN r.sse_buffered = 0 AND r.ttfb_ms > 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.sse_buffered = 1 THEN 1 ELSE 0 END),0),
		AVG(CASE WHEN r.sse_buffered = 0 AND r.ttfb_ms > 0 THEN r.ttfb_ms END),
		-- The same column on the buffered population, where it holds the TOTAL response time
		-- rather than a first-byte time (proxy.go leaves the first-byte instant zero and
		-- msSince falls back to now). For a buffered response those coincide from the client's
		-- point of view, since nothing is written until the whole response has arrived.
		AVG(CASE WHEN r.sse_buffered = 1 AND r.ttfb_ms > 0 THEN r.ttfb_ms END),
		-- Breakpoint placement by location, and the requests behind it.
		COALESCE(SUM(r.cache_bp_system),0), COALESCE(SUM(r.cache_bp_tools),0),
		COALESCE(SUM(r.cache_bp_messages),0), COALESCE(SUM(r.cache_bp_blocks),0),
		COALESCE(SUM(CASE WHEN r.cache_bp_system + r.cache_bp_tools
			+ r.cache_bp_messages + r.cache_bp_blocks > 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(r.fresh_input + r.cache_read + r.cache_write),0),
		-- Coverage for the two new columns. A streaming request that carries neither a TTFB
		-- nor the buffered flag predates the capture; counting those apart is what keeps a
		-- history of zeros from reading as a measurement of zero.
		COALESCE(SUM(CASE WHEN r.ttfb_ms > 0 OR r.sse_buffered = 1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.stream = 1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN r.cache_ttl <> '' THEN 1 ELSE 0 END),0),
		-- The SAVING half of the keep-alive ledger: the per-request credit the write path
		-- computed while it still had the session's gap and the ping's own refreshed-token
		-- count in hand. The COST half cannot be summed here — this query excludes ping rows
		-- (Filter.WithKeepAlive) so the request count and every average stay agent-only — so it
		-- has its own query below.
		COALESCE(SUM(r.keepalive_saved_usd),0),
		COALESCE(SUM(CASE WHEN r.keepalive_saved_usd > 0 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(r.cache_write_1h),0)
		FROM requests r WHERE `+cond, args...).Scan(
		&o.Requests, &o.Sessions, &o.TokensBefore, &o.TokensAfter, &o.SavedUnique,
		&o.AttemptedTokens, &o.FrozenTokens, &o.FreshInput, &o.CacheRead, &o.CacheWrite,
		&o.OutputTokens, &o.CostUSD, &o.BaselineCostUSD, &o.CGLLMCostUSD, &o.CacheSavedUSD,
		&o.CachesplitSavedUSD, &o.SplitRequests, &o.SplitTailMoved, &o.SplitCredited,
		&o.SplitCreditedMoved,
		&o.PrefixChangeCost, &o.PrefixChangeRequests, &o.PrefixChangeCostAll, &o.PrefixChangeRequestsAll,
		&cgAvg, &upAvg, &o.Expands, &o.ExpandTokens, &o.Reverts, &o.Passthroughs,
		&o.SafetyCost.CGLatencyMsTotal,
		&o.SSEStreamed, &o.SSEBuffered, &ttfbAvg, &upBufAvg,
		&o.Breakpoints.System, &o.Breakpoints.Tools, &o.Breakpoints.Messages,
		&o.Breakpoints.Blocks, &o.Breakpoints.Requests, &o.BilledInputTokens,
		&o.SSERecorded, &o.SSEStreamRows, &o.CacheTTLRecorded,
		&o.KeepAliveSavedUSD, &o.KeepAliveMissesAvoided, &o.CacheWrite1h)
	if err != nil {
		return nil, err
	}
	o.CGLatencyMsAvg, o.UpstreamMsAvg = cgAvg.Float64, upAvg.Float64
	o.TTFBMsAvgStreamed, o.TotalMsAvgBuffered = ttfbAvg.Float64, upBufAvg.Float64
	if n := o.SSEStreamed + o.SSEBuffered; n > 0 {
		o.SSEBufferedPct = float64(o.SSEBuffered) / float64(n) * 100
	}
	o.SavedGross = o.TokensBefore - o.TokensAfter
	o.SavedAdjusted = o.SavedUnique - int64(o.ExpandTokens)
	if o.SavedUnique > 0 {
		o.OvercountRatio = float64(o.SavedGross) / float64(o.SavedUnique)
	}
	if o.ReplayTokens = o.SavedGross - o.SavedUnique; o.ReplayTokens < 0 {
		o.ReplayTokens = 0
	}
	// The ceiling on that replay: every unique reduction times the number of later AGENT turns
	// in its own session. A correlated count off the (session_id, ts) index, ~80 ms over 14k
	// requests, and only over rows that removed something.
	//
	// Computed as ALL later turns minus the later PINGS, in two statements, rather than as one
	// count with `p.keepalive = 0` inside it. The predicate belongs there logically — a ping is
	// a verbatim resend of a prefix the agent already sent, it carries no new transcript, and
	// PR #86's invariant is that a ping is not a turn — but `keepalive` is not in
	// idx_requests_session, so putting it in the correlated subquery turns an index-only count
	// into a row fetch per candidate. Measured: 231 ms to 277 ms on the 10,000-row perf
	// fixture, and 10.6 s to 15.3 s of that same test under -race, which is enough to blow its
	// budget on a loaded box. The correction below is driven from the PING side instead, which
	// is a tiny population — one linear pass plus an index lookup per ping.
	//
	// The bug this fixes: the inner count had NO predicate at all, so a ping counted as a later
	// turn that could replay the reduction. On a fixture with ONE ping it inflated the ceiling
	// from 30 to 50 tokens and dragged replay_realized_pct down with it, which reads as
	// compaction realising less of its value than it does. Found by
	// TestPingRowsStayOutOfAgentAggregates, which compares the whole Overview key by key rather
	// than the aggregates somebody thought of.
	//
	// Neither statement is scoped to the window or the tenant on its INNER side, and they agree
	// about that, which is what matters: session ids are `tenant:uuid` so a cross-tenant
	// collision is not reachable, and the ceiling is deliberately about the session's whole life
	// rather than the filtered slice of it.
	//
	// NOT rewritten as a window function, despite looking like the same shape CompactionResets
	// was fixed for. Tried it, verified against live production data: byte-identical result,
	// but consistently ~50-70% SLOWER (0.9s correlated vs 1.5s windowed, 5 runs). The difference
	// from CompactionResets: that query's outer filter (tokens_before > 0) matches ~100% of
	// rows, so a correlated subquery paid its per-invocation overhead on nearly every row and a
	// single sort over everyone wins. Here the outer filter (saved_unique > 0) matches only
	// 8.5% of rows (measured on the live corpus) — the correlated subquery only ever runs for
	// that 8.5%, each an indexed lookup, while a window function must still sort the WHOLE
	// table before the outer filter can narrow anything. Left as the correlated form on purpose.
	// Everything from here to the percentiles below is a separate, independent read against
	// the same filter — none of these queries consumes another's result (the two exceptions,
	// the inflation correction and the estimator median, are noted where they are), so nothing
	// stops them running concurrently instead of one after another.
	//
	// THE GROUP BUYS NO WALL TIME AT TODAY'S SIZES, and the claim that it does — that this
	// "drops to roughly the slowest single query rather than their sum" — is retracted. Measured
	// on a 16,444-request corpus: the sixteen queries in this function sum to 587 ms and the
	// slowest is 186 ms, and Overview() measures 520-556 ms. That is the sum. The reason is the
	// driver: `modernc.org/sqlite` reads do not run in parallel here. N copies of one ~280 ms
	// query, 16 cores, SetMaxOpenConns(100) — n=2 is 1.05x, n=4 is 0.68x, n=8 is 0.51x, so
	// concurrency is SLOWER than sequential once there is any of it.
	//
	// What is NOT established: the ~9s this comment used to attribute to sequential execution,
	// and the /api/stats timeouts attributed to that. Both figures predate the LAG rewrite below
	// and idx_tooldecl_session, and nobody has re-measured this stretch sequentially either side
	// of those two changes — so which change earned that improvement is an open question, not
	// something this comment gets to answer. Note also that 0.51x at n=8 is a measurement of
	// TODAY's sizes; it does not establish that concurrency never helped at 9s scale, where the
	// contention profile may genuinely have differed. So the group stays: a deletion needs its
	// own before/after, which nobody has run.
	//
	// Still true, and unaffected by any of the above: SQLite's WAL mode already gave each of
	// these its own read snapshot when they ran sequentially — nothing here was ever one atomic
	// view of the table — so concurrency changes only how long the caller waits, never what any
	// individual query reads.
	var (
		replayProjectedRaw, inflation          int64
		ttl                                    map[string]int64
		bs, bt, bm, bb, modalRequests          int64
		estimatorDivergenceRows                int64
		estimatorDivergence                    float64
		keepAlivePings                         int64
		keepAlivePingUSD                       float64
		accountingM, cacheMissM, uncompressedM map[string]int64
		p95cg, p95up                           float64
		invalidConfigRequests                  int64
	)
	var g errgroup.Group
	// See InvalidConfigRequests' own comment: preset is set to "invalid" by
	// proxy/tenancy.go's build() exactly when this account's stored configuration failed to
	// build, on every request forwarded uncompacted while that lasts. Cheap: cond already
	// scopes this to the tenant/window idx_requests_tenant covers, so this is a filter over an
	// already-narrow row set, not a fresh scan.
	g.Go(func() error {
		return d.sql.QueryRow(`SELECT COUNT(*) FROM requests r
			WHERE `+cond+` AND r.preset = 'invalid'`, args...).Scan(&invalidConfigRequests)
	})
	// The replay ceiling's raw form, corrected below by the inflation query once both are in —
	// see the comment above `splitMoved` usage earlier in this function for the ceiling's own
	// derivation, and see the correlated-vs-window-function tradeoff explained where this query
	// used to live: NOT rewritten as a window function despite looking like the same shape
	// CompactionResets was fixed for. Tried it, verified against live production data:
	// byte-identical result, but consistently ~50-70% SLOWER (0.9s correlated vs 1.5s windowed,
	// 5 runs). The difference from CompactionResets: that query's outer filter (tokens_before >
	// 0) matches ~100% of rows, so a correlated subquery paid its per-invocation overhead on
	// nearly every row and a single sort over everyone wins. Here the outer filter
	// (saved_unique > 0) matches only 8.5% of rows (measured on the live corpus) — the
	// correlated subquery only ever runs for that 8.5%, each an indexed lookup, while a window
	// function must still sort the WHOLE table before the outer filter can narrow anything.
	// Left as the correlated form on purpose.
	g.Go(func() error {
		return d.sql.QueryRow(`SELECT COALESCE(SUM(r.saved_unique * (
				SELECT COUNT(*) FROM requests p WHERE p.session_id = r.session_id
				  AND (p.ts > r.ts OR (p.ts = r.ts AND p.id > r.id)))),0)
			FROM requests r WHERE `+cond+` AND r.saved_unique > 0`, args...).Scan(&replayProjectedRaw)
	})
	// The correction to the above, driven from the PING side: one pass to find the pings, then
	// ONE indexed sum per ping over the earlier rows of its own session. O(pings x session)
	// rather than a second full pass over the filtered rows, and pings are ~1% of rows — on a
	// deployment that has never pinged it is a single index scan over an empty set.
	//
	// It used to sit behind an `EXISTS(SELECT 1 FROM requests WHERE keepalive = 1)` gate, claiming
	// the correction then "costs literally nothing where the mechanism has never run". It does not:
	// `keepalive` is in no index, so the EXISTS is itself a scan. Timed alternately on the
	// 10,000-row perf fixture, 5 trials: gated 52.7 ms against 53.0 ms ungated at 0 pings, and
	// 58.0 against 57.3 at 100 — one extra scan for no measurable saving, so it is gone.
	g.Go(func() error {
		return d.sql.QueryRow(`SELECT COALESCE(SUM((
				SELECT COALESCE(SUM(r.saved_unique),0) FROM requests r
				  WHERE r.session_id = p.session_id AND r.saved_unique > 0
				    AND (r.ts < p.ts OR (r.ts = p.ts AND r.id < p.id))
				    AND `+cond+`)),0)
			FROM requests p WHERE p.keepalive = 1`, args...).Scan(&inflation)
	})
	// Which cache TTL tier each request asked for. A map rather than columns because ""
	// (no cache_control at all) is a real third answer that must not be folded into 5m.
	g.Go(func() error {
		m, err := d.countBy(cond, args, "cache_ttl")
		if err != nil {
			return err
		}
		ttl = m
		return nil
	})
	// The MODAL breakpoint placement, not the mean: 1.5 breakpoints in a system block is not
	// a thing any request did, and an average across locations describes no prompt at all.
	g.Go(func() error {
		err := d.sql.QueryRow(`SELECT r.cache_bp_system, r.cache_bp_tools, r.cache_bp_messages,
				r.cache_bp_blocks, COUNT(*) AS n
			FROM requests r WHERE `+cond+`
			GROUP BY 1,2,3,4 ORDER BY n DESC LIMIT 1`, args...).Scan(&bs, &bt, &bm, &bb, &modalRequests)
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	})
	// Context resets, DERIVED: a turn that arrived smaller than the previous turn of its own
	// session. This is the bound on amortization — replay accrues only while removed content
	// keeps being re-sent, and this is where that stops — so it belongs beside the replay
	// ceiling that is computed by assuming it never happens.
	//
	// A window function, not the correlated subquery this used to be: that version priced a
	// covering index (idx_requests_session_tb, see schema.go) and still measured ~20s on the
	// production corpus, unchanged by the index — because the cost was never the lookup, it
	// was doing 65k+ SEPARATE subquery invocations. tokens_before > 0 is true for essentially
	// every row here, so the filter narrows nothing; a correlated subquery per outer row pays
	// per-invocation overhead 65k times over where a window function sorts once. LAG only
	// looks at the IMMEDIATELY preceding row, where the subquery it replaces would skip past a
	// prior row with tokens_before <= 0 to find one further back — on this corpus that never
	// happens (tokens_before > 0 holds for every row measured), so this is not a behavior
	// change in practice, but it is not a byte-for-byte port of the old query's guarantee.
	g.Go(func() error {
		// Two columns through the window sort, not `r.*`. The CTE used to select every column
		// of `requests` and carry all of them through the PARTITION BY sort, when the only thing
		// the outer query wants from it is one LAG value per row. Emitting (id, prev) and joining
		// back on the primary key sorts ~2 columns instead of ~56; the 16k primary-key probes
		// that costs are cheaper than the payload they replace, which is the opposite of the
		// tradeoff Facets' component list turns on (see query.go) — invocation count wins there,
		// sort payload wins here, so neither is a rule.
		//
		// Measured: 242-316 ms to 59-63 ms on the 16,444-request corpus (5 runs), and 1,324 ms
		// to 553 ms read-only against the production database. Result identical service-wide
		// (37,954 rows there, 526 here) and under each tenant scope separately. Keeping the CTE
		// self-contained but listing only the filterable columns was also tried and is the worse
		// of the two at 135 ms.
		q := `WITH prev AS (
			SELECT r.id AS rid, LAG(CASE WHEN r.tokens_before > 0 THEN r.tokens_before END)
				OVER (PARTITION BY r.session_id ORDER BY r.ts, r.id) AS prev_tokens_before
			FROM requests r
		) SELECT COUNT(*) FROM requests r JOIN prev ON prev.rid = r.id WHERE ` + cond + `
			AND r.tokens_before > 0 AND prev.prev_tokens_before IS NOT NULL
			AND r.tokens_before < prev.prev_tokens_before`
		return d.sql.QueryRow(q, args...).Scan(&o.CompactionResets)
	})
	// The estimator check, on the only population where the two counts are comparable: requests
	// where nothing was removed, so tokens_before and the provider's billed input describe the
	// same prompt. A median rather than a mean, and rather than a ratio of sums, because a
	// handful of enormous requests otherwise dominate and the figure stops describing a typical
	// row. Computed with OFFSET over an ordered ratio, the same shape as DB.percentile — which
	// takes a bare column name and so cannot express this ratio. The median query depends on
	// the row-count query run just before it (the OFFSET is half the count), so those two stay
	// sequential WITHIN this one goroutine — only across the other units does this run concurrently.
	g.Go(func() error {
		const ratioPop = ` AND r.tokens_before = r.tokens_after AND r.tokens_before > 0
			AND r.token_accounting = 'complete' AND r.fresh_input + r.cache_read + r.cache_write > 0`
		if err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests r WHERE `+cond+ratioPop,
			args...).Scan(&estimatorDivergenceRows); err != nil {
			return err
		}
		if estimatorDivergenceRows > 0 {
			var med sql.NullFloat64
			if err := d.sql.QueryRow(`SELECT
				CAST(r.fresh_input + r.cache_read + r.cache_write AS REAL) / r.tokens_before AS ratio
				FROM requests r WHERE `+cond+ratioPop+`
				ORDER BY ratio ASC LIMIT 1 OFFSET ?`,
				append(append([]any(nil), args...), estimatorDivergenceRows/2)...).Scan(&med); err != nil {
				return err
			}
			estimatorDivergence = med.Float64
		}
		return nil
	})
	// The COST half, over the same window and the same filters but with ping rows included.
	// A second query rather than a CASE in the first, because the first deliberately cannot see
	// them: one predicate, one meaning.
	g.Go(func() error {
		kaCond, kaArgs := withKeepAlive(f).where()
		return d.sql.QueryRow(`SELECT
			COALESCE(SUM(CASE WHEN r.keepalive = 1 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN r.keepalive = 1 THEN r.cost_usd ELSE 0 END),0)
			FROM requests r WHERE `+kaCond, kaArgs...).Scan(&keepAlivePings, &keepAlivePingUSD)
	})
	for name, col := range map[string]string{
		"accounting": "token_accounting", "cache_miss": "cache_miss_reason", "uncompressed": "uncompressed_reason",
	} {
		name, col := name, col // captured per-goroutine, not by the shared loop variable
		g.Go(func() error {
			m, err := d.countBy(cond, args, col)
			if err != nil {
				return err
			}
			switch name {
			case "accounting":
				accountingM = m
			case "cache_miss":
				cacheMissM = m
			case "uncompressed":
				uncompressedM = m
			}
			return nil
		})
	}
	g.Go(func() error {
		v, err := d.percentile(cond, args, "cg_latency_ms", 0.95)
		if err != nil {
			return err
		}
		p95cg = v
		return nil
	})
	g.Go(func() error {
		v, err := d.percentile(cond, args, "upstream_ms", 0.95)
		if err != nil {
			return err
		}
		p95up = v
		return nil
	})
	if err := g.Wait(); err != nil {
		return nil, err
	}

	o.ReplayProjectedTokens = replayProjectedRaw
	if o.ReplayProjectedTokens -= inflation; o.ReplayProjectedTokens < 0 {
		o.ReplayProjectedTokens = 0
	}
	if o.ReplayProjectedTokens > 0 {
		o.ReplayRealizedPct = float64(o.ReplayTokens) / float64(o.ReplayProjectedTokens) * 100
	}
	o.CacheTTL = ttl
	o.Breakpoints.ModalRequests = modalRequests
	if modalRequests > 0 {
		o.Breakpoints.Modal = fmt.Sprintf("system=%d, tools=%d, messages=%d, blocks=%d", bs, bt, bm, bb)
	}
	o.EstimatorDivergenceRows = estimatorDivergenceRows
	o.EstimatorDivergence = estimatorDivergence
	if o.Requests > 0 {
		o.ExpandRate = float64(o.Expands) / float64(o.Requests)
	}
	o.NetSavedUSD = o.BaselineCostUSD - o.CostUSD - o.CGLLMCostUSD
	o.KeepAlivePings, o.KeepAlivePingUSD = keepAlivePings, keepAlivePingUSD
	// The keep-alive's net, and the one number a decision rests on. Not folded into
	// TotalSavedUSD: presenting a saving without the spend that bought it is exactly the
	// dishonesty this ledger exists to prevent, and CostUSD above no longer carries the pings
	// (they are excluded from the agent-traffic aggregate), so the two halves are only
	// comparable here.
	o.KeepAliveNetUSD = o.KeepAliveSavedUSD - o.KeepAlivePingUSD
	// THE NET, never the gross. o.CostUSD excludes ping rows, so the pings' own spend appears
	// nowhere else in this walk — adding KeepAliveSavedUSD here would present a saving without
	// the spend that bought it, which is the exact dishonesty the ledger exists to prevent. The
	// three addends are disjoint token sets: compaction's removals, the prefix split's stable
	// half, and the entries the pings refreshed.
	o.TotalSavedUSD = o.NetSavedUSD + o.CachesplitSavedUSD + o.KeepAliveNetUSD
	// Both totals exist before the priced additions land, so a deployment with no rates — or a
	// caller that does not attach the credit — reads a total that is short rather than one that
	// is absent. SetDeclCredit moves both.
	o.TotalReducedUSD = o.TotalSavedUSD
	o.Accounting = accountingM
	o.CacheMiss = cacheMissM
	o.Uncompressed = uncompressedM
	o.CGLatencyMsP95, o.UpstreamMsP95 = p95cg, p95up
	o.InvalidConfigRequests = invalidConfigRequests

	o.SafetyCost.FrozenTokens = o.FrozenTokens
	o.SafetyCost.RestoredTokens = o.ExpandTokens
	o.SafetyCost.RevertedRuns = o.Reverts
	o.SafetyCost.CGLLMCostUSD = o.CGLLMCostUSD
	// The freeze's own caveat, on the payload rather than only in a comment: frozen == 0 is
	// not evidence the provider's cache is cold. It means OUR tracker was reset (restart,
	// evicted entry) — 3,092 such requests on the production corpus still cache-HIT, for
	// 404.4M cache-read tokens. Anyone reading a low frozen fraction as permission to rewrite
	// deep history is reading it backwards, and priced on sonnet-5 that is worth about -$708
	// against +$0.62 of upside.
	o.SafetyCost.Description = "What context-guru's own protective mechanisms cost, " +
		"shown beside what they bought: cache-aware freezing forgoes compaction on the " +
		"already-cached prefix (its benefit is the cache reads it preserved), restored " +
		"tokens are offloads the model asked back for, reverted runs are the never-worse " +
		"guard firing, and the LLM cost is context-guru's own model spend. A LOW frozen " +
		"figure is not evidence the provider's cache is cold: zero means our own prefix " +
		"tracker was reset, not that the provider dropped anything — measured, 3,092 such " +
		"requests still hit cache for 404.4M read tokens."

	o.Denominators = o.denominators()
	o.Waterfall = o.waterfall()
	return o, nil
}

// SetTiers attaches the read-time per-tier costing and the two figures that depend on it:
// the safety panel's benefit half, which the panel has always described in prose and never
// had a number for. Called by the API layer, which holds the rates — DB.Overview deliberately
// cannot price anything, so that a rate change never rewrites a stored row.
func (o *Overview) SetTiers(t *TierCosts) {
	if t == nil {
		return
	}
	o.Tiers = t
	o.SafetyCost.FrozenReadUSD = t.FrozenReadUSD
	o.SafetyCost.FrozenWriteRiskUSD = t.FrozenWriteRiskUSD
	o.SafetyCost.Priced = true
}

// SetDeclCredit attaches the declarations-no-longer-sent credit and folds each half into the
// total it belongs in: the filter's measured saving into TotalSavedUSD (it is ours), the account's
// own removals into TotalReducedUSD only (they are not). Called by the API layer, which holds the
// rates, for the same reason SetTiers is — DB.Overview deliberately cannot price anything.
func (o *Overview) SetDeclCredit(c *DeclCredit) {
	if c == nil {
		return
	}
	o.DeclFilterUSD, o.DeclFilterReads = c.FilterUSD, c.FilterReads
	o.DeclFilterRequests = int64(c.FilterRequests)
	o.SelfRemovedUSD, o.SelfRemovedReads = c.SelfUSD, c.SelfReads
	o.SelfRemovedItems, o.SelfRemovedOverlap = int64(c.SelfItems), int64(c.SelfOverlap)
	o.DeclCreditPriced = c.Priced
	o.TotalSavedUSD += c.FilterUSD
	o.TotalReducedUSD = o.TotalSavedUSD + c.SelfUSD
}

// denominators builds the labelled savings ratios. Each one names its divisor.
func (o *Overview) denominators() []Denominator {
	// New provider-billed input: what actually entered the model as new content
	// this window (fresh + cache-write), plus what we removed before it could be
	// billed. Guarded on the billed figure being non-zero, so a deployment with no
	// usage data cannot divide savings by themselves and report ~100%.
	var newInput int64
	newInputAvail := o.FreshInput+o.CacheWrite > 0
	if newInputAvail {
		newInput = o.FreshInput + o.CacheWrite + o.SavedUnique
	}
	ds := []Denominator{
		// GROSS over attempted, not unique over attempted. Both sides of this ratio are now
		// per-turn quantities: attempted_tokens is what compaction was allowed to touch on
		// each turn, re-counted every turn, so the numerator has to be the saving counted the
		// same way. Dividing a numerator deduplicated ACROSS turns by a denominator recounted
		// on every turn is a basis mismatch, and it made this ratio 13x too small (0.140%
		// where the same-basis figure is 1.838% on production traffic) — the one bar on the
		// page whose job is to answer "are we any good when we do have something to work
		// with" was the one reading closest to zero. unique_whole below is still there for
		// the conservative view.
		denom("attempted", "gross, of what we tried to compact", o.SavedGross, o.AttemptedTokens,
			"GROSS savings ÷ the tokens compaction was ALLOWED to touch this turn (the "+
				"uncached tail when cache-aware). Both sides are per-turn quantities — the "+
				"denominator is re-counted every turn, so the numerator is too. Answers 'are "+
				"we good when we have something to work with?' Excludes the frozen prefix we "+
				"deliberately never touched."),
		denom("new_input", "of new provider-billed input", o.SavedUnique, newInput,
			"Unique savings ÷ (fresh input + cache writes + what we removed). The most "+
				"honest economic ratio: it does not recount transcript history that the "+
				"provider served from cache and never re-billed. Unavailable when the "+
				"provider reports no usage data — reported as n/a, never as 100%."),
		denom("whole_request", "of the whole request (diluted)", o.SavedGross, o.TokensBefore,
			"Gross savings ÷ every content token in every request. Kept for transparency, "+
				"but a long session re-sends its history each turn, so this denominator "+
				"grows quadratically and the ratio trends to ~0% however well compaction works."),
		denom("unique_whole", "unique, of the whole request", o.SavedUnique, o.TokensBefore,
			"Unique savings over the same diluted denominator: the most conservative "+
				"number this dashboard can produce."),
	}
	if !newInputAvail {
		ds[1].Description += " (No provider usage data in this window.)"
	}
	return ds
}

// waterfall builds the honest cost walk: baseline, each reduction, each penalty we owe back,
// and the net. Signed deltas — negative is money not spent — and each bar is drawn against
// the largest absolute delta rather than accumulated, so a step may be read on its own.
func (o *Overview) waterfall() []WaterfallStep {
	compactionSaving := o.BaselineCostUSD - o.CostUSD
	// Split the compaction saving into the part attributable to LLM-based
	// components and the deterministic remainder, proportional to unique savings.
	steps := []WaterfallStep{
		{Key: "baseline", Label: "Baseline cost (no context-guru)", DeltaUSD: o.BaselineCostUSD, Total: true,
			Description: "What this window's requests would have cost with nothing removed: the " +
				"billed cost, plus the UNIQUE removed tokens priced at the cache-WRITE rate they " +
				"would have entered as, plus the re-sent remainder priced at the rate each request " +
				"ACTUALLY paid — the cache-read rate where its cache hit, the cache-creation rate " +
				"where it had expired and the whole prompt was re-billed."},
		{Key: "compaction", Label: "Compaction savings", DeltaUSD: -compactionSaving,
			Description: "Cost avoided because content never reached the provider. Only the UNIQUE " +
				"saving earns the cache-write rate (12.5x a read on a prompt-caching backend); the " +
				"re-sent remainder earns whatever the request itself was billed at, which is the " +
				"read rate on a warm turn and the creation rate on a turn whose cache had expired. " +
				"Pricing gross savings as writes is how a dashboard overstates itself by its own " +
				"overcount_ratio; pricing an expired turn's removals as reads was how this one " +
				"understated the turns that matter most."},
		{Key: "cg_llm", Label: "context-guru's own LLM cost", DeltaUSD: o.CGLLMCostUSD,
			Description: "What context-guru's own model calls (extract_llm, summarize) cost. Paid " +
				"out of the savings above; a component whose spend exceeds its saving is " +
				"underwater and the per-component view says so."},
		{Key: "net", Label: "Net cost with context-guru", DeltaUSD: o.CostUSD + o.CGLLMCostUSD, Total: true,
			Description: "Billed cost plus context-guru's own spend — what you actually paid."},
		{Key: "net_saved", Label: "Net savings", DeltaUSD: o.NetSavedUSD, Total: true,
			Description: "Baseline minus net. Negative means context-guru cost more than it saved " +
				"in this window, which is a real outcome the dashboard will not hide."},
		{Key: "cachesplit_saved", Label: "Prefix-cache savings", DeltaUSD: -o.CachesplitSavedUSD,
			Description: "A SECOND saving, outside the walk above, and the only cache figure we " +
				"claim. Claude Code ends its big system block with a live environment snapshot, so " +
				"the block's own breakpoint hashes the churn and the prefix never matches across " +
				"sessions; cachesplit moves the breakpoint onto the stable half. Counted only where " +
				"the component rewrote the prefix, the provider then read it from cache, AND the " +
				"snapshot had MOVED since this session's previous request — the turn that would " +
				"otherwise have missed. A turn whose snapshot did not move would have hit whether " +
				"or not we ever split, so it is not ours. Priced against a cache " +
				"MISS, which is what the counterfactual actually is: those tokens carry " +
				"cache_control, so a miss bills them as creation at 1.25x fresh, not at 1x. A " +
				"floor — a stable prefix serves a whole session and this counts one request of it."},
		{Key: "keepalive_ping", Label: "Keep-alive pings", DeltaUSD: o.KeepAlivePingUSD,
			Description: "What the idle keep-alive SPENT: one minimal request per idle span, " +
				"re-reading a session's cached prefix so the provider refreshes its 5-minute " +
				"lifetime for free. This is the caller's own money, spent while nobody was at the " +
				"keyboard, which is why it is a line of its own and why the mechanism is opt-in. " +
				"Every ping is a row, priced from its own usage — no estimate."},
		{Key: "keepalive_saved", Label: "Keep-alive savings", DeltaUSD: -o.KeepAliveSavedUSD,
			Description: "The prefix re-creations those pings avoided. Counted only on a request " +
				"that resumed after a gap wider than the provider's lifetime, was served from " +
				"cache anyway, read more than it wrote, and had a ping of ours during the gap; " +
				"credited at most the tokens that ping actually refreshed. Priced against a cache " +
				"MISS, because those tokens carry cache_control and a miss bills them as creation " +
				"at 1.25x rather than 1x. A ceiling rather than a floor, and the one figure here " +
				"that is: the provider's cache is keyed on content, so another session sending the " +
				"same prefix would have refreshed it for nothing."},
		{Key: "decl_filter_saved", Label: "Declarations no longer sent", DeltaUSD: -o.DeclFilterUSD,
			Description: "Tool and MCP schemas, and skill listing entries, that the declaration " +
				"filter stopped sending — measured on requests that were really sent, from the " +
				"count the filter itself wrote on each one, priced at the tier that request paid. " +
				"It is a THIRD saving outside the compaction walk above and for a mechanical " +
				"reason: that walk is built on tokens_before, which counts message text only, and " +
				"a tool declaration lives in the top-level `tools` array. So this reduction moved " +
				"no figure in it and appeared in no total on this page at all, even though 82.7% " +
				"of a declared catalogue is never invoked and it is the largest lever measured in " +
				"this project."},
		{Key: "total_saved", Label: "Total cost avoided", DeltaUSD: o.TotalSavedUSD, Total: true,
			Description: "Net compaction savings, plus prefix-cache savings, plus the idle " +
				"keep-alive's NET — its savings minus what its pings cost — plus the declarations " +
				"the removal filter stopped sending. FOUR disjoint token " +
				"sets, all ours, so nothing is counted twice: compaction's removals never reached " +
				"the provider, the split moved a breakpoint over tokens that did, the " +
				"keep-alive refreshed entries that already existed, and the filter's declarations " +
				"were dropped before the compaction baseline was even measured — so they are " +
				"absent from the first line rather than counted in it. The keep-alive enters as a net " +
				"because the pings' own spend is not in the billed-cost line above — ping rows are " +
				"excluded from agent traffic — so this is the only place both halves are " +
				"comparable, and its saving half is a CEILING (see Keep-alive savings). It is not " +
				"the cost of a fully " +
				"uncached world: the provider's own cache saved far more than this on the same " +
				"traffic (cache_saved_usd, reported by the API as a diagnostic), and none of that " +
				"is credited here."},
		{Key: "self_removed", Label: "Removed by you, not by us", DeltaUSD: -o.SelfRemovedUSD,
			Description: "Declarations this ACCOUNT stopped carrying on its own — an MCP server " +
				"you removed, a skill you switched off. The tokens genuinely were not billed, and " +
				"no component of ours was involved, so this is deliberately OUTSIDE the total " +
				"above and inside the one below. It is also MODELLED rather than measured: the " +
				"evidence is that the inventory is a time series and a name is in its early part " +
				"and not its late part, which cannot tell a removal from a session that ran in a " +
				"different environment — on the production snapshot the largest candidate is the " +
				"editor integration, which is simply absent outside the IDE. Every row and its " +
				"own evidence is on the Inventory tab; rows the filter is already credited for " +
				"are excluded here rather than counted twice."},
		{Key: "total_reduced", Label: "Total the bill came down", DeltaUSD: o.TotalReducedUSD,
			Total: true,
			Description: "Everything above, ours and yours: the total cost avoided plus what you " +
				"removed yourself. It answers 'how much smaller did this bill get', where the " +
				"total above answers 'how much of that did context-guru do'. Both are worth " +
				"knowing and they are not the same number, so neither is printed under the " +
				"other's label."},
	}
	return steps
}

// countBy groups the filtered window by one column.
func (d *DB) countBy(cond string, args []any, col string) (map[string]int64, error) {
	rows, err := d.sql.Query(`SELECT r.`+col+`, COUNT(*) FROM requests r WHERE `+cond+` GROUP BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// percentile computes an exact percentile with an ORDER BY + OFFSET, which is
// what makes p95 answerable at all — headroom exposes no histogram, so its p95 is
// uncomputable. Exact beats a bucketed estimate here: SQLite sorts a filtered
// window of a few hundred thousand floats in milliseconds off the ts index.
func (d *DB) percentile(cond string, args []any, col string, p float64) (float64, error) {
	var n int64
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM requests r WHERE `+cond+` AND r.`+col+` > 0`, args...).Scan(&n); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	idx := int64(float64(n-1) * p)
	var v sql.NullFloat64
	err := d.sql.QueryRow(`SELECT r.`+col+` FROM requests r WHERE `+cond+` AND r.`+col+` > 0
		ORDER BY r.`+col+` ASC LIMIT 1 OFFSET ?`, append(append([]any(nil), args...), idx)...).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v.Float64, err
}
