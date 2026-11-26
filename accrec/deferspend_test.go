package accrec

import (
	"math"
	"testing"
	"time"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/tooldefer"
)

// The search turns of one deferred request are three spend operations on the tooldefer operator, each a Call with its
// own usage (#1441).
func TestDeferSpendOps(t *testing.T) {
	ops := DeferSpendOps(tooldefer.SearchSpend{Output: 60, OutputEstimated: true, Loaded: 1500, Write1h: true})
	if len(ops) != 3 {
		t.Fatalf("%d operations, want 3: %+v", len(ops), ops)
	}
	ids := map[string]accountant.CallUsage{}
	for _, o := range ops {
		if o.Kind != accountant.KindSpend || o.Operator != accwire.Operator(accwire.OpToolDefer) || o.Call == nil || o.Call.Model != "" || o.ChangeID != "" {
			t.Errorf("operation %+v: want a spend on the tooldefer operator with a call and no ChangeID", o)
		}
		if o.Call.Params[accwire.SpendParamEstimated] != "true" {
			t.Errorf("%s params %v: want estimated", o.ID, o.Call.Params)
		}
		ids[o.ID] = o.Call.Usage
	}
	if u := ids["defer-search-output"]; u.Output != 60 || u.Input != 0 {
		t.Errorf("output usage %+v", u)
	}
	if u := ids["defer-search-second-pass"]; u.Input != 1500 || u.CacheWrite != 0 {
		t.Errorf("second pass usage %+v", u)
	}
	if u := ids["defer-search-next-write"]; u.CacheWrite != 1500 || u.CacheWrite1h != 1500 {
		t.Errorf("next write usage %+v, want a 1-hour write", u)
	}
	// A stated output is not an estimate, and a 5-minute thread writes at the 5-minute price.
	ops = DeferSpendOps(tooldefer.SearchSpend{Output: 40, Loaded: 100})
	if ops[0].Call.Params[accwire.SpendParamEstimated] != "" || ops[2].Call.Usage.CacheWrite1h != 0 {
		t.Errorf("stated output: %+v", ops)
	}
	// Nothing to pay, no operation.
	if got := DeferSpendOps(tooldefer.SearchSpend{}); len(got) != 0 {
		t.Errorf("empty spend made %d operations", len(got))
	}
}

type flatPricer struct{}

func (flatPricer) PriceAt(_ string, _ time.Time, t accountant.Tier, _ int64) (float64, bool) {
	switch t {
	case accountant.TierCacheRead:
		return 0.3e-6, false
	case accountant.TierCacheWrite5m:
		return 3.75e-6, false
	case accountant.TierOutput:
		return 15e-6, false
	}
	return 3e-6, false
}

// DeferMoney maps the library's figure: net is the tooldefer component less its spend lines, gross is its saved side,
// cost is gross less net. It prices nothing itself.
func TestDeferMoneyMapsTheLibrarysFigure(t *testing.T) {
	op := accwire.Operator(accwire.OpToolDefer)
	ops := []accountant.PricedOp{{OpID: "r", Kind: accountant.KindRewrite, Operator: op, ChangeID: "c", Saved: accountant.TierTokens{CacheRead: 20_000}}}
	for _, o := range DeferSpendOps(tooldefer.SearchSpend{Output: 30, OutputEstimated: true, Loaded: 1500}) {
		ops = append(ops, accountant.PricedOp{OpID: o.ID, Kind: o.Kind, Operator: o.Operator, Call: o.Call})
	}
	f := accountant.Figure(accountant.FigureInput{Model: "m", Billed: accountant.Usage{CacheRead: 100_000}, Output: 100,
		Result: accountant.Result{Ops: ops}}, flatPricer{})
	m := DeferMoney(f)
	gross := 20_000 * 0.3e-6
	spend := 30*15e-6 + 1500*3e-6 + 1500*3.75e-6
	if math.Abs(m.Gross-gross) > 1e-12 || math.Abs(m.Net-(gross-spend)) > 1e-12 || math.Abs(m.Cost-spend) > 1e-12 {
		t.Fatalf("%+v, want gross %v net %v cost %v", m, gross, gross-spend, spend)
	}
	if Held(accountant.Result{}) || !Held(accountant.Result{Unpriced: []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}}}) {
		t.Fatal("Held")
	}
}
