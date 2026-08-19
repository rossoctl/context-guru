package dash

import "database/sql"

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

	TokensBefore int64 `json:"tokens_before"`
	TokensAfter  int64 `json:"tokens_after"`
	// SavedGross re-counts the same compaction every turn the agent re-sends the
	// transcript. SavedUnique counts each distinct compaction once. SavedAdjusted
	// subtracts content we offloaded and then had to serve back.
	SavedGross     int64   `json:"saved_gross"`
	SavedUnique    int64   `json:"saved_unique"`
	SavedAdjusted  int64   `json:"saved_adjusted"`
	OvercountRatio float64 `json:"overcount_ratio"`

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
	// CacheSavedUSD is what the provider's prompt cache saved over paying the fresh
	// rate for the same tokens, and CacheSavedProtectedUSD is the part of it that
	// landed on requests where one of OUR cache components acted — the honest upper
	// bound on what to credit context-guru for, since the agent places most
	// breakpoints itself.
	CacheSavedUSD          float64 `json:"cache_saved_usd"`
	CacheSavedProtectedUSD float64 `json:"cache_saved_protected_usd"`
	// TotalSavedUSD is both savings together: what compaction avoided plus what the
	// prompt cache avoided, less our own spend. The headline number, decomposed by
	// the waterfall so no part of it is taken on trust.
	TotalSavedUSD float64 `json:"total_saved_usd"`

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

	Denominators []Denominator   `json:"denominators"`
	Waterfall    []WaterfallStep `json:"waterfall"`
	// SafetyCost reports what our own safety mechanisms cost, beside what they
	// bought. A compaction proxy that only reports tokens removed is unfalsifiable.
	SafetyCost SafetyCost `json:"safety_cost"`
}

// SafetyCost is the price of context-guru's own protective mechanisms.
type SafetyCost struct {
	// FrozenTokens is content cache-aware compaction deliberately left alone. Its
	// benefit is the cache reads it preserved; its cost is compaction not done.
	FrozenTokens int64 `json:"frozen_tokens"`
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

// Overview computes the headline aggregates for the filtered window.
func (d *DB) Overview(f Filter) (*Overview, error) {
	cond, args := f.where()
	o := &Overview{
		Since: f.Since, Until: f.Until,
		Accounting: map[string]int64{}, CacheMiss: map[string]int64{}, Uncompressed: map[string]int64{},
	}
	var cgAvg, upAvg sql.NullFloat64
	err := d.sql.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT r.session_id),
		COALESCE(SUM(r.tokens_before),0), COALESCE(SUM(r.tokens_after),0), COALESCE(SUM(r.saved_unique),0),
		COALESCE(SUM(r.attempted_tokens),0), COALESCE(SUM(r.frozen_tokens),0),
		COALESCE(SUM(r.fresh_input),0), COALESCE(SUM(r.cache_read),0), COALESCE(SUM(r.cache_write),0),
		COALESCE(SUM(r.output_tokens),0),
		COALESCE(SUM(r.cost_usd),0), COALESCE(SUM(r.baseline_cost_usd),0), COALESCE(SUM(r.cg_llm_cost_usd),0),
		COALESCE(SUM(r.cache_saved_usd),0),
		-- The cache-saving subset that landed on requests where a component of ours whose job
		-- IS the cache actually acted. Folded into this aggregate rather than run as a second
		-- query, so a dashboard load still costs one scan of the window (measured: 199 ms as
		-- its own query over 200,000 rows). A CASE, not a JOIN: joining request_components
		-- would multiply every other SUM here by the number of components that ran.
		COALESCE(SUM(CASE WHEN EXISTS (SELECT 1 FROM request_components c
			WHERE c.request_id = r.id AND c.mutated = 1
			  AND c.component IN ('cachesplit','cacheinject')) THEN r.cache_saved_usd ELSE 0 END),0),
		AVG(r.cg_latency_ms), AVG(r.upstream_ms),
		COALESCE(SUM(r.expands),0), COALESCE(SUM(r.expand_tokens),0), COALESCE(SUM(r.reverts),0),
		COALESCE(SUM(CASE WHEN r.uncompressed_reason <> '' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(r.cg_latency_ms),0)
		FROM requests r WHERE `+cond, args...).Scan(
		&o.Requests, &o.Sessions, &o.TokensBefore, &o.TokensAfter, &o.SavedUnique,
		&o.AttemptedTokens, &o.FrozenTokens, &o.FreshInput, &o.CacheRead, &o.CacheWrite,
		&o.OutputTokens, &o.CostUSD, &o.BaselineCostUSD, &o.CGLLMCostUSD, &o.CacheSavedUSD,
		&o.CacheSavedProtectedUSD, &cgAvg, &upAvg, &o.Expands, &o.ExpandTokens, &o.Reverts, &o.Passthroughs,
		&o.SafetyCost.CGLatencyMsTotal)
	if err != nil {
		return nil, err
	}
	o.CGLatencyMsAvg, o.UpstreamMsAvg = cgAvg.Float64, upAvg.Float64
	o.SavedGross = o.TokensBefore - o.TokensAfter
	o.SavedAdjusted = o.SavedUnique - int64(o.ExpandTokens)
	if o.SavedUnique > 0 {
		o.OvercountRatio = float64(o.SavedGross) / float64(o.SavedUnique)
	}
	if o.Requests > 0 {
		o.ExpandRate = float64(o.Expands) / float64(o.Requests)
	}
	o.NetSavedUSD = o.BaselineCostUSD - o.CostUSD - o.CGLLMCostUSD
	o.TotalSavedUSD = o.NetSavedUSD + o.CacheSavedUSD

	for name, col := range map[string]string{
		"accounting": "token_accounting", "cache_miss": "cache_miss_reason", "uncompressed": "uncompressed_reason",
	} {
		m, err := d.countBy(cond, args, col)
		if err != nil {
			return nil, err
		}
		switch name {
		case "accounting":
			o.Accounting = m
		case "cache_miss":
			o.CacheMiss = m
		case "uncompressed":
			o.Uncompressed = m
		}
	}

	p95cg, err := d.percentile(cond, args, "cg_latency_ms", 0.95)
	if err != nil {
		return nil, err
	}
	p95up, err := d.percentile(cond, args, "upstream_ms", 0.95)
	if err != nil {
		return nil, err
	}
	o.CGLatencyMsP95, o.UpstreamMsP95 = p95cg, p95up

	o.SafetyCost.FrozenTokens = o.FrozenTokens
	o.SafetyCost.RestoredTokens = o.ExpandTokens
	o.SafetyCost.RevertedRuns = o.Reverts
	o.SafetyCost.CGLLMCostUSD = o.CGLLMCostUSD
	o.SafetyCost.Description = "What context-guru's own protective mechanisms cost, " +
		"shown beside what they bought: cache-aware freezing forgoes compaction on the " +
		"already-cached prefix (its benefit is the cache reads it preserved), restored " +
		"tokens are offloads the model asked back for, reverted runs are the never-worse " +
		"guard firing, and the LLM cost is context-guru's own model spend."

	o.Denominators = o.denominators()
	o.Waterfall = o.waterfall()
	return o, nil
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
		denom("attempted", "of what we tried to compact", o.SavedUnique, o.AttemptedTokens,
			"Unique savings ÷ the tokens compaction was ALLOWED to touch this turn (the "+
				"uncached tail when cache-aware). Answers 'are we good when we have something "+
				"to work with?' Excludes the frozen prefix we deliberately never touched."),
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
		{Key: "cache_saved", Label: "Prompt-cache savings", DeltaUSD: -o.CacheSavedUSD,
			Description: "A SECOND saving, outside the walk above: every cache-read token was " +
				"billed at the read rate instead of the fresh rate it would have cost with no " +
				"cache at all. The provider's mechanism, not ours — but a compaction proxy that " +
				"rewrites deep history destroys it, so protecting it is work, and " +
				"cache_saved_protected_usd is the part that landed on requests where one of our " +
				"cache components acted. Not inside net savings: the baseline above already " +
				"assumes the cache behaved as it did, so adding it there would double-count."},
		{Key: "total_saved", Label: "Total cost avoided", DeltaUSD: o.TotalSavedUSD, Total: true,
			Description: "Net compaction savings plus prompt-cache savings: what this window did " +
				"not pay for what it sent, plus what it did not pay for what we removed. The two " +
				"token sets are disjoint, so nothing is counted twice — but this is not the cost " +
				"of a fully uncached world either, because the compaction half still prices a warm " +
				"turn's re-sent remainder as a cache read. It also includes requests context-guru " +
				"did not touch: on bypassed or observe-mode traffic the compaction half is zero " +
				"and every dollar here is the provider's cache."},
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
