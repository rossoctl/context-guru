package predictor

import (
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
