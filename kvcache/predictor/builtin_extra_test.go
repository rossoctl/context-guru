package predictor

import (
	"math"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// value is a small helper: extract feature id's Value from Extract's output, failing loudly
// if the feature is not even registered — a typo in a test's own feature id must not read as
// "the feature is legitimately absent".
func value(t *testing.T, feats map[string]Value, id string) Value {
	t.Helper()
	v, ok := feats[id]
	if !ok {
		t.Fatalf("feature %q is not registered", id)
	}
	return v
}

// TestEveryRegisteredFeatureDeclaresAvailabilityAndSource is the catalog-completeness check
// requirement #8 asks for: a Feature with a blank SourceTable/SourceColumn is not
// self-documenting, and Register already refuses a blank Availability, so this only needs to
// check the source columns.
func TestEveryRegisteredFeatureDeclaresAvailabilityAndSource(t *testing.T) {
	for _, feat := range DefaultFeatures().List() {
		if feat.Availability == "" {
			t.Errorf("feature %q: Availability is blank", feat.ID)
		}
		if feat.SourceTable == "" || feat.SourceColumn == "" {
			t.Errorf("feature %q: SourceTable=%q SourceColumn=%q, want both non-empty",
				feat.ID, feat.SourceTable, feat.SourceColumn)
		}
	}
}

// TestDefaultFeaturesToleratesTheZeroValueRequest is the invalid/missing-config fallback
// check: a request with every optional field unset (no temperature, no preset, no cache
// breakpoints) must extract without panicking, and every optional feature must come back
// absent rather than a fabricated zero standing in for "unknown".
func TestDefaultFeaturesToleratesTheZeroValueRequest(t *testing.T) {
	rows := derive([]*kvcache.Request{{ID: 1, User: "u", ConversationID: "s", TS: base, Model: "m"}})
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory(), Tenant: NewTenantStats()}
	_, feats := DefaultFeatures().Extract(rows, 0, stats)
	if v := value(t, feats, FeatureTemperature); v.Present {
		t.Error("Temperature must be absent, not 0, when the request carried none")
	}
	if v := value(t, feats, FeatureTopP); v.Present {
		t.Error("TopP must be absent when the request carried none")
	}
	if v := value(t, feats, FeatureMaxTokens); !v.Present || v.Num != 0 {
		t.Errorf("MaxTokens = %+v, want Present=true Num=0 (0 is a legitimate unset default)", v)
	}
}

func TestTimeFeaturesAgreeWithTheClock(t *testing.T) {
	// 2026-01-04 is a Sunday, 15:30 UTC.
	ts := time.Date(2026, 1, 4, 15, 30, 0, 0, time.UTC).UnixMilli()
	rows := derive([]*kvcache.Request{{ID: 1, User: "u", ConversationID: "s", TS: ts, Model: "m",
		HourUTC: 15, Bucket: kvcache.BucketAt(ts)}})
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	_, feats := DefaultFeatures().Extract(rows, 0, stats)

	wantSin, wantCos := math.Sin(2*math.Pi*15/24), math.Cos(2*math.Pi*15/24)
	if v := value(t, feats, FeatureHourSin); math.Abs(v.Num-wantSin) > 1e-9 {
		t.Errorf("hour_sin = %v, want %v", v.Num, wantSin)
	}
	if v := value(t, feats, FeatureHourCos); math.Abs(v.Num-wantCos) > 1e-9 {
		t.Errorf("hour_cos = %v, want %v", v.Num, wantCos)
	}
	if v := value(t, feats, FeatureIsWeekend); v.Num != 1 {
		t.Errorf("is_weekend = %v on a Sunday, want 1", v.Num)
	}
	if v := value(t, feats, FeatureMinuteOfHour); v.Num != 30 {
		t.Errorf("minute_of_hour = %v, want 30", v.Num)
	}
}

func TestTimeSinceWindowStartIsAbsentWithoutOne(t *testing.T) {
	rows := derive([]*kvcache.Request{{ID: 1, User: "u", ConversationID: "s", TS: base, Model: "m"}})
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()} // WindowStart unset
	_, feats := DefaultFeatures().Extract(rows, 0, stats)
	if v := value(t, feats, FeatureTimeSinceWindowStartMs); v.Present {
		t.Error("time_since_window_start_ms must be absent when the caller supplied no WindowStart")
	}
}

// TestModelSwitchedInSessionOnlyFiresAcrossAModelChange drives real accumulation through
// Replay (not a hand-built StatsContext), the same footing the tenant/session-count family's
// own tests below stand on: TenantStats must actually be fed by the replay walk for this
// feature to answer anything but "absent".
func TestModelSwitchedInSessionOnlyFiresAcrossAModelChange(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "model-a", 100, "tool_use", "hit", 0, 0),
		req(2, "u", "s", base+60_000, "model-a", 100, "tool_use", "hit", 0, 0),
		req(3, "u", "s", base+120_000, "model-b", 100, "tool_use", "hit", 0, 0),
	})
	dps, err := Replay(rows, DefaultFeatures(), NewPredictors())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]DecisionPoint{}
	for _, dp := range dps {
		byID[dp.RequestID] = dp
	}
	if v := byID[1].Features[FeatureModelSwitchedInSession]; v.Num != 0 {
		t.Errorf("request 1: model_switched_in_session = %v, want 0 (only one model seen)", v.Num)
	}
	if v := byID[3].Features[FeatureModelSwitchedInSession]; v.Num != 1 {
		t.Errorf("request 3: model_switched_in_session = %v, want 1 (model-b joined model-a in session s)", v.Num)
	}
	// And the ordinary per-model conversation key must still see request 3 as a FRESH
	// conversation (Turn=1), proving this feature did not have to widen kvcache.Conversation
	// itself to answer — see TenantStats.ModelSwitchedInSession's own doc comment.
	if v := byID[3].Features[FeatureTurn]; v.Num != 1 {
		t.Errorf("request 3: turn = %v, want 1 (model-b's own conversation key has no history yet)", v.Num)
	}
}

// TestTenantStatsFamilyAccumulatesAcrossSessions is the tenant/session-count family's
// integration test: cold start fires exactly once, distinct sessions grows with each NEW
// session id, and concurrent sessions counts an overlapping OTHER session but not a stale
// one.
func TestTenantStatsFamilyAccumulatesAcrossSessions(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "t1", "s1", base, "m", 100, "tool_use", "hit", 0, 0),
		req(2, "t1", "s2", base+1_000, "m", 100, "tool_use", "hit", 0, 0), // concurrent with s1
		req(3, "t1", "s1", base+2_000, "m", 100, "tool_use", "hit", 0, 0),
		req(4, "t1", "s3", base+time.Hour.Milliseconds(), "m", 100, "tool_use", "hit", 0, 0), // s1/s2 gone stale
	})
	dps, err := Replay(rows, DefaultFeatures(), NewPredictors())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]DecisionPoint{}
	for _, dp := range dps {
		byID[dp.RequestID] = dp
	}
	if v := byID[1].Features[FeatureColdStart]; v.Num != 1 {
		t.Errorf("request 1: cold_start = %v, want 1 (tenant t1's first request)", v.Num)
	}
	if v := byID[2].Features[FeatureColdStart]; v.Num != 0 {
		t.Errorf("request 2: cold_start = %v, want 0", v.Num)
	}
	if v := byID[2].Features[FeatureDistinctSessions]; v.Num != 2 {
		t.Errorf("request 2: distinct_sessions_for_tenant = %v, want 2 (s1, s2)", v.Num)
	}
	if v := byID[3].Features[FeatureConcurrentSessions]; v.Num != 1 {
		t.Errorf("request 3: concurrent_sessions_for_tenant = %v, want 1 (s2, active 2s ago)", v.Num)
	}
	if v := byID[4].Features[FeatureConcurrentSessions]; v.Num != 0 {
		t.Errorf("request 4: concurrent_sessions_for_tenant = %v, want 0 (s1/s2 last active an hour ago)", v.Num)
	}
	if v := byID[4].Features[FeatureDistinctSessions]; v.Num != 3 {
		t.Errorf("request 4: distinct_sessions_for_tenant = %v, want 3 (s1, s2, s3)", v.Num)
	}
	rate, ok := byID[4].Features[FeatureTenantRequestRatePerHour].Num, byID[4].Features[FeatureTenantRequestRatePerHour].Present
	if !ok {
		t.Fatal("request 4: tenant_request_rate_per_hour absent, want present (an hour has elapsed since request 1)")
	}
	if rate != 4.0 { // 4 requests over exactly 1 hour
		t.Errorf("request 4: tenant_request_rate_per_hour = %v, want 4", rate)
	}
	if v := byID[1].Features[FeatureTenantRequestRatePerHour]; v.Present {
		t.Error("request 1: tenant_request_rate_per_hour must be absent — zero elapsed time is not a rate")
	}
}

// TestTenantAgeDaysNeedsAnExplicitlySuppliedMap proves FeatureTenantAgeDays is not a stub: it
// answers correctly when a caller supplies StatsContext.TenantCreatedAt, and stays honestly
// absent otherwise (Replay never populates this map — see that field's own doc comment).
func TestTenantAgeDaysNeedsAnExplicitlySuppliedMap(t *testing.T) {
	rows := derive([]*kvcache.Request{{ID: 1, User: "t1", ConversationID: "s", TS: base, Model: "m"}})

	withoutMap := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	_, feats := DefaultFeatures().Extract(rows, 0, withoutMap)
	if v := value(t, feats, FeatureTenantAgeDays); v.Present {
		t.Error("tenant_age_days must be absent when no TenantCreatedAt map was supplied")
	}

	createdAt := base - 10*24*time.Hour.Milliseconds() // account created 10 days before this request
	withMap := StatsContext{
		Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory(),
		TenantCreatedAt: map[string]int64{"t1": createdAt},
	}
	_, feats = DefaultFeatures().Extract(rows, 0, withMap)
	if v := value(t, feats, FeatureTenantAgeDays); !v.Present || math.Abs(v.Num-10) > 1e-6 {
		t.Errorf("tenant_age_days = %+v, want Present=true Num=10", v)
	}
}

func TestFrequencyFeaturesOnConstantGaps(t *testing.T) {
	// Five requests, exactly 60s apart — a constant-gap conversation.
	rows := make([]*kvcache.Request, 5)
	for k := range rows {
		rows[k] = req(int64(k+1), "u", "s", base+int64(k)*60_000, "m", 100, "tool_use", "hit", 0, 0)
	}
	rows = derive(rows)
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	_, feats := DefaultFeatures().Extract(rows, 4, stats) // the fifth request: 4 closed gaps, all 60s

	if v := value(t, feats, FeatureGapLag1); v.Num != 60_000 {
		t.Errorf("gap_lag1 = %v, want 60000", v.Num)
	}
	if v := value(t, feats, FeatureGapLag1); v.Num != float64(rows[4].TS-rows[3].TS) {
		t.Error("gap_lag1 must equal SinceLastMs's own arithmetic")
	}
	if v := value(t, feats, FeatureRollingMedianGapMs); v.Num != 60_000 {
		t.Errorf("rolling_median_gap_ms = %v, want 60000", v.Num)
	}
	if v := value(t, feats, FeatureRollingIQRGapMs); v.Num != 0 {
		t.Errorf("rolling_iqr_gap_ms = %v, want 0 on constant gaps", v.Num)
	}
	if v := value(t, feats, FeatureGapVarianceMs2); v.Num != 0 {
		t.Errorf("gap_variance_ms2 = %v, want 0 on constant gaps", v.Num)
	}
	if v := value(t, feats, FeatureBurstIndicator); v.Num != 0 {
		t.Errorf("burst_indicator = %v, want 0 — the just-closed gap matches its own history, not a burst", v.Num)
	}
	if v := value(t, feats, FeatureRequestsPrev5m); v.Num != 4 {
		t.Errorf("requests_prev_5m = %v, want 4 (all four past requests are within 5 minutes)", v.Num)
	}
}

func TestBurstIndicatorFiresOnAShortGapAfterLongOnes(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "m", 100, "tool_use", "hit", 0, 0),
		req(2, "u", "s", base+600_000, "m", 100, "tool_use", "hit", 0, 0),   // 10 min gap
		req(3, "u", "s", base+1_200_000, "m", 100, "tool_use", "hit", 0, 0), // another 10 min gap
		req(4, "u", "s", base+1_201_000, "m", 100, "tool_use", "hit", 0, 0), // 1s gap — a burst
	})
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	_, feats := DefaultFeatures().Extract(rows, 3, stats)
	if v := value(t, feats, FeatureBurstIndicator); v.Num != 1 {
		t.Errorf("burst_indicator = %v, want 1 — a 1s gap after two 10-minute ones is a burst", v.Num)
	}
}

func TestPrefixSizeDeltaAndSecondsToTTLDeadline(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "m", 100_000, "tool_use", "hit", 0, 0),
		req(2, "u", "s", base+60_000, "m", 150_000, "tool_use", "hit", 0, 0), // grew by 50k, 60s after a 5m-TTL row
	})
	rows[0].TTL = kvcache.TTL5m
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	_, feats := DefaultFeatures().Extract(rows, 1, stats)
	if v := value(t, feats, FeaturePrefixSizeDelta); v.Num != 50_000 {
		t.Errorf("prefix_size_delta = %v, want 50000", v.Num)
	}
	// 5m lifetime (300s) minus a 60s gap = 240s of margin left.
	if v := value(t, feats, FeatureSecondsToTTLDeadline); v.Num != 240 {
		t.Errorf("seconds_to_ttl_deadline = %v, want 240", v.Num)
	}
	// First request: no prior entry to measure a margin against.
	_, feats0 := DefaultFeatures().Extract(rows, 0, stats)
	if v := value(t, feats0, FeaturePrefixSizeDelta); v.Num != 0 {
		t.Errorf("first request prefix_size_delta = %v, want 0", v.Num)
	}
	if v := value(t, feats0, FeatureSecondsToTTLDeadline); v.Present {
		t.Error("first request seconds_to_ttl_deadline must be absent")
	}
}

func TestReuseWithin1hMirrorsThe5mFamily(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s1", base, "m", 100, "end_turn", "hit", 0, 0),
		req(2, "u", "s2", base+30*time.Minute.Milliseconds(), "m", 100, "end_turn", "hit", 0, 0),
	})
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	stats.Hist.Observe("u", "m", kvcache.BucketAt(rows[0].TS), 30*time.Minute)
	_, feats := DefaultFeatures().Extract(rows, 1, stats)
	if v := value(t, feats, FeatureReuseWithin1h); !v.Present || v.Num != 1 {
		t.Errorf("reuse_within_1h = %+v, want Present=true Num=1 (30 min is within the 1h horizon)", v)
	}
	if v := value(t, feats, FeatureReuseWithin5m); !v.Present || v.Num != 0 {
		t.Errorf("reuse_within_5m = %+v, want Present=true Num=0 — the only observed gap (30 min) misses the 5m horizon", v)
	}
}
