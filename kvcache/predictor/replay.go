package predictor

import (
	"fmt"
	"sort"
	"time"

	"github.com/rossoctl/context-guru/kvcache"
)

// Outcome is the ground truth for one decision point — computed AFTER the fact, and never
// handed to a Feature or a Predictor. It is built directly from kvcache.Derive's own
// output (HasNext/IdleMs/Within5m/Within1h), which is why Replay requires reqs to have
// already been through Derive.
type Outcome struct {
	// Censored is true when this row has NO OBSERVED SUCCESSOR within the window Replay was
	// given — the last request of its conversation. It is NOT "never came back": the
	// conversation may have continued past the window's edge, or gone quiet forever, and
	// this dataset cannot tell the two apart. Within5m/Within1h/IdleMs are meaningless on a
	// censored row and must not be scored as "did not return within the horizon" — that
	// would count an open span as a miss it never was.
	Censored bool
	Within5m bool
	Within1h bool
	// IdleMs is the actual gap that followed, nil exactly when Censored is true.
	IdleMs *int64
}

// Prediction is one predictor's answer for one horizon at one decision point.
type Prediction struct {
	P  float64
	OK bool
}

// DecisionPoint is one reconstructed decision: the request identity, the leak-free
// Observation built for it, every registered feature's value, every registered predictor's
// answer, and — last, and never fed to either of the first two — the ground truth.
type DecisionPoint struct {
	RequestID                 int64
	User, Conversation, Model string
	Now                       int64
	Observation               kvcache.Observation
	Features                  map[string]Value
	// Predictions is keyed "id@version@5m" / "id@version@1h" — see predictionKey.
	Predictions map[string]Prediction
	Outcome     Outcome
}

// predictionKey names one predictor's answer for one horizon inside DecisionPoint.Predictions.
func predictionKey(id, version, horizon string) string { return id + "@" + version + "@" + horizon }

// Replay walks reqs in GLOBAL wall-clock (ts, id) order — across every conversation at
// once, the same order kvcache.Simulate walks in and for the same reason: the statistics in
// StatsContext accumulate gaps as they close, and a gap in conversation B closing later, in
// wall-clock time, than a decision being made right now in conversation A must not be
// visible to that decision — grouping by conversation first and walking each to completion
// would let exactly that happen.
//
// Precondition: reqs has already been through kvcache.Derive. Outcome reads its Next*/
// HasNext/Idle*/Within* fields directly; Replay does not call Derive itself, because a
// caller handing in a deliberately filtered SUBSET of a larger, already-Derived dataset
// wants that subset's successors to still mean "the next request in the real history", not
// "the next request that happens to survive this filter" — which is what re-deriving on the
// subset would silently compute instead.
//
// Deterministic: for the same reqs, the same *Features and the same *Predictors, two calls
// produce identical output — the walk order is a stable sort on (ts, id), never map-range
// order; Predictors.List() and Features.List() are both sorted; and nothing here reads
// time.Now or any other ambient state.
func Replay(reqs []*kvcache.Request, freg *Features, preds *Predictors) ([]DecisionPoint, error) {
	if freg == nil {
		return nil, fmt.Errorf("predictor: Replay requires a non-nil Features registry")
	}
	if len(reqs) == 0 {
		return nil, nil
	}

	order := make([]*kvcache.Request, len(reqs))
	copy(order, reqs)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].TS != order[j].TS {
			return order[i].TS < order[j].TS
		}
		return order[i].ID < order[j].ID
	})

	stats := StatsContext{
		Hist: kvcache.NewHistory(), Clustered: NewClusteredHistory(), Tenant: NewTenantStats(),
		WindowStart: order[0].TS,
	}
	convRows := map[kvcache.Conversation][]*kvcache.Request{}

	registered := []Registered(nil)
	if preds != nil {
		registered = preds.List()
	}

	out := make([]DecisionPoint, 0, len(order))
	for _, r := range order {
		key := r.Key()
		group := convRows[key]
		i := len(group)

		// ── close the previous span, THEN decide — the no-leakage invariant in one line:
		// every statistic below reflects gaps that closed strictly before this instant.
		if i > 0 {
			prior := group[i-1]
			gap := time.Duration(r.TS-prior.TS) * time.Millisecond
			if gap < 0 {
				gap = 0
			}
			b := kvcache.BucketAt(prior.TS)
			stats.Hist.Observe(r.User, r.Model, b, gap)
			stats.Clustered.Observe(r.User, r.Model, b, kvcache.ClusterOf(prior.StopReason), gap)
		}

		group = append(group, r)
		convRows[key] = group

		// TenantStats sees every request, not just ones whose gap has closed — it is a fact
		// about arrivals, not about idle spans — and it is recorded BEFORE Extract for the
		// same reason Hist/Clustered are advanced before it: a read inside Extract must see
		// this request already counted, never one that has not happened yet.
		stats.Tenant.Observe(r.User, r.ConversationID, r.Model, r.ID, r.TS)

		o, feats := freg.Extract(group, i, stats)

		dp := DecisionPoint{
			RequestID: r.ID, User: r.User, Conversation: r.ConversationID, Model: r.Model,
			Now: r.TS, Observation: o, Features: feats,
			Predictions: make(map[string]Prediction, 2*len(registered)),
		}
		for _, rp := range registered {
			p5, ok5 := rp.Predictor.ReuseProbability(o, kvcache.Horizon5m)
			dp.Predictions[predictionKey(rp.ID, rp.Version, "5m")] = Prediction{P: p5, OK: ok5}
			p1, ok1 := rp.Predictor.ReuseProbability(o, kvcache.Horizon1h)
			dp.Predictions[predictionKey(rp.ID, rp.Version, "1h")] = Prediction{P: p1, OK: ok1}
		}

		dp.Outcome = Outcome{
			Censored: !r.HasNext, Within5m: r.Within5m, Within1h: r.Within1h, IdleMs: r.IdleMs,
		}
		out = append(out, dp)
	}
	return out, nil
}

// AtDecisionPoint reconstructs exactly what was available for ONE request: the same
// Observation, Features and Outcome Replay would have produced for it, found by running
// Replay over the full dataset and picking that row out.
//
// It deliberately does not take a shortcut of running the walk over a filtered slice
// ending at requestID: kvcache.History (and ClusteredHistory) are LIVE accumulators, and
// their state at any instant depends on every gap that closed strictly before it across the
// WHOLE dataset the caller considers in scope — filtering first would answer a different,
// leak-free-by-luck-only question. See kvcache.History's own doc comment.
func AtDecisionPoint(reqs []*kvcache.Request, requestID int64, freg *Features, preds *Predictors) (DecisionPoint, error) {
	all, err := Replay(reqs, freg, preds)
	if err != nil {
		return DecisionPoint{}, err
	}
	for _, dp := range all {
		if dp.RequestID == requestID {
			return dp, nil
		}
	}
	return DecisionPoint{}, fmt.Errorf("predictor: request %d not found in the given dataset", requestID)
}
