package accrec

import (
	"testing"

	"github.com/rossoctl/accountant"
	"github.com/rossoctl/context-guru/accwire"
)

// The gate is one decision for the dashboard row and the /stats ledger. Each gate names its reason.
func TestGate(t *testing.T) {
	mask := accwire.Operator("mask")
	op := accountant.PricedOp{Operator: mask, ChangeID: "c", Saved: accountant.TierTokens{CacheRead: 100}}
	res := accountant.Result{Ops: []accountant.PricedOp{op}}
	fig := accountant.USDFigure{ByComponent: map[string]float64{mask: 1}, ByComponentFirst: map[string]float64{}, ByComponentReplay: map[string]float64{}}
	rows := []GateRow{{Name: "mask", Acted: true}}
	// The library measures the request: 10 message tokens, and an operation that saved 100 of them.
	imp := op
	imp.SavedMessages = accountant.TierTokens{CacheRead: 100}
	implausible := accountant.Result{Ops: []accountant.PricedOp{imp}, MessageTokens: 10}
	ok := GateIn{Rows: rows}
	if r, _ := Gate(res, fig, ok); r != "" {
		t.Fatalf("a clean request is gated: %q", r)
	}
	for name, tc := range map[string]struct {
		res  accountant.Result
		fig  accountant.USDFigure
		in   GateIn
		want string
	}{
		"ops dropped": {res, fig, GateIn{OpsDropped: 1, Rows: rows}, GateOpsDropped},
		"observe":     {res, fig, GateIn{Observe: true, Rows: rows}, GateObserve},
		"implausible": {implausible, fig, ok, GateImplausibleSize},
		"no row":      {res, fig, GateIn{}, GateFigureWithoutRow},
		"duplicate":   {res, fig, GateIn{Rows: append(rows[:1:1], rows[0])}, GateDuplicateName},
		"acted":       {res, accountant.USDFigure{ByComponent: map[string]float64{}}, GateIn{Rows: rows}, GateActedWithoutFigure},
		"unpriced":    {accountant.Result{Ops: res.Ops, Unpriced: []accountant.Unpriced{{OpID: "o1", Reason: "no_price"}}}, accountant.USDFigure{ByComponent: fig.ByComponent, Unpriced: []string{"o1:no_price"}}, ok, GateUnpriced},
		"fatal check": {accountant.Result{Ops: res.Ops, Checks: []accountant.CheckFailure{{Check: accountant.CheckSum}}}, fig, ok, GateCheckSum},
	} {
		if r, _ := Gate(tc.res, tc.fig, tc.in); r != tc.want {
			t.Errorf("%s: reason %q, want %q", name, r, tc.want)
		}
	}
	// penalty_unknown does not gate: the figure is held, not refused.
	held := accountant.Result{Ops: res.Ops, Unpriced: []accountant.Unpriced{{Reason: accountant.ReasonPenaltyUnknown}}}
	if r, _ := Gate(held, fig, ok); r != "" {
		t.Errorf("penalty_unknown gated: %q", r)
	}
	// A sum failure that no operation claims does not gate (#1465): the library decides, with Withhold. A claimed one does.
	zero, one := 0, 1
	unclaimed := accountant.Result{Ops: res.Ops, Checks: []accountant.CheckFailure{{Check: accountant.CheckSum, Section: accountant.SectionMessages, Claims: &zero}}}
	claimed := accountant.Result{Ops: res.Ops, Checks: []accountant.CheckFailure{{Check: accountant.CheckSum, Section: accountant.SectionMessages, Claims: &one}}}
	if r, _ := Gate(unclaimed, fig, ok); r != "" {
		t.Errorf("an unclaimed sum failure gated: %q", r)
	}
	if r, _ := Gate(claimed, fig, ok); r != GateCheckSum {
		t.Errorf("a claimed sum failure gives %q, want %q", r, GateCheckSum)
	}
	// The same rule as the metrics push, which calls Withhold with no size.
	if accountant.Withhold(unclaimed) != "" || accountant.Withhold(claimed) != GateCheckSum {
		t.Error("the gate and accountant.Withhold disagree")
	}
}
