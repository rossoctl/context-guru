package kvcache

import (
	"math"
	"sort"
	"time"
)

// The historical replay simulator: walk the selected trajectories in wall-clock order,
// ask the strategy what to do using only what was knowable at each instant, then use the
// real next-request time ONLY to score the decision.
//
// # What a strategy can and cannot change
//
// A TTL policy can convert a `ttl_expiry` miss into a hit, and it can turn a hit into a
// miss by letting an entry lapse. It CANNOT fix a `prefix_change` — the content moved, so
// there was nothing to match at any lifetime — and it cannot fix a `cold_start`, which is
// the absence of an entry rather than the expiry of one. Those two reasons therefore force
// a miss whatever the strategy chose, and the count is reported as ForcedMisses so the
// ceiling on any arm is visible rather than implied. A simulator that let a 1-hour TTL
// "rescue" a prefix change would report savings that no configuration can reach.
//
// # What a ping actually does
//
// A keep-alive fires one INTERVAL after the last activity and every interval after that, up
// to MaxPings, and only while the idle span is still open. At the instant it fires:
//
//   - the entry is still alive: the ping is a cache READ, priced at the read rate, and it
//     refreshes the entry for its own tier's lifetime (Semantics.PingRefreshesTTL).
//   - the entry has already lapsed: the ping is a cache WRITE. It re-creates the prefix at
//     the creation rate — 12.5x what a refresh costs at the 5-minute tier, 20x at the
//     one-hour tier — which is the pathology of a schedule whose interval exceeds the
//     lifetime it is protecting. Counted as PingsThatRewrote, and never priced as a read.
//
// The INTERVAL depends on the tier, and that is the whole cost difference between a
// five-minute and a one-hour keep-alive: one ping costs the same either way (a read), but a
// five-minute entry must be touched roughly twelve times as often to be held for the same
// span. Config.PingIdle is the 5-minute interval and Config.PingIdle1h the one-hour one.

// PingsPerSpan is how many keep-alives one idle span of `gap` attracts under (idle, max).
//
// The first fires at `idle` after the last activity, each subsequent one `idle` after the
// last, and `max` caps the count. A span no longer than `idle` attracts none.
//
// This is the same arithmetic as dash.PingsPerSpan, which serves the keep-alive tab's own
// calculator. It is restated here because the dependency runs the other way — dash imports
// this package — and TestPingScheduleMatchesTheKeepAliveTab in package dash asserts the two
// agree over a table of inputs, so they cannot drift.
func PingsPerSpan(gap, idle time.Duration, max int) int {
	if idle <= 0 || max <= 0 || gap <= idle {
		return 0
	}
	n := int(math.Floor(float64(gap-idle)/float64(idle))) + 1
	if n > max {
		n = max
	}
	return n
}

// Config is everything the replay needs beyond the strategy itself.
type Config struct {
	// Prices is the rate table. A model with no rates leaves its requests UNPRICED — counted,
	// never valued.
	Prices *PriceList
	// Semantics is the provider's cache behaviour. Zero value means DefaultSemantics().
	Semantics Semantics
	// PingIdle is the keep-alive interval for a FIVE-MINUTE entry and PingIdle1h the interval
	// for a ONE-HOUR one; MaxPings caps either. Both default a little inside the lifetime they
	// protect, so a refresh lands before the deadline rather than on it. Zero values mean the
	// shipped defaults.
	PingIdle   time.Duration
	PingIdle1h time.Duration
	MaxPings   int
	// WindowEnd bounds the OPEN idle span at the end of a conversation, in epoch ms.
	//
	// A conversation whose last request is inside the window has an idle span whose length
	// nobody knows — its successor may be outside the window, or may never come. Its pings and
	// its retention are therefore billed only up to here, and that is not a rounding-down: a
	// keep-alive policy cannot know a conversation is over either, so it really does spend up
	// to MaxPings on the last request of every dead session, and charging that is what stops
	// the ping arms looking cheaper than they are. What the bound does is stop the charge
	// running past the data.
	//
	// The pings it produces are counted apart (Result.PingsOnOpenSpans) because their NUMBER
	// depends on where the window happens to end whenever the remaining span is shorter than
	// MaxPings intervals, which is an assumption the closed spans do not need.
	//
	// 0 means the last timestamp in the dataset. Never time.Now(): the same historical window
	// has to replay to the same answer twice.
	WindowEnd int64
}

func (c Config) withDefaults(reqs []*Request) Config {
	if c.Semantics == (Semantics{}) {
		c.Semantics = DefaultSemantics()
	}
	if c.PingIdle <= 0 {
		c.PingIdle = DefaultPingIdle
	}
	if c.PingIdle1h <= 0 {
		c.PingIdle1h = DefaultPingIdle1h
	}
	if c.MaxPings <= 0 {
		c.MaxPings = DefaultMaxPings
	}
	if c.WindowEnd <= 0 {
		for _, r := range reqs {
			if r.TS > c.WindowEnd {
				c.WindowEnd = r.TS
			}
		}
	}
	return c
}

// pingInterval is the keep-alive interval for one tier.
func (c Config) pingInterval(tier TTL) time.Duration {
	if tier == TTL1h {
		return c.PingIdle1h
	}
	return c.PingIdle
}

// Group is one strategy's result restricted to one user or one model.
type Group struct {
	Key      string  `json:"key"`
	Requests int64   `json:"requests"`
	TotalUSD float64 `json:"total_usd"`
	Hits     int64   `json:"hits"`
	Misses   int64   `json:"misses"`
	HitRate  float64 `json:"hit_rate_pct"`
	Pings    int64   `json:"pings"`
	PingUSD  float64 `json:"ping_usd"`
	Writes5m int64   `json:"writes_5m"`
	Writes1h int64   `json:"writes_1h"`
	Unpriced int64   `json:"unpriced"`
	// Valued is whether ANY request in this group could be priced, exactly as Result.Valued is
	// for the whole replay — and it is here because omitting it forced the consumer to spell the
	// predicate a second time as `unpriced < requests` on every per-user and per-model row. Two
	// spellings of one predicate is how they come to disagree the day one of them gains a caveat,
	// which is the argument for Result.Valued and applies unchanged one level down.
	Valued bool `json:"valued"`
}

// Latency is the window's own measured cost of a cache miss, in milliseconds, and the two
// populations it was measured over.
//
// Derived from the selected rows and nothing else: the mean upstream time of the requests
// that really hit, against the mean of the ones that really missed. Known=false where either
// population is too small to mean anything, and then no latency figure is reported at all
// rather than a difference of two noisy means.
type Latency struct {
	PerMissMs float64 `json:"per_miss_ms"`
	HitN      int64   `json:"hit_n"`
	MissN     int64   `json:"miss_n"`
	HitMeanMs float64 `json:"hit_mean_ms"`
	MissMean  float64 `json:"miss_mean_ms"`
	Known     bool    `json:"known"`
}

// latencyMinN is how many of each population the differential needs. 20 is small, and it is
// stated rather than assumed: below it the difference of two means is not a measurement.
const latencyMinN = 20

// MeasureLatency computes the window's own hit/miss latency differential from the rows.
func MeasureLatency(reqs []*Request) Latency {
	var l Latency
	var hitSum, missSum float64
	for _, r := range reqs {
		if r.UpstreamMs <= 0 {
			continue // not recorded: absent, not zero
		}
		if r.Hit {
			l.HitN++
			hitSum += r.UpstreamMs
			continue
		}
		l.MissN++
		missSum += r.UpstreamMs
	}
	if l.HitN < latencyMinN || l.MissN < latencyMinN {
		return l
	}
	l.HitMeanMs = hitSum / float64(l.HitN)
	l.MissMean = missSum / float64(l.MissN)
	l.PerMissMs = l.MissMean - l.HitMeanMs
	l.Known = true
	return l
}

// Result is one strategy's whole replay.
type Result struct {
	Strategy    string `json:"strategy"`
	Description string `json:"description,omitempty"`

	Requests      int64 `json:"requests"`
	Conversations int64 `json:"conversations"`

	// The cost decomposition. TotalUSD is their sum and is the figure a comparison uses.
	TotalUSD      float64 `json:"total_usd"`
	FreshInputUSD float64 `json:"fresh_input_usd"`
	CacheReadUSD  float64 `json:"cache_read_usd"`
	CacheWriteUSD float64 `json:"cache_write_usd"`
	OutputUSD     float64 `json:"output_usd"`
	PingUSD       float64 `json:"ping_usd"`

	// UncachedUSD is the same traffic with no prompt cache at all. TotalUSD − UncachedUSD is
	// the INCREMENTAL CACHE PREMIUM: what the caching machinery itself cost or earned,
	// separate from the bill it sits inside. Negative means the cache paid for itself.
	UncachedUSD  float64 `json:"uncached_usd"`
	CachePremium float64 `json:"cache_premium_usd"`

	Hits    int64   `json:"hits"`
	Misses  int64   `json:"misses"`
	HitRate float64 `json:"hit_rate_pct"`
	// MissRate is reported as well as HitRate rather than left to the reader's subtraction,
	// because Unpriced rows are in neither and the two would not sum to 100 for a reader who
	// assumed they did.
	MissRate float64 `json:"miss_rate_pct"`
	// ForcedMisses are the rows whose own recorded reason (prefix_change, cold_start) no TTL
	// policy could have rescued. The ceiling on every arm, reported so it is not implied.
	ForcedMisses int64 `json:"forced_misses"`

	Pings            int64 `json:"pings"`
	PingsThatRewrote int64 `json:"pings_that_rewrote"`
	// PingsThatUpgraded are keep-alives that DELIBERATELY paid a longer tier's creation
	// rate to extend a LIVE entry — ActionWrite5mPing1h's whole mechanism. Counted apart from
	// PingsThatRewrote because the two mean opposite things: one is a policy buying a hold it
	// chose, the other is a schedule repairing damage it caused. One counter for both would
	// make a working arm and a broken one look identical.
	PingsThatUpgraded int64 `json:"pings_that_upgraded"`
	// PingsOnOpenSpans are pings billed into an idle span whose end is unknown, bounded by
	// Config.WindowEnd. Counted apart because they rest on an assumption the closed spans
	// do not need.
	PingsOnOpenSpans int64 `json:"pings_on_open_spans"`

	Writes5m int64 `json:"writes_5m"`
	Writes1h int64 `json:"writes_1h"`
	Expires  int64 `json:"expires"`

	// AvoidedRecomputations is the simulated hits, and AvoidedTokens the prefix tokens they
	// served from cache instead of re-creating.
	AvoidedRecomputations int64 `json:"avoided_recomputations"`
	AvoidedTokens         int64 `json:"avoided_tokens"`

	// RetainedMs is the UNION of the intervals during which this strategy held a live cache
	// entry, summed over conversations and clipped to the window. A union rather than a sum
	// of lifetimes: overlapping refreshes would otherwise count the same second twice.
	RetainedMs int64 `json:"retained_ms"`

	// Unpriced is requests whose model has no rates. They are counted in Requests and in the
	// hit/miss split, and contribute NOTHING to any dollar figure — an unpriced request is
	// not a free one.
	Unpriced int64 `json:"unpriced"`

	// Valued is whether ANY of these requests could be priced at all.
	//
	// False means every dollar figure on this Result is zero because nothing had RATES — not
	// because nothing was spent. Derivable from Unpriced == Requests, and stated anyway,
	// because a consumer that has to derive it is a consumer that will forget to: the price
	// map is fetched in the background, so the first load after a restart genuinely has no
	// rates for a few seconds, and in that window every arm costs 0.00 and the exact ceiling
	// degenerates to "never cache anything, 0%% hit rate" — a fabricated recommendation
	// rendered in the style reserved for the cheapest plan that exists.
	//
	// Nothing here refuses to produce a plan when it cannot price one; refusing would trade a
	// misleading answer for a missing panel, and the caller is better placed to choose. What
	// this guarantees is that the caller cannot MISS the condition. Anything that renders a
	// cost, a saving or a ceiling must check it first.
	Valued bool `json:"valued"`

	// Decisions counts each action the strategy chose.
	Decisions map[Action]int64 `json:"decisions"`
	// StatsLevels counts which fallback level the account's own statistics could answer at,
	// at each decision instant. How much of an arm was actually personalised, rather than
	// decided on the service-wide average.
	StatsLevels map[string]int64 `json:"stats_levels"`
	// ObservedCoverage is how many decisions the observed-policy arm could base on a
	// RECORDED tier. Absent (nil) for every other arm.
	ObservedCoverage *Coverage `json:"observed_coverage,omitempty"`

	ByUser  []Group `json:"by_user"`
	ByModel []Group `json:"by_model"`

	Latency Latency `json:"latency"`
}

// Coverage is "how much of this arm is a measurement": decisions that rested on a recorded
// fact against decisions that rested on an assumed default.
type Coverage struct {
	Recorded int64 `json:"recorded"`
	Assumed  int64 `json:"assumed"`
}

// Savings is one strategy against one baseline. Nothing here is clamped.
type Savings struct {
	Strategy string `json:"strategy"`
	Baseline string `json:"baseline"`
	// BaselineUSD and StrategyUSD are the two totals.
	BaselineUSD float64 `json:"baseline_usd"`
	StrategyUSD float64 `json:"strategy_usd"`
	// AbsoluteUSD = baseline − strategy. NEGATIVE where the strategy costs more, and it is
	// reported that way: clamping it to zero is how a comparison stops being one.
	AbsoluteUSD float64 `json:"absolute_usd"`
	// PercentUSD = absolute / baseline × 100, and Known is false when the baseline is zero —
	// a percentage of nothing is not 0%, it is undefined.
	PercentUSD float64 `json:"percent_usd"`
	Known      bool    `json:"percent_known"`
	// HitDelta is simulated hits gained (or lost) against the baseline.
	HitDelta int64 `json:"hit_delta"`
	// LatencyAvoidedMs is HitDelta × the window's own measured per-miss latency cost. Absent
	// where that differential could not be measured.
	LatencyAvoidedMs float64 `json:"latency_avoided_ms"`
	LatencyKnown     bool    `json:"latency_known"`
}

// Compare scores a strategy against a baseline.
//
//	absolute_savings   = baseline_cost − strategy_cost
//	percentage_savings = absolute_savings / baseline_cost × 100
func Compare(baseline, s *Result) Savings {
	out := Savings{Strategy: s.Strategy, Baseline: baseline.Strategy,
		BaselineUSD: baseline.TotalUSD, StrategyUSD: s.TotalUSD,
		AbsoluteUSD: baseline.TotalUSD - s.TotalUSD,
		HitDelta:    s.Hits - baseline.Hits}
	if baseline.TotalUSD != 0 {
		out.PercentUSD = out.AbsoluteUSD / baseline.TotalUSD * 100
		out.Known = true
	}
	if s.Latency.Known {
		out.LatencyAvoidedMs = float64(out.HitDelta) * s.Latency.PerMissMs
		out.LatencyKnown = true
	}
	return out
}

// convState is one conversation's simulated cache entry plus the decision governing its
// currently-open idle span.
type convState struct {
	tokens  int64
	tier    TTL
	expires int64
	lastTS  int64
	turn    int
	// pending is the action chosen at lastTS. It governs the span from lastTS to whatever
	// closes it, which is why it is held rather than applied immediately: a ping's cost
	// depends on how long the span turned out to be, and that is scoring, not deciding.
	pending Action
	// coveredUntil is the far end of the union of alive intervals so far, for RetainedMs.
	coveredUntil int64
	// user and model are the last request's, so the OPEN-span pass can price its pings at the
	// same rates and attribute them to the same groups the closed spans used.
	user  string
	model string
}

// group accumulates one Group.
type groupAcc struct{ g Group }

// Simulate replays the dataset under one strategy.
//
// reqs must be the DERIVED dataset (see Derive): sorted chronologically and carrying each
// request's own successor. It is not mutated.
func Simulate(reqs []*Request, s Strategy, cfg Config) *Result {
	cfg = cfg.withDefaults(reqs)
	sem := cfg.Semantics
	out := &Result{Strategy: s.Name(), Decisions: map[Action]int64{},
		StatsLevels: map[string]int64{}, ByUser: []Group{}, ByModel: []Group{},
		Latency: MeasureLatency(reqs)}
	if d, ok := s.(Describer); ok {
		out.Description = d.Describe()
	}
	obs, isObserved := s.(*Observed)
	if isObserved {
		out.ObservedCoverage = &Coverage{}
	}

	hist := NewHistory()
	states := map[Conversation]*convState{}
	byUser := map[string]*groupAcc{}
	byModel := map[string]*groupAcc{}
	acc := func(m map[string]*groupAcc, k string) *groupAcc {
		if g := m[k]; g != nil {
			return g
		}
		g := &groupAcc{g: Group{Key: k}}
		m[k] = g
		return g
	}

	// order is the chronological walk. Sorted here rather than trusted: Simulate is exported
	// and a caller that filtered the dataset after Derive would otherwise replay out of order,
	// which is a silent correctness failure rather than a loud one.
	order := make([]*Request, len(reqs))
	copy(order, reqs)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].TS != order[j].TS {
			return order[i].TS < order[j].TS
		}
		return order[i].ID < order[j].ID
	})

	for _, r := range order {
		key := r.Key()
		st := states[key]
		if st == nil {
			st = &convState{}
			states[key] = st
			out.Conversations++
		}
		price := cfg.Prices.For(r.Model)
		ug, mg := acc(byUser, r.User), acc(byModel, r.Model)

		// ── 1. close the previous span ──────────────────────────────────────
		// The gap that has just ended becomes HISTORY here, and only here. Every decision
		// below therefore sees gaps that closed at or before this instant, which is the
		// no-leakage invariant in one line.
		if st.turn > 0 {
			gap := time.Duration(r.TS-st.lastTS) * time.Millisecond
			if gap < 0 {
				gap = 0
			}
			hist.Observe(r.User, r.Model, BucketAt(st.lastTS), gap)
			simulatePings(out, ug, mg, st, price, sem, cfg, r.TS, false)
		}

		// ── 2. hit or miss, under THIS strategy's own history ───────────────
		alive := st.tokens > 0 && r.TS < st.expires
		forced := r.MissReason == "prefix_change" || r.MissReason == "cold_start"
		if forced {
			out.ForcedMisses++
		}
		hit := alive && !forced
		reusable := int64(0)
		if hit {
			reusable = st.tokens
			if reusable > r.CachedContext {
				reusable = r.CachedContext
			}
		}

		// ── 3. decide, with nothing but the past ────────────────────────────
		o := Observation{
			User: r.User, Conversation: r.ConversationID, Model: r.Model, RequestID: r.ID,
			Now: r.TS, HourUTC: r.HourUTC, Bucket: r.Bucket,
			CachedTokens: r.CachedContext, TTL: st.tier, ExpiresAt: st.expires,
			Turn: st.turn + 1, Stats: hist, Pricing: price,
		}
		if st.turn > 0 {
			o.SinceLastMs = r.TS - st.lastTS
		}
		_, n, level := hist.ReuseWithin(r.User, r.Model, r.Bucket, Horizon5m)
		if n == 0 {
			level = LevelNone
		}
		out.StatsLevels[level]++
		action := s.Decide(o)
		out.Decisions[action]++
		if isObserved {
			if obs.Covered(r.ID) {
				out.ObservedCoverage.Recorded++
			} else {
				out.ObservedCoverage.Assumed++
			}
		}

		// ── 4. bill this request under the chosen tier ──────────────────────
		tier := action.Tier()
		fresh, read, write := r.InputTokens, reusable, int64(0)
		switch {
		case tier == TTLNone:
			// No cache_control at all: the whole prompt is fresh input, and anything the
			// entry might have held is irrelevant because nothing was marked cacheable.
			fresh += r.CachedContext
			read = 0
		default:
			write = r.CachedContext - reusable
			if write < 0 {
				write = 0
			}
		}
		out.Requests++
		ug.g.Requests++
		mg.g.Requests++
		if !price.Known {
			out.Unpriced++
			ug.g.Unpriced++
			mg.g.Unpriced++
		} else {
			out.FreshInputUSD += float64(fresh) * price.Input
			out.CacheReadUSD += float64(read) * price.CacheRead
			out.CacheWriteUSD += float64(write) * price.writeRate(tier)
			out.OutputUSD += float64(r.OutputTokens) * price.Output
			out.UncachedUSD += price.UncachedCost(r.InputTokens, r.CachedContext, r.OutputTokens)
			cost := price.RequestCost(fresh, read, write, r.OutputTokens, tier)
			ug.g.TotalUSD += cost
			mg.g.TotalUSD += cost
		}
		if hit {
			out.Hits++
			ug.g.Hits++
			mg.g.Hits++
			out.AvoidedRecomputations++
			out.AvoidedTokens += read
		} else {
			out.Misses++
			ug.g.Misses++
			mg.g.Misses++
		}
		// A WRITE is a cache-creation event, not a decision: a request that hit and re-marked
		// the same prefix wrote nothing and is not a write. The decision counts live in
		// Result.Decisions, which is where "how often did the arm choose 1h" is answered.
		switch {
		case tier == TTLNone:
			out.Expires++
		case write > 0 && tier == TTL5m:
			out.Writes5m++
			ug.g.Writes5m++
			mg.g.Writes5m++
		case write > 0 && tier == TTL1h:
			out.Writes1h++
			ug.g.Writes1h++
			mg.g.Writes1h++
		}

		// ── 5. the entry this request leaves behind ─────────────────────────
		if tier == TTLNone {
			st.tokens, st.tier, st.expires = 0, TTLNone, 0
		} else {
			st.tokens, st.tier = r.CachedContext, tier
			// A hit refreshes the lifetime; a write starts it. Both land on the same
			// expression here, and Semantics.HitRefreshesTTL is what makes the difference
			// visible for a provider where it is not true: there, a hit keeps the entry's
			// ORIGINAL deadline.
			if hit && !sem.HitRefreshesTTL {
				if st.expires < r.TS {
					st.expires = r.TS + int64(tier.Lifetime()/time.Millisecond)
				}
			} else {
				st.expires = r.TS + int64(tier.Lifetime()/time.Millisecond)
			}
			retain(out, st, r.TS, st.expires, cfg.WindowEnd)
		}
		st.lastTS, st.pending, st.turn = r.TS, action, st.turn+1
		st.user, st.model = r.User, r.Model
	}

	// The OPEN spans: a conversation whose last request is inside the window has an idle span
	// nobody knows the length of. Its pings are billed only to WindowEnd, priced at the last
	// request's own model, and counted apart in PingsOnOpenSpans — because their number rests
	// on where the window happens to end rather than on an observed next request.
	for _, st := range statesInOrder(states) {
		if st.turn == 0 || !st.pending.Pings() {
			continue
		}
		simulatePings(out, acc(byUser, st.user), acc(byModel, st.model),
			st, cfg.Prices.For(st.model), sem, cfg, cfg.WindowEnd, true)
	}

	if d := out.Hits + out.Misses; d > 0 {
		out.HitRate = 100 * float64(out.Hits) / float64(d)
		out.MissRate = 100 * float64(out.Misses) / float64(d)
	}
	// Set BEFORE the totals are read by anything: an all-unpriced replay sums to 0.00, which
	// is indistinguishable from free.
	out.Valued = out.Requests > 0 && out.Unpriced < out.Requests
	out.TotalUSD = out.FreshInputUSD + out.CacheReadUSD + out.CacheWriteUSD + out.OutputUSD +
		out.PingUSD
	out.CachePremium = out.TotalUSD - out.UncachedUSD
	out.ByUser = finishGroups(byUser)
	out.ByModel = finishGroups(byModel)
	return out
}

// retain adds one alive interval to the UNION already recorded, clipped to the window.
func retain(out *Result, st *convState, from, until, windowEnd int64) {
	if windowEnd > 0 && until > windowEnd {
		until = windowEnd
	}
	if st.coveredUntil > from {
		from = st.coveredUntil
	}
	if until > from {
		out.RetainedMs += until - from
	}
	if until > st.coveredUntil {
		st.coveredUntil = until
	}
}

// pingOutcome is what one idle span's keep-alives cost and the entry they leave behind.
//
// Pure: pingSpan mutates nothing. Both the accounting path (simulatePings) and the exact
// ceiling's dynamic program (NewOptimal) read this ONE implementation, so a second copy of
// the ping arithmetic cannot drift away from the first — the same reason the Python port in
// deploy/harbor/kv_ttl_cost_model.py has exactly this shape.
type pingOutcome struct {
	cost     float64
	fired    int64
	rewrote  int64
	upgraded int64
	tier     TTL
	expires  int64
	// refreshes is (firedAt, newExpires) per ping, for the retention union.
	refreshes [][2]int64
}

// pingSpan is the keep-alives that fire in (lastTS, spanEnd) under `pending`.
//
// The interval follows the entry's CURRENT tier, recomputed each time round, rather than the
// action's target tier fixed once. That matters only for ActionWrite5mPing1h, whose entry
// starts at five minutes and becomes hourly partway through — so its first keep-alive has to
// land inside five minutes and the rest need only land inside an hour. For every other action
// the tier never moves and this is the same fixed cadence as before, which is what the ping
// schedule table in deploy/harbor/kv_ttl_cost_drift_test.go pins.
func pingSpan(tokens int64, tier TTL, expires, lastTS int64, pending Action, spanEnd int64,
	price Pricing, sem Semantics, cfg Config) pingOutcome {
	out := pingOutcome{tier: tier, expires: expires}
	if !pending.Pings() || spanEnd <= lastTS || tokens <= 0 {
		return out
	}
	want := pending.PingTier()
	at := lastTS
	for i := 0; i < cfg.MaxPings; i++ {
		step := int64(cfg.pingInterval(out.tier) / time.Millisecond)
		if step <= 0 {
			break
		}
		at += step
		if at >= spanEnd {
			break
		}
		alive := at < out.expires
		var cost float64
		switch {
		case !alive:
			// The entry lapsed before this ping fired, so the "refresh" re-creates it at the
			// tier the action was aiming for. Priced as a write, and counted, because a
			// schedule that does this is paying 12.5x to fix a problem it caused.
			out.tier = want
			cost = price.RecreateCost(tokens, out.tier, sem)
			out.rewrote++
		case want == TTL1h && out.tier != TTL1h:
			// A deliberate UPGRADE of a live entry: pay the 1-hour creation rate now to hold it
			// for an hour instead of five minutes. This is ActionWrite5mPing1h's mechanism, and
			// it is the one case where a keep-alive is a write on purpose rather than by
			// accident.
			out.tier = TTL1h
			cost = price.RecreateCost(tokens, TTL1h, sem)
			out.upgraded++
		default:
			cost = price.KeepAliveCost(tokens, sem)
		}
		out.fired++
		if price.Known {
			out.cost += cost
		}
		if sem.PingRefreshesTTL || !alive {
			if life := int64(out.tier.Lifetime() / time.Millisecond); life > 0 {
				out.expires = at + life
				out.refreshes = append(out.refreshes, [2]int64{at, out.expires})
			}
		}
	}
	return out
}

// simulatePings bills the keep-alives that fire between st.lastTS and spanEnd.
//
// `open` marks a span with no successor: its pings are counted separately, because their
// number rests on where the window ends rather than on an observed next request.
func simulatePings(out *Result, ug, mg *groupAcc, st *convState, price Pricing, sem Semantics,
	cfg Config, spanEnd int64, open bool) {
	// Whatever happens below, this span is now settled: clearing the pending action is what
	// stops a conversation being charged twice if the open-span pass visits it as well.
	pending := st.pending
	st.pending = ActionExpire
	r := pingSpan(st.tokens, st.tier, st.expires, st.lastTS, pending, spanEnd, price, sem, cfg)
	if r.fired == 0 {
		return
	}
	st.tier, st.expires = r.tier, r.expires
	out.Pings += r.fired
	out.PingsThatRewrote += r.rewrote
	out.PingsThatUpgraded += r.upgraded
	if open {
		out.PingsOnOpenSpans += r.fired
	}
	if ug != nil {
		ug.g.Pings += r.fired
	}
	if mg != nil {
		mg.g.Pings += r.fired
	}
	if price.Known {
		out.PingUSD += r.cost
		if ug != nil {
			ug.g.PingUSD += r.cost
			ug.g.TotalUSD += r.cost
		}
		if mg != nil {
			mg.g.PingUSD += r.cost
			mg.g.TotalUSD += r.cost
		}
	}
	for _, ref := range r.refreshes {
		retain(out, st, ref[0], ref[1], cfg.WindowEnd)
	}
}

// statesInOrder is the conversation states in a deterministic order, so an open-span pass
// produces the same figures twice.
func statesInOrder(m map[Conversation]*convState) []*convState {
	keys := make([]Conversation, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].User != keys[j].User {
			return keys[i].User < keys[j].User
		}
		return keys[i].Conversation < keys[j].Conversation
	})
	out := make([]*convState, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// finishGroups turns the accumulators into a sorted slice with rates filled in.
func finishGroups(m map[string]*groupAcc) []Group {
	out := make([]Group, 0, len(m))
	for _, a := range m {
		g := a.g
		if d := g.Hits + g.Misses; d > 0 {
			g.HitRate = 100 * float64(g.Hits) / float64(d)
		}
		g.Valued = g.Requests > 0 && g.Unpriced < g.Requests
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalUSD != out[j].TotalUSD {
			return out[i].TotalUSD > out[j].TotalUSD
		}
		return out[i].Key < out[j].Key
	})
	return out
}
