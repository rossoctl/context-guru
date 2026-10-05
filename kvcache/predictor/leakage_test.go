package predictor

import (
	"fmt"
	"testing"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// TestNoRegisteredFeatureReadsTheFuture is the loud, planted-signal check requirement #4
// asks for. It does not name features one at a time: it iterates EVERY feature
// DefaultFeatures() registers, present and lagged alike, so a newly added feature is
// covered automatically as long as it registers through *Features — the same guarantee
// kvcache's own TestStrategiesCannotSeeTheFuture gives every Strategy via Simulate.
//
// Every row after the decision point carries a distinctive, unmistakable value on every
// field a feature in this package reads (Reverts, Expands, CachedContext, MissReason, TS).
// If ANY feature's Value ever equals one of those markers, a future field leaked into a
// present decision.
func TestNoRegisteredFeatureReadsTheFuture(t *testing.T) {
	const n = 5
	rows := make([]*kvcache.Request, n)
	for k := 0; k < n; k++ {
		rows[k] = req(int64(k+1), "u", "s", base+int64(k)*60_000, "m",
			int64(700_000+k), // CachedContext — unique per row
			"tool_use", fmt.Sprintf("poison-reason-%d", k),
			900_000+k, 800_000+k) // Reverts, Expands — unique per row
	}
	rows = derive(rows)

	freg := DefaultFeatures()
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}

	for i := 0; i < n; i++ {
		// Close the gap exactly as Replay does, so stats-backed features have something
		// real (but strictly past) to answer from.
		if i > 0 {
			gap := time.Duration(rows[i].TS-rows[i-1].TS) * time.Millisecond
			b := kvcache.BucketAt(rows[i-1].TS)
			stats.Hist.Observe("u", "m", b, gap)
			stats.Clustered.Observe("u", "m", b, kvcache.ClusterOf(rows[i-1].StopReason), gap)
		}

		futureNum := map[float64]string{}
		futureStr := map[string]string{}
		for _, later := range rows[i+1:] {
			futureNum[float64(later.CachedContext)] = fmt.Sprintf("row %d's own CachedContext (future)", later.ID)
			futureNum[float64(later.Reverts)] = fmt.Sprintf("row %d's own Reverts (future)", later.ID)
			futureNum[float64(later.Expands)] = fmt.Sprintf("row %d's own Expands (future)", later.ID)
			futureNum[float64(later.TS)] = fmt.Sprintf("row %d's own timestamp (future)", later.ID)
			futureStr[later.MissReason] = fmt.Sprintf("row %d's own MissReason (future)", later.ID)
		}

		_, feats := freg.Extract(rows, i, stats)
		for id, v := range feats {
			if !v.Present {
				continue
			}
			if what, bad := futureNum[v.Num]; bad {
				t.Errorf("decision %d: feature %q = %v, which is %s", i, id, v.Num, what)
			}
			if v.Str != "" {
				if what, bad := futureStr[v.Str]; bad {
					t.Errorf("decision %d: feature %q = %q, which is %s", i, id, v.Str, what)
				}
			}
		}
	}
}

// TestPastCannotBeWidenedByReslicing proves the structural guarantee directly: a feature
// that tries to defeat the cap by reslicing past to its own capacity gets nothing wider,
// because Features.Extract handed it over with len == cap in the first place.
func TestPastCannotBeWidenedByReslicing(t *testing.T) {
	rows := derive([]*kvcache.Request{
		req(1, "u", "s", base, "m", 1, "tool_use", "hit", 0, 0),
		req(2, "u", "s", base+1000, "m", 2, "tool_use", "hit", 0, 0),
		req(3, "u", "s", base+2000, "m", 3, "end_turn", "hit", 0, 0),
	})
	f := NewFeatures()
	var widenedLen int
	if err := f.Register(Feature{
		ID: "cheat", Availability: Live,
		Extract: func(_ kvcache.Observation, past []*kvcache.Request, _ StatsContext) Value {
			wider := past[:cap(past)] // the attempted cheat
			widenedLen = len(wider)
			return Value{Present: true}
		},
	}); err != nil {
		t.Fatal(err)
	}
	stats := StatsContext{Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory()}
	f.Extract(rows, 2, stats)
	if widenedLen != 2 {
		t.Fatalf("reslicing past to its own capacity yielded len %d, want 2 — the decision "+
			"row (index 2) must stay unreachable", widenedLen)
	}
}

// TestTenantIsolationAcrossASharedConversationID mirrors kvcache's own
// TestDeriveNeverCrossesAConversationBoundary: two tenants presenting the SAME session id
// (client-supplied, so not hypothetical) must never have their rows spliced into one
// trajectory's features.
func TestTenantIsolationAcrossASharedConversationID(t *testing.T) {
	shared := "same-session-id"
	rows := derive([]*kvcache.Request{
		req(1, "tenant-a", shared, base, "m", 111, "tool_use", "hit", 0, 0),
		req(2, "tenant-b", shared, base+1000, "m", 999_999, "tool_use", "hit", 555, 666),
		req(3, "tenant-a", shared, base+60_000, "m", 222, "end_turn", "hit", 0, 0),
	})
	dps, err := Replay(rows, DefaultFeatures(), NewPredictors())
	if err != nil {
		t.Fatal(err)
	}
	var third DecisionPoint
	for _, dp := range dps {
		if dp.RequestID == 3 {
			third = dp
		}
	}
	if third.Observation.SinceLastMs != 60_000 {
		t.Errorf("tenant-a's SinceLastMs = %d, want 60000 — tenant-b's request must not "+
			"shorten tenant-a's own gap", third.Observation.SinceLastMs)
	}
	if v := third.Features[FeatureRevertsLag1]; v.Present && v.Num == 555 {
		t.Error("tenant-a's reverts_lag1 reflects tenant-b's Reverts — cross-tenant leak " +
			"through a shared, client-supplied session id")
	}
	if v := third.Features[FeatureCachedTokens]; v.Num == 999_999 {
		t.Error("tenant-a's own cached_tokens reflects tenant-b's row")
	}
}
