package predictor

import (
	"math"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// mustRegister panics on a registration error. Every call site in this file registers a
// fixed, compile-time-known Feature, so an error here is a bug in this file, not a runtime
// condition a caller could hit — the same reasoning regexp.MustCompile rests on.
func mustRegister(f *Features, feat Feature) {
	if err := f.Register(feat); err != nil {
		panic(err)
	}
}

// Feature IDs for the built-in catalog, so a caller can name one without a string literal.
const (
	FeatureCachedTokens         = "cached_tokens"
	FeatureSinceLastMs          = "since_last_ms"
	FeatureTurn                 = "turn"
	FeatureHourUTC              = "hour_utc"
	FeatureRequestedTTL         = "requested_ttl"
	FeatureStopReason           = "stop_reason"
	FeatureStopReasonCluster    = "stop_reason_cluster"
	FeatureReuseWithin5m        = "reuse_within_5m"
	FeatureReuseWithin5mN       = "reuse_within_5m_n"
	FeatureReuseWithin5mLevel   = "reuse_within_5m_level"
	FeatureReuseWithin5mCluster = "reuse_within_5m_clustered"
	FeatureRevertsLag1          = "reverts_lag1"
	FeatureExpandsLag1          = "expands_lag1"
	FeatureCacheMissReasonLag1  = "cache_miss_reason_lag1"
	FeatureReuseWithin1h        = "reuse_within_1h"
	FeatureReuseWithin1hN       = "reuse_within_1h_n"
	FeatureReuseWithin1hLevel   = "reuse_within_1h_level"

	// ── time ───────────────────────────────────────────────────────────────────────────
	FeatureHourSin                 = "hour_sin"
	FeatureHourCos                 = "hour_cos"
	FeatureWeekdaySin              = "weekday_sin"
	FeatureWeekdayCos              = "weekday_cos"
	FeatureIsWeekend               = "is_weekend"
	FeatureMinuteOfHour            = "minute_of_hour"
	FeatureTimeSinceSessionStartMs = "time_since_session_start_ms"
	FeatureTimeSinceWindowStartMs  = "time_since_window_start_ms"

	// ── session metadata ───────────────────────────────────────────────────────────────
	FeatureModel                  = "model"
	FeatureAgent                  = "agent"
	FeaturePreset                 = "preset"
	FeatureMode                   = "mode"
	FeatureModelSwitchedInSession = "model_switched_in_session"

	// ── request parameters ─────────────────────────────────────────────────────────────
	FeatureMaxTokens       = "max_tokens"
	FeatureStream          = "stream"
	FeatureToolChoice      = "tool_choice"
	FeatureTemperature     = "temperature"
	FeatureTopP            = "top_p"
	FeatureReasoningEffort = "reasoning_effort"
	FeatureThinkingMode    = "thinking_mode"
	FeatureThinkingBudget  = "thinking_budget"
	FeatureToolsDeclared   = "tools_declared"
	FeatureSystemBlocks    = "system_blocks"
	FeatureCacheBPSystem   = "cache_bp_system"
	FeatureCacheBPTools    = "cache_bp_tools"
	FeatureCacheBPMessages = "cache_bp_messages"
	FeatureCacheBPBlocks   = "cache_bp_blocks"

	// ── cache / economics ───────────────────────────────────────────────────────────────
	FeatureCacheReadTokens      = "cache_read_tokens"
	FeatureCacheWriteTokens     = "cache_write_tokens"
	FeatureCacheWrite1hTokens   = "cache_write_1h_tokens"
	FeaturePrefixSizeDelta      = "prefix_size_delta"
	FeatureSecondsToTTLDeadline = "seconds_to_ttl_deadline"

	// ── request frequency / burstiness (all over THIS conversation's own past) ─────────
	FeatureGapLag1            = "gap_lag1"
	FeatureGapLag2            = "gap_lag2"
	FeatureGapLag3            = "gap_lag3"
	FeatureEWMAGapMs          = "ewma_gap_ms"
	FeatureRollingMedianGapMs = "rolling_median_gap_ms"
	FeatureRollingIQRGapMs    = "rolling_iqr_gap_ms"
	FeatureGapVarianceMs2     = "gap_variance_ms2"
	FeatureBurstIndicator     = "burst_indicator"
	FeatureRequestsPrev1m     = "requests_prev_1m"
	FeatureRequestsPrev5m     = "requests_prev_5m"
	FeatureRequestsPrev10m    = "requests_prev_10m"
	FeatureRequestsPrev60m    = "requests_prev_60m"

	// ── tenant / session-count ──────────────────────────────────────────────────────────
	FeatureTenant                   = "tenant"
	FeatureColdStart                = "cold_start"
	FeatureDistinctSessions         = "distinct_sessions_for_tenant"
	FeatureConcurrentSessions       = "concurrent_sessions_for_tenant"
	FeatureTenantRequestRatePerHour = "tenant_request_rate_per_hour"
	// FeatureTenantAgeDays is OfflineDerivable, not Live — see its own registration below
	// and StatsContext.TenantCreatedAt's doc comment for why Replay cannot wire it today.
	FeatureTenantAgeDays = "tenant_age_days"

	// FeatureNextTS and FeatureIdleMs are cataloged with Availability=Impossible and no
	// Extractor — see Features.Register. They exist so a reader can find, by name, exactly
	// the two facts kvcache.Optimal alone is allowed to use (see kvcache/registry.go's
	// Optimal doc comment: "It reads the true next-request time... never a result"), and so
	// nobody has to rediscover by trial and error that a predictor cannot read them.
	FeatureNextTS = "next_ts"
	FeatureIdleMs = "idle_ms"
)

// levelValue maps a Stats fallback level to a small numeric code, purely so a level can
// travel through the same float64-valued Value a numeric feature uses without inventing a
// third field on Value for one caller. The STRING level (which is the one worth reading) is
// on FeatureReuseWithin5mLevel via Value.Str; this numeric companion exists for a caller
// that wants to threshold or sort on specificity without a string compare.
var levelValue = map[string]float64{
	kvcache.LevelUserBucket: 4, kvcache.LevelUserModel: 3, kvcache.LevelUser: 2,
	kvcache.LevelModel: 1, kvcache.LevelGlobal: 0,
}

// DefaultFeatures builds the feature registry with every feature this package ships,
// registered under the IDs above. A caller adding a project-specific feature registers it
// into the SAME *Features (Register refuses a duplicate ID), rather than building a second
// registry a replay would have to consult twice.
func DefaultFeatures() *Features {
	f := NewFeatures()

	mustRegister(f, Feature{
		ID: FeatureCachedTokens, Description: "The billed prefix (read + write) this row leaves behind.",
		SourceTable: "requests", SourceColumn: "cached_context_tokens",
		AvailableAt: "immediately, off the provider's own response", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.CachedTokens), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureSinceLastMs, Description: "The gap that had ALREADY closed before this request arrived; 0 on a conversation's first request.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately — it is the gap that already closed, kvcache.Derive's own successor arithmetic run one step behind", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.SinceLastMs), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureTurn, Description: "How many requests this conversation has served so far, including this one.",
		SourceTable: "requests", SourceColumn: "(derived: row count within the conversation)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.Turn), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureHourUTC, Description: "The UTC hour this request landed in.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.HourUTC), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureRequestedTTL, Description: "The cache tier this request itself asked for (none|ephemeral_5m|ephemeral_1h).",
		SourceTable: "requests", SourceColumn: "cache_ttl",
		AvailableAt: "immediately — it is a header the client already sent", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: string(o.TTL), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureStopReason, Description: "This request's own terminal stop reason (end_turn|tool_use|stop_sequence|...).",
		SourceTable: "requests", SourceColumn: "stop_reason",
		AvailableAt: "immediately — it belongs to the request just served, not a future one", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.StopReason, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureStopReasonCluster, Description: "kvcache.ClusterOf(stop_reason): still_working | looks_done_isnt | actually_done.",
		SourceTable: "requests", SourceColumn: "stop_reason (derived)",
		AvailableAt: "immediately, derived from stop_reason", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: string(kvcache.ClusterOf(o.StopReason)), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureReuseWithin5m, Description: "Share of this user+model+bucket's already-closed gaps (falling back through kvcache.History's ladder) no longer than 5 minutes.",
		SourceTable: "requests", SourceColumn: "(derived: kvcache.History over closed gaps)",
		AvailableAt: "immediately — accumulated from gaps that closed strictly before Now", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Hist == nil {
				return Value{}
			}
			p, n, _ := stats.Hist.ReuseWithin(o.User, o.Model, o.Bucket, kvcache.Horizon5m)
			if n == 0 {
				return Value{}
			}
			return Value{Num: p, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureReuseWithin5mN, Description: "Sample size (n) FeatureReuseWithin5m's answer rests on, at whichever fallback level it resolved to.",
		SourceTable: "requests", SourceColumn: "(derived: kvcache.History over closed gaps)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Hist == nil {
				return Value{}
			}
			_, n, level := stats.Hist.ReuseWithin(o.User, o.Model, o.Bucket, kvcache.Horizon5m)
			if level == kvcache.LevelNone {
				return Value{}
			}
			return Value{Num: float64(n), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureReuseWithin5mLevel, Description: "Which fallback level FeatureReuseWithin5m actually resolved to (user+model+bucket|user+model|user|model|global|none).",
		SourceTable: "requests", SourceColumn: "(derived: kvcache.History over closed gaps)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Hist == nil {
				return Value{Str: kvcache.LevelNone, Present: true}
			}
			_, n, level := stats.Hist.ReuseWithin(o.User, o.Model, o.Bucket, kvcache.Horizon5m)
			if n == 0 {
				level = kvcache.LevelNone
			}
			return Value{Str: level, Num: levelValue[level], Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureReuseWithin5mCluster, Description: "Like FeatureReuseWithin5m, but every cell is additionally keyed on this request's own StopReasonCluster — never dropped in fallback. See ClusteredHistory's doc comment for the trap this closes.",
		SourceTable: "requests", SourceColumn: "(derived: ClusteredHistory over closed gaps)",
		AvailableAt: "immediately in a replay; NOT wired into the live proxy today — see this file's Availability note", Availability: OfflineDerivable,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Clustered == nil {
				return Value{}
			}
			c := kvcache.ClusterOf(o.StopReason)
			p, n, _ := stats.Clustered.ReuseWithin(o.User, o.Model, o.Bucket, c, kvcache.Horizon5m)
			if n == 0 {
				return Value{}
			}
			return Value{Num: p, Present: true}
		},
	})

	// ── the lagged trio: reverts, expands, cache_miss_reason ──────────────────────────
	//
	// All three LOOK present-tense the way stop_reason is — they are columns on the same
	// row a decision is being made about — but their AVAILABLE-AT timestamp trails the
	// request they describe: dash's async analysis pipeline finishes classifying a row
	// after it has already been billed, so a live decision at row i's own instant cannot
	// actually read row i's own value yet, whatever the historical store shows once that
	// pipeline has caught up. Reading row i's OWN reverts/expands/cache_miss_reason would
	// therefore silently use information that was not really there — the same one-directional
	// bias kvcache.Conversation's own doc comment describes for the missing-model bug: every
	// arm built on the leak looks better than it can be. Lagging by exactly one row keeps the
	// feature on the near side of that delay in every case, because MORE wall-clock time has
	// passed since row i-1 was served than since row i was.
	mustRegister(f, Feature{
		ID: FeatureRevertsLag1, Description: "kvcache.Request.Reverts from the PRIOR row in this conversation, never the current one.",
		SourceTable: "requests", SourceColumn: "reverts",
		AvailableAt: "trails the request it describes (dash's async analysis pipeline); lagged one row to stay on its near side", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(_ kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			if len(past) == 0 {
				return Value{}
			}
			return Value{Num: float64(past[len(past)-1].Reverts), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureExpandsLag1, Description: "kvcache.Request.Expands from the PRIOR row in this conversation, never the current one.",
		SourceTable: "requests", SourceColumn: "expands",
		AvailableAt: "trails the request it describes; lagged one row to stay on its near side", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(_ kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			if len(past) == 0 {
				return Value{}
			}
			return Value{Num: float64(past[len(past)-1].Expands), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureCacheMissReasonLag1, Description: "kvcache.Request.MissReason from the PRIOR row in this conversation, never the current one.",
		SourceTable: "requests", SourceColumn: "cache_miss_reason",
		AvailableAt: "trails the request it describes; lagged one row to stay on its near side", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(_ kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			if len(past) == 0 {
				return Value{}
			}
			return Value{Str: past[len(past)-1].MissReason, Present: true}
		},
	})

	// ── the 1h counterpart to reuse_within_5m ──────────────────────────────────────────
	mustRegister(f, Feature{
		ID: FeatureReuseWithin1h, Description: "Share of this user+model+bucket's already-closed gaps (falling back through kvcache.History's ladder) no longer than 1 hour.",
		SourceTable: "requests", SourceColumn: "(derived: kvcache.History over closed gaps)",
		AvailableAt: "immediately — accumulated from gaps that closed strictly before Now", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Hist == nil {
				return Value{}
			}
			p, n, _ := stats.Hist.ReuseWithin(o.User, o.Model, o.Bucket, kvcache.Horizon1h)
			if n == 0 {
				return Value{}
			}
			return Value{Num: p, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureReuseWithin1hN, Description: "Sample size (n) FeatureReuseWithin1h's answer rests on.",
		SourceTable: "requests", SourceColumn: "(derived: kvcache.History over closed gaps)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Hist == nil {
				return Value{}
			}
			_, n, level := stats.Hist.ReuseWithin(o.User, o.Model, o.Bucket, kvcache.Horizon1h)
			if level == kvcache.LevelNone {
				return Value{}
			}
			return Value{Num: float64(n), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureReuseWithin1hLevel, Description: "Which fallback level FeatureReuseWithin1h resolved to.",
		SourceTable: "requests", SourceColumn: "(derived: kvcache.History over closed gaps)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Hist == nil {
				return Value{Str: kvcache.LevelNone, Present: true}
			}
			_, n, level := stats.Hist.ReuseWithin(o.User, o.Model, o.Bucket, kvcache.Horizon1h)
			if n == 0 {
				level = kvcache.LevelNone
			}
			return Value{Str: level, Num: levelValue[level], Present: true}
		},
	})

	// ── time ────────────────────────────────────────────────────────────────────────────
	mustRegister(f, Feature{
		ID: FeatureHourSin, Description: "sin(2*pi*hour_utc/24) — a cyclic encoding of the hour so 23:00 and 00:00 are adjacent, not 23 apart.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: math.Sin(2 * math.Pi * float64(o.HourUTC) / 24), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureHourCos, Description: "cos(2*pi*hour_utc/24), the other half of the cyclic hour encoding.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: math.Cos(2 * math.Pi * float64(o.HourUTC) / 24), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureWeekdaySin, Description: "sin(2*pi*weekday/7), UTC weekday (Sunday=0) cyclically encoded.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			wd := float64(time.UnixMilli(o.Now).UTC().Weekday())
			return Value{Num: math.Sin(2 * math.Pi * wd / 7), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureWeekdayCos, Description: "cos(2*pi*weekday/7), the other half of the cyclic weekday encoding.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			wd := float64(time.UnixMilli(o.Now).UTC().Weekday())
			return Value{Num: math.Cos(2 * math.Pi * wd / 7), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureIsWeekend, Description: "1 if this request landed on a UTC Saturday or Sunday, else 0. UTC only — see this package's own doc comment on why there is no per-tenant timezone anywhere in the store.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			wd := time.UnixMilli(o.Now).UTC().Weekday()
			v := 0.0
			if wd == time.Saturday || wd == time.Sunday {
				v = 1
			}
			return Value{Num: v, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureMinuteOfHour, Description: "The UTC minute (0-59) this request landed in.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(time.UnixMilli(o.Now).UTC().Minute()), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureTimeSinceSessionStartMs, Description: "Milliseconds since this conversation's own FIRST request; 0 on that first request itself (this IS the start, not a zero-length wait).",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately — past[0] is already-closed history", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			if len(past) == 0 {
				return Value{Present: true}
			}
			return Value{Num: float64(o.Now - past[0].TS), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureTimeSinceWindowStartMs, Description: "Milliseconds since the FIRST request in the whole dataset Replay was given — NOT this conversation's own start (see FeatureTimeSinceSessionStartMs for that).",
		SourceTable: "requests", SourceColumn: "(derived: Replay's own first row)",
		AvailableAt: "immediately in a replay; NOT wired into the live proxy today, which has no fixed window start", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.WindowStart == 0 {
				return Value{}
			}
			return Value{Num: float64(o.Now - stats.WindowStart), Present: true}
		},
	})

	// ── session metadata ───────────────────────────────────────────────────────────────
	mustRegister(f, Feature{
		ID: FeatureModel, Description: "The model this request named.",
		SourceTable: "requests", SourceColumn: "model",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.Model, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureAgent, Description: "The client dialect (claude-code, codex, ...) this request arrived under.",
		SourceTable: "requests", SourceColumn: "agent",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.Agent, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeaturePreset, Description: "The deployment preset this request landed under.",
		SourceTable: "requests", SourceColumn: "preset",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.Preset, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureMode, Description: "The operating mode (active|bypass|observe) this request ran under.",
		SourceTable: "requests", SourceColumn: "mode",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.Mode, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureModelSwitchedInSession, Description: "Whether this CLIENT-SUPPLIED session id has now carried more than one distinct model, including this request. Deliberately session-scoped rather than kvcache.Conversation-scoped — see TenantStats.ModelSwitchedInSession's own doc comment for why the two must disagree.",
		SourceTable: "requests", SourceColumn: "(derived: TenantStats per-session model set)",
		AvailableAt: "immediately in a replay; NOT wired into the live proxy today", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Tenant == nil {
				return Value{}
			}
			v := 0.0
			if stats.Tenant.ModelSwitchedInSession(o.User, o.Conversation) {
				v = 1
			}
			return Value{Num: v, Present: true}
		},
	})

	// ── request parameters — all client-declared knobs, known the instant the request
	// arrives, the same footing StopReason/CachedTokens already stand on ─────────────────
	mustRegister(f, Feature{
		ID: FeatureMaxTokens, Description: "The client's output token cap.",
		SourceTable: "requests", SourceColumn: "max_tokens",
		AvailableAt: "immediately — a header the client already sent", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.MaxTokens), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureStream, Description: "Whether the client asked for a streamed response.",
		SourceTable: "requests", SourceColumn: "stream",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			v := 0.0
			if o.Stream {
				v = 1
			}
			return Value{Num: v, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureToolChoice, Description: "The normalized tool-forcing mode (auto|any|none|required|tool).",
		SourceTable: "requests", SourceColumn: "tool_choice",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.ToolChoice, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureTemperature, Description: "The client's temperature, or absent if it sent none — 0 and absent are different facts, so this is Present=false rather than 0 when unset.",
		SourceTable: "requests", SourceColumn: "temperature",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			if o.Temperature == nil {
				return Value{}
			}
			return Value{Num: *o.Temperature, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureTopP, Description: "The client's top_p, or absent if it sent none.",
		SourceTable: "requests", SourceColumn: "top_p",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			if o.TopP == nil {
				return Value{}
			}
			return Value{Num: *o.TopP, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureReasoningEffort, Description: "Anthropic output_config.effort / OpenAI reasoning_effort, normalized.",
		SourceTable: "requests", SourceColumn: "reasoning_effort",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.ReasoningEffort, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureThinkingMode, Description: "Anthropic thinking.type: adaptive|enabled|disabled, '' absent.",
		SourceTable: "requests", SourceColumn: "thinking_mode",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.ThinkingMode, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureThinkingBudget, Description: "thinking.budget_tokens (pre-4.6 models only; 0 = unset).",
		SourceTable: "requests", SourceColumn: "thinking_budget",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.ThinkingBudget), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureToolsDeclared, Description: "How many tools this request declared.",
		SourceTable: "requests", SourceColumn: "tools",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.ToolsDeclared), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureSystemBlocks, Description: "How many blocks the top-level system array carried.",
		SourceTable: "requests", SourceColumn: "system_blocks",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.SystemBlocks), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureCacheBPSystem, Description: "Prompt-cache breakpoints in the system array on arrival.",
		SourceTable: "requests", SourceColumn: "cache_bp_system",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.CacheBPSystem), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureCacheBPTools, Description: "Prompt-cache breakpoints in the tools array on arrival.",
		SourceTable: "requests", SourceColumn: "cache_bp_tools",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.CacheBPTools), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureCacheBPMessages, Description: "Prompt-cache breakpoints in the messages array on arrival.",
		SourceTable: "requests", SourceColumn: "cache_bp_messages",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.CacheBPMessages), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureCacheBPBlocks, Description: "Total prompt-cache breakpoints on arrival, summed across all locations (provider cap is 4).",
		SourceTable: "requests", SourceColumn: "cache_bp_blocks",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyAggregate, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.CacheBPBlocks), Present: true}
		},
	})

	// ── cache / economics ───────────────────────────────────────────────────────────────
	mustRegister(f, Feature{
		ID: FeatureCacheReadTokens, Description: "Tokens this request read FROM the prompt cache — the realized half of CachedTokens.",
		SourceTable: "requests", SourceColumn: "cache_read",
		AvailableAt: "immediately, off the provider's own response", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.CacheRead), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureCacheWriteTokens, Description: "Tokens this request wrote INTO the prompt cache.",
		SourceTable: "requests", SourceColumn: "cache_write",
		AvailableAt: "immediately, off the provider's own response", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.CacheWrite), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureCacheWrite1hTokens, Description: "The part of CacheWrite billed at the one-hour tier — the only proof a requested 1h was honoured.",
		SourceTable: "requests", SourceColumn: "cache_write_1h",
		AvailableAt: "immediately, off the provider's own response", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(o.CacheWrite1h), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeaturePrefixSizeDelta, Description: "CachedTokens minus the PRIOR row's own CachedTokens in this conversation; 0 on the first request. Positive means the prefix grew, negative means it shrank or was recreated smaller.",
		SourceTable: "requests", SourceColumn: "cached_context_tokens (derived)",
		AvailableAt: "immediately — the prior row is already-closed history", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			if len(past) == 0 {
				return Value{Present: true}
			}
			return Value{Num: float64(o.CachedTokens - past[len(past)-1].CachedContext), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureSecondsToTTLDeadline, Description: "The PRIOR row's own TTL lifetime minus SinceLastMs, in seconds: how much margin was left before that entry would have expired, given how long it actually sat idle before this request arrived. Negative means it had already lapsed. Absent on a conversation's first request — there is no prior entry to measure a margin against.",
		SourceTable: "requests", SourceColumn: "cache_ttl, ts (derived)",
		AvailableAt: "immediately — both operands are already-closed history", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			if len(past) == 0 {
				return Value{}
			}
			lifetimeMs := past[len(past)-1].TTL.Lifetime().Milliseconds()
			return Value{Num: float64(lifetimeMs-o.SinceLastMs) / 1000, Present: true}
		},
	})

	// ── request frequency / burstiness — all over THIS conversation's own past, the same
	// scope kvcache.Conversation already keys everything else on ─────────────────────────
	mustRegister(f, Feature{
		ID: FeatureGapLag1, Description: "The most recently closed gap — identical to FeatureSinceLastMs, registered under this name too for the 'gaps at several lags' family.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			g := closedGaps(past, o.Now)
			if len(g) < 1 {
				return Value{}
			}
			return Value{Num: float64(g[len(g)-1]), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureGapLag2, Description: "The SECOND most recently closed gap in this conversation.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			g := closedGaps(past, o.Now)
			if len(g) < 2 {
				return Value{}
			}
			return Value{Num: float64(g[len(g)-2]), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureGapLag3, Description: "The THIRD most recently closed gap in this conversation.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			g := closedGaps(past, o.Now)
			if len(g) < 3 {
				return Value{}
			}
			return Value{Num: float64(g[len(g)-3]), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureEWMAGapMs, Description: "Exponentially-weighted moving average of this conversation's closed gaps (alpha=0.3, newest-heaviest).",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			v, ok := ewmaGap(closedGaps(past, o.Now))
			if !ok {
				return Value{}
			}
			return Value{Num: v, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureRollingMedianGapMs, Description: "Median of this conversation's last 10 closed gaps (fewer if the conversation is younger).",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			g := tail(closedGaps(past, o.Now), rollingWindow)
			if len(g) == 0 {
				return Value{}
			}
			return Value{Num: quantile(sortedCopy(g), 0.5), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureRollingIQRGapMs, Description: "Interquartile range (Q3-Q1) of this conversation's last 10 closed gaps.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			g := tail(closedGaps(past, o.Now), rollingWindow)
			if len(g) == 0 {
				return Value{}
			}
			s := sortedCopy(g)
			return Value{Num: quantile(s, 0.75) - quantile(s, 0.25), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureGapVarianceMs2, Description: "Population variance (ms^2) of this conversation's own closed gaps.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			v, ok := gapVariance(closedGaps(past, o.Now))
			if !ok {
				return Value{}
			}
			return Value{Num: v, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureBurstIndicator, Description: "1 if the just-closed gap is under half this conversation's own PRIOR median gap, else 0 — a fixed, unfitted heuristic threshold, not a measured one. Absent until at least two gaps have closed (one to be 'just closed', one to seed the prior median).",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			g := closedGaps(past, o.Now)
			if len(g) < 2 {
				return Value{}
			}
			current := g[len(g)-1]
			prior := tail(g[:len(g)-1], rollingWindow)
			median := quantile(sortedCopy(prior), 0.5)
			v := 0.0
			if float64(current) < median/2 {
				v = 1
			}
			return Value{Num: v, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureRequestsPrev1m, Description: "How many of this conversation's own past requests arrived in the 1 minute before now.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(countSince(past, o.Now, time.Minute)), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureRequestsPrev5m, Description: "How many of this conversation's own past requests arrived in the 5 minutes before now.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(countSince(past, o.Now, kvcache.Horizon5m)), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureRequestsPrev10m, Description: "How many of this conversation's own past requests arrived in the 10 minutes before now.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(countSince(past, o.Now, 10*time.Minute)), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureRequestsPrev60m, Description: "How many of this conversation's own past requests arrived in the 60 minutes before now.",
		SourceTable: "requests", SourceColumn: "ts (derived)",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingZero, Privacy: PrivacyTenant, Cost: CostScan,
		Extract: func(o kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			return Value{Num: float64(countSince(past, o.Now, kvcache.Horizon1h)), Present: true}
		},
	})

	// ── tenant / session-count ──────────────────────────────────────────────────────────
	mustRegister(f, Feature{
		ID: FeatureTenant, Description: "The owning tenant id. PrivacyTenant — pseudonymize before it leaves this process in any report.",
		SourceTable: "requests", SourceColumn: "tenant_id",
		AvailableAt: "immediately", Availability: Live,
		Missing: MissingError, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, _ StatsContext) Value {
			return Value{Str: o.User, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureColdStart, Description: "Whether this is the very FIRST request this tenant has made, within the window Replay was given.",
		SourceTable: "requests", SourceColumn: "(derived: TenantStats accumulator)",
		AvailableAt: "immediately in a replay; NOT wired into the live proxy today", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Tenant == nil {
				return Value{}
			}
			v := 0.0
			if stats.Tenant.ColdStart(o.User, o.RequestID) {
				v = 1
			}
			return Value{Num: v, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureDistinctSessions, Description: "How many distinct sessions this tenant has opened so far, including this one.",
		SourceTable: "requests", SourceColumn: "(derived: TenantStats accumulator)",
		AvailableAt: "immediately in a replay; NOT wired into the live proxy today", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			n, ok := stats.Tenant.DistinctSessions(o.User)
			if !ok {
				return Value{}
			}
			return Value{Num: float64(n), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureConcurrentSessions, Description: "How many of this tenant's OTHER sessions were last active within 5 minutes of now — an approximation from arrival timestamps, since nothing in this store records a session's true close.",
		SourceTable: "requests", SourceColumn: "(derived: TenantStats accumulator)",
		AvailableAt: "immediately in a replay; NOT wired into the live proxy today", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			if stats.Tenant == nil {
				return Value{}
			}
			return Value{Num: float64(stats.Tenant.ConcurrentSessions(o.User, o.Conversation, o.Now)), Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureTenantRequestRatePerHour, Description: "This tenant's requests-per-hour so far: total requests observed divided by the span since its first one. Absent on a tenant's very first request.",
		SourceTable: "requests", SourceColumn: "(derived: TenantStats accumulator)",
		AvailableAt: "immediately in a replay; NOT wired into the live proxy today", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostStatCell,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			v, ok := stats.Tenant.RequestRate(o.User, o.Now)
			if !ok {
				return Value{}
			}
			return Value{Num: v, Present: true}
		},
	})
	mustRegister(f, Feature{
		ID: FeatureTenantAgeDays, Description: "Days since this tenant's account was created (cg-control.db's tenants.created_at). Requires a caller to populate StatsContext.TenantCreatedAt by hand — see that field's own doc comment for why Replay cannot: this package stays SQL-free, and the source is a DIFFERENT database than the one every other statistic here is accumulated from.",
		SourceTable: "tenants (cg-control.db)", SourceColumn: "created_at",
		AvailableAt: "immediately, IF the caller supplies StatsContext.TenantCreatedAt; absent otherwise", Availability: OfflineDerivable,
		Missing: MissingSkip, Privacy: PrivacyTenant, Cost: CostFieldRead,
		Extract: func(o kvcache.Observation, _ []*kvcache.Request, stats StatsContext) Value {
			created, ok := stats.TenantCreatedAt[o.User]
			if !ok {
				return Value{}
			}
			days := float64(o.Now-created) / float64(24*time.Hour/time.Millisecond)
			return Value{Num: days, Present: true}
		},
	})

	// ── the named ceiling: cataloged, never computable ────────────────────────────────
	mustRegister(f, Feature{
		ID: FeatureNextTS, Description: "The next request's own timestamp. Impossible by definition — the fact kvcache.Optimal alone reads, and only to compute a BOUND, never a policy's input.",
		SourceTable: "requests", SourceColumn: "ts (of the NEXT row)",
		AvailableAt: "never, at the decision instant", Availability: Impossible,
	})
	mustRegister(f, Feature{
		ID: FeatureIdleMs, Description: "How long the idle span starting now will turn out to last. Impossible by definition — see kvcache.Observation's own doc comment: 'a predictor that can see how long the gap turned out to be will predict it perfectly'.",
		SourceTable: "requests", SourceColumn: "(derived: next row's ts minus this row's ts)",
		AvailableAt: "never, at the decision instant", Availability: Impossible,
	})

	return f
}
