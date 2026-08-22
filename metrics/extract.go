package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
)

// Extraction metrics (issue #28 part F). The existing per-component stats answer "how
// many tokens did it save?", which for extract_llm is the wrong headline: it is the only
// component that spends money, so gross savings can look great while the component is
// underwater. /stats reported the tool's LLM cost in a SEPARATE field from savings, so
// nothing anywhere showed the component net-negative — the ~8x loss was invisible until
// someone divided two numbers by hand.
//
// NET-AFTER-COST is therefore the headline here, and the trigger reason is recorded per
// activation because an operator's first question about an expensive component is always
// "why did this run?".
//
// These are process-global counters, matching cheapmodel.Usage's existing scope.
var (
	xCalls      atomic.Int64 // extraction LLM calls actually made
	xCacheHits  atomic.Int64 // calls avoided by the global result cache
	xSuppressed atomic.Int64 // calls suppressed by the economic gate
	xGrossSaved atomic.Int64 // tokens removed (unique, first application only)
	xLatencyMs  atomic.Int64 // cumulative wall time in extraction calls
	xLookups    atomic.Int64 // result-cache lookups (hits + misses), for the hit rate
	// xValueNano is the realized dollar value of what was removed, in nanodollars, recorded
	// BY THE COMPONENT at the rate each removal was actually worth.
	//
	// It exists because the alternative — tokens x a constant rate chosen from the cache MODE
	// — was wrong by an order of magnitude in the one regime this component runs in. /stats
	// priced every saved token at the cache-READ rate and counted it once, while the component
	// itself values a cold-turn token at the cache-WRITE rate (12.5x more) and collects it
	// again on each replay. MEASURED: 44,073 tokens reported as $0.0132 of value against
	// $0.8291 of spend — a 63x loss — where the honest figure over the same data is between
	// 1.4x and 7.5x underwater. A metric that overstates a loss by 12x is as unusable as one
	// that hides it: both make the fix unattributable.
	//
	// Nanodollars so the accumulator can stay atomic; a call's value is ~1e-5 USD, so an
	// int64 of nanodollars holds ~9e9 USD of headroom.
	xValueNano atomic.Int64

	xReasonMu sync.Mutex
	xReasons  = map[string]int64{} // trigger/suppression reason -> count

	// A ring of recent per-call latencies, for the MEDIAN. The mean cannot answer "are
	// calls slow?" on this workload: measured n=8 on one gateway, p50 3,748 ms against a
	// max of 11,663, and an 8-token no-op call spanned 1,490-15,800 ms. Latency here is
	// queue time, so one tail sample moves the mean past a brake the typical call is
	// nowhere near. A ring rather than a histogram because the only consumer is one
	// threshold comparison and 64 samples is already more evidence than the gate needs.
	xLatMu   sync.Mutex
	xLatRing [64]float64
	xLatN    int
)

// latencyP50 returns the median of the retained samples, and how many there are.
func latencyP50() (float64, int) {
	xLatMu.Lock()
	defer xLatMu.Unlock()
	n := xLatN
	if n > len(xLatRing) {
		n = len(xLatRing)
	}
	if n == 0 {
		return 0, 0
	}
	cp := make([]float64, n)
	copy(cp, xLatRing[:n])
	sort.Float64s(cp)
	return cp[n/2], n
}

// RecordExtractionCall notes one extraction LLM call and its wall time.
func RecordExtractionCall(latencyMs float64) {
	xCalls.Add(1)
	xLatencyMs.Add(int64(latencyMs))
	xLatMu.Lock()
	xLatRing[xLatN%len(xLatRing)] = latencyMs
	xLatN++
	xLatMu.Unlock()
}

// ExtractionP50LatencyMs returns the MEDIAN observed wall time per extraction call and the
// number of samples behind it. The gate reads this rather than the mean to decide whether
// speculative calls have become too slow to be worth their wall clock — see
// offload.tooSlowToExplore for the measurement that made the mean unusable here.
func ExtractionP50LatencyMs() (float64, int64) {
	p50, n := latencyP50()
	return p50, int64(n)
}

// RecordExtractionCacheLookup notes one global result-cache lookup and whether it hit.
// A hit is a call AVOIDED — the cheapest possible outcome, and the source of ~93% of the
// component's realized value in the Terminal-Bench measurement.
func RecordExtractionCacheLookup(hit bool) {
	xLookups.Add(1)
	if hit {
		xCacheHits.Add(1)
	}
}

// ExtractionAvgLatencyMs returns the observed mean wall time per extraction call and the
// number of calls it averages. The gate reads this to stop SPECULATIVE calls once they are
// observed to be slow — exploration spends wall clock as well as money, and an agent with a
// task deadline feels the former more (PR #37: 17.8s across 2 calls that saved 0 tokens).
func ExtractionAvgLatencyMs() (float64, int64) {
	calls := xCalls.Load()
	if calls == 0 {
		return 0, 0
	}
	return float64(xLatencyMs.Load()) / float64(calls), calls
}

// RecordExtractionSuppressed notes that the economic gate declined a call, with its reason.
func RecordExtractionSuppressed(reason string) {
	xSuppressed.Add(1)
	RecordExtractionReason(reason)
}

// RecordExtractionReason counts one trigger/suppression reason.
func RecordExtractionReason(reason string) {
	if reason == "" {
		return
	}
	xReasonMu.Lock()
	xReasons[reason]++
	xReasonMu.Unlock()
}

// RecordExtractionSaving notes tokens removed by an accepted extraction (count each
// distinct compaction once — the caller dedups by content key).
func RecordExtractionSaving(tokens int) {
	if tokens > 0 {
		xGrossSaved.Add(int64(tokens))
	}
}

// RecordExtractionValue notes the dollars one removal was worth, priced by the component at
// the rate that removal was actually billed at — the cache-write rate on a cold turn for the
// first application, the cache-read rate for each later replay of the frozen result.
//
// Called at BOTH sites deliberately: crediting only the first application under-reports by
// however much the replay is worth, and crediting the replays at the first application's rate
// over-reports by 12.5x. The component is the only layer that knows which regime a request
// was in, so it is the layer that prices it.
func RecordExtractionValue(usd float64) {
	if usd > 0 {
		xValueNano.Add(int64(usd * 1e9))
	}
}

// ExtractStats is the extraction economics block served inside /stats. It is ADDITIVE:
// every pre-existing /stats field keeps its name and meaning, because deploy/harbor/*.py
// parses them.
type ExtractStats struct {
	Calls            int64 `json:"calls"`            // extraction LLM calls made
	CallsAvoided     int64 `json:"calls_avoided"`    // global result-cache hits
	CallsSuppressed  int64 `json:"calls_suppressed"` // declined by the economic gate
	CacheLookups     int64 `json:"cache_lookups"`    //
	GrossSavedTokens int64 `json:"gross_saved_tokens"`

	// CacheHitRate is calls_avoided / cache_lookups.
	CacheHitRate float64 `json:"cache_hit_rate"`
	// AvgLatencyMs is mean wall time per extraction call — the component's latency cost.
	AvgLatencyMs float64 `json:"avg_latency_ms"`

	// PromptCacheReadTokens is the evidence for issue #28 part A. If this stays 0 while
	// calls climbs, the preamble's cache_control breakpoint is being SILENTLY IGNORED
	// (the prefix is below the model's minimum cacheable length) and the split is buying
	// nothing. Do not infer a cache win from the fact that a breakpoint was placed.
	PromptCacheReadTokens  int64 `json:"prompt_cache_read_tokens"`
	PromptCacheWriteTokens int64 `json:"prompt_cache_write_tokens"`

	// ExtractionCostUSD is what the component SPENT; GrossValueUSD is what its saved
	// tokens are WORTH at the rate they would actually have been billed; NetValueUSD is
	// the honest headline. Negative means the component is underwater and should be off.
	ExtractionCostUSD float64 `json:"extraction_cost_usd"`
	GrossValueUSD     float64 `json:"gross_value_usd"`
	NetValueUSD       float64 `json:"net_value_usd"`

	// Reasons counts why extraction ran or was suppressed, most frequent first.
	Reasons map[string]int64 `json:"reasons,omitempty"`
	// TopReason is the single most common reason — the one-line operator answer.
	TopReason string `json:"top_reason,omitempty"`
}

// ExtractSnapshot builds the extraction stats.
//
// cost is the component's own LLM spend. perSavedTokenUSD is the value of ONE saved token
// at the rate it would actually have been billed (cache-read vs fresh — the caller knows
// the traffic's cache-awareness); the value side is computed HERE, against this
// component's own GrossSavedTokens.
//
// Taking a RATE rather than a pre-computed total is deliberate. The obvious signature
// (grossValue float64) invites the caller to pass the pipeline-wide savings figure, which
// prices every other component's work (format, dedup, cmdfilter, extract, …) against
// extract_llm's cost and reports the component as POSITIVE when its own arithmetic says
// otherwise. That is the single number this whole issue exists to get right, so the
// signature makes the mistake impossible to express.
func ExtractSnapshot(cost, perSavedTokenUSD float64, cacheWrite, cacheRead int64) ExtractStats {
	calls := xCalls.Load()
	lookups := xLookups.Load()
	hits := xCacheHits.Load()
	gross := xGrossSaved.Load()
	// The component's own realized valuation wins where it has one: it knows each removal's
	// regime and counts the replays. perSavedTokenUSD stays the fallback for a host that
	// records nothing (library users, /compact), where one flat rate is all there is.
	grossValue := float64(gross) * perSavedTokenUSD
	if v := xValueNano.Load(); v > 0 {
		grossValue = float64(v) / 1e9
	}

	s := ExtractStats{
		Calls: calls, CallsAvoided: hits, CallsSuppressed: xSuppressed.Load(),
		CacheLookups: lookups, GrossSavedTokens: gross,
		PromptCacheReadTokens: cacheRead, PromptCacheWriteTokens: cacheWrite,
		ExtractionCostUSD: round4(cost), GrossValueUSD: round4(grossValue),
		NetValueUSD: round4(grossValue - cost),
	}
	if lookups > 0 {
		s.CacheHitRate = float64(hits) / float64(lookups)
	}
	if calls > 0 {
		s.AvgLatencyMs = float64(xLatencyMs.Load()) / float64(calls)
	}

	xReasonMu.Lock()
	if len(xReasons) > 0 {
		s.Reasons = make(map[string]int64, len(xReasons))
		keys := make([]string, 0, len(xReasons))
		for k, v := range xReasons {
			s.Reasons[k] = v
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if xReasons[keys[i]] != xReasons[keys[j]] {
				return xReasons[keys[i]] > xReasons[keys[j]]
			}
			return keys[i] < keys[j] // stable output for equal counts
		})
		s.TopReason = keys[0]
	}
	xReasonMu.Unlock()
	return s
}

func round4(f float64) float64 {
	return float64(int64(f*10000+sign(f)*0.5)) / 10000
}

func sign(f float64) float64 {
	if f < 0 {
		return -1
	}
	return 1
}
