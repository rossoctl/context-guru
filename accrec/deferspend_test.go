package accrec

import (
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
	"github.com/rossoctl/context-guru/tooldefer"
)

// The search turns of one deferred request are three spend operations on the tooldefer operator, each a Call with its
// own usage (#1441).
func TestDeferSpendOps(t *testing.T) {
	ops := DeferSpendOps("m", tooldefer.SearchSpend{Output: 60, OutputEstimated: true, Loaded: 1500, Write1h: true, PromptTokens: 90_000})
	if len(ops) != 3 {
		t.Fatalf("%d operations, want 3: %+v", len(ops), ops)
	}
	ids := map[string]accountant.CallUsage{}
	for _, o := range ops {
		if o.Kind != accountant.KindSpend || o.Operator != accwire.Operator(accwire.OpToolDefer) || o.Call == nil || o.Call.Model != "m" || o.ChangeID != "" {
			t.Errorf("operation %+v: want a spend on the tooldefer operator with a call and no ChangeID", o)
		}
		if o.Call.Params[accwire.SpendParamEstimated] != "true" || o.Call.Params[accwire.SpendParamPromptTokens] != "90000" {
			t.Errorf("%s params %v: want estimated and the prompt size", o.ID, o.Call.Params)
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
	ops = DeferSpendOps("m", tooldefer.SearchSpend{Output: 40, Loaded: 100})
	if ops[0].Call.Params[accwire.SpendParamEstimated] != "" || ops[2].Call.Usage.CacheWrite1h != 0 {
		t.Errorf("stated output: %+v", ops)
	}
	// Nothing to pay, no operation.
	if got := DeferSpendOps("m", tooldefer.SearchSpend{}); len(got) != 0 {
		t.Errorf("empty spend made %d operations", len(got))
	}
}
